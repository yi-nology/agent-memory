// Package parity 跨后端共享行为断言。
//
// # 为什么要有这一层
//
// 记忆库会有两个以上后端（SQLite 本地 / PostgreSQL 服务端）。**两个实现各自的
// 单测都绿，不代表它们行为一致**——而记忆的行为一致性是产品承诺：
// 「同一批记忆，换个部署形态，召回结果不该变」。
//
// 典型的不一致长这样，而且每一种都极难靠单测发现：
//   - 软退役的过滤条件一个写 `superseded_by=” AND deprecated_at IS NULL`，
//     另一个只判其中一列 → 召回多出一条已作废的事实；
//   - 分页游标一个用 `<=` 一个用 `<` → 某条事件永远取不到或永远重复；
//   - 时间窗一个是闭区间一个是开区间 → 跨月首日的情节落在窗外。
//
// 所以断言写在这里，**两个后端都跑同一份**。新后端实现了 store.Store 之后，
// 唯一要做的就是接上这套断言——它不通过，就不算实现完了。
//
// # 用法
//
//	func TestParity(t *testing.T) {
//	    parity.Run(t, func(t *testing.T) store.Store {
//	        s, err := sqlite.OpenMemory(t.Name())
//	        …
//	        t.Cleanup(func() { _ = s.Close() })
//	        return s
//	    })
//	}
//
// # 一条硬规矩
//
// **跳过的用例不证明兼容。** 上游 octop-memory 对此的措辞是「skipped PG cases
// do not prove compatibility」——被跳过的断言等于不存在，它给的是假安全感。
// 因此调用方必须让跳过**可见**（t.Skip 会打印），且不得把「全绿」当成
// 「两种后端都验证过」。真实 PG 用例必须对着真服务器跑（Dsn 环境变量）。
package parity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// OpenFn 造一个干净的后端。实现方负责在 t.Cleanup 里关掉。
type OpenFn func(t *testing.T) store.Store

// Run 跑全套跨后端行为断言。
func Run(t *testing.T, open OpenFn) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, store.Store)
	}{
		{"捕获事件：存取往返", tRawRoundTrip},
		{"捕获事件：批量写", tRawBatch},
		{"捕获事件：倒序列出与时间游标", tRawListCursor},
		{"捕获事件：全文检索", tRawSearch},
		{"捕获事件：按龄清理连带索引", tRawDeleteBefore},
		{"捕获事件：来源凭证往返", tRawProvenance},
		{"候选：存取与状态流转", tCandidateLifecycle},
		{"候选：重复检测只认未落定", tCandidateDuplicate},
		{"候选：乐观锁挡住并发覆盖", tCandidateStaleWrite},
		{"事实卡：软退役两态都排除出召回", tAtomRetiredExcluded},
		{"事实卡：签名去重", tAtomSignature},
		{"事实卡：全文检索只命中相关事实", tAtomSearch},
		{"事实卡：中文子串检索命中真实形态中文", tAtomSearchChineseSubstring},
		{"事实卡：形状校验拦无出处", tAtomValidation},
		{"事实卡：退役后仍可搜到（可追问「当初为何」）", tAtomRetiredStillSearchable},
		{"事实卡：重复保存是更新而不是冲突", tAtomSaveIsUpsert},
		{"事实卡：并列时的返回顺序确定", tAtomOrderIsDeterministic},
		{"事实卡：重要度排序按语义序而非字典序", tAtomImportanceOrder},
		{"事实卡：按实体取全部（含已退役）", tAtomListByEntity},
		{"事实卡：带向量的卡不入库（后端不落向量）", tAtomVectorNotStored},
		{"事实卡：归属隔离（全局 + 同归属）", tAtomScopeIsolation},
		{"事实卡：检索路也按归属隔离", tAtomScopeSearchIsolation},
		{"事实卡：归属过滤在取数层（配额不被别家吃掉）", tAtomScopeSearchQuota},
		{"捕获事件：归属隔离", tRawScopeIsolation},
		{"候选：归属往返", tCandidateScopeRoundTrip},
		{"实体：规范名与别名两条路径都能查到", tEntityLookupBothPaths},
		{"实体：计数钳到 0 不报错", tEntityCountClamp},
		{"活跃实体栈：钉住项不参与淘汰", tActiveEntityPin},
		{"活跃实体栈：按最近提及排序", tActiveEntityOrder},
		{"页面：标脏在页面不存在时自建", tPageMarkDirtyCreates},
		{"页面：重生成失败保持脏标记", tPageRegenFailureKeepsDirty},
		{"页面：重生成成功清脏清失败", tPageRegenSuccessClears},
		{"情节与周期摘要", tEpisodeDigest},
		{"审计条目按主体检索", tJournalBySubject},
		{"元数据按命名空间隔离", tMetaIsolation},
		{"命名空间之间互不可见", tNamespaceIsolation},
		{"事务：出错整体回滚", tTransactionRollback},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, open(t))
		})
	}
}

// ── 夹具 ──

var base = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

const nsA = store.Namespace("ns-a")
const nsB = store.Namespace("ns-b")

func atom(id, entity, assertion, evidence string, imp types.Importance) *types.AtomCard {
	return &types.AtomCard{
		ID:            id,
		EntityID:      entity,
		CandidateID:   "c-" + id,
		RawEventIDs:   []string{evidence},
		Assertion:     assertion,
		VerbatimQuote: "「" + assertion + "」",
		QuoteEventID:  evidence,
		// 检索词由断言派生（取首词）：若给所有夹具固定检索词，它们会在 FTS
		// 正文里互相命中，「只命中相关事实」这条断言就测不出东西了。
		SearchTerms: []string{firstTerm(assertion)},
		OccurredAt:  base,
		Confidence:  types.ConfidenceHigh,
		Importance:  imp,
		CreatedAt:   base,
		UpdatedAt:   base,
		Provenance:  types.Provenance{Origin: types.OriginOperator},
	}
}

// bg 取一个干净上下文。叫 bg 而不是 ctx：测试函数里普遍有 ctx 局部变量，
// 同名会让 `bg()` 在那个作用域里解析成变量而编译失败。
func bg() context.Context { return context.Background() }

// firstTerm 取断言的首词作为检索词（按 rune 切，避免把多字节字符切坏）。
func firstTerm(s string) string {
	for _, f := range strings.Fields(s) {
		r := []rune(f)
		switch {
		case len(r) >= 2:
			return string(r[:2])
		case len(r) == 1:
			return f
		}
	}
	return s
}

// ── 捕获事件 ──

func tRawRoundTrip(t *testing.T, s store.Store) {
	in := &types.RawEvent{
		ID: "e1", Host: "h", SessionID: "sess", ThreadID: "th", User: "u",
		Timestamp: base, Type: types.EventTurnEnd, Content: "磁盘长期偏小",
		Payload: map[string]any{"k": "v"},
	}
	if err := s.SaveRaw(bg(), nsA, in); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, err := s.GetRaw(bg(), nsA, "e1")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Content != in.Content || got.SessionID != "sess" || got.User != "u" {
		t.Fatalf("往返失真: %+v", got)
	}
	if !got.Timestamp.Equal(base) {
		t.Fatalf("时刻往返失真: 期望 %v 实得 %v", base, got.Timestamp)
	}
	if got.Payload["k"] != "v" {
		t.Fatalf("附加载荷丢失: %+v", got.Payload)
	}

	if _, err := s.GetRaw(bg(), nsA, "不存在"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("不存在应判 ErrNotFound，实得 %v", err)
	}
}

func tRawBatch(t *testing.T, s store.Store) {
	batch := []*types.RawEvent{
		{ID: "b1", Timestamp: base, Type: types.EventManual, Content: "第一条"},
		{ID: "b2", Timestamp: base.Add(time.Second), Type: types.EventManual, Content: "第二条"},
	}
	if err := s.SaveRawBatch(bg(), nsA, batch); err != nil {
		t.Fatalf("批量写失败: %v", err)
	}
	got, err := s.GetRawByIDs(bg(), nsA, []string{"b1", "b2"})
	if err != nil {
		t.Fatalf("批量读失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("批量读应得 2 条，实得 %d", len(got))
	}
}

func tRawListCursor(t *testing.T, s store.Store) {
	// 同一秒内写入三条：分页顺序必须仍然是确定的（这是 ts 存纳秒的理由）。
	for i, id := range []string{"c1", "c2", "c3"} {
		ev := &types.RawEvent{
			ID: id, Timestamp: base.Add(time.Duration(i) * time.Millisecond),
			Type: types.EventManual, Content: "事件 " + id,
		}
		if err := s.SaveRaw(bg(), nsA, ev); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListRaw(bg(), nsA, "", time.Time{}, store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("应列出 3 条，实得 %d", len(all))
	}
	if all[0].ID != "c3" {
		t.Fatalf("倒序应从最新开始，实得首条 %s", all[0].ID)
	}
	// 游标翻页：以 all[0] 的时刻为界，不应再包含它自身
	next, err := s.ListRaw(bg(), nsA, "", all[0].Timestamp, store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range next {
		if ev.ID == "c3" {
			t.Fatal("时间游标取到了边界自身，会导致同一条被反复返回")
		}
	}
}

func tRawSearch(t *testing.T, s store.Store) {
	for _, e := range []*types.RawEvent{
		// 真实中文形态：句子内部没有空格分词。
		// 夹具写成「磁盘 剩余 2G」会让 unicode61 正好切成两个词元，
		// 于是「中文子串检索失效」这个缺陷在 parity 下全绿——夹具替实现背书。
		{ID: "s1", Timestamp: base, Type: types.EventManual, Content: "磁盘剩余 2G"},
		{ID: "s2", Timestamp: base, Type: types.EventManual, Content: "内存占用偏高"},
	} {
		if err := s.SaveRaw(bg(), nsA, e); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.SearchRaw(bg(), nsA, "", "磁盘", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "s1" {
		t.Fatalf("应只命中 s1，实得 %+v", hits)
	}
	// 词中子串同样要命中。
	hits2, err := s.SearchRaw(bg(), nsA, "", "占用", 5)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits2) != 1 || hits2[0].ID != "s2" {
		t.Fatalf("词中子串应只命中 s2，实得 %+v", hits2)
	}
}

func tRawDeleteBefore(t *testing.T, s store.Store) {
	for i, id := range []string{"old1", "old2"} {
		_ = s.SaveRaw(bg(), nsA, &types.RawEvent{
			ID: id, Timestamp: base.Add(time.Duration(i) * time.Hour),
			Type: types.EventManual, Content: "旧事件 " + id,
		})
	}
	_ = s.SaveRaw(bg(), nsA, &types.RawEvent{
		ID: "keep", Timestamp: base.Add(48 * time.Hour),
		Type: types.EventManual, Content: "保留事件",
	})

	n, err := s.DeleteRawBefore(bg(), nsA, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应清理 2 条，实得 %d", n)
	}
	// 索引必须同步清：漏清会留下「搜得到但取不到」的幽灵。
	hits, err := s.SearchRaw(bg(), nsA, "", "旧事件", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("清理后索引仍有 %d 条幽灵结果", len(hits))
	}
	keep, err := s.GetRaw(bg(), nsA, "keep")
	if err != nil || keep.ID != "keep" {
		t.Fatalf("窗口外的事件不该被删: %v", err)
	}
}

func tRawProvenance(t *testing.T, s store.Store) {
	_ = s.SaveRaw(bg(), nsA, &types.RawEvent{
		ID: "p1", Timestamp: base, Type: types.EventToolResult, Content: "命令输出",
		Provenance: types.Provenance{
			Origin: types.OriginTool, Tool: "shell", RuntimeError: true, Redacted: true, Host: "h",
		},
	})
	got, err := s.GetRaw(bg(), nsA, "p1")
	if err != nil {
		t.Fatal(err)
	}
	p := got.Provenance
	if !p.RuntimeError || !p.Redacted || p.Tool != "shell" || p.Origin != types.OriginTool {
		t.Fatalf("来源凭证往返失真: %+v", p)
	}
	if got.Capturable() {
		t.Fatal("标了运行期错误的事件不得进入自动捕获")
	}
}

// ── 候选 ──

func tCandidateLifecycle(t *testing.T, s store.Store) {
	c := &types.Candidate{
		ID: "c1", RawEventIDs: []string{"e1"}, Type: types.CandidateFact,
		Status: types.CandidatePending, Assertion: "磁盘长期偏小",
		SubjectName: "机器甲", Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		CreatedAt: base,
	}
	if err := s.SaveCandidate(bg(), nsA, c); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateCandidateStatus(bg(), nsA, "c1",
		types.CandidatePending, types.CandidatePromoted, types.DecidedRule, "证据充分"); err != nil {
		t.Fatalf("落定失败: %v", err)
	}
	got, err := s.GetCandidate(bg(), nsA, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.CandidatePromoted || got.DecidedBy != types.DecidedRule {
		t.Fatalf("落定结果失真: %+v", got)
	}
	if !got.Decided() || !got.Settled() {
		t.Fatal("已晋升候选应既表态又落定")
	}
	// 枚举校验：未知状态必须被拒，不能静默落库。
	if err := s.UpdateCandidateStatus(bg(), nsA, "c1",
		types.CandidatePromoted, types.CandidateStatus("大概吧"), types.DecidedRule, ""); err == nil {
		t.Fatal("未知候选状态必须被拒")
	}
}

func tCandidateDuplicate(t *testing.T, s store.Store) {
	mk := func(id string, status types.CandidateStatus) *types.Candidate {
		return &types.Candidate{
			ID: id, RawEventIDs: []string{"e1"}, Type: types.CandidateFact,
			Status: status, Assertion: "磁盘长期偏小", SubjectName: "机器甲",
			CreatedAt: base, Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		}
	}
	_ = s.SaveCandidate(bg(), nsA, mk("p1", types.CandidatePending))

	dup, err := s.FindDuplicateCandidate(bg(), nsA, "机器甲"+types.SignatureSep+"磁盘长期偏小")
	if err != nil {
		t.Fatalf("重复检测失败: %v", err)
	}
	if dup.ID != "p1" {
		t.Fatalf("应命中未落定的 p1，实得 %s", dup.ID)
	}

	// 落定后不应再挡住新候选——晋升层真正关心的是事实卡，不是候选。
	_ = s.UpdateCandidateStatus(bg(), nsA, "p1",
		types.CandidatePending, types.CandidatePromoted, types.DecidedRule, "")
	if _, err := s.FindDuplicateCandidate(bg(), nsA, "机器甲"+types.SignatureSep+"磁盘长期偏小"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("落定候选不该继续挡住，实得 %v", err)
	}
}

// tCandidateStaleWrite 两个晋升器同时处理同一条候选：后到的必须知道自己撞了。
func tCandidateStaleWrite(t *testing.T, s store.Store) {
	_ = s.SaveCandidate(bg(), nsA, &types.Candidate{
		ID: "c1", RawEventIDs: []string{"e1"}, Type: types.CandidateFact,
		Status: types.CandidatePending, Assertion: "x", CreatedAt: base,
	})
	// 第一个晋升器按 pending 判定成功。
	if err := s.UpdateCandidateStatus(bg(), nsA, "c1",
		types.CandidatePending, types.CandidatePromoted, types.DecidedRule, "先到"); err != nil {
		t.Fatal(err)
	}
	// 第二个晋升器仍以为现值是 pending —— 必须被挡下。
	err := s.UpdateCandidateStatus(bg(), nsA, "c1",
		types.CandidatePending, types.CandidateDropped, types.DecidedModel, "后到")
	if !errors.Is(err, store.ErrStaleWrite) {
		t.Fatalf("陈旧覆盖必须判 ErrStaleWrite，实得 %v", err)
	}
	got, _ := s.GetCandidate(bg(), nsA, "c1")
	if got.Status != types.CandidatePromoted || got.PromotionReason != "先到" {
		t.Fatalf("后到的判定不得覆盖先到的: %+v", got)
	}
}

// ── 实体 ──

func tEntityLookupBothPaths(t *testing.T, s store.Store) {
	_ = s.SaveEntity(bg(), nsA, &types.Entity{
		ID: "e1", EntityType: "机器", CanonicalName: "机器甲", CreatedAt: base,
	})
	// 规范名路径
	got, err := s.FindEntityByName(bg(), nsA, "机器甲")
	if err != nil {
		t.Fatalf("规范名查不到: %v", err)
	}
	if got.ID != "e1" {
		t.Fatalf("实得 %s", got.ID)
	}
	// 别名路径——漏掉这条的后果是「用户换个说法就召回不到」
	_ = s.SaveAlias(bg(), nsA, &types.Alias{
		Alias: "那台开发机", EntityID: "e1", EntityType: "机器",
		CreatedBy: types.DecidedUser, CreatedAt: base,
	})
	viaAlias, err := s.FindEntityByName(bg(), nsA, "那台开发机")
	if err != nil {
		t.Fatalf("别名查不到: %v", err)
	}
	if viaAlias.ID != "e1" || len(viaAlias.Aliases) != 1 {
		t.Fatalf("别名路径结果失真: %+v", viaAlias)
	}
}

func tEntityCountClamp(t *testing.T, s store.Store) {
	_ = s.SaveEntity(bg(), nsA, &types.Entity{ID: "e1", CanonicalName: "机器甲", CreatedAt: base})
	// 退役一条事实时计数会减，计数已是 0 时不许报错（否则真事实退役被阻断）。
	if err := s.BumpEntityAtomCount(bg(), nsA, "e1", -1); err != nil {
		t.Fatalf("计数为负必须钳住而不是报错: %v", err)
	}
	got, err := s.GetEntity(bg(), nsA, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AtomCount != 0 {
		t.Fatalf("计数应钳到 0，实得 %d", got.AtomCount)
	}
}

func tActiveEntityPin(t *testing.T, s store.Store) {
	_ = s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-old", CanonicalName: "很久以前",
		LastSeen: base.Add(-time.Hour),
	})
	_ = s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-pin", CanonicalName: "刚确认的",
		LastSeen: base.Add(-2 * time.Hour), Pinned: true, PinTime: base,
	})
	_ = s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-new", CanonicalName: "刚提到的",
		LastSeen: base,
	})

	victims, err := s.EvictActiveEntities(bg(), nsA, "th", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range victims {
		if v.EntityID == "e-pin" {
			t.Fatal("钉住项不得被淘汰：用户刚确认过的对象滑出栈会让「那个」立刻指错人")
		}
	}
	left, err := s.ListActiveEntities(bg(), nsA, "th", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range left {
		ids[a.EntityID] = true
	}
	if !ids["e-pin"] || !ids["e-new"] {
		t.Fatalf("淘汰后应留下钉住项与最近项，实得 %v", ids)
	}
}

func tActiveEntityOrder(t *testing.T, s store.Store) {
	_ = s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e1", LastSeen: base.Add(-time.Hour),
	})
	_ = s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e2", LastSeen: base,
	})
	got, err := s.ListActiveEntities(bg(), nsA, "th", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].EntityID != "e2" {
		t.Fatalf("应按最近提及倒序，栈顶是当前话题")
	}
}

// ── 页面 ──

// 晋升先于首次生成是常态，所以标脏必须能自建页面——
// 报「页面不存在」会把一次正常写入变成错误。
func tPageMarkDirtyCreates(t *testing.T, s store.Store) {
	if _, err := s.GetEntityPage(bg(), nsA, "e-new"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("前置条件失真，页面本不该存在: %v", err)
	}
	if err := s.MarkEntityPageDirty(bg(), nsA, "e-new", base); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	got, err := s.GetEntityPage(bg(), nsA, "e-new")
	if err != nil {
		t.Fatalf("标脏应自建页面: %v", err)
	}
	if !got.Dirty {
		t.Fatal("自建的页面应处于脏态")
	}
}

func tPageRegenFailureKeepsDirty(t *testing.T, s store.Store) {
	_ = s.MarkEntityPageDirty(bg(), nsA, "e1", base)
	_ = s.RecordEntityPageRegenFailure(bg(), nsA, "e1", "模型超时", base)
	got, err := s.GetEntityPage(bg(), nsA, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Dirty {
		t.Fatal("重生成失败必须保持脏标记：清掉就永远不再进队列，而用户看到的页面与正常页面长得一样")
	}
	if got.RegenFailure != "模型超时" {
		t.Fatalf("失败原因必须留痕，实得 %q", got.RegenFailure)
	}
	if !got.RegenerationDue(base.Add(time.Hour), time.Minute) {
		t.Fatal("失败不豁免重试")
	}
}

func tPageRegenSuccessClears(t *testing.T, s store.Store) {
	_ = s.MarkEntityPageDirty(bg(), nsA, "e1", base)
	_ = s.RecordEntityPageRegenFailure(bg(), nsA, "e1", "上次超时", base)
	_ = s.ApplyEntityPageRegen(bg(), nsA, "e1", &types.EntityPage{
		Headline: "长期偏小", Body: "正文", AtomIDs: []string{"a1"},
	}, base)
	got, err := s.GetEntityPage(bg(), nsA, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Dirty || got.RegenFailure != "" {
		t.Fatalf("生成成功应清脏清失败，实得 dirty=%v failure=%q", got.Dirty, got.RegenFailure)
	}
	if len(got.AtomIDs) != 1 || got.AtomIDs[0] != "a1" {
		t.Fatalf("覆盖的事实集合丢失: %+v", got.AtomIDs)
	}
}

// ── 情节 / 摘要 / 审计 / 元数据 ──

func tEpisodeDigest(t *testing.T, s store.Store) {
	_ = s.SaveEpisode(bg(), nsA, &types.Episode{
		ID: "ep1", SessionID: "s1", Title: "一次排查",
		Summary: "定位到磁盘", Decisions: []string{"清理旧文件"},
		OccurredAt: base, CreatedAt: base,
	})
	eps, err := s.ListEpisodesInWindow(bg(), nsA, base.Add(-time.Hour), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || len(eps[0].Decisions) != 1 {
		t.Fatalf("时间窗查询失真: %+v", eps)
	}
	_ = s.SaveDigest(bg(), nsA, &types.DigestRecord{
		Period: types.PeriodDay, PeriodKey: "2026-10-19", Markdown: "# 日报",
		EpisodeIDs: []string{"ep1"}, WindowStart: base, WindowEnd: base.Add(24 * time.Hour),
		GeneratedAt: base,
	})
	d, err := s.GetDigest(bg(), nsA, types.PeriodDay, "2026-10-19")
	if err != nil {
		t.Fatal(err)
	}
	if d.Markdown != "# 日报" || len(d.EpisodeIDs) != 1 {
		t.Fatalf("周期摘要往返失真: %+v", d)
	}
}

func tJournalBySubject(t *testing.T, s store.Store) {
	for i := 1; i <= 2; i++ {
		_ = s.AppendJournal(bg(), nsA, &types.JournalEntry{
			ID: string(rune('j' + i)), At: base.Add(time.Duration(i) * time.Minute),
			Kind: types.JournalPromote, Subject: "a1", Actor: types.DecidedRule,
			Summary: "晋升", Detail: map[string]any{"n": i},
		})
	}
	_ = s.AppendJournal(bg(), nsA, &types.JournalEntry{
		ID: "jz", At: base, Kind: types.JournalDrop, Subject: "a2", Actor: types.DecidedRule,
	})
	got, err := s.ListJournal(bg(), nsA, "a1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("按主体审计应得 2 条，实得 %d", len(got))
	}
	if got[0].At.Before(got[1].At) {
		t.Fatal("审计应按时刻倒序")
	}
}

func tMetaIsolation(t *testing.T, s store.Store) {
	if err := s.SetMeta(bg(), nsA, "extractor", "v1"); err != nil {
		t.Fatal(err)
	}
	v, err := s.GetMeta(bg(), nsA, "extractor")
	if err != nil || v != "v1" {
		t.Fatalf("元数据往返失真: %q %v", v, err)
	}
	if _, err := s.GetMeta(bg(), nsB, "extractor"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("命名空间隔离失守，另一库读到了本库的键: %v", err)
	}
}

// ── 命名空间与事务 ──

// 一个库服务多个记忆库时，隔离是最容易在「忘了加 WHERE 子句」处破的。
func tNamespaceIsolation(t *testing.T, s store.Store) {
	_ = s.SaveAtom(bg(), nsA, atom("only-a", "e1", "只属于 A 库的事实", "ev1", types.ImportanceHigh))
	if _, err := s.GetAtom(bg(), nsB, "only-a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B 库读到了 A 库的事实: %v", err)
	}
	got, err := s.ListAtoms(bg(), nsB, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("B 库不该列出任何事实，实得 %d", len(got))
	}
}

// 事务是「候选落定 + 事实入库 + 计数 + 审计」四步的唯一保障。
func tTransactionRollback(t *testing.T, s store.Store) {
	boom := errors.New("中途失败")
	err := s.Transaction(bg(), func(ctx context.Context, tx store.Store) error {
		if err := tx.SaveAtom(bg(), nsA, atom("rolled-back", "e1", "不该留下", "ev1", types.ImportanceHigh)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("事务应把回调错误原样抛出，实得 %v", err)
	}
	if _, err := s.GetAtom(bg(), nsA, "rolled-back"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("回滚后不该留下任何行: %v", err)
	}

	// 正常提交必须真的落库。
	if err := s.Transaction(bg(), func(ctx context.Context, tx store.Store) error {
		return tx.SaveAtom(bg(), nsA, atom("committed", "e1", "该留下", "ev1", types.ImportanceHigh))
	}); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if _, err := s.GetAtom(bg(), nsA, "committed"); err != nil {
		t.Fatalf("提交后应能读到: %v", err)
	}
}
