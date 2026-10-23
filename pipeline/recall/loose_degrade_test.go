package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：宽松词面检索这条路的**真实覆盖** + 它不可用时的降级路径。
//
// # 为什么要重写已有的那条用例
//
// v0.7.0 加宽松路时配了一条用例（TestLoosePathAddsPartialMatches），
// 它断言「只共享部分词的事实能被捞进来」——**但它验的不是宽松路**。
//
// 原因：候选池是「查询命中 ∪ 高价值兜底」两路（gather.collectAtoms），
// 而共享的假存储 fakeReader 的 ListAtoms 把**全部**事实都返回
// ⇒ 那条「只命中部分词」的事实从**兜底路**就进来了。
//
// 实测（2026-10-23 变异验证）：把 collectLoose 改成直接返回 nil
// （宽松路整个废掉），**旧用例照样绿**。
//
// ⇒ 这是「用例的判别特征选错了」的典型：它绿是因为**另一个机制**，
// 而它守的东西（宽松路）根本没人守。本文件的那条用例换掉了判别特征：
// 兜底路只给严格命中的那些，于是「只命中部分词」那条**只能**从宽松路进来。
//
// # 降级这一组在守什么
//
// 宽松路是**增强**（多话题查询的召回率），不是**前提**：
// 宿主没实现那个能力时，召回链必须照跑（少召回一些）。
//
// 三种失效形态，每一种都长得不一样：
//
//   - **编译期打红**（最响）：把可选能力放回已发布的接口里
//     ⇒ 仓外的适配层当天编译不过（issue #12 的原形）；
//   - **装配期报错**（也很响，但方向错）：把增强当前提
//     ⇒ 宿主装不起就崩，而它本来可以不装；
//   - **静默降级**（最阴）：能力没实现，召回照跑，只是多话题查询打不中
//     ⇒ 症状只是「召回变少了」，看着像内容不够。
//
// 本文件钉住第三种形态的**可观测性**：降级必须能被看见
// （Result.LooseSearchUnavailable + 指标），否则它就是静默的。

// looseBaseReader：兜底路（ListAtoms）只返回**严格命中**的那些。
//
// 这是本文件的关键：**「只命中部分词」那条事实只能从宽松路进来**。
// 共享的 fakeReader 做不到这一点（它的 ListAtoms 返回全部事实），
// 用它写出来的用例会因为「兜底路」而绿，与宽松路无关。
type looseBaseReader struct {
	f     *fakeReader
	query string
}

func (s *looseBaseReader) ListAtoms(ctx context.Context, ns store.Namespace, flt store.AtomFilter) ([]*types.AtomCard, error) {
	// 兜底路只给严格命中的那些（模拟「它不够高价值、没进兜底」）。
	return s.f.SearchAtoms(ctx, ns, s.query, flt)
}

func (s *looseBaseReader) SearchAtoms(ctx context.Context, ns store.Namespace, q string, flt store.AtomFilter) ([]*types.AtomCard, error) {
	return s.f.SearchAtoms(ctx, ns, q, flt)
}

func (s *looseBaseReader) GetEntityPage(ctx context.Context, ns store.Namespace, id string, sc types.Scope) (*types.EntityPage, error) {
	return s.f.GetEntityPage(ctx, ns, id, sc)
}

func (s *looseBaseReader) UpsertActiveEntity(ctx context.Context, ns store.Namespace, a *types.ActiveEntity) error {
	return s.f.UpsertActiveEntity(ctx, ns, a)
}

func (s *looseBaseReader) ListActiveEntities(ctx context.Context, ns store.Namespace, th string, limit int) ([]*types.ActiveEntity, error) {
	return s.f.ListActiveEntities(ctx, ns, th, limit)
}

func (s *looseBaseReader) EvictActiveEntities(ctx context.Context, ns store.Namespace, th string, keep int) ([]types.ActiveEntity, error) {
	return s.f.EvictActiveEntities(ctx, ns, th, keep)
}

// ListRaw 捕获面（**必需**能力，与宽松路正相反）。
//
// 刻意实现它：这样两组用例的**唯一**差别就是「宽松路有没有」，
// 差异归因才干净（否则混进「raw 源配了却没实现」这条无关变量）。
//
// 顺带说明两者的分野：raw 源配了却没实现 ⇒ **装配期响亮失败**
// （那是必需能力）；宽松路没实现 ⇒ **运行期降级**（那是增强）。
func (s *looseBaseReader) ListRaw(ctx context.Context, ns store.Namespace, sc types.Scope,
	before time.Time, p store.Page) ([]*types.RawEvent, error) {
	return s.f.ListRaw(ctx, ns, sc, before, p)
}

// looseCapableReader：在 looseBaseReader 之上**实现**宽松路。
//
// 内嵌 looseBaseReader（它没有宽松路）⇒ 这个类型有宽松路、
// 而它的兜底路仍然只给严格命中的那些 ⇒ 宽松路的贡献可归因。
type looseCapableReader struct {
	*looseBaseReader
}

func (l *looseCapableReader) SearchAtomsLoose(ctx context.Context, ns store.Namespace, q string,
	flt store.AtomFilter) ([]*types.AtomCard, error) {
	return l.f.SearchAtomsLoose(ctx, ns, q, flt)
}

// looseFixtures 两条事实：一条三个词全中（严格路命中），一条只中一个词（**只有宽松路**能捞）。
func looseFixtures() []*types.AtomCard {
	at := time.Now().UTC()
	return []*types.AtomCard{
		{ID: "a-all", EntityID: "e1", Assertion: "磁盘 内存 清理 都要做",
			Importance: types.ImportanceHigh, OccurredAt: at, CreatedAt: at, UpdatedAt: at},
		{ID: "a-part", EntityID: "e2", Assertion: "磁盘 的容量不够了",
			Importance: types.ImportanceHigh, OccurredAt: at, CreatedAt: at, UpdatedAt: at},
	}
}

func hasSnippet(res *recall.Result, refID string) bool {
	for _, s := range res.Snippets {
		if s.RefID == refID {
			return true
		}
	}
	return false
}

// 宽松路：只共享**部分**词的事实要能进候选池。
//
// 严格路是隐式 AND ⇒ 「磁盘 内存 清理」里只有同时含三个词的事实才算命中，
// 而一条只含「磁盘」的事实对这条查询明明是相关的。
// 基线数字把这件事量出来了（docs/EVAL_BASELINE.md）：Recall@3 之后不再上升。
//
// ⚠️ 判别特征：兜底路只给严格命中的那些（looseBaseReader），
// 故这条事实**只能**从宽松路进来——把宽松路废掉，用例立刻红。
func TestLoosePathAddsPartialMatches(t *testing.T) {
	f := &fakeReader{atoms: looseFixtures()}
	reader := &looseCapableReader{looseBaseReader: &looseBaseReader{f: f, query: "磁盘 内存 清理"}}
	p := newPipe(t, reader, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘 内存 清理"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasSnippet(res, "a-part") {
		t.Fatalf("只共享部分词的事实该被宽松路捞进来: %+v", res.Snippets)
	}
	// 完全匹配的那条必须**排前面**（它在两路都出现，融合分最高）。
	if len(res.Snippets) == 0 || res.Snippets[0].RefID != "a-all" {
		t.Fatalf("完全匹配的那条该排在最前: %+v", res.Snippets)
	}
}

// 宿主**没实现**宽松检索时：召回照跑，但只有严格路那条进来；
// 且降级必须**说清**（Result 字段 + 指标），不许静默。
func TestLooseSearchUnavailableDegradesToStrictPath(t *testing.T) {
	f := &fakeReader{atoms: looseFixtures()}
	m := ports.NewMemMetrics()
	cfg := recall.DefaultConfig()
	cfg.Metrics = m
	reader := &looseBaseReader{f: f, query: "磁盘 内存 清理"}
	p := newPipe(t, reader, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘 内存 清理"})
	if err != nil {
		t.Fatalf("没实现宽松路不该让召回失败（它是增强不是前提）: %v", err)
	}
	// 严格路那条必须还在：降级是「少召回」，不是「不召回」。
	if !hasSnippet(res, "a-all") {
		t.Fatalf("降级后严格路那条该还在: %+v", res.Snippets)
	}
	// 只共享部分词的那条进不来（宽松路没了）。
	if hasSnippet(res, "a-part") {
		t.Fatalf("没有宽松路时不该捞到只命中部分词的事实: %+v", res.Snippets)
	}
	// 降级必须可观测：不然症状只是「召回变少了」。
	if !res.LooseSearchUnavailable {
		t.Fatal("降级必须说清（Result.LooseSearchUnavailable）")
	}
	snap := m.Snapshot()
	if got := snap.Counts[recall.MetricLooseSearchUnavailable]; got != 1 {
		t.Fatalf("降级该记一次指标，实得 %d", got)
	}
}

// 对照组：宿主**实现了**宽松路时，那条只命中部分词的事实要能进来，
// 且**不许**报降级。
//
// 这一条是上面那条的**判别特征**：没有它，上面那条可能是因为
// 「宽松路整体坏了」而绿——而那与「宿主没实现」是两回事。
func TestLooseSearchAvailableKeepsPartialMatches(t *testing.T) {
	f := &fakeReader{atoms: looseFixtures()}
	m := ports.NewMemMetrics()
	cfg := recall.DefaultConfig()
	cfg.Metrics = m
	reader := &looseCapableReader{looseBaseReader: &looseBaseReader{f: f, query: "磁盘 内存 清理"}}
	p := newPipe(t, reader, cfg)

	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "磁盘 内存 清理"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasSnippet(res, "a-part") {
		t.Fatalf("实现了宽松路就该捞到只命中部分词的事实: %+v", res.Snippets)
	}
	if res.LooseSearchUnavailable {
		t.Fatal("实现了宽松路不该报降级")
	}
	snap := m.Snapshot()
	if got := snap.Counts[recall.MetricLooseSearchUnavailable]; got != 0 {
		t.Fatalf("没降级就不该记降级指标，实得 %d", got)
	}
}

// 降级计数的口径：**每次召回**记一次，缓存命中那次也算
// （它端出来的就是降级结果）。
//
// 这条守的是「指标口径」：同一个问题在两种口径下数字不同，
// 看板上的曲线就没法解释（宿主会问「为什么刚才还有缓存命中却没涨」）。
func TestLooseDegradationCounterIsPerRecall(t *testing.T) {
	f := &fakeReader{atoms: looseFixtures()}
	m := ports.NewMemMetrics()
	cfg := recall.DefaultConfig()
	cfg.Metrics = m
	cfg.CacheTTL = time.Minute
	reader := &looseBaseReader{f: f, query: "磁盘 内存 清理"}
	p := newPipe(t, reader, cfg)

	req := recall.Request{Namespace: ns, Query: "磁盘 内存 清理"}
	for i := 0; i < 3; i++ {
		if _, err := p.Recall(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	snap := m.Snapshot()
	if got := snap.Counts[recall.MetricLooseSearchUnavailable]; got != 3 {
		t.Fatalf("三次召回该记三次降级，实得 %d", got)
	}
}
