package cli_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// fakeLLM 返回固定 JSON 的假模型。
type fakeLLM struct{ out string }

func (f fakeLLM) Complete(context.Context, ports.LLMRequest) (string, error) { return f.out, nil }

// 本文件钉住记忆主链的完整性：捕获 → 提炼 → 晋升 → 落库。
//
// 这条链曾经断在最后一环：晋升的五道检查全过、返回 promote 判决，
// 却不产生事实卡、不落定候选、不增实体计数、不写审计条目——
// 候选永远停在 pending，召回永远捞不到东西，而每一段单独测都是绿的。
//
// 缺陷位置：pipeline/promotion/promotion.go 的 Promote —— 走到 DecisionPromote
// 那一行时，只返回一个判决就结束了：
//
//	return &Verdict{Decision: DecisionPromote, EntityID: entityID,
//	    Check: "dup", Reason: "通过全部检查"}, nil
//
// 全仓 SaveAtom 的调用点只有四处：merge（并入既有卡）、entity 合并、
// lifecycle 整合、portable 导入——**promote 路径不在其中**。
// UpdateCandidateStatus 也只有 merge 一处在调。
//
// 后果：候选永远停在 pending，事实卡一张都不产生。捕获→提炼→晋升→召回
// 这条主链在最后一环断掉，而每一环的单元测试都是绿的——因为没有一条用例
// 走完整条链。
//
// 佐证：Verdict.Atom 的文档写着「Decision 为 promote/merge 时有值」，
// 而 promote 分支从不给 Atom 赋值——注释描述的契约与实现不一致。

func TestPromoteMustPersistAtom(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.OpenMemory("promote-loop")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ns := store.Namespace("t")
	now := time.Now().UTC()

	// 宿主先登记实体（归一要能命中它，否则会走送人工）。
	if err := s.SaveEntity(ctx, ns, &types.Entity{
		ID: "e1", EntityType: types.EntityType("service"), CanonicalName: "机器甲", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	svc, err := application.New(
		application.Config{Namespace: ns, Host: "h"},
		application.Deps{
			Store:    s,
			Redactor: ports.PatternRedactor{},
			Bander:   ports.MemoryBand{},
			LLM: fakeLLM{out: `[{
				"assertion":"磁盘剩余空间为 2G",
				"quote":"磁盘剩余空间为 2G",
				"event_id":"ev1",
				"type":"fact",
				"subject_name":"机器甲",
				"importance":"high",
				"confidence":"high"}]`},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// 1 捕获
	cap, err := svc.Capture(ctx, application.CaptureInput{
		EventID: "ev1", Type: types.EventTurnEnd,
		Content: "磁盘剩余空间为 2G 需要清理", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cap.Accepted {
		t.Fatalf("捕获应被接受: %s", cap.SkipReason)
	}

	// 2 提炼
	cands, err := svc.Extract(ctx, ns, "s1", []*types.RawEvent{cap.Event})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("应抽出 1 条候选，实得 %d 条", len(cands))
	}
	// 提炼**不落库**（「落库由调用方在看过结果后决定」），故先存候选。
	// 晋升的第四步要改候选的状态，候选不在库里时整笔晋升会回滚。
	if err := s.SaveCandidate(ctx, ns, cands[0]); err != nil {
		t.Fatal(err)
	}

	// 3 晋升
	v, err := svc.Promote(ctx, ns, cands[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Decision) != "promote" {
		t.Fatalf("五道检查都应通过，判决实得 %+v", v)
	}

	// 4 落库——期望事实卡真的产生，候选也落定为 promoted。
	atoms, err := s.ListAtoms(ctx, ns, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(atoms) != 1 {
		t.Fatalf("晋升通过后必须产生 1 张事实卡，实得 %d 张（晋升路径不落库）", len(atoms))
	}
	if atoms[0].Assertion != "磁盘剩余空间为 2G" {
		t.Fatalf("事实卡正文不对: %q", atoms[0].Assertion)
	}
	if atoms[0].EntityID != "e1" {
		t.Fatalf("事实卡应归属实体 e1，实得 %q", atoms[0].EntityID)
	}
	// 候选状态也必须落定，否则它会永远留在待判定队列里。
	got, err := s.GetCandidate(ctx, ns, cands[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.CandidatePromoted {
		t.Fatalf("候选状态应落定为 promoted，实得 %q", got.Status)
	}
}

// 回滚断言：晋升的四步写必须原子。
//
// 候选不在库里时，「候选落定」那一步会失败——此时库里**不能**留下半成品
// （一张没有归属的孤儿事实卡）。这正是把四步放进同一个事务的理由。
func TestPromoteRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.OpenMemory("promote-rollback")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ns := store.Namespace("t")
	now := time.Now().UTC()
	if err := s.SaveEntity(ctx, ns, &types.Entity{
		ID: "e1", EntityType: types.EntityType("service"), CanonicalName: "机器甲", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	svc, err := application.New(
		application.Config{Namespace: ns, Host: "h"},
		application.Deps{
			Store: s, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{},
			LLM: fakeLLM{out: `[{
				"assertion":"磁盘剩余空间为 2G",
				"quote":"磁盘剩余空间为 2G",
				"event_id":"ev1",
				"type":"fact",
				"subject_name":"机器甲",
				"importance":"high",
				"confidence":"high"}]`},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	cap, err := svc.Capture(ctx, application.CaptureInput{
		EventID: "ev1", Type: types.EventTurnEnd,
		Content: "磁盘剩余空间为 2G 需要清理", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	cands, err := svc.Extract(ctx, ns, "s1", []*types.RawEvent{cap.Event})
	if err != nil {
		t.Fatal(err)
	}
	// 刻意**不存**候选：晋升的第四步会因此失败。
	if _, err := svc.Promote(ctx, ns, cands[0]); err == nil {
		t.Fatal("候选不在库里时晋升必须失败")
	}
	atoms, err := s.ListAtoms(ctx, ns, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(atoms) != 0 {
		t.Fatalf("晋升失败后不得留下半成品，实得 %d 张事实卡", len(atoms))
	}
}
