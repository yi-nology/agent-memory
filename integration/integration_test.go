// Package integration 跨包端到端测试的**专属位置**。
//
// # 为什么要有这个包
//
// 架构门禁的依赖方向规则对 `_test.go` 同样生效，而端到端用例天生要跨好几层
// （捕获 → 提炼 → 晋升 → 页面 → 情节 → 召回 → 迁移）。没有这个位置时，
// 那些用例要么写不进任何包（撞方向规则），要么被塞进某个流水线包并顺手
// 放宽它的 requires——而「为了塞下一个测试而放宽生产包的依赖方向」
// 是门禁最容易被绕过的一种方式。
//
// 故 architecture-policy.yaml 里登记了 integration 模块，roots 为本目录，
// requires 放开各层；方向规则对��仍然生效（不许反向依赖宿主与编排），
// 但它**可以**正当地把各层放在一起看。
//
// # 它验的是什么
//
// 每条流水线各自单测都绿，而主链在最后一环断掉（晋升不落库）这类缺陷，
// 只有走完整条链的用例才抓得到。
// 这个包里的用例就是那种用例：**用真实的存储后端，走真实的公开 API**，
// 不 mock 掉任何一层——mock 掉的那一层正是最可能断的地方。
package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/lifecycle"
	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/pipeline/entity"
	"github.com/yi-nology/agent-memory/pipeline/episode"
	"github.com/yi-nology/agent-memory/pipeline/page"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

const ns = store.Namespace("it")

// fakeLLM 按用途返回固定的 JSON。
//
// 它的存在只为让「需要模型的那几步」（页面重生成、情节摘要）能在测试里跑起来——
// 本包要验的是**接线**，不是模型质量。
type fakeLLM struct {
	extract string
	page    string
	episode string
	digest  string
}

func (f fakeLLM) Complete(_ context.Context, req ports.LLMRequest) (string, error) {
	switch req.Purpose {
	case "memory.extract":
		return f.extract, nil
	case "memory.page.regen":
		return f.page, nil
	case "memory.episode.extract":
		return f.episode, nil
	case "memory.digest":
		return f.digest, nil
	default:
		return "[]", nil
	}
}

// openFresh 开一份干净的内存库。
//
// 库名带序号：SQLite 的具名内存库是**共享缓存**的，同名即同一个库——
// 而一个用例里开两份同名库时，第二份拿到的不是空库而是第一份的延续，
// 于是「导入到新库」这类断言会撞唯一键，症状与真实缺陷难辨。
var dbSeq int

func openFresh(t *testing.T) *sqlite.Store {
	t.Helper()
	dbSeq++
	s, err := sqlite.OpenMemory(fmt.Sprintf("%s_%d", t.Name(), dbSeq))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestMainMemoryLoop 记忆主链走通一遍：捕获 → 提炼 → 晋升 → 落库 → 召回。
//
// 这是本包存在的主要理由：主链断在最后一环（晋升返回判决但不落库）时，
// 各包自己的单测**全绿**——晋升的用例断言的是「判决正确」，
// 不是「判决之后库里多了东西」。
func TestMainMemoryLoop(t *testing.T) {
	ctx := context.Background()
	s := openFresh(t)
	llm := fakeLLM{
		// 引用必须**逐字**出现在素材里——这是抽取面唯一的防幻觉闸。
		extract: `[{"assertion":"机器甲的磁盘长期偏小","quote":"机器甲的磁盘长期偏小",` +
			`"event_id":"ev1","type":"fact","subject_name":"机器甲",` +
			`"importance":"high","confidence":"high"}]`,
		page:    `{"headline":"机器甲的磁盘长期偏小","body":"机器甲的磁盘长期偏小，需要清理。"}`,
		episode: `{"title":"一次磁盘排查","summary":"定位到磁盘偏小","decisions":["清理旧文件"]}`,
		digest:  "本周：定位并清理了机器甲的磁盘。",
	}

	svc, err := application.New(
		application.Config{Namespace: ns, Host: "it"},
		application.Deps{
			Store: s, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{},
			LLM: llm,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// 1 捕获
	cap, err := svc.Capture(ctx, application.CaptureInput{
		EventID: "ev1", Type: types.EventTurnEnd,
		Content: "机器甲的磁盘长期偏小，需要清理旧文件", SessionID: "s1", ThreadID: "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Accepted {
		t.Fatalf("捕获应被接受: %s", cap.SkipReason)
	}

	// 2 提炼（要模型）
	cands, err := svc.Extract(ctx, ns, "s1", []*types.RawEvent{cap.Event})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("应抽出 1 条候选，实得 %d 条", len(cands))
	}
	// 提炼不落库：调用方看过结果再决定，故先存候选。
	if err := s.SaveCandidate(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}

	// 3 归一：宿主先登记实体，否则晋升会送人工（送人工是默认姿态，不是 bug）。
	res, err := entity.New(s)
	if err != nil {
		t.Fatal(err)
	}
	ent, err := res.Register(ctx, ns, "机器甲", types.EntityType("service"), "那台研发机", types.DecidedUser)
	if err != nil {
		t.Fatal(err)
	}

	// 4 晋升 → 落库
	v, err := svc.Promote(ctx, ns, cands[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Decision) != "promote" {
		t.Fatalf("五道检查都应通过，判决实得 %+v", v)
	}
	atoms, err := s.ListAtoms(ctx, ns, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(atoms) != 1 {
		t.Fatalf("晋升后必须产生 1 张事实卡，实得 %d 张", len(atoms))
	}
	if atoms[0].EntityID != ent.ID {
		t.Fatalf("事实卡应归属实体 %s，实得 %q", ent.ID, atoms[0].EntityID)
	}
	// 候选必须落定，否则它会永远留在待判定队列里。
	got, err := s.GetCandidate(ctx, ns, cands[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.CandidatePromoted {
		t.Fatalf("候选状态应落定为 promoted，实得 %q", got.Status)
	}

	// 5 页面重生成（要模型）：晋升时页面已被标脏，重生成后清脏。
	//
	// ⚠️ 这里**不再手工标脏**：标脏是写入侧该做的事（晋升 → 标脏 → 重生成），
	// 手工标脏会让「接线有没有接上」这件事在用例里看不出来——
	// 而它恰恰是最容易漏的一环（画像于是永远停在旧版本，
	// 而宿主侧的 page.regen 只重生成脏页面，于是它安静地什么都不做）。
	//
	// 先断言晋升已经把该实体的页面标脏了：
	pgDirty, err := s.GetEntityPage(ctx, ns, ent.ID, "")
	if err != nil {
		t.Fatalf("晋升后应自建脏页面: %v", err)
	}
	if !pgDirty.Dirty {
		t.Fatal("晋升成事实后必须把该实体的画像标脏：事实变了摘要就该重算")
	}
	pg, err := page.New(s, llm, page.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pageres, err := pg.RegenerateDirty(ctx, ns, "")
	if err != nil {
		t.Fatal(err)
	}
	if pageres.Regenerated == 0 {
		t.Fatalf("脏页面应被重生成: %+v", pageres)
	}
	pgNow, err := s.GetEntityPage(ctx, ns, ent.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if pgNow.Dirty {
		t.Fatal("重生成成功后必须清脏")
	}
	if pgNow.Headline == "" {
		t.Fatal("重生成后必须有画像")
	}

	// 6 情节与周期摘要（要模型）：与事实卡是两条独立的路。
	epx, err := episode.New(s, llm, episode.DefaultPrompt)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := epx.ExtractFromSession(ctx, ns, "s1", []*types.RawEvent{cap.Event})
	if err != nil {
		t.Fatal(err)
	}
	if ep == nil || ep.Title == "" {
		t.Fatalf("情节应被抽出: %+v", ep)
	}
	dig, err := episode.NewDigester(s, llm)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := dig.Digest(ctx, ns, episode.PeriodWeek, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Markdown == "" {
		t.Fatal("周期摘要应有内容")
	}

	// 7 召回：刚记下的事实必须能被召回（这是主链的最后一环）。
	rr, err := svc.Recall(ctx, recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rr.Empty() {
		t.Fatal("刚晋升的事实必须能被召回")
	}

	// 8 生命周期：回收 / 合并 / 保留期三件套都能跑通。
	if _, err := lifecycle.Consolidate(ctx, s, ns); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Retention(ctx, s, ns); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.GC(ctx, s, ns, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// TestEntityMergeMovesOwnership 实体合并：事实卡跟着迁移。
//
// 归一错了极难归因（症状是「总是答非所问」），所以必须有一条**人走的修路**。
func TestEntityMergeMovesOwnership(t *testing.T) {
	ctx := context.Background()
	s := openFresh(t)
	res, err := entity.New(s)
	if err != nil {
		t.Fatal(err)
	}
	a, err := res.Register(ctx, ns, "机器甲", types.EntityType("service"), "", types.DecidedUser)
	if err != nil {
		t.Fatal(err)
	}
	b, err := res.Register(ctx, ns, "研发机03", types.EntityType("service"), "", types.DecidedUser)
	if err != nil {
		t.Fatal(err)
	}
	// 事实卡挂在 b 名下。
	atom := &types.AtomCard{
		ID: "a1", EntityID: b.ID, Assertion: "磁盘长期偏小", QuoteEventID: "ev1",
		Importance: types.ImportanceHigh, Confidence: types.ConfidenceMed,
		OccurredAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := s.SaveAtom(ctx, ns, atom); err != nil {
		t.Fatal(err)
	}

	merged, err := res.Merge(ctx, ns, a.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if merged.MovedAtoms != 1 {
		t.Fatalf("应迁移 1 张事实卡，实得 %d", merged.MovedAtoms)
	}
	// 迁移是**改归属**而不是复制一份——否则同一件事会挂在两个实体名下。
	got, err := s.GetAtom(ctx, ns, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EntityID != a.ID {
		t.Fatalf("事实卡应改归属到 %s，实得 %q", a.ID, got.EntityID)
	}
}

// TestMigrationPackageRoundTrip 导出 → 检查 → 导入。
//
// 迁移是**最容易出静默错误**的环节：包缺一半，导入「成功」了，
// 而宿主从此拥有一段残缺的记忆，记忆库不会告诉你它缺了什么。
// 故 doctor 与往返都要验。
func TestMigrationPackageRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openFresh(t)
	if err := s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "ev1", Content: "磁盘长期偏小", Timestamp: time.Now().UTC(),
		Type: types.EventManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小", QuoteEventID: "ev1",
		Importance: types.ImportanceHigh, Confidence: types.ConfidenceMed,
		OccurredAt: time.Now().UTC(), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	pkg := dir + "/it.hmpkg"
	man, err := portable.ExportFile(ctx, s, ns, pkg, portable.ExportOptions{Host: "it"})
	if err != nil {
		t.Fatal(err)
	}
	if man.Counts["atoms"] != 1 {
		t.Fatalf("导出应含 1 张事实卡，实得 %v", man.Counts)
	}
	rep, err := portable.Doctor(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("包应完整: %v", rep.Problems)
	}

	s2 := openFresh(t)
	imp, err := portable.Import(ctx, pkg, s2, ns)
	if err != nil {
		t.Fatal(err)
	}
	if imp.Counts["atoms"] != 1 {
		t.Fatalf("导入应含 1 张事实卡，实得 %v", imp.Counts)
	}
	back, err := s2.GetAtom(ctx, ns, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if back.Assertion != "磁盘长期偏小" {
		t.Fatalf("往返后正文失真: %q", back.Assertion)
	}
}

// TestScopedPromotionMarksItsOwnView 归属贯穿写入链：
// 捕获时定的归属一路带到事实卡，画像也按那个归属分视角标脏。
//
// 这条链上任何一环把归属丢掉，后果都是**内容泄漏**：
// 事实卡变成全局的（人人看得见），或画像变成全局的（一张所有人能读的摘要）。
// 而这两种失效在数据上完全看不出来——事实卡就是一条说得通的事实，
// 画像就是一段正常的文字。
func TestScopedPromotionMarksItsOwnView(t *testing.T) {
	const scope = types.Scope("team-a")
	ctx := context.Background()
	s := openFresh(t)
	llm := fakeLLM{
		extract: `[{"assertion":"机器甲的磁盘长期偏小","quote":"机器甲的磁盘长期偏小",` +
			`"event_id":"ev1","type":"fact","subject_name":"机器甲",` +
			`"importance":"high","confidence":"high"}]`,
	}
	svc, err := application.New(
		application.Config{Namespace: ns, Host: "it"},
		application.Deps{
			Store: s, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{}, LLM: llm,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	cap, err := svc.Capture(ctx, application.CaptureInput{
		EventID: "ev1", Type: types.EventTurnEnd, Scope: scope,
		Content: "机器甲的磁盘长期偏小，需要清理旧文件", SessionID: "s1", ThreadID: "t1",
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
	if cands[0].Scope != scope {
		t.Fatalf("候选应继承捕获时的归属，实得 %q", cands[0].Scope)
	}
	if err := s.SaveCandidate(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}
	res, err := entity.New(s)
	if err != nil {
		t.Fatal(err)
	}
	ent, err := res.Register(ctx, ns, "机器甲", types.EntityType("service"), "那台研发机", types.DecidedUser)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Promote(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}

	// 事实卡带着归属。
	atoms, err := s.ListAtoms(ctx, ns, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(atoms) != 1 || atoms[0].Scope != scope {
		t.Fatalf("事实卡应带归属 %q: %+v", scope, atoms)
	}
	// 画像按**那一归属**标脏。
	own, err := s.GetEntityPage(ctx, ns, ent.ID, scope)
	if err != nil {
		t.Fatalf("本归属的页面应被标脏自建: %v", err)
	}
	if !own.Dirty {
		t.Fatal("本归属的页面应处于脏态")
	}
	// 全局页**不该**被建出来：带归属的事实不属于全局视角。
	if _, err := s.GetEntityPage(ctx, ns, ent.ID, ""); err == nil {
		t.Fatal("带归属的事实不该把全局页建出来：那是所有人能读的那一页")
	}
}

// TestStatsCountEverything 计数面：看板与体检靠它。
func TestStatsCountEverything(t *testing.T) {
	ctx := context.Background()
	s := openFresh(t)
	if err := s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "ev1", Content: "磁盘长期偏小", Timestamp: time.Now().UTC(),
		Type: types.EventManual,
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.CountStats(ctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	if st.RawEvents != 1 {
		t.Fatalf("计数不对: %+v", st)
	}
	// 计数面要能被渲染（CLI 的 stats 命令直接吐它）。
	if _, err := json.Marshal(st); err != nil {
		t.Fatal(err)
	}
}
