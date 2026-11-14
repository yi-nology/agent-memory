package parity

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：2026-11-14 issue 清零批（#48-#59）补的跨后端断言。

// ── 事实卡：retire 本体的退役态单向守卫（#48）──

// 专职退役 API（SupersedeAtom / DeprecateAtom）与 SaveAtom 受同一份单向约束：
// 已退役的卡不得被二次退役改写退役态。修法前两后端同病——retire 是无守卫的
// 读改写 UPDATE，DeprecateAtom 的 mutate 无条件清空 superseded_by，
// 对已被取代的卡调人工作废会把既有取代链静默抹掉；两个并发 retire 同卡时
// 后提交者覆盖先提交者（SaveAtom 注释点名的失效在这条路径原样存在）。
func tAtomRetireOneWay(t *testing.T, s store.Store) {
	// 已被取代的卡再调人工作废：取代链必须原样保留。
	a := atom("a-ow-sup", "e1", "退役单向：取代后再作废", "ev1", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeAtom(bg(), nsA, "a-ow-sup", "a-ow-newer"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeprecateAtom(bg(), nsA, "a-ow-sup", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAtom(bg(), nsA, "a-ow-sup")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != "a-ow-newer" {
		t.Fatalf("二次退役（作废）抹掉了既有取代链: 期望 a-ow-newer 实得 %q", got.SupersededBy)
	}
	if !got.DeprecatedAt.IsZero() {
		t.Fatalf("已被取代的卡被二次退役写上作废时刻，互斥态被改写: %+v", got.DeprecatedAt)
	}

	// 反方向同样：人工作废的卡再调取代，不得写下新的取代链。
	b := atom("a-ow-dep", "e1", "退役单向：作废后再取代", "ev2", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, b); err != nil {
		t.Fatal(err)
	}
	if err := s.DeprecateAtom(bg(), nsA, "a-ow-dep", base); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeAtom(bg(), nsA, "a-ow-dep", "a-ow-late"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAtom(bg(), nsA, "a-ow-dep")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != "" {
		t.Fatalf("二次退役（取代）在人工作废的卡上写下了取代链: %q", got.SupersededBy)
	}
	if !got.DeprecatedAt.Equal(base) {
		t.Fatalf("二次退役挪动了既有的作废时刻: 期望 %v 实得 %v", base, got.DeprecatedAt)
	}

	// 并发取代同一张活跃卡：先提交者的取代链不得被后提交者抹掉。
	// 谁赢取决于后端的事务串行化方式（SQLite 单连接排队、PG 靠行上的
	// CASE 仲裁），所以只断言「赢家稳定」：终值是两方之一，且第三次
	// retire（读到的已是退役态）不得改写它。
	c := atom("a-ow-race", "e1", "退役单向：并发取代", "ev3", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, c); err != nil {
		t.Fatal(err)
	}
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		retErr error
	)
	for _, winner := range []string{"a-ow-w1", "a-ow-w2"} {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			if err := s.SupersedeAtom(bg(), nsA, "a-ow-race", w); err != nil {
				mu.Lock()
				retErr = err
				mu.Unlock()
			}
		}(winner)
	}
	wg.Wait()
	if retErr != nil {
		t.Fatalf("并发取代失败: %v", retErr)
	}
	got, err = s.GetAtom(bg(), nsA, "a-ow-race")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != "a-ow-w1" && got.SupersededBy != "a-ow-w2" {
		t.Fatalf("并发取代后取代链既不是 w1 也不是 w2: %q", got.SupersededBy)
	}
	winner := got.SupersededBy
	if err := s.SupersedeAtom(bg(), nsA, "a-ow-race", "a-ow-w3"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAtom(bg(), nsA, "a-ow-race")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != winner {
		t.Fatalf("第三次取代抹掉了先到者的取代链: 期望 %s 实得 %s", winner, got.SupersededBy)
	}
}

// ── 实体：SaveEntity 的整实体语义（#51）──

// Merge 的失败恢复故事押在「最后一步 SaveEntity 把 from 计数清 0」上：
// SaveEntity 的 ON CONFLICT 只更新 entity_type/canonical_name 的话，
// 清零是 no-op，中途失败的 Bump 差量留下的幽灵计数永久留在库里
// （ListEntities 还按 atom_count DESC 把它排最前）。
// 本用例在**真后端**上走「迁移半途失败（一次 Bump 差量丢失）→ 重跑同一条
// Merge」的完整序列，断言终态 from 计数为 0——钉的是 SQL 语义，
// 不是 fake 的语义（整实体覆盖的 fake 恰好会替坏实现背书）。
func tEntitySaveEntityWholeRow(t *testing.T, s store.Store) {
	const total = 5
	_ = s.SaveEntity(bg(), nsA, &types.Entity{
		ID: "e-to", EntityType: "机器", CanonicalName: "目标机器", CreatedAt: base,
	})
	_ = s.SaveEntity(bg(), nsA, &types.Entity{
		ID: "e-from", EntityType: "机器", CanonicalName: "来源机器",
		CreatedAt: base, AtomCount: total, LastPromotedAt: base,
	})
	// 5 张卡挂在 from 名下（计数与卡数一致）。
	for i := range total {
		a := atom(fmt.Sprintf("atom-mv-%d", i), "e-from",
			fmt.Sprintf("迁移用例事实 %d 号", i), fmt.Sprintf("ev-mv-%d", i), types.ImportanceMed)
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}
	// 迁移一步 = 卡改归属 + 双向 Bump（entity.Merge 的机械迁移）。
	move := func(id string) {
		t.Helper()
		a, err := s.GetAtom(bg(), nsA, id)
		if err != nil {
			t.Fatal(err)
		}
		a.EntityID = "e-to"
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
		if err := s.BumpEntityAtomCount(bg(), nsA, "e-to", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.BumpEntityAtomCount(bg(), nsA, "e-from", -1); err != nil {
			t.Fatal(err)
		}
	}
	moveAtomOnly := func(id string) {
		// 只搬卡、不记账：Merge 在「SaveAtom 与 Bump 之间」失败的样子——
		// 这正是它声称被终态抹平的「中途失败的 Bump 差量」。
		t.Helper()
		a, err := s.GetAtom(bg(), nsA, id)
		if err != nil {
			t.Fatal(err)
		}
		a.EntityID = "e-to"
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}

	// 第一轮：完整迁走 atom-0，迁 atom-1 时**卡搬了、计数没记**就失败，
	// 终态的 SaveEntity(from, 0) 也没跑。
	move("atom-mv-0")
	moveAtomOnly("atom-mv-1")

	// 重跑同一条 Merge：ListAtomsByEntity(from) 只剩没搬走的 3 张，
	// 逐张迁移，最后 from 退役（计数清 0、晋升时刻清空）。
	move("atom-mv-2")
	move("atom-mv-3")
	move("atom-mv-4")
	from, err := s.GetEntity(bg(), nsA, "e-from")
	if err != nil {
		t.Fatal(err)
	}
	from.AtomCount = 0
	from.LastPromotedAt = time.Time{}
	if err := s.SaveEntity(bg(), nsA, from); err != nil {
		t.Fatal(err)
	}

	gotFrom, err := s.GetEntity(bg(), nsA, "e-from")
	if err != nil {
		t.Fatal(err)
	}
	if gotFrom.AtomCount != 0 {
		t.Fatalf("重跑收敛后 from 计数应为 0（脏差量被终态抹平），实得 %d", gotFrom.AtomCount)
	}
	if !gotFrom.LastPromotedAt.IsZero() {
		t.Fatalf("重跑收敛后 from 的 LastPromotedAt 应已清空: %v", gotFrom.LastPromotedAt)
	}
	// to 计数带一次永久丢失的 Bump 差量（4 而非 5）：契约只承诺
	// 「from 的计数误差被终态抹平」，to 侧那次丢失的差量不在承诺内。
	// 没有这条断言的话，move 换成纯 Bump 也能凑出 from=0（假绿）。
	gotTo, err := s.GetEntity(bg(), nsA, "e-to")
	if err != nil {
		t.Fatal(err)
	}
	if gotTo.AtomCount != total-1 {
		t.Fatalf("to 计数应为 %d（含一次丢失的 Bump 差量），实得 %d", total-1, gotTo.AtomCount)
	}
}

// ── 归属：空读者（全局读者）在取数层只见全局（#53）──

// 同一个「空」在两条判据上必须同口径：
//   - 作为**过滤条件**（AtomFilter.Scope 空）= 不隔离——导出/扫描取全量；
//   - 作为**读者**（GlobalReader 置位）= 只见全局——types.Scope.VisibleTo
//     在空读者上的自然结果。
//
// 修复前 SQL 层没有「空读者」的表达（两个 scope 字段都把空当「不用这条」），
// 召回层只好先取全量再后置滤除——TopK 配额先被别家的高分条目占满，
// 全局读者自己的全局事实根本没进候选池。
//
// 配额断言用 ListAtoms（重要度序是**钉死**的 parity 断言，可确定性构造
// 「别家排前面」）；检索路只断言「一条别组的都没有」（名次跨后端不钉，
// 见 AGENTS.md「排序不是存储层的承诺」）。
func tAtomGlobalReaderSeesGlobalOnly(t *testing.T, s store.Store) {
	// 别家的卡重要度更高：不隔离的列表路里它们排前面。
	other1 := atomInScope("gr-b1", "e1", "scope-b", "别组的高价值事实甲", "ev1", types.ImportanceHigh)
	other2 := atomInScope("gr-b2", "e1", "scope-b", "别组的高价值事实乙", "ev2", types.ImportanceHigh)
	g1 := atomInScope("gr-g1", "e1", "", "全局事实甲：内核版本", "ev3", types.ImportanceMed)
	g2 := atomInScope("gr-g2", "e1", "", "全局事实乙：磁盘清理", "ev4", types.ImportanceMed)
	for _, a := range []*types.AtomCard{other1, other2, g1, g2} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}

	// 列表路 + LIMIT 2：别家的两条按重要度排在前面——
	// 后置滤除的实现会返回「2 条别组卡被滤掉后的空集/残集」；
	// 取数层过滤的实现必得 2 条全局卡。
	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{GlobalReader: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("全局读者 LIMIT 2 应得 2 条全局卡（配额不被别家占掉），实得 %d: %v",
			len(got), atomIDs(got))
	}
	ids := atomIDs(got)
	if !hasAtom(ids, "gr-g1") || !hasAtom(ids, "gr-g2") {
		t.Fatalf("全局读者应得全局卡，实得 %v", ids)
	}

	// 检索路：命中集里一条别组的都不能有（纵深防御口径不变）。
	hits, err := s.SearchAtoms(bg(), nsA, "事实", store.AtomFilter{GlobalReader: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Scope != "" {
			t.Fatalf("全局读者的检索结果混进了别家的卡: %+v", h)
		}
	}
	// GlobalReader 与 Scope 同时置位时以 GlobalReader 为准（不叠加、不报错）。
	got, err = s.ListAtoms(bg(), nsA, store.AtomFilter{GlobalReader: true, Scope: "scope-b", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range got {
		if a.Scope != "" {
			t.Fatalf("GlobalReader 置位时 Scope 应让位（仍只见全局），实得 %q", a.Scope)
		}
	}
	// 反向对照：同样的卡，不隔离的列表路 4 条都在（该语义保持不变）。
	all, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("不隔离取全量应得 4 条，实得 %d", len(all))
	}
}

// 原始事件的全局读者取数（store.GlobalRawReader）：只取全局事件，
// 且配额不被别家**更新的事件**占掉。
func tRawGlobalReaderSeesGlobalOnly(t *testing.T, s store.Store) {
	g, ok := store.GlobalRawReaderOf(s)
	if !ok {
		t.Skip("后端未实现 GlobalRawReader（降级路径在 recall 层）")
	}
	mk := func(id string, scope types.Scope, content string, at time.Time) *types.RawEvent {
		return &types.RawEvent{
			ID: id, Host: "h", SessionID: "s1", Scope: scope, Timestamp: at,
			Type: types.EventExternal, Content: content,
		}
	}
	baseT := base
	// 别家的事件**更新**：按时间倒序它们排前面。
	ev := []*types.RawEvent{
		mk("gr-raw-o1", "scope-b", "别组事件：更新", baseT.Add(3*time.Hour)),
		mk("gr-raw-o2", "scope-b", "别组事件：次新", baseT.Add(2*time.Hour)),
		mk("gr-raw-g1", "", "全局事件：较旧", baseT.Add(time.Hour)),
		mk("gr-raw-g2", "", "全局事件：最旧", baseT),
	}
	for _, e := range ev {
		if err := s.SaveRaw(bg(), nsA, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := g.ListRawGlobal(bg(), nsA, time.Time{}, store.Page{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("全局读者 LIMIT 2 应得 2 条全局事件（配额不被别家占掉），实得 %d", len(got))
	}
	for _, e := range got {
		if e.Scope != "" {
			t.Fatalf("全局读者取到了别家的事件: %+v", e)
		}
	}
	if got[0].ID != "gr-raw-g1" || got[1].ID != "gr-raw-g2" {
		t.Fatalf("全局事件应按时间倒序: %v", []string{got[0].ID, got[1].ID})
	}
}

// ── 关系边：分页取全（#54）──

// 单实体出边可以远超 ListEdges 的缺省上限（100）：多跳遍历按页取尽，
// 不能拿缺省冒充全量。钉两层：缺省截断的存在（100 条）与
// Offset 翻页取尽（150 条一条不少、不重）。
func tEdgePaginationToExhaustion(t *testing.T, s store.Store) {
	es := edgeStoreOf(t, s)
	const total = 150
	for i := range total {
		e := &types.EntityEdge{
			ID:           fmt.Sprintf("edge-pg-%03d", i),
			FromEntityID: "hub", ToEntityID: fmt.Sprintf("to-%03d", i),
			Relation: "依赖", CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := es.UpsertEdge(bg(), nsA, e); err != nil {
			t.Fatalf("写边失败(%s): %v", e.ID, err)
		}
	}
	// 缺省（Limit 0）= 前 100 条：截断是**已知语义**，这里顺带钉住它，
	// 让「缺省是 100」这件事显形而不是靠猜。
	def, err := es.ListEdges(bg(), nsA, store.EdgeFilter{FromEntityIDs: []string{"hub"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 100 {
		t.Fatalf("缺省上限应取前 100 条，实得 %d", len(def))
	}
	// 分页取全：每页 60 条翻页，拼起来恰是 150 条、无重复。
	var (
		all  []*types.EntityEdge
		seen = map[string]bool{}
	)
	for offset := 0; ; offset += 60 {
		batch, err := es.ListEdges(bg(), nsA, store.EdgeFilter{
			FromEntityIDs: []string{"hub"}, Limit: 60, Offset: offset,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range batch {
			if seen[e.ID] {
				t.Fatalf("翻页出现重复: %s", e.ID)
			}
			seen[e.ID] = true
		}
		all = append(all, batch...)
		if len(batch) < 60 {
			break
		}
	}
	if len(all) != total {
		t.Fatalf("分页取全应得 %d 条，实得 %d", total, len(all))
	}
}

// ── 词面检索：TopK 的**集合**口径跨后端一致（#58）──

// 词面检索的相关度函数是**方言**（SQLite FTS5 rank/bm25，PG ts_rank），
// 名次不进 parity 断言（AGENTS.md「排序不是存储层的承诺」：
// 「哪条该进 prompt」是召回层重排的职责）。但**命中集合**是产品契约：
// 同一夹具、同一查询，两个后端必须吐出同一批标识（limit ≥ 命中数时
// 集合逐字相等）。截断面（limit < 命中数）因名次方言只能断言
// 「结果 ⊆ 命中集」——这一放宽是**已声明的方言差异**，不是疏忽
// （声明见 docs/AI_GUIDE.md 与两后端 searchFTS 的注释）。
func tAtomSearchTopKSet(t *testing.T, s store.Store) {
	hits := []string{"tk-1", "tk-2", "tk-3", "tk-4"}
	for i, id := range hits {
		a := atom(id, "e1", fmt.Sprintf("磁盘巡检结论第 %d 号：状态正常", i+1), "ev-"+id, types.ImportanceMed)
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}
	// 不命中的一条：集合断言要能看见「多出来的」。
	if err := s.SaveAtom(bg(), nsA, atom("tk-x", "e1", "内存占用偏高", "ev-x", types.ImportanceMed)); err != nil {
		t.Fatal(err)
	}

	// limit ≥ 命中数：集合逐字相等（跨后端同一份断言 ⇒ 集合同一口径）。
	got, err := s.SearchAtoms(bg(), nsA, "磁盘", store.AtomFilter{Limit: len(hits) + 1})
	if err != nil {
		t.Fatal(err)
	}
	ids := atomIDs(got)
	if len(ids) != len(hits) {
		t.Fatalf("词面检索应命中 %d 条，实得 %v", len(hits), ids)
	}
	for _, id := range hits {
		if !hasAtom(ids, id) {
			t.Fatalf("词面检索漏命中 %s（实得 %v）", id, ids)
		}
	}
	if hasAtom(ids, "tk-x") {
		t.Fatalf("不相关的卡混进了命中集: %v", ids)
	}

	// 截断面（limit < 命中数）：只断言「结果 ⊆ 命中集」。
	// 名次跨后端是方言（bm25 vs ts_rank），谁被截掉不进契约。
	top2, err := s.SearchAtoms(bg(), nsA, "磁盘", store.AtomFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(top2) != 2 {
		t.Fatalf("TopK=2 应得 2 条，实得 %v", atomIDs(top2))
	}
	for _, a := range top2 {
		if !hasAtom(hits, a.ID) {
			t.Fatalf("截断面出现了命中集之外的卡: %v", atomIDs(top2))
		}
	}
}
