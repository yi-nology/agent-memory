package parity

import (
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：归属维度（记录级隔离）的跨后端断言。
//
// # 为什么归属要单独一组断言
//
// 归属的失效形态是**泄漏**而不是报错：别组的环境事实进了 prompt，
// 而内容看着完全正常（就是一条说得通的事实）。
// 两个后端各写各的 SQL 条件时，这种偏差**在自己的单测里全绿**——
// 故它必须落在共享断言里，两个后端跑同一份。

// atomInScope 造一条带归属的事实卡。
func atomInScope(id, entity string, scope types.Scope, assertion, evidence string,
	imp types.Importance) *types.AtomCard {
	a := atom(id, entity, assertion, evidence, imp)
	a.Scope = scope
	return a
}

// 归属隔离：读者只看得到「全局 + 同归属」。
//
// 三张卡：一张全局的（人人都该看到）、一张本组的、一张别组的。
func tAtomScopeIsolation(t *testing.T, s store.Store) {
	global := atomInScope("sc-g", "e1", "", "内核版本为 5.15", "ev1", types.ImportanceHigh)
	mine := atomInScope("sc-a", "e1", "scope-a", "磁盘长期偏小", "ev1", types.ImportanceHigh)
	other := atomInScope("sc-b", "e1", "scope-b", "内存占用偏高", "ev1", types.ImportanceHigh)
	for _, a := range []*types.AtomCard{global, mine, other} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}

	// 读者 scope-a：全局 + 本组，不含别组。
	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Scope: "scope-a", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ids := atomIDs(got)
	if !hasAtom(ids, "sc-g") || !hasAtom(ids, "sc-a") || hasAtom(ids, "sc-b") {
		t.Fatalf("读者 scope-a 应看到「全局 + 本组」，实得 %v", ids)
	}

	// 读者 scope-b：全局 + 本组（另一组），不含 scope-a。
	got, err = s.ListAtoms(bg(), nsA, store.AtomFilter{Scope: "scope-b", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ids = atomIDs(got)
	if !hasAtom(ids, "sc-g") || !hasAtom(ids, "sc-b") || hasAtom(ids, "sc-a") {
		t.Fatalf("读者 scope-b 应看到「全局 + 本组」，实得 %v", ids)
	}

	// 归属为空 = **不隔离**：三条都在（既有行为不变）。
	all, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("不隔离时应得 3 条，实得 %d", len(all))
	}

	// 归属要能往返：存进去读出来还是它。
	one, err := s.GetAtom(bg(), nsA, "sc-a")
	if err != nil {
		t.Fatal(err)
	}
	if one.Scope != "scope-a" {
		t.Fatalf("归属往返失真: %+v", one.Scope)
	}
}

// 检索路的归属过滤：**结果里一条别组的都不能有**。
//
// 检索是取数面的一条独立路径——只在列表路上过滤是不够的。
func tAtomScopeSearchIsolation(t *testing.T, s store.Store) {
	global := atomInScope("ss-g", "e1", "", "磁盘长期偏小", "ev1", types.ImportanceHigh)
	mine := atomInScope("ss-a", "e1", "scope-a", "磁盘偏小要清理", "ev1", types.ImportanceHigh)
	other := atomInScope("ss-b", "e1", "scope-b", "磁盘占用很高", "ev1", types.ImportanceHigh)
	for _, a := range []*types.AtomCard{global, mine, other} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘", store.AtomFilter{Scope: "scope-a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Scope == "scope-b" {
			t.Fatalf("检索结果里混进了别组的卡: %+v", h)
		}
	}
	if !hasAtom(atomIDs(hits), "ss-g") || !hasAtom(atomIDs(hits), "ss-a") {
		t.Fatalf("读者 scope-a 应搜到「全局 + 本组」，实得 %v", atomIDs(hits))
	}
}

// 检索路的归属过滤必须在**取数层**生效：配额不能被别家的卡吃掉。
//
// 这条钉的是「先滤后取」而不是「先取后滤」：后置过滤时，
// 排在前面的别家的卡已经占掉了 LIMIT，于是**该给我的那条根本没进候选池**——
// 症状只是「召回变少了」，看着像排序问题，根因却在取数。
//
// # 为什么只断言「可见的那条还在」
//
// 「后置过滤」与「取数层过滤」在**排序**上无法稳定区分：谁排在前面
// 取决于后端的相关度算法（bm25 / ts_rank），而那条差异不是本仓的契约。
// 故这条断言只钉**确定的那一半**：取数层过滤的实现恒定拿回那条可见的，
// 绝不会因为别家的卡存在而变空。
// 换句话说：它**不会假红**，但对后置过滤的实现**多半**会红。
func tAtomScopeSearchQuota(t *testing.T, s store.Store) {
	// 可见的那条：只含查询词一次。
	mine := atomInScope("sq-a", "e1", "scope-a", "磁盘偏小", "ev1", types.ImportanceHigh)
	mine.SearchTerms = nil
	// 别家的那几条：查询词重复多次（相关度更高，正是后置过滤会先取到的那几条）。
	others := make([]*types.AtomCard, 0, 3)
	for _, id := range []string{"sq-b1", "sq-b2", "sq-b3"} {
		a := atomInScope(id, "e1", "scope-b",
			"磁盘磁盘磁盘磁盘磁盘磁盘磁盘磁盘磁盘磁盘"+id, "ev1", types.ImportanceHigh)
		a.SearchTerms = nil
		others = append(others, a)
	}
	if err := s.SaveAtom(bg(), nsA, mine); err != nil {
		t.Fatal(err)
	}
	for _, a := range others {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}
	// limit=1：取数层过滤保证「拿回的那一条是可见的」。
	// 后置过滤的话，别家的卡先占掉配额，滤完就空了。
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘", store.AtomFilter{Scope: "scope-a", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("limit=1 应拿回 1 条可见的，实得 %d 条（配额被别家的卡吃掉了）", len(hits))
	}
	if hits[0].ID != "sq-a" {
		t.Fatalf("拿回的应是可见的那条，实得 %s", hits[0].ID)
	}
}

// 归属也要管住**原始事件**面。
//
// 原始事件是召回的兜底源，一次就取 MaxRaw 条——
// 归属不管住它，它就是另一条泄漏路径（别组的对话进了 prompt）。
func tRawScopeIsolation(t *testing.T, s store.Store) {
	global := &types.RawEvent{ID: "rs-g", Timestamp: base, Type: types.EventManual,
		Content: "磁盘剩余 2G"}
	mine := &types.RawEvent{ID: "rs-a", Timestamp: base, Type: types.EventManual,
		Content: "内存占用偏高", Scope: "scope-a"}
	other := &types.RawEvent{ID: "rs-b", Timestamp: base, Type: types.EventManual,
		Content: "内核版本 5.15", Scope: "scope-b"}
	for _, e := range []*types.RawEvent{global, mine, other} {
		if err := s.SaveRaw(bg(), nsA, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListRaw(bg(), nsA, "scope-a", time.Time{}, store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	ids := rawIDs(got)
	if !hasRaw(ids, "rs-g") || !hasRaw(ids, "rs-a") || hasRaw(ids, "rs-b") {
		t.Fatalf("读者 scope-a 应看到「全局 + 本组」，实得 %v", ids)
	}
	hits, err := s.SearchRaw(bg(), nsA, "scope-a", "内核", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("别组的事件不该被搜到，实得 %+v", hits)
	}
}

// 候选的归属要能往返。
//
// 候选是归属的**中途站**（提炼出来、还没晋升）：这一站没存归属的话，
// 归属就在「提炼→晋升」之间断掉，于是这一组的事实变成了全局的（**泄漏**）。
func tCandidateScopeRoundTrip(t *testing.T, s store.Store) {
	c := &types.Candidate{
		ID: "sc-c1", RawEventIDs: []string{"ev1"}, Type: types.CandidateFact,
		Status: types.CandidatePending, Assertion: "磁盘长期偏小需要清理",
		QuoteEventID: "ev1", SubjectName: "机器甲",
		Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		Scope: "scope-a", CreatedAt: base,
	}
	if err := s.SaveCandidate(bg(), nsA, c); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCandidate(bg(), nsA, "sc-c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "scope-a" {
		t.Fatalf("候选的归属往返失真: %q", got.Scope)
	}
}

// ── 小工具 ──

func atomIDs(as []*types.AtomCard) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.ID)
	}
	return out
}

func hasAtom(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func rawIDs(es []*types.RawEvent) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func hasRaw(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
