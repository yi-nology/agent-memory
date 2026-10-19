package promotion_test

import (
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/promotion"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件钉住晋升的**落库面**：五道检查全过之后，四样东西必须真的写下去。
//
// 这条曾经整段缺失：Promote 走到最后一个分支就返回了一个 promote 判决，
// 不产生事实卡、不落定候选、不增实体计数、不写审计条目。
// 于是记忆系统的主写路径不成立——捕获与提炼都正常，判定也正常，
// 但**没有事实被记下来**，而召回永远捞不到东西。
//
// 每一段单独测都是绿的：晋升的用例断言的是「判决正确」，
// 不是「判决之后库里多了东西」。

func promotedStore(t *testing.T) (*fakeStore, *promotion.Promoter) {
	t.Helper()
	f := newFake()
	f.raws["e1"] = &types.RawEvent{
		ID: "e1", Content: "磁盘剩余空间为 2G",
		Timestamp: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC),
		Provenance: types.Provenance{
			Origin: types.OriginTool, Host: "h1", Tool: "df",
		},
	}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	return f, newPromoter(t, f, nil)
}

// 四步写全都要落地：事实入库 + 实体计数 + 候选落定 + 审计条目。
func TestPromoteWritesAllFourSteps(t *testing.T) {
	f, p := promotedStore(t)

	v, err := p.Promote(ctx2(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionPromote {
		t.Fatalf("五道检查都应通过，实得 %+v", v)
	}
	// 1 事实入库
	if len(f.savedA) != 1 {
		t.Fatalf("应产生 1 张事实卡，实得 %d 张", len(f.savedA))
	}
	atom := f.savedA[0]
	// 2 实体计数
	if f.counts["e1"] != 1 {
		t.Fatalf("实体计数应 +1，实得 %d", f.counts["e1"])
	}
	// 3 候选落定
	if f.status["c1"] != types.CandidatePromoted {
		t.Fatalf("候选应落定为 promoted，实得 %q", f.status["c1"])
	}
	// 4 审计条目
	if len(f.journal) != 1 {
		t.Fatalf("应写 1 条审计条目，实得 %d 条", len(f.journal))
	}
	if f.journal[0].Kind != types.JournalPromote {
		t.Fatalf("审计条目类型应��� promote，实得 %q", f.journal[0].Kind)
	}

	// 判决里必须带上那张事实卡（Verdict.Atom 的文档就是这么写的）。
	if v.Atom == nil {
		t.Fatal("晋升判决必须带上事实卡：Verdict.Atom 的契约是 promote/merge 时有值")
	}
	if v.Atom.ID != atom.ID {
		t.Fatalf("判决里的事实卡与落库的那张不是同一张: %q vs %q", v.Atom.ID, atom.ID)
	}
	if v.EntityID != "e1" {
		t.Fatalf("判决应带上归一后的实体，实得 %q", v.EntityID)
	}
}

// 事实卡的内容要接得住审计链：出处、证据、来源凭证、发生时刻。
func TestPromoteAtomCarriesEvidenceProvenance(t *testing.T) {
	f, p := promotedStore(t)

	if _, err := p.Promote(ctx2(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲")); err != nil {
		t.Fatal(err)
	}
	atom := f.savedA[0]
	if atom.QuoteEventID != "e1" {
		t.Fatalf("事实卡必须指出处事件，实得 %q", atom.QuoteEventID)
	}
	if len(atom.RawEventIDs) == 0 {
		t.Fatal("事实卡必须带证据事件")
	}
	if atom.CandidateID != "c1" {
		t.Fatalf("事实卡必须记着它晋升自哪条候选，实得 %q", atom.CandidateID)
	}
	// 来源凭证取自**证据事件**：审计链要能从事实回到那条事件。
	if atom.Provenance.Origin != types.OriginTool || atom.Provenance.Tool != "df" {
		t.Fatalf("事实卡的来源凭证应取自证据事件，实得 %+v", atom.Provenance)
	}
	// 发生时刻是**事情发生的时刻**（证据事件的时刻），不是写入时刻。
	want := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	if !atom.OccurredAt.Equal(want) {
		t.Fatalf("发生时刻应取证据事件的时刻，实得 %v", atom.OccurredAt)
	}
}

// 同一条候选被重复晋升时不得凭空多出一张事实卡。
//
// 事实卡 ID 由候选 ID 派生，配合存储层的 upsert 语义即幂等。
func TestPromoteIsIdempotentForSameCandidate(t *testing.T) {
	f, p := promotedStore(t)
	c := cand("磁盘剩余空间为 2G", "e1", "机器甲")

	if _, err := p.Promote(ctx2(), ns, c); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Promote(ctx2(), ns, c); err != nil {
		t.Fatal(err)
	}
	if len(f.savedA) != 2 {
		t.Fatalf("两次晋升应写两次同一张卡，实得 %d 次", len(f.savedA))
	}
	if f.savedA[0].ID != f.savedA[1].ID {
		t.Fatalf("同一条候选两次晋升应落到同一张卡: %q vs %q", f.savedA[0].ID, f.savedA[1].ID)
	}
}

// 并入既有事实时也要留痕（审计面：记忆改写自己就得记下来）。
func TestMergeAlsoWritesJournal(t *testing.T) {
	f, p := promotedStore(t)
	existing := &types.AtomCard{
		ID: "a-old", EntityID: "e1", Assertion: "磁盘剩余空间为 2G",
		QuoteEventID: "e1", Importance: types.ImportanceMed,
		Confidence: types.ConfidenceMed, CreatedAt: time.Now().UTC(),
	}
	f.atoms = append(f.atoms, existing)

	v, err := p.Promote(ctx2(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionMerge {
		t.Fatalf("同签名应并入既有事实，实得 %+v", v)
	}
	if f.status["c1"] != types.CandidateMerged {
		t.Fatalf("候选应落定为 merged，实得 %q", f.status["c1"])
	}
	if len(f.journal) != 1 || f.journal[0].Kind != types.JournalMerge {
		t.Fatalf("并入也要写审计条目，实得 %+v", f.journal)
	}
	// 并入不新增事实：实体计数不该变。
	if f.counts["e1"] != 0 {
		t.Fatalf("并入既有事实不新增计数，实得 %d", f.counts["e1"])
	}
}

// 存储层若拒绝落库，晋升必须以错误结束，不得返回「已晋升」的判决。
func TestPromotePropagatesWriteFailure(t *testing.T) {
	f, p := promotedStore(t)
	f.failSave = true

	v, err := p.Promote(ctx2(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲"))
	if err == nil {
		t.Fatalf("落库失败必须上抛，实得判决 %+v", v)
	}
	if v != nil {
		t.Fatalf("落库失败不得给出判决: %+v", v)
	}
}
