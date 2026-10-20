package recall_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：召回面「排序可插拔」的用例。
//
// # 这条缝给谁用
//
// 走语义轨的宿主有自己的打分口径（向量相似度、双轨归并……）。
// 没有注入缝，它只能**绕开整条召回链自造一套**——
// 而两套排序口径并存，后续无论往哪边收敛都要付迁移成本。
//
// # 默认零漂移
//
// 不注入打分器时，行为与打分器这个端口出现之前**逐条等价**
// （那份等价性由 ports 包的用例钉住：TestLexicalRankerMatchesLegacyFormula）。

// fixedRanker 给每条候选打固定的分数（按候选下标递减）。
type fixedRanker struct {
	// reverse true 时第一条排最后——用来验证「注入的打分器说了算」。
	reverse bool
	// calls 收到过几次打分。
	calls int
	// sawQuery 上一次打分时看到的查询词。
	sawQuery string
	// sawBodies 上一次打分时看到的正文形态。
	sawBodies []string
}

func (r *fixedRanker) Score(_ context.Context, query string, cands []ports.RankCandidate,
	_ time.Time) ([]float64, error) {
	r.calls++
	r.sawQuery = query
	r.sawBodies = r.sawBodies[:0]
	for _, c := range cands {
		r.sawBodies = append(r.sawBodies, c.Body)
	}
	out := make([]float64, len(cands))
	for i := range cands {
		if r.reverse {
			out[i] = float64(len(cands) - i)
			continue
		}
		out[i] = float64(i + 1)
	}
	return out, nil
}

// badLenRanker 返回的分数条数与候选数不符。
type badLenRanker struct{}

func (badLenRanker) Score(_ context.Context, _ string, cands []ports.RankCandidate,
	_ time.Time) ([]float64, error) {
	return make([]float64, len(cands)+1), nil
}

// errRanker 打分失败。
type errRanker struct{}

func (errRanker) Score(_ context.Context, _ string, _ []ports.RankCandidate,
	_ time.Time) ([]float64, error) {
	return nil, errors.New("打分器坏了")
}

// 注入的打分器说了算。
func TestInjectedRankerDecidesOrder(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
		atom("a2", "e2", "内存占用偏高", types.ImportanceLow),
	}}
	r := &fixedRanker{reverse: true}
	cfg := recall.DefaultConfig()
	cfg.Ranker = r
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"})
	if err != nil {
		t.Fatal(err)
	}
	if r.calls == 0 {
		t.Fatal("召回没调用注入的打分器")
	}
	if r.sawQuery != "磁盘" {
		t.Fatalf("打分器没拿到查询词: %q", r.sawQuery)
	}
	if len(res.Snippets) < 2 {
		t.Fatalf("召回结果不足两条: %d", len(res.Snippets))
	}
	// 注入的打分器把 a2 排在最前，结果就该是 a2 在前。
	if res.Snippets[0].RefID != "a2" {
		t.Fatalf("注入的打分器没起作用，首条是 %s", res.Snippets[0].RefID)
	}
}

// 打分器拿到的正文必须是**未包裹**的：标签词每段都有，
// 算进去会让所有候选的得分虚高且完全同分。
func TestInjectedRankerSeesUnwrappedBody(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	r := &fixedRanker{}
	cfg := recall.DefaultConfig()
	cfg.Ranker = r
	p := newPipe(t, f, cfg)
	if _, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"}); err != nil {
		t.Fatal(err)
	}
	if len(r.sawBodies) == 0 {
		t.Fatal("打分器没拿到候选")
	}
	if r.sawBodies[0] != "磁盘长期偏小" {
		t.Fatalf("打分器拿到的正文被包裹过了: %q", r.sawBodies[0])
	}
}

// 分数与候选错位必须报错：错位不会崩，只会静默换序。
func TestRankerScoreCountMismatchFails(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	cfg := recall.DefaultConfig()
	cfg.Ranker = badLenRanker{}
	p := newPipe(t, f, cfg)
	if _, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"}); err == nil {
		t.Fatal("分数条数与候选数不符时必须报错：错位只会静默换序")
	}
}

// 打分失败必须上抛，不许静默回退到缺省打分器。
//
// 静默回退的失效形态是「排序悄悄变回老样子」，而调用方看不出发生了什么。
func TestRankerFailurePropagates(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	cfg := recall.DefaultConfig()
	cfg.Ranker = errRanker{}
	p := newPipe(t, f, cfg)
	_, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"})
	if err == nil {
		t.Fatal("打分失败必须上抛")
	}
	if !strings.Contains(err.Error(), "打分器坏了") {
		t.Fatalf("错误信息要带原因: %v", err)
	}
}
