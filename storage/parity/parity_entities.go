package parity

import (
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：实体与活跃实体栈的跨后端断言。
// 从 parity.go 拆出（文件行数门禁）：按表分文件，与各后端
// 「每张表一个内嵌窄结构」的切法同构。

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

// 钉住项不改变「最近提及倒序」的**读取顺序**。
//
// 与 tActiveEntityPin（那条测的是**淘汰**）刻意分开：钉住的语义是
// 「不让刚确认过的对象滑出栈」，不是「当前话题优先指它」——
// 读取序里 pinned 优先的话，只要栈里有钉住项，代词「那个」就恒定
// 指向三天前提及的那个，而不是当前正在聊的对象（issue #33）。
func tActiveEntityPinnedKeepsOrder(t *testing.T, s store.Store) {
	_ = s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th", EntityID: "ent-pinned-old", LastSeen: base.Add(-72 * time.Hour),
		Pinned: true, PinTime: base.Add(-72 * time.Hour),
	})
	_ = s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th", EntityID: "ent-current", LastSeen: base,
	})
	got, err := s.ListActiveEntities(bg(), nsA, "th", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应有两个实体: %+v", got)
	}
	if got[0].EntityID != "ent-current" {
		t.Fatalf("读取序必须按最近提及倒序：栈顶应是当前话题，实得 %q", got[0].EntityID)
	}
	if !got[1].Pinned {
		t.Fatalf("钉住位仍应随行返回（淘汰语义要用它），实得 %+v", got[1])
	}
}
