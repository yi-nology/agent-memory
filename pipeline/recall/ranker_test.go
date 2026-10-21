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

// fixedRanker 给每条候选打固定的分数。
type fixedRanker struct {
	// reverse true 时**第一条排最后**——用来验证「注入的打分器说了算」。
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
			// reverse：第一条拿**最低**分，于是它排到最后。
			out[i] = float64(i + 1)
			continue
		}
		// 默认：第一条拿最高分，于是它排在最前。
		out[i] = float64(len(cands) - i)
	}
	return out, nil
}

// relevanceRanker 按「正文命中查询词」打分：命中的分高，不命中的分低。
//
// 它是**语义轨打分器的最小替身**：宿主接了自己的向量召回时，
// 契约就是这样——分数大小决定次序，绝对值没有意义。
type relevanceRanker struct{}

func (relevanceRanker) Score(_ context.Context, query string, cands []ports.RankCandidate,
	_ time.Time) ([]float64, error) {
	out := make([]float64, len(cands))
	for i, c := range cands {
		if query != "" && strings.Contains(c.Body, query) {
			out[i] = 1
			continue
		}
		out[i] = 0
	}
	return out, nil
}

// fixedEstimator 每段都算同样多的 token：让「预算只装得下一条」这件事可复现。
//
// 用真实计量器的话，token 数取决于正文长度与包裹标签，
// 预算边界就成了「差不多刚好」，而差不多刚好正是那种**偶发红**的来源。
type fixedEstimator struct{ n int }

func (e fixedEstimator) Estimate(context.Context, string) (int, error) { return e.n, nil }

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

// ── 排序方向 ──

// 分数大的必须在前。
//
// 这条是「排序方向写反」那条缺陷的钉子。
//
// # 为什么这条曾经是绿的
//
// 初版有两处**方向相反**的错，互相抵消了：
//
//   - 本文件里的 fixedRanker.reverse 分支给第一条**最高**分
//     （与它自己注释里「第一条排最后」相反）；
//   - 召回层的排序是**升序**（与 ports.Ranker 的契约「分数越大越靠前」相反）。
//
// 一处把第一条顶到前面，一处把它压到最后，于是「注入的打分器说了算」
// 这条断言照样成立——**用例是绿的，实现是错的**。
//
// 只修排序不改这里，那条用例会红；只改这里不修排序，用例照样绿而实现照样错。
// 两处必须一起改，且各自都要有自己的断言——
// 故下面两条用例分别钉「次序」与「预算取舍」，
// 它们不依赖 fixedRanker 的语义，故不受那次抵消影响。
func TestHigherScoreComesFirst(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "内存占用偏高", types.ImportanceLow),
		atom("a2", "e2", "磁盘长期偏小", types.ImportanceHigh),
	}}
	cfg := recall.DefaultConfig()
	cfg.Ranker = relevanceRanker{}
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) < 2 {
		t.Fatalf("召回结果不足两条: %d", len(res.Snippets))
	}
	// 命中查询词的那条得分高，必须在前。
	if res.Snippets[0].RefID != "a2" {
		t.Fatalf("分数高的没排在前面，首条是 %s", res.Snippets[0].RefID)
	}
}

// 预算装不下时，留下的必须是**更相关**的那条。
//
// 这条比次序那条更要命：排序错了只影响「谁在前」，
// 而预算分配是**按次序逐条接收**的，于是排序一错，
// 预算一紧就先把最该保留的事实丢掉——
// 而症状只是「召回里少了几条」，看着像内容不够，不像是排序错了。
func TestTightBudgetKeepsMostRelevant(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "内存占用偏高", types.ImportanceLow),
		atom("a2", "e2", "磁盘长期偏小", types.ImportanceHigh),
	}}
	cfg := recall.DefaultConfig()
	cfg.Ranker = relevanceRanker{}
	// 事实桶配额 = int(15 × 0.7) = 10；每段固定 10 token ⇒ 只装得下一条。
	cfg.TotalBudget = 15
	p, err := recall.New(cfg, f, fixedEstimator{n: 10}, ports.MemoryBand{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) != 1 {
		t.Fatalf("预算只装得下一条，实得 %d 条", len(res.Snippets))
	}
	if res.Snippets[0].RefID != "a2" {
		t.Fatalf("预算紧时装下的应是更相关的那条，实得 %s", res.Snippets[0].RefID)
	}
}
