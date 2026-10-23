package application_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
)

// 本文件：**对外发布契约的冻结清单**。
//
// # 这份清单在守什么
//
// 本仓的存储契约、召回契约与端口契约都是**对外发布**的：
// 仓外的宿主会自己实现它们（宿主仓 bianque 的做法是
// `var _ store.AtomStore = (*Store)(nil)`）。
//
// 于是在这些接口上**加一个方法**，对实现者就是**隐式 breaking**：
//
//	v0.8.0 给 AtomStore 加 SearchAtomsLoose
//	⇒ 宿主仓那行编译期断言当场红
//	⇒ 上游发 tag 的当天，下游整个仓编译不过。
//
// 这不是个别宿主的问题：**任何**在仓外实现这些接口的项目都会被同一条打中，
// 而「自己实现内核存储契约」正是本仓鼓励的用法（机制面下沉、存储面自留）。
//
// # 为什么清单放在 application 这一层
//
// 因为它是**唯一**同时看见这三张契约面的地方：
// 装配面知道「谁跟谁怎么装」，而这三张面正是在装配处对接的。
// 放回各自的包里会散成三份清单——三份就要同步三份，漏一份就等于没冻。
//
// # 清单变了意味着什么
//
// 改动这份清单 = **一次有意的破坏性变更**。它必须同时做三件事：
//
//  1. 走**大版本**（对外承诺的破坏）；
//  2. 在发布说明里**点名**：「本次新增方法会让外部实现者编译失败，
//     迁移方式：……」；
//  3. 确认新方法**不是**「可选能力」——
//     可选能力该落成独立小接口 + 运行时探测 + 降级路径（见 store/optional.go）。
//
// 反过来：**新能力一律走可选接口**，那份清单不用动。
//
// # 这不是「不许演进」
//
// 它是把演进从「顺手加个方法」变成「一次显式决定」。
// 上一轮就是「顺手加个方法」⇒ 仓外下游当天编译不过。

// frozenContract 一张对外发布的接口 + 它被冻结的方法集。
type frozenContract struct {
	name string
	// iface 指向接口类型的指针（反射取方法集用）。
	iface   any
	methods []string
}

// frozenContracts 冻结清单。
//
// ⚠️ 改这里之前先读本文件的抬头：改动 = 一次有意的破坏性变更。
var frozenContracts = []frozenContract{
	// ── store：仓外适配层实现的主面 ──
	{"store.RawStore", (*store.RawStore)(nil), []string{
		"DeleteRawBefore", "GetRaw", "GetRawByIDs", "ListRaw", "SaveRaw", "SaveRawBatch", "SearchRaw",
	}},
	{"store.RawWriter", (*store.RawWriter)(nil), []string{
		"SaveRaw", "SaveRawBatch",
	}},
	{"store.CandidateStore", (*store.CandidateStore)(nil), []string{
		"FindDuplicateCandidate", "GetCandidate", "ListCandidates", "SaveCandidate", "SearchCandidates", "UpdateCandidateStatus",
	}},
	{"store.AtomStore", (*store.AtomStore)(nil), []string{
		"CountStats", "DeprecateAtom", "FindAtomBySignature", "GetAtom", "ListAtoms",
		"ListAtomsByEntity", "SaveAtom", "SearchAtoms", "SupersedeAtom",
	}},
	{"store.EntityStore", (*store.EntityStore)(nil), []string{
		"BumpEntityAtomCount", "EvictActiveEntities", "FindEntityByName", "GetEntity",
		"ListActiveEntities", "ListAliases", "ListEntities", "SaveAlias", "SaveEntity", "UpsertActiveEntity",
	}},
	{"store.PageStore", (*store.PageStore)(nil), []string{
		"ApplyEntityPageRegen", "GetEntityPage", "ListDirtyEntityPages", "ListEntityPages",
		"MarkEntityPageDirty", "RecordEntityPageRegenFailure", "UpsertEntityPage",
	}},
	{"store.EdgeStore", (*store.EdgeStore)(nil), []string{
		"DeleteEdge", "DeleteEdgesBySource", "GetEdge", "ListEdges", "UpsertEdge",
	}},
	{"store.EpisodeStore", (*store.EpisodeStore)(nil), []string{
		"GetDigest", "ListDigests", "ListEpisodes", "ListEpisodesInWindow", "SaveDigest", "SaveEpisode",
	}},
	{"store.JournalStore", (*store.JournalStore)(nil), []string{
		"AppendJournal", "ListJournal",
	}},
	{"store.MetaStore", (*store.MetaStore)(nil), []string{
		"GetMeta", "SetMeta",
	}},
	// store.Store 是聚合面：给它加一个新子接口 = 仓外实现者也要实现那一整片。
	{"store.Store", (*store.Store)(nil), []string{
		"AppendJournal", "ApplyEntityPageRegen", "BumpEntityAtomCount", "Close", "CountStats",
		"DeleteRawBefore", "DeprecateAtom", "EvictActiveEntities", "FindAtomBySignature",
		"FindDuplicateCandidate", "FindEntityByName", "GetAtom", "GetCandidate", "GetDigest",
		"GetEntity", "GetEntityPage", "GetMeta", "GetRaw", "GetRawByIDs", "ListActiveEntities",
		"ListAliases", "ListAtoms", "ListAtomsByEntity", "ListCandidates", "ListDigests",
		"ListDirtyEntityPages", "ListEntities", "ListEntityPages", "ListEpisodes",
		"ListEpisodesInWindow", "ListJournal", "ListRaw", "MarkEntityPageDirty",
		"RecordEntityPageRegenFailure", "SaveAlias", "SaveAtom", "SaveCandidate", "SaveDigest",
		"SaveEntity", "SaveEpisode", "SaveRaw", "SaveRawBatch", "SearchAtoms", "SearchCandidates",
		"SearchRaw", "SetMeta", "SupersedeAtom", "Transaction", "UpdateCandidateStatus",
		"UpsertActiveEntity", "UpsertEntityPage",
	}},
	// 可选能力面同样冻结：宿主实现了它，给它加方法同样打红下游。
	{"store.AtomLooseSearcher", (*store.AtomLooseSearcher)(nil), []string{
		"SearchAtomsLoose",
	}},

	// ── recall：宿主把存储喂进来的那张面 ──
	{"recall.Reader", (*recall.Reader)(nil), []string{
		"EvictActiveEntities", "GetEntityPage", "ListActiveEntities", "ListAtoms",
		"SearchAtoms", "UpsertActiveEntity",
	}},
	{"recall.RawReader", (*recall.RawReader)(nil), []string{
		"ListRaw",
	}},

	// ── ports：宿主注入/实现的能力面 ──
	{"ports.TokenEstimator", (*ports.TokenEstimator)(nil), []string{
		"Estimate",
	}},
	{"ports.Bander", (*ports.Bander)(nil), []string{
		"WrapMemory",
	}},
	{"ports.Embedder", (*ports.Embedder)(nil), []string{
		"Embed",
	}},
	{"ports.VectorIndex", (*ports.VectorIndex)(nil), []string{
		"Delete", "Search", "Upsert",
	}},
	{"ports.LLM", (*ports.LLM)(nil), []string{
		"Complete",
	}},
	{"ports.Metrics", (*ports.Metrics)(nil), []string{
		"Gauge", "Incr", "Observe",
	}},
	{"ports.Ranker", (*ports.Ranker)(nil), []string{
		"Score",
	}},
	{"ports.Redactor", (*ports.Redactor)(nil), []string{
		"Redact",
	}},
	{"ports.SemanticReader", (*ports.SemanticReader)(nil), []string{
		"Search",
	}},
}

// TestPublishedContractsAreFrozen 已发布的契约面不许悄悄变。
//
// 失败时它给出**该怎么做**，而不只是「不匹配」：
// 新增方法 ⇒ 仓外实现者编译失败（issue #12 的原形）。
func TestPublishedContractsAreFrozen(t *testing.T) {
	for _, c := range frozenContracts {
		got := ifaceMethodNames(c.iface)
		if diff := contractDiff(c.methods, got); diff != "" {
			t.Errorf("%s 的方法集变了：%s\n"+
				"  这张接口是**对外发布**的：仓外宿主会实现它，"+
				"加方法 ⇒ 它们的编译期断言当场红（上游发 tag 的当天下游就得改）。\n"+
				"  该怎么做：\n"+
				"    ① 若新方法是**可选能力** ⇒ 落成独立小接口 + 运行时探测 + 降级路径"+
				"（见 store/optional.go 的「契约演进规约」），清单不用动；\n"+
				"    ② 若确实是有意的破坏性变更 ⇒ 走大版本 + 发布说明点名"+
				"「外部实现者需要改」+ 同步改这份清单。",
				c.name, diff)
		}
	}
}

// contractDiff 比对两份方法集，返回人类可读的差异（相同则空串）。
//
// 单独成函数，是为了让「比对本身不是空转」这件事可被用例验
// （见 TestContractDiffDetectsDrift）——
// 一条永远返回空的比对函数会让上面那条用例**恒绿**。
func contractDiff(want, got []string) string {
	w := map[string]bool{}
	for _, m := range want {
		w[m] = true
	}
	g := map[string]bool{}
	for _, m := range got {
		g[m] = true
	}
	var added, removed []string
	for _, m := range got {
		if !w[m] {
			added = append(added, m)
		}
	}
	for _, m := range want {
		if !g[m] {
			removed = append(removed, m)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	var b strings.Builder
	if len(added) > 0 {
		fmt.Fprintf(&b, "新增 %v", added)
	}
	if len(removed) > 0 {
		if b.Len() > 0 {
			b.WriteString("；")
		}
		fmt.Fprintf(&b, "移除 %v", removed)
	}
	return b.String()
}

// ifaceMethodNames 取接口的方法名（排序后）。
func ifaceMethodNames(p any) []string {
	t := reflect.TypeOf(p)
	if t == nil || t.Kind() != reflect.Ptr || t.Elem().Kind() != reflect.Interface {
		return nil
	}
	et := t.Elem()
	names := make([]string, 0, et.NumMethod())
	for i := 0; i < et.NumMethod(); i++ {
		names = append(names, et.Method(i).Name)
	}
	sort.Strings(names)
	return names
}

// TestContractDiffDetectsDrift 比对逻辑本身要能发现差异。
//
// ⚠️ 这条是**上面那条的判别特征**：没有它，上面那条可能因为
// 「比对函数恒返回空串」而绿——而它守的东西（冻结）根本没人守。
func TestContractDiffDetectsDrift(t *testing.T) {
	if d := contractDiff([]string{"A"}, []string{"A"}); d != "" {
		t.Fatalf("相同的方法集不该报差异: %s", d)
	}
	d := contractDiff([]string{"A"}, []string{"A", "B"})
	if d == "" {
		t.Fatal("新增方法必须被报出来")
	}
	if !strings.Contains(d, "B") {
		t.Fatalf("差异信息该点名新增的方法: %s", d)
	}
	d = contractDiff([]string{"A", "B"}, []string{"A"})
	if d == "" {
		t.Fatal("移除方法必须被报出来")
	}
	if !strings.Contains(d, "B") {
		t.Fatalf("差异信息该点名被移除的方法: %s", d)
	}
}
