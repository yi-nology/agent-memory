package recall_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/query"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：混合检索（词面 + 语义 + RRF 融合）的用例。
//
// # 三条要钉住的
//
//  1. **语义路命中而词面路没命中的事实，要能进候选池**——
//     这正是「混合」的意义：纯词面召回拿不到「意思像但字面不像」的那条。
//  2. **没装语义面时行为逐条不变**（零漂移）——
//     那条降级路径（没有模型也能召回）必须一直能跑。
//  3. **归属隔离在语义路同样生效**——
//     语义面是宿主写的，它漏了过滤条件的后果是别家的事实进了 prompt。

// fakeSemantic 假语义面：按固定名单返回命中。
type fakeSemantic struct {
	ids    []string
	err    error
	called int
	// sawScope 上一次收到的归属（用来断言参数传对了）。
	sawScope string
}

func (s *fakeSemantic) Search(_ context.Context, _, _, scope string, _ int) ([]ports.SemanticHit, error) {
	s.called++
	s.sawScope = scope
	if s.err != nil {
		return nil, s.err
	}
	out := make([]ports.SemanticHit, 0, len(s.ids))
	for i, id := range s.ids {
		out = append(out, ports.SemanticHit{ID: id, Score: float64(len(s.ids) - i)})
	}
	return out, nil
}

// 语义路命中、词面路没命中的事实要进候选池。
func TestSemanticHitEntersPool(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
		atom("a2", "e2", "内存占用偏高", types.ImportanceHigh),
	}}
	// 词面路只会命中「磁盘」（查询词），a2 不在任何词面路的结果里。
	sem := &fakeSemantic{ids: []string{"a2"}}
	cfg := recall.DefaultConfig()
	cfg.Semantic = sem
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"})
	if err != nil {
		t.Fatal(err)
	}
	if sem.called == 0 {
		t.Fatal("装了语义面却没被调用")
	}
	found := false
	for _, s := range res.Snippets {
		if s.RefID == "a2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("语义命中的事实没进候选池: %+v", res.Snippets)
	}
}

// 语义路的归属参数要传对（过滤在语义面那一层生效）。
func TestSemanticScopePassedThrough(t *testing.T) {
	f := &fakeReader{}
	sem := &fakeSemantic{}
	cfg := recall.DefaultConfig()
	cfg.Semantic = sem
	p := newPipe(t, f, cfg)

	if _, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", Scope: "team-a",
	}); err != nil {
		t.Fatal(err)
	}
	if sem.sawScope != "team-a" {
		t.Fatalf("归属没传给语义面: %q", sem.sawScope)
	}
}

// 语义面失败**上抛**：静默忽略的话，混合检索悄悄退化成纯词面，
// 而宿主以为自己在用语义轨。
func TestSemanticFailurePropagates(t *testing.T) {
	f := &fakeReader{}
	cfg := recall.DefaultConfig()
	cfg.Semantic = &fakeSemantic{err: errSemantic}
	p := newPipe(t, f, cfg)

	if _, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"}); err == nil {
		t.Fatal("语义面失败必须上抛：静默退化的症状是「召回质量莫名下降」")
	}
}

// 没装语义面时：一次都不该调语义面，且结果与装之前**逐条一致**。
func TestNoSemanticKeepsBehavior(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
		atom("a2", "e2", "内存占用偏高", types.ImportanceLow),
	}}
	cfg := recall.DefaultConfig()
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) == 0 {
		t.Fatal("没装语义面也该召回得到")
	}
	// 词面命中在前（缺省打分器给命中查询词的更高分）。
	if res.Snippets[0].RefID != "a1" {
		t.Fatalf("缺省打分器的次序不对: %+v", res.Snippets)
	}
}

// 融合分要传到打分器（排序口径是它的事）。
//
// 判据用一个「看得见融合分」的假打分器：
// 它给最高分的候选返 FusionScore —— 若内核没把融合分传过去，
// 它看到的就全是 0。
func TestFusionScoreReachesRanker(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	sem := &fakeSemantic{ids: []string{"a1"}}
	cfg := recall.DefaultConfig()
	cfg.Semantic = sem
	echo := &fusionEchoRanker{}
	cfg.Ranker = echo
	p := newPipe(t, f, cfg)

	if _, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"}); err != nil {
		t.Fatal(err)
	}
	if !echo.sawFusion {
		t.Fatal("融合分没传到打分器：排序口径就看不到这一路信号")
	}
}

// fusionEchoRanker 把自己看到的最大融合分记下来。
type fusionEchoRanker struct {
	sawFusion bool
}

func (r *fusionEchoRanker) Score(_ context.Context, _ string, cands []ports.RankCandidate,
	_ time.Time) ([]float64, error) {
	out := make([]float64, len(cands))
	for i, c := range cands {
		if c.FusionScore > 0 {
			r.sawFusion = true
		}
		out[i] = 1
	}
	return out, nil
}

// 查询理解接进召回：问句里的时间窗要落到**取数层**。
//
// 判据：造两条同内容但发生时刻不同的事实，
// 问「昨天那次」时只该召回昨天那条。
func TestAnalyzerTimeWindowFiltersAtFetch(t *testing.T) {
	yesterday := base().AddDate(0, 0, -1)
	longAgo := base().AddDate(0, 0, -5)
	f := &fakeReader{atoms: []*types.AtomCard{
		// 今天那条：窗口**之后** → 不该召回。
		//
		// 三条的断言**必须不同**：同一实体同一断言会被去重闸塌成一条，
		// 那样「窗口外那条根本没进结果」的原因是去重而不是时间窗——
		// 用例就钉错了东西（而它照样是绿的）。
		{ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小（今日）", Importance: types.ImportanceHigh,
			OccurredAt: base(), CreatedAt: base(), UpdatedAt: base()},
		// 昨天那条：窗口之内 → 该召回。
		{ID: "a2", EntityID: "e1", Assertion: "磁盘长期偏小（昨日）", Importance: types.ImportanceHigh,
			OccurredAt: yesterday, CreatedAt: yesterday, UpdatedAt: yesterday},
		// 五天前那条：窗口**之前** → 不该召回。
		//
		// 这一条是判别特征：只有上界时它会被召回（它早于上界），
		// 故没有它，用例就钉不住「下界生效」这件事。
		{ID: "a3", EntityID: "e1", Assertion: "磁盘长期偏小（五日前）", Importance: types.ImportanceHigh,
			OccurredAt: longAgo, CreatedAt: longAgo, UpdatedAt: longAgo},
	}}
	cfg := recall.DefaultConfig()
	cfg.Analyzer = query.New(query.Config{Now: base(), Loc: time.Local})
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "昨天那次磁盘"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range res.Snippets {
		if s.RefID == "a1" {
			t.Fatal("今天那条不该被「昨天」的时间窗召回")
		}
		if s.RefID == "a3" {
			t.Fatal("五天前那条不该被「昨天」的时间窗召回：窗口的下界没生效")
		}
		if s.RefID == "a2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("昨天那条该被召回: %+v", res.Snippets)
	}
}

// 指标面：召回后要有数（口径由内核定，宿主只搬运）。
func TestMetricsCollected(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	m := ports.NewMemMetrics()
	cfg := recall.DefaultConfig()
	cfg.Metrics = m
	// 缓存默认关着（CacheTTL=0 ⇒ 根本没有缓存对象），
	// 故要观察缓存指标必须把缓存打开。
	cfg.CacheTTL = time.Minute
	p := newPipe(t, f, cfg)

	if _, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘"}); err != nil {
		t.Fatal(err)
	}
	snap := m.Snapshot()
	if snap.Counts[recall.MetricCacheMiss] != 1 {
		t.Fatalf("缓存未命中应计数: %+v", snap.Counts)
	}
	if snap.Gauges[recall.MetricSnippets] < 1 {
		t.Fatalf("片段数应有值: %+v", snap.Gauges)
	}
	if snap.Durations[recall.MetricRecallDuration].Count != 1 {
		t.Fatalf("耗时应有一条观测: %+v", snap.Durations)
	}
}

// errSemantic 假语义面的错误。
var errSemantic = errors.New("语义面坏了")

// 解析结果要进**缓存键**：代词消解依赖活跃实体栈，而栈会随对话变。
//
// 不进键的话，同一句「那个呢」在栈变化后会命中上一次的缓存——
// 而那次缓存里是**另一个实体**的召回结果。
// 症状是「同一句话两次问，答案不一样」，看着像随机。
func TestCacheKeyIncludesResolvedPlan(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	f.stack = []*types.ActiveEntity{{ThreadID: "th1", EntityID: "e-old", LastSeen: base()}}
	cfg := recall.DefaultConfig()
	cfg.CacheTTL = time.Minute
	cfg.Analyzer = query.New(query.Config{Now: base(), Loc: time.Local})
	p := newPipe(t, f, cfg)

	req := recall.Request{Namespace: ns, Query: "那个呢", SessionID: "s1", ThreadID: "th1"}
	if _, err := p.Recall(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// 栈变了：最近提及的实体换了 → 代词消解的结果变了。
	f.stack = []*types.ActiveEntity{{ThreadID: "th1", EntityID: "e-new", LastSeen: base()}}
	second, err := p.Recall(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.FromCache {
		t.Fatal("解析结果变了（栈变了）就不该命中缓存：否则会端上一次的解析结果")
	}
}

// 宽松路：只共享**部分**词的事实要能进候选池。
//
// ⚠️ 这条用例已移到 loose_degrade_test.go，且**换了判别特征**。
//
// 原版用共享的 fakeReader——而它的 ListAtoms 把**全部**事实都返回
// （那是「高价值兜底」那一路），于是「只共享部分词」那条从兜底路就进来了：
// 把宽松路整个废掉（collectLoose 直接返回 nil），**原版照样绿**
// （2026-10-23 变异验证实测）。
//
// ⇒ 原版验的不是宽松路，是兜底路。宽松路那条特性因此**一直没有真覆盖**。
// 新版让兜底路只给严格命中的那些 ⇒ 那条事实**只能**从宽松路进来。
func TestEventWindowFiltersOldEvents(t *testing.T) {
	old := base().AddDate(0, 0, -60)
	mk := func(id, text string, kind types.FactKind, at time.Time) *types.AtomCard {
		return &types.AtomCard{
			ID: id, EntityID: "e1", Assertion: text, Kind: kind,
			Importance: types.ImportanceHigh, OccurredAt: at, CreatedAt: at, UpdatedAt: at,
		}
	}
	f := &fakeReader{atoms: []*types.AtomCard{
		mk("a-old-event", "磁盘 清理 记录", types.FactEvent, old),
		mk("a-old-lesson", "磁盘 清理 经验", types.FactLesson, old),
	}}
	cfg := recall.DefaultConfig()
	cfg.EventWindow = 30 * 24 * time.Hour
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘 清理"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Snippets {
		if s.RefID == "a-old-event" {
			t.Fatal("过了窗口的事件类事实不该进候选池")
		}
	}
	// 长期经验不按时间筛：它**不因时间失效**。
	found := false
	for _, s := range res.Snippets {
		if s.RefID == "a-old-lesson" {
			found = true
		}
	}
	if !found {
		t.Fatalf("长期经验不该被事件窗口挡掉: %+v", res.Snippets)
	}

	// 用户显式问了历史（带时间窗）⇒ 窗口让位，那条老事件应当能被召回。
	res2, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘 清理",
		OccurredFrom: old.AddDate(0, 0, -1), OccurredTo: old.AddDate(0, 0, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, s := range res2.Snippets {
		if s.RefID == "a-old-event" {
			found = true
		}
	}
	if !found {
		t.Fatalf("显式问历史时事件窗口应让位: %+v", res2.Snippets)
	}
}
