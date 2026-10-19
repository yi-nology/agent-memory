package parity

import (
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// ── 事实卡 ──

func tAtomRetiredExcluded(t *testing.T, s store.Store) {
	live := atom("a-live", "e1", "活跃事实", "ev1", types.ImportanceHigh)
	sup := atom("a-sup", "e1", "被推翻的事实", "ev1", types.ImportanceMed)
	dep := atom("a-dep", "e1", "被作废的事实", "ev1", types.ImportanceLow)
	for _, a := range []*types.AtomCard{live, sup, dep} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SupersedeAtom(bg(), nsA, "a-sup", "a-live"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeprecateAtom(bg(), nsA, "a-dep", base); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a-live" {
		var ids []string
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		t.Fatalf("只该召回活跃事实，实得 %v", ids)
	}
	// 不过滤时两条退役事实都还在（软退役不删）。
	all, err := s.ListAtoms(bg(), nsA, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("不过滤时应得 3 条，实得 %d", len(all))
	}
}

func tAtomSignature(t *testing.T, s store.Store) {
	_ = s.SaveAtom(bg(), nsA, atom("a1", "e1", "磁盘剩余 2G", "ev1", types.ImportanceMed))
	dup, err := s.FindAtomBySignature(bg(), nsA, atom("tmp", "e1", "  磁盘剩余 2G  ", "ev2", types.ImportanceMed).Signature())
	if err != nil {
		t.Fatalf("空白差异应折叠成同一签名: %v", err)
	}
	if dup.ID != "a1" {
		t.Fatalf("应命中 a1，实得 %s", dup.ID)
	}
	// 数量不同的事实必须分开——它们互相矛盾，该送审不是合并。
	if _, err := s.FindAtomBySignature(bg(), nsA,
		atom("tmp2", "e1", "磁盘剩余 3G", "ev3", types.ImportanceMed).Signature()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("数量不同的事实不该撞成同一签名，实得 %v", err)
	}
}

func tAtomSearch(t *testing.T, s store.Store) {
	_ = s.SaveAtom(bg(), nsA, atom("m1", "e1", "磁盘 剩余 2G", "ev1", types.ImportanceHigh))
	_ = s.SaveAtom(bg(), nsA, atom("m2", "e1", "内存 占用 偏高", "ev1", types.ImportanceHigh))
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "m1" {
		t.Fatalf("应只命中 m1，实得 %+v", hits)
	}
}

// 没有出处的记忆既不能审计也不能推翻——入库前就该拦下。
func tAtomValidation(t *testing.T, s store.Store) {
	bad := &types.AtomCard{
		ID: "a1", Assertion: "无出处", Confidence: types.ConfidenceHigh,
		Importance: types.ImportanceMed, OccurredAt: base, CreatedAt: base,
	}
	if err := s.SaveAtom(bg(), nsA, bad); !errors.Is(err, types.ErrAtomNoEvidence) {
		t.Fatalf("无出处的事实卡必须被拒，实得 %v", err)
	}
	// 冲突的退役两态同样在入库前拦。
	bad2 := atom("a2", "e1", "x", "ev1", types.ImportanceMed)
	bad2.SupersededBy = "a3"
	bad2.DeprecatedAt = base
	if err := s.SaveAtom(bg(), nsA, bad2); !errors.Is(err, types.ErrAtomDoubleRetired) {
		t.Fatalf("冲突退役态必须被拒，实得 %v", err)
	}
	// 空继任者会让事实永远查不到自己被谁推翻。
	if err := s.SupersedeAtom(bg(), nsA, "a2", ""); err == nil {
		t.Fatal("空继任者必须被拒")
	}
}

// SaveAtom 是 upsert：事实卡会被更新（并入补证据、退役改状态、合并迁归属）。
// 用纯 INSERT 的话，第二次保存就撞唯一键——而这个 bug 潜伏了很久，
// 只在「真的发生并入」时暴露。
// SaveAtom 是 upsert：事实卡会被更新（并入补证据、退役改状态、合并迁归属）。
func tAtomSaveIsUpsert(t *testing.T, s store.Store) {
	a := atom("a-up", "e1", "某条事实", "ev1", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatal(err)
	}
	a.RawEventIDs = []string{"ev1", "ev2"}
	a.UpdatedAt = base.Add(time.Hour)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatalf("重复保存应是更新而不是唯一键冲突: %v", err)
	}
	got, err := s.GetAtom(bg(), nsA, "a-up")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RawEventIDs) != 2 {
		t.Fatalf("重复保存未落更新: %+v", got.RawEventIDs)
	}
}

// 并列时的返回顺序必须确定。
//
// 重要度与发生时刻都相同的两张卡，若没有稳定序，「同签名合并时保留哪一张」
// 就随遍历顺序变——同一批数据合并两次得到不同结果。
func tAtomOrderIsDeterministic(t *testing.T, s store.Store) {
	for _, id := range []string{"d3", "d1", "d2"} {
		if err := s.SaveAtom(bg(), nsA, atom(id, "e1", "并列事实 "+id, "ev1", types.ImportanceMed)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatal("同一次查询返回条数不该变")
		}
		for j := range again {
			if again[j].ID != first[j].ID {
				t.Fatalf("并列时的返回顺序不确定: 第 %d 位 %s vs %s", j, first[j].ID, again[j].ID)
			}
		}
	}
}

func tAtomRetiredStillSearchable(t *testing.T, s store.Store) {
	_ = s.SaveRaw(bg(), nsA, &types.RawEvent{
		ID: "ev1", Timestamp: base, Type: types.EventManual, Content: "磁盘 曾经很紧张",
	})
	_ = s.SaveAtom(bg(), nsA, atom("a1", "e1", "磁盘 曾经很紧张", "ev1", types.ImportanceHigh))
	if err := s.DeprecateAtom(bg(), nsA, "a1", base); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !hits[0].SoftRetired() {
		t.Fatalf("退役事实必须仍可搜到（用于回答「当初为何这么判」）: %+v", hits)
	}
}
