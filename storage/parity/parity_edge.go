package parity

import (
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：生效区间（事实卡的时序）、标识集合取数、关系边（派生物）的跨后端断言。
//
// # 为什么这三块要落在共享断言里
//
// ① 时序：SQL 判据与 types.AtomCard.EffectiveAt 的判据必须**逐条一致**。
//    两处各写一遍的话，改了一处就会漂；而漂了的失效形态是
//    「SQL 取到的集合」与「Go 侧算出来的集合」不同——
//    症状是「按时刻查出来的结果里多了一条或少了一条」，
//    而那在结果里完全看不出来。
//
// ② 标识集合：漏了它，多路召回融合出来的结果会被当成「全部事实」——
//    症状是「召回里混进一堆不相关的东西」，同样看不出来。
//
// ③ 边：边的失效形态是**泄漏**（别家的边进了候选池）而不是报错，
//    两个后端各写各的 SQL 条件时，这种偏差**在自己的单测里全绿**。
//
// # 为什么单独成文件
//
// parity.go 已经接近本仓的文件行数上限（门禁 threshold-file-lines），
// 而这几组断言要连同「为什么」一起写——挤进 parity.go 会把上限顶破。

// ── 事实卡的时序 ──

// tAtomTemporalFilter 按时刻过滤事实卡（生效区间）。
//
// 三张卡：
//
//	长期有效（valid_to 未设）——任何时刻都成立；
//	到点失效（valid_to = base+2h）——失效时刻起不成立；
//	后来才生效（valid_from = base+1h）——生效时刻起成立。
//
// 三张卡的断言都带同一个词，于是检索路也能验同一套判据。
func tAtomTemporalFilter(t *testing.T, s store.Store) {
	long := atom("tp-long", "e1", "磁盘长期有效", "ev1", types.ImportanceHigh)
	expired := atom("tp-exp", "e1", "磁盘到点失效", "ev1", types.ImportanceHigh)
	expired.ValidFrom = base
	expired.ValidTo = base.Add(2 * time.Hour)
	later := atom("tp-later", "e1", "磁盘后来才生效", "ev1", types.ImportanceHigh)
	later.ValidFrom = base.Add(time.Hour)

	for _, a := range []*types.AtomCard{long, expired, later} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}

	for _, tc := range []struct {
		name string
		at   time.Time
		want []string
	}{
		// 生效时刻那一刻**成立**（左闭）。
		{"生效时刻成立", base.Add(time.Hour), []string{"tp-long", "tp-exp", "tp-later"}},
		// 失效时刻那一刻**不成立**（右开）。
		{"失效时刻不成立", base.Add(2 * time.Hour), []string{"tp-long", "tp-later"}},
		// 生效之前不成立。
		{"生效之前不成立", base.Add(-time.Hour), nil},
	} {
		got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{At: tc.at, Limit: 100})
		if err != nil {
			t.Fatalf("%s: 列表失败: %v", tc.name, err)
		}
		if ids := atomIDs(got); !sameIDs(ids, tc.want) {
			t.Fatalf("%s: 应得 %v，实得 %v", tc.name, tc.want, ids)
		}
	}

	// 检索路要守同一套判据：SearchAtoms 漏了时刻条件的话，
	// 已失效的事实照样会被搜出来——而症状是「召回里混进过期的事实」，
	// 那在结果里完全看不出来。
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘",
		store.AtomFilter{At: base.Add(2 * time.Hour), Limit: 100})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if ids := atomIDs(hits); !sameIDs(ids, []string{"tp-long", "tp-later"}) {
		t.Fatalf("检索路应同样按时刻过滤，应得 %v，实得 %v",
			[]string{"tp-long", "tp-later"}, ids)
	}
}

// tAtomLegacyEffectiveByOccurredAt 老数据（没有生效区间）按发生时刻算。
//
// valid_from 未设（零值）= 按发生时刻算生效区间。
// 这是**老数据**的语义：升级前后判同一张卡必须一致，
// 否则同一个库升级前后召回到的东西不一样。
func tAtomLegacyEffectiveByOccurredAt(t *testing.T, s store.Store) {
	early := atom("lg-early", "e1", "磁盘早些时候发生", "ev1", types.ImportanceHigh)
	early.OccurredAt = base
	late := atom("lg-late", "e1", "磁盘晚些时候发生", "ev1", types.ImportanceHigh)
	late.OccurredAt = base.Add(2 * time.Hour)

	for _, a := range []*types.AtomCard{early, late} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}

	// 发生在该时刻之前的那张成立，之后的那张不成立。
	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{At: base, Limit: 100})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if ids := atomIDs(got); !sameIDs(ids, []string{"lg-early"}) {
		t.Fatalf("按发生时刻判，应得 %v，实得 %v", []string{"lg-early"}, ids)
	}

	// 到了两张都发生之后，两张都成立（没有失效时刻 = 一直有效）。
	got, err = s.ListAtoms(bg(), nsA, store.AtomFilter{At: base.Add(3 * time.Hour), Limit: 100})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if ids := atomIDs(got); !sameIDs(ids, []string{"lg-early", "lg-late"}) {
		t.Fatalf("两张都该成立，应得 %v，实得 %v", []string{"lg-early", "lg-late"}, ids)
	}
}

// tAtomByIDs 按标识集合取事实卡。
//
// 多路召回（词面 + 语义）融合之后，要把「融合出的那一批标识」**一次**取回。
// 漏了它的话，融合的结果会被当成「全部事实」——
// 症状是「召回里混进一堆不相关的东西」。
func tAtomByIDs(t *testing.T, s store.Store) {
	for _, a := range []*types.AtomCard{
		atom("id-1", "e1", "磁盘第一张", "ev1", types.ImportanceHigh),
		atom("id-2", "e1", "磁盘第二张", "ev1", types.ImportanceHigh),
		atom("id-3", "e1", "磁盘第三张", "ev1", types.ImportanceHigh),
	} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}

	for _, tc := range []struct {
		name string
		ids  []string
		want []string
	}{
		{"前两张", []string{"id-1", "id-2"}, []string{"id-1", "id-2"}},
		{"后两张", []string{"id-2", "id-3"}, []string{"id-2", "id-3"}},
		{"一张", []string{"id-3"}, []string{"id-3"}},
		{"集合里有一个不存在的", []string{"id-1", "id-不存在"}, []string{"id-1"}},
	} {
		got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{IDs: tc.ids, Limit: 100})
		if err != nil {
			t.Fatalf("%s: 列表失败: %v", tc.name, err)
		}
		if ids := atomIDs(got); !sameIDs(ids, tc.want) {
			t.Fatalf("%s: 应得 %v，实得 %v（取到不属于集合的行就是错）", tc.name, tc.want, ids)
		}
	}

	// 检索路也要守：查询词命中三张，但标识集合只放行集合内的那几张。
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘",
		store.AtomFilter{IDs: []string{"id-1", "id-3"}, Limit: 100})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if ids := atomIDs(hits); !sameIDs(ids, []string{"id-1", "id-3"}) {
		t.Fatalf("检索路应按标识集合过滤，应得 %v，实得 %v", []string{"id-1", "id-3"}, ids)
	}

	// 空的标识集合 = 不限（既有行为不变）。
	all, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("不限时应得 3 条，实得 %d", len(all))
	}
}

// ── 关系边 ──

// tEdgeScopeIsolation 边的归属隔离：读者只看得到「全局 + 同归属」。
//
// 与事实卡同一判据：全局的边人人都该看到，别组的一条都不给。
func tEdgeScopeIsolation(t *testing.T, s store.Store) {
	es := edgeStoreOf(t, s)
	for _, e := range []*types.EntityEdge{
		{ID: "eg-g", FromEntityID: "e1", ToEntityID: "e2", Relation: "关系甲", CreatedAt: base},
		{ID: "eg-a", FromEntityID: "e1", ToEntityID: "e2", Relation: "关系甲",
			Scope: "scope-a", CreatedAt: base},
		{ID: "eg-b", FromEntityID: "e1", ToEntityID: "e2", Relation: "关系甲",
			Scope: "scope-b", CreatedAt: base},
	} {
		if err := es.UpsertEdge(bg(), nsA, e); err != nil {
			t.Fatalf("写边失败(%s): %v", e.ID, err)
		}
	}

	for _, tc := range []struct {
		scope string
		want  []string
	}{
		{"scope-a", []string{"eg-g", "eg-a"}},
		{"scope-b", []string{"eg-g", "eg-b"}},
	} {
		got, err := es.ListEdges(bg(), nsA, store.EdgeFilter{Scope: types.Scope(tc.scope), Limit: 100})
		if err != nil {
			t.Fatalf("列边失败(scope=%q): %v", tc.scope, err)
		}
		if ids := edgeIDs(got); !sameIDs(ids, tc.want) {
			t.Fatalf("读者 %q 应看到 %v，实得 %v", tc.scope, tc.want, ids)
		}
	}

	// 归属为空 = **不隔离**：三条都在（既有行为不变）。
	all, err := es.ListEdges(bg(), nsA, store.EdgeFilter{Limit: 100})
	if err != nil {
		t.Fatalf("列边失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("不隔离时应得 3 条，实得 %d", len(all))
	}
}

// tEdgeAtFilter 边按时刻过滤：有效期内取得到，失效后取不到。
//
// 边界与事实卡同一口径：**左闭右开**——
// 失效时刻那一刻已不成立（那正是「到这一刻为止有效」）。
func tEdgeAtFilter(t *testing.T, s store.Store) {
	es := edgeStoreOf(t, s)
	for _, e := range []*types.EntityEdge{
		{ID: "ea-win", FromEntityID: "e1", ToEntityID: "e2", Relation: "关系甲",
			ValidFrom: base, ValidTo: base.Add(time.Hour), CreatedAt: base},
		{ID: "ea-all", FromEntityID: "e1", ToEntityID: "e3", Relation: "关系甲",
			ValidFrom: base, CreatedAt: base},
	} {
		if err := es.UpsertEdge(bg(), nsA, e); err != nil {
			t.Fatalf("写边失败(%s): %v", e.ID, err)
		}
	}

	for _, tc := range []struct {
		name string
		at   time.Time
		want []string
	}{
		{"有效期内", base.Add(30 * time.Minute), []string{"ea-win", "ea-all"}},
		{"失效时刻那一刻", base.Add(time.Hour), []string{"ea-all"}},
		{"生效之前", base.Add(-time.Hour), nil},
	} {
		got, err := es.ListEdges(bg(), nsA, store.EdgeFilter{At: tc.at, Limit: 100})
		if err != nil {
			t.Fatalf("%s: 列边失败: %v", tc.name, err)
		}
		if ids := edgeIDs(got); !sameIDs(ids, tc.want) {
			t.Fatalf("%s: 应得 %v，实得 %v", tc.name, tc.want, ids)
		}
	}
}

// tEdgeDeleteBySource 按来源清理：事实卡被推翻时清掉它的投影。
//
// 留着旧边等于「关系还在」，而事实已经不成立了——那是一张说谎的图。
func tEdgeDeleteBySource(t *testing.T, s store.Store) {
	es := edgeStoreOf(t, s)
	for _, e := range []*types.EntityEdge{
		{ID: "ds-1", FromEntityID: "e1", ToEntityID: "e2", Relation: "关系甲",
			SourceAtomID: "a1", CreatedAt: base},
		{ID: "ds-2", FromEntityID: "e2", ToEntityID: "e3", Relation: "关系甲",
			SourceAtomID: "a1", CreatedAt: base},
		{ID: "ds-3", FromEntityID: "e3", ToEntityID: "e4", Relation: "关系甲",
			SourceAtomID: "a2", CreatedAt: base},
	} {
		if err := es.UpsertEdge(bg(), nsA, e); err != nil {
			t.Fatalf("写边失败(%s): %v", e.ID, err)
		}
	}

	if err := es.DeleteEdgesBySource(bg(), nsA, "a1"); err != nil {
		t.Fatalf("按来源清理失败: %v", err)
	}
	got, err := es.ListEdges(bg(), nsA, store.EdgeFilter{Limit: 100})
	if err != nil {
		t.Fatalf("列边失败: %v", err)
	}
	if ids := edgeIDs(got); !sameIDs(ids, []string{"ds-3"}) {
		t.Fatalf("只该剩下另一来源的那条，实得 %v", ids)
	}
	if _, err := es.GetEdge(bg(), nsA, "ds-3"); err != nil {
		t.Fatalf("另一来源的边不该被删: %v", err)
	}
}

// tEdgeEndpointsFilter 多跳取边：起点、终点、关系名三个切片都要对。
//
// 多跳遍历靠这一面拿「一跳的邻居」，取错了就会走出不存在的路径。
func tEdgeEndpointsFilter(t *testing.T, s store.Store) {
	es := edgeStoreOf(t, s)
	for _, e := range []*types.EntityEdge{
		{ID: "ep-1", FromEntityID: "e1", ToEntityID: "e2", Relation: "关系甲", CreatedAt: base},
		{ID: "ep-2", FromEntityID: "e1", ToEntityID: "e3", Relation: "关系甲", CreatedAt: base},
		{ID: "ep-3", FromEntityID: "e4", ToEntityID: "e2", Relation: "关系乙", CreatedAt: base},
	} {
		if err := es.UpsertEdge(bg(), nsA, e); err != nil {
			t.Fatalf("写边失败(%s): %v", e.ID, err)
		}
	}

	for _, tc := range []struct {
		name string
		f    store.EdgeFilter
		want []string
	}{
		{"按起点", store.EdgeFilter{FromEntityIDs: []string{"e1"}}, []string{"ep-1", "ep-2"}},
		{"按终点", store.EdgeFilter{ToEntityIDs: []string{"e2"}}, []string{"ep-1", "ep-3"}},
		{"两端都限定", store.EdgeFilter{FromEntityIDs: []string{"e1"}, ToEntityIDs: []string{"e3"}},
			[]string{"ep-2"}},
		{"按关系名", store.EdgeFilter{Relations: []string{"关系乙"}}, []string{"ep-3"}},
	} {
		tc.f.Limit = 100
		got, err := es.ListEdges(bg(), nsA, tc.f)
		if err != nil {
			t.Fatalf("%s: 列边失败: %v", tc.name, err)
		}
		if ids := edgeIDs(got); !sameIDs(ids, tc.want) {
			t.Fatalf("%s: 应得 %v，实得 %v", tc.name, tc.want, ids)
		}
	}
}

// ── 小工具 ──

// edgeStoreOf 取后端的关系边存取面。
//
// 用类型断言而不是改 store.Store：store.Store 是上游在动的契约，
// 而共享断言只要求「这个后端实现了边这一面」——
// 后端没实现就**响亮失败**（跳过等于没验证）。
func edgeStoreOf(t *testing.T, s store.Store) store.EdgeStore {
	t.Helper()
	es, ok := s.(store.EdgeStore)
	if !ok {
		t.Fatalf("后端未实现 store.EdgeStore：共享断言要求两个后端都提供关系边这一面")
	}
	return es
}

// edgeIDs 取边的标识列表。
func edgeIDs(es []*types.EntityEdge) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

// sameIDs 两个标识集合是否相同（**不看顺序**）。
//
// 不看顺序：列表路的顺序由排序表达式决定（重要度、发生时刻、标识），
// 而顺序不是本仓的契约——把顺序断言下来就把排序实现写成了产品契约。
func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	count := make(map[string]int, len(got))
	for _, id := range got {
		count[id]++
	}
	for _, id := range want {
		count[id]--
		if count[id] < 0 {
			return false
		}
	}
	return true
}
