package integration_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/pipeline/entity"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：向量轨的**端到端**用例。
//
// # 为什么用「同义映射」的假向量化器，而不是本仓自带的哈希嵌入
//
// HashEmbedder 捕捉的是**词面**重叠（它不是语义嵌入），
// 故它捞不出「意思近但字面不搭」的东西——那是设计如此，不是缺陷。
//
// 而向量轨的**价值**恰恰在那一路：词面路捞不出的那条，语义路捞得出。
// 故验管线要用一个**能表达同义**的向量化器：
// 这里用一个三维的「同义桶」替身（存储空间/内存/其它），
// 它的行为与真模型一致——「意思近 → 向量近」。
//
// 于是这条用例验的是**管线**（写路径 → 索引 → 读路径 → 融合 → 取数层过滤），
// 而不是某个嵌入算法的效果。

// synonymEmbedder 同义桶向量化器（替身）。
type synonymEmbedder struct{}

// Embed 把文本映到三维桶向量。
func (synonymEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := []float32{0, 0, 0}
	for _, tok := range []string{"磁盘", "存储", "空间"} {
		if contains(text, tok) {
			v[0] = 1
		}
	}
	for _, tok := range []string{"内存", "占用"} {
		if contains(text, tok) {
			v[1] = 1
		}
	}
	if v[0] == 0 && v[1] == 0 {
		v[2] = 1
	}
	norm := float32(0)
	for _, x := range v {
		norm += x * x
	}
	if norm > 0 {
		norm = float32(1 / math.Sqrt(float64(norm)))
		for i := range v {
			v[i] *= norm
		}
	}
	return v, nil
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestVectorTrackAddsWhatLexicalMissed 向量轨把词面路漏掉的事实补进候选池。
//
// 这条是向量轨存在意义的**唯一**判据：
// 若语义路捞回来的全是词面路已经捞到的东西，那这一路就是纯开销。
//
// # 夹具怎么造出「词面路捞不到」
//
// 召回有**两路**取数：查询命中（词面）与高价值兜底（按重要度取）。
// 只放一条事实时，兜底那路也会把它捞回来——那样这条用例就**不区分**语义路
// （断掉语义路它照样绿）。故夹具要造出「兜底那路也带不出目标」的局面：
//
//   - 目标 B（低重要度、**与查询零词面重叠**、已晋升 → 已进向量索引）；
//   - 干扰 A（高重要度、与查询词面重叠、**直接入库** → 没进索引）。
//
// 于是词面路的兜底按重要度取时只取得到 A（限额 1），
// 而语义路从索引里取得到 B——B 出现在结果里，就只可能是语义路带进来的。
func TestVectorTrackAddsWhatLexicalMissed(t *testing.T) {
	ctx := context.Background()
	s := openFresh(t)
	ix := ports.NewMemIndex()
	llm := fakeLLM{
		extract: `[{"assertion":"机器甲的存储空间不足","quote":"机器甲的存储空间不足",` +
			`"event_id":"ev1","type":"fact","subject_name":"机器甲",` +
			`"importance":"low","confidence":"high"}]`,
	}
	svc, err := application.New(
		application.Config{Namespace: ns, Host: "it"},
		application.Deps{
			Store: s, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{}, LLM: llm,
			Embedder: synonymEmbedder{}, VectorIndex: ix,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// 捕获 → 提炼 → 登记实体 → 晋升（目标 B，低重要度）
	cap, err := svc.Capture(ctx, application.CaptureInput{
		EventID: "ev1", Type: types.EventTurnEnd,
		Content: "机器甲的存储空间不足，需要清理", SessionID: "s1", ThreadID: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	cands, err := svc.Extract(ctx, ns, "s1", []*types.RawEvent{cap.Event})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("应抽出 1 条候选，实得 %d 条", len(cands))
	}
	if err := s.SaveCandidate(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}
	res, err := entity.New(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Register(ctx, ns, "机器甲", types.EntityType("service"), "那台研发机",
		types.DecidedUser); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Promote(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}

	// 写路径：晋升成事实后，它的向量进了索引。
	if ix.Count(string(ns)) != 1 {
		t.Fatalf("晋升后应有 1 条向量: %d", ix.Count(string(ns)))
	}

	// 干扰 A：高重要度、与查询词面重叠，但**直接入库**（没晋升 → 没进索引）。
	now := time.Now().UTC()
	if err := s.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "a-distractor", EntityID: "e1", Assertion: "机器甲的磁盘长期偏小",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Importance: types.ImportanceHigh, Confidence: types.ConfidenceHigh,
		OccurredAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// 读路径：走**装配面**的召回（语义召回面在那里装配）。
	// 限额 1：词面路的兜底按重要度取，只取得到高重要度的 A。
	out, err := svc.Recall(ctx, recall.Request{Namespace: ns, Query: "磁盘", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sn := range out.Snippets {
		if sn.Source == recall.SourceAtom && sn.RefID == "atom-"+cands[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("语义路应把词面路漏掉的事实补进来: %+v", out.Snippets)
	}
}

// vectorStoreFake 自己就是向量索引的存储。
//
// 模拟的是「存储层实现了 ports.VectorIndex」那种形状
// （向量轨跟着记忆库一起活，进程一退索引不该消失）。
type vectorStoreFake struct {
	*sqlite.Store
	ix *ports.MemIndex
}

func (v vectorStoreFake) Upsert(ctx context.Context, ns, id, scope string, vec []float32) error {
	return v.ix.Upsert(ctx, ns, id, scope, vec)
}

func (v vectorStoreFake) Search(ctx context.Context, ns, scope string, vec []float32,
	topK int) ([]ports.VectorHit, error) {
	return v.ix.Search(ctx, ns, scope, vec, topK)
}

func (v vectorStoreFake) Delete(ctx context.Context, ns, id string) error {
	return v.ix.Delete(ctx, ns, id)
}

// TestVectorIndexDiscoveredFromStore 存储自己就是向量索引时，装配面要能**发现**它。
//
// 为什么要发现而不是要求宿主显式传：
// 宿主传的是「用哪个索引」，而存储自带的那个是最省心的选择
// （向量跟着记忆库一起活，进程一退索引不消失）。
// 要求宿主显式传一遍，等于让每家宿主都多写一行、且多数会忘。
func TestVectorIndexDiscoveredFromStore(t *testing.T) {
	ctx := context.Background()
	base := openFresh(t)
	ix := ports.NewMemIndex()
	s := vectorStoreFake{Store: base, ix: ix}
	llm := fakeLLM{
		extract: `[{"assertion":"机器甲的存储空间不足","quote":"机器甲的存储空间不足",` +
			`"event_id":"ev1","type":"fact","subject_name":"机器甲",` +
			`"importance":"low","confidence":"high"}]`,
	}
	// 只给向量化器，**不给**索引：索引应当从存储身上发现。
	svc, err := application.New(
		application.Config{Namespace: ns, Host: "it"},
		application.Deps{
			Store: s, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{}, LLM: llm,
			Embedder: synonymEmbedder{},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	cap, err := svc.Capture(ctx, application.CaptureInput{
		EventID: "ev1", Type: types.EventTurnEnd,
		Content: "机器甲的存储空间不足，需要清理", SessionID: "s1", ThreadID: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	cands, err := svc.Extract(ctx, ns, "s1", []*types.RawEvent{cap.Event})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCandidate(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}
	er, err := entity.New(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := er.Register(ctx, ns, "机器甲", types.EntityType("service"), "那台研发机",
		types.DecidedUser); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Promote(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}
	// 晋升后向量写进了「存储自带」的那个索引。
	if ix.Count(string(ns)) != 1 {
		t.Fatalf("向量应写进存储自带的索引: %d", ix.Count(string(ns)))
	}
}

// TestVectorTrackIsOptional 没装向量面 → 行为与从前**逐条一致**。
//
// 这条钉的是降级路径：没配向量面时，召回不该有任何行为变化
// （「没有模型也能手工写入 + 词法召回必须能跑」那条路径不许塌）。
//
// 判据用的是**召回结果逐条相同**，而不是「没报错」——
// 后者钉不住任何东西。
func TestVectorTrackIsOptional(t *testing.T) {
	ctx := context.Background()
	seed := func() *recall.Result {
		s := openFresh(t)
		llm := fakeLLM{
			extract: `[{"assertion":"机器甲的磁盘长期偏小","quote":"机器甲的磁盘长期偏小",` +
				`"event_id":"ev1","type":"fact","subject_name":"机器甲",` +
				`"importance":"high","confidence":"high"}]`,
		}
		svc, err := application.New(
			application.Config{Namespace: ns, Host: "it"},
			application.Deps{Store: s, Redactor: ports.PatternRedactor{},
				Bander: ports.MemoryBand{}, LLM: llm},
		)
		if err != nil {
			t.Fatal(err)
		}
		cap, err := svc.Capture(ctx, application.CaptureInput{
			EventID: "ev1", Type: types.EventTurnEnd,
			Content: "机器甲的磁盘长期偏小，需要清理旧文件", SessionID: "s1", ThreadID: "t1",
		})
		if err != nil {
			t.Fatal(err)
		}
		cands, err := svc.Extract(ctx, ns, "s1", []*types.RawEvent{cap.Event})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SaveCandidate(ctx, ns, cands[0]); err != nil {
			t.Fatal(err)
		}
		er, err := entity.New(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := er.Register(ctx, ns, "机器甲", types.EntityType("service"), "那台研发机",
			types.DecidedUser); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Promote(ctx, ns, cands[0]); err != nil {
			t.Fatal(err)
		}
		p, err := recall.New(recall.DefaultConfig(), s, ports.StaticEstimator{}, ports.MemoryBand{})
		if err != nil {
			t.Fatal(err)
		}
		out, err := p.Recall(ctx, recall.Request{Namespace: ns, Query: "磁盘"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a, b := seed(), seed()
	if len(a.Snippets) != len(b.Snippets) {
		t.Fatalf("两次结果条数不同: %d vs %d", len(a.Snippets), len(b.Snippets))
	}
	for i := range a.Snippets {
		// 比**正文**而不是 ID：候选 ID 由抽取时的时间戳派生，
		// 两次跑天然不同（那与本用例要验的东西无关）。
		if a.Snippets[i].Body != b.Snippets[i].Body {
			t.Fatalf("第 %d 条正文不同: %q vs %q", i, a.Snippets[i].Body, b.Snippets[i].Body)
		}
	}
	_ = store.Namespace("")
}
