package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：实体页面「按归属分视角」的行为契约。
//
// # 为什么这不是「多一个字段」
//
// 页面是事实卡的摘要，而事实卡已按归属隔离。摘要若不分视角，
// 等于把别家的事实揉进一张全局画像里——那是**内容泄漏**，不是「少取」：
// 画像是人读的一整段话，读者分不清哪句属于自己。
// 故归属进键：同一实体在不同归属下各有一页，互相取不到。
//
// 这份契约与 storage/parity 的跨后端断言是同一套口径；
// 本仓自己再钉一遍，是因为「通用件的验收标准是本仓自己在用」，
// 且存储层的失效形态（取到别家的画像）在库里完全看不出来。

// TestEntityPageScopeIsolation 同实体的多个视角互不串。
func TestEntityPageScopeIsolation(t *testing.T) {
	s, err := OpenMemory(t.Name())
	if err != nil {
		t.Fatalf("开内存库失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	const scopeA = types.Scope("scope-a")
	const scopeB = types.Scope("scope-b")

	for _, p := range []*types.EntityPage{
		{EntityID: "e1", Scope: scopeA, Headline: "a 视角", Body: "a 正文"},
		{EntityID: "e1", Scope: scopeB, Headline: "b 视角", Body: "b 正文"},
		{EntityID: "e1", Scope: "", Headline: "全局视角", Body: "全局正文"},
	} {
		if err := s.UpsertEntityPage(ctx, "ns", p); err != nil {
			t.Fatalf("写页面失败(scope=%q): %v", p.Scope, err)
		}
	}

	// 精确取键：各视角取回自己的那一页。
	for _, tc := range []struct {
		scope types.Scope
		want  string
	}{
		{scopeA, "a 视角"},
		{scopeB, "b 视角"},
		{"", "全局视角"},
	} {
		got, err := s.GetEntityPage(ctx, "ns", "e1", tc.scope)
		if err != nil {
			t.Fatalf("取页面失败(scope=%q): %v", tc.scope, err)
		}
		if got.Headline != tc.want {
			t.Fatalf("scope=%q 取到了别的视角的页面: %q", tc.scope, got.Headline)
		}
		if got.Scope != tc.scope {
			t.Fatalf("页面视角字段失真: 期望 %q 实得 %q", tc.scope, got.Scope)
		}
	}

	// 没建立过的视角取不到——取不到就是取不到，不回退成别的视角。
	if _, err := s.GetEntityPage(ctx, "ns", "e1", "scope-c"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("未建立的视角应取不到, 实得: %v", err)
	}

	// 跨视角全列（导包要完整）：三个视角都得在。
	all, err := s.ListEntityPages(ctx, "ns", 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[types.Scope]bool{}
	for _, p := range all {
		if p.EntityID == "e1" {
			seen[p.Scope] = true
		}
	}
	for _, sc := range []types.Scope{scopeA, scopeB, ""} {
		if !seen[sc] {
			t.Fatalf("跨视角全列漏了 scope=%q: %+v", sc, all)
		}
	}
}

// TestEntityPageDirtyPropagation 标脏的传播规则。
//
// 归属事实只让本视角的画像过时；全局事实出现在每个视角的画像里，
// 于是所有视角都过时——只标一页的后果是别家的画像停在旧版本，
// 而看板上「页面存在且是旧的」与「页面正常」长得一模一样。
func TestEntityPageDirtyPropagation(t *testing.T) {
	s, err := OpenMemory(t.Name())
	if err != nil {
		t.Fatalf("开内存库失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	const scopeA = types.Scope("scope-a")
	const scopeB = types.Scope("scope-b")

	for _, p := range []*types.EntityPage{
		{EntityID: "e1", Scope: scopeA, Headline: "旧"},
		{EntityID: "e1", Scope: scopeB, Headline: "旧"},
		{EntityID: "e1", Scope: "", Headline: "旧"},
	} {
		if err := s.UpsertEntityPage(ctx, "ns", p); err != nil {
			t.Fatalf("写页面失败: %v", err)
		}
	}

	// 归属事实 → 只标本视角那一页。
	if err := s.MarkEntityPageDirty(ctx, "ns", "e1", scopeA, legacyBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	for _, tc := range []struct {
		scope types.Scope
		want  int
	}{
		{scopeA, 1},
		{scopeB, 0},
		{"", 0},
	} {
		got, err := s.ListDirtyEntityPages(ctx, "ns", tc.scope, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tc.want {
			t.Fatalf("scope=%q 的脏页数应为 %d，实得 %d", tc.scope, tc.want, len(got))
		}
	}

	// 全局事实 → 所有视角都标脏。
	if err := s.MarkEntityPageDirty(ctx, "ns", "e1", "", legacyBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	for _, sc := range []types.Scope{scopeA, scopeB, ""} {
		got, err := s.ListDirtyEntityPages(ctx, "ns", sc, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("全局事实变了，scope=%q 的页面也该标脏，实得脏页 %d 条", sc, len(got))
		}
	}
}

// TestEntityPageDirtyCreatesPage 标脏时页面不存在要自建。
//
// 晋升先于首次生成是常态，报「页面不存在」会把一次正常写入变成错误。
func TestEntityPageDirtyCreatesPage(t *testing.T) {
	s, err := OpenMemory(t.Name())
	if err != nil {
		t.Fatalf("开内存库失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	const scopeA = types.Scope("scope-a")

	// 该视角没有页面 → 自建一条空脏页。
	if err := s.MarkEntityPageDirty(ctx, "ns", "e-new", scopeA, legacyBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	got, err := s.GetEntityPage(ctx, "ns", "e-new", scopeA)
	if err != nil {
		t.Fatalf("标脏应自建页面: %v", err)
	}
	if !got.Dirty {
		t.Fatal("自建的页面应处于脏态")
	}
	if got.Scope != scopeA {
		t.Fatalf("自建页面的视角失真: 期望 %q 实得 %q", scopeA, got.Scope)
	}

	// 该实体一行都没有时，全局事实也要能自建一条空脏页（视角为空）。
	if err := s.MarkEntityPageDirty(ctx, "ns", "e-new2", "", legacyBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	got2, err := s.GetEntityPage(ctx, "ns", "e-new2", "")
	if err != nil {
		t.Fatalf("标脏应自建页面: %v", err)
	}
	if !got2.Dirty {
		t.Fatal("自建的页面应处于脏态")
	}
}

// TestEntityPageRegenScoped 生成与失败都只落在本视角那一页。
//
// 生成结果写到 (entity_id, scope) 那一页：覆盖别家的页面等于替别人重写画像。
func TestEntityPageRegenScoped(t *testing.T) {
	s, err := OpenMemory(t.Name())
	if err != nil {
		t.Fatalf("开内存库失败: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	const scopeA = types.Scope("scope-a")
	const scopeB = types.Scope("scope-b")

	for _, p := range []*types.EntityPage{
		{EntityID: "e1", Scope: scopeA, Headline: "旧"},
		{EntityID: "e1", Scope: scopeB, Headline: "旧"},
	} {
		if err := s.UpsertEntityPage(ctx, "ns", p); err != nil {
			t.Fatalf("写页面失败: %v", err)
		}
	}

	// 生成成功：只改本视角那一页，且清脏清失败。
	if err := s.ApplyEntityPageRegen(ctx, "ns", "e1", scopeA, &types.EntityPage{
		Headline: "新", Body: "新正文",
	}, legacyBase()); err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	gotA, err := s.GetEntityPage(ctx, "ns", "e1", scopeA)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.Headline != "新" || gotA.Dirty || gotA.RegenFailure != "" {
		t.Fatalf("生成后状态失真: %+v", gotA)
	}
	gotB, err := s.GetEntityPage(ctx, "ns", "e1", scopeB)
	if err != nil {
		t.Fatal(err)
	}
	if gotB.Headline != "旧" {
		t.Fatalf("生成不该动别的视角的页面: %+v", gotB)
	}

	// 生成失败：只记在本视角那一页，且**保持脏**。
	if err := s.RecordEntityPageRegenFailure(ctx, "ns", "e1", scopeB, "模型超时", legacyBase()); err != nil {
		t.Fatalf("记失败失败: %v", err)
	}
	gotB, err = s.GetEntityPage(ctx, "ns", "e1", scopeB)
	if err != nil {
		t.Fatal(err)
	}
	if !gotB.Dirty {
		t.Fatal("重生成失败必须保持脏标记：清掉就永远不再进队列，而用户看到的页面与正常页面长得一样")
	}
	if gotB.RegenFailure != "模型超时" {
		t.Fatalf("失败原因没记上: %+v", gotB)
	}
	gotA, err = s.GetEntityPage(ctx, "ns", "e1", scopeA)
	if err != nil {
		t.Fatal(err)
	}
	if gotA.Dirty || gotA.RegenFailure != "" {
		t.Fatalf("别家的失败不该落到本视角的页面上: %+v", gotA)
	}
}
