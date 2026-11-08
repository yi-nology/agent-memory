package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：活跃实体淘汰的**白盒**用例（黑盒造不出「SELECT 之后、DELETE 之前
// 被并发钉上」的确定形态，见 parity 的 tActiveEntityEvictPinRace 注释）。
//
// 钉的缺陷（#22）：SELECT 排除 pinned、DELETE 却不复核——两步之间并发的
// UpsertActiveEntity 把实体置为 pinned（用户刚确认过的对象），DELETE 照删，
// 「那个」立刻指错人。修复是条件删除（pinned = 0 进 DELETE 谓词）。

var evictPinBase = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// TestEvictDeleteRechecksPinned 条件删除必须在 DELETE 时复核 pinned：
// 「SELECT 后才被钉上」的行删不掉，「未钉」的行照删。
func TestEvictDeleteRechecksPinned(t *testing.T) {
	s, err := OpenMemory("evict-pin-wb")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	ns := store.Namespace("ns-wb")

	// 形态一：行还在、未钉——条件删除照删。
	if err := s.UpsertActiveEntity(ctx, ns, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-unpinned", CanonicalName: "普通提及",
		LastSeen: evictPinBase,
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.deleteUnpinnedActiveEntity(ctx, ns, "th", "e-unpinned")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("未钉的行应被条件删除删掉")
	}

	// 形态二：SELECT 拿到受害者之后、DELETE 执行之前，行被并发钉上——
	// （确定地构造这个形态：先落未钉的行，再钉住它，再执行条件删除。）
	if err := s.UpsertActiveEntity(ctx, ns, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-pin", CanonicalName: "刚确认的",
		LastSeen: evictPinBase,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertActiveEntity(ctx, ns, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-pin", CanonicalName: "刚确认的",
		LastSeen: evictPinBase, Pinned: true, PinTime: evictPinBase,
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err = s.deleteUnpinnedActiveEntity(ctx, ns, "th", "e-pin")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("被并发钉住的行不得被条件删除删掉（DELETE 未复核 pinned）")
	}
	left, err := s.ListActiveEntities(ctx, ns, "th", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].EntityID != "e-pin" || !left[0].Pinned {
		t.Fatalf("钉住行必须原样保留: %+v", left)
	}

	// 形态三：解除钉住之后，条件删除恢复工作（不是「钉过就永不删」）。
	if err := s.UpsertActiveEntity(ctx, ns, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-pin", CanonicalName: "刚确认的",
		LastSeen: evictPinBase, Pinned: false,
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err = s.deleteUnpinnedActiveEntity(ctx, ns, "th", "e-pin"); err != nil {
		t.Fatal(err)
	} else if !deleted {
		t.Fatal("解除钉住后应能被条件删除删掉")
	}
}
