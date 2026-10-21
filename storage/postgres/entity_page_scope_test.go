package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：实体页面「按归属分视角」的用例。
//
// 为什么单独钉：页面是事实的摘要，而摘要是**人读的一整段话**——
// 读者分不清哪句属于自己。故「取到别家的视角」不是「少取一条」，
// 而是内容泄漏；它必须由存储层钉死，而不是靠召回层事后过滤。

// TestEntityPageScopeIsolation 同实体的不同视角各有一页，互不串味。
func TestEntityPageScopeIsolation(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 页面归属路径**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	ctx := context.Background()

	const (
		scopeA = types.Scope("scope-a")
		scopeB = types.Scope("scope-b")
	)
	for _, p := range []*types.EntityPage{
		{EntityID: "e1", Scope: scopeA, Headline: "a 视角"},
		{EntityID: "e1", Scope: scopeB, Headline: "b 视角"},
		{EntityID: "e1", Scope: "", Headline: "全局视角"},
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

	// 没建立过的视角取不到（取不到就是取不到，不回退成别的视角）。
	if _, err := s.GetEntityPage(ctx, "ns", "e1", types.Scope("scope-c")); !errors.Is(err, store.ErrNotFound) {
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
	for _, want := range []types.Scope{scopeA, scopeB, ""} {
		if !seen[want] {
			t.Fatalf("跨视角全列漏了 scope=%q 的页面", want)
		}
	}
}

// TestEntityPageDirtyScopePropagation 标脏的传播规则。
//
// 归属事实只标本视角；全局事实出现在每个视角的画像里，故标**所有**视角。
// 只标一页的后果不是「少了一次重生成」，而是别家的画像停在旧版本——
// 而看板上「页面存在且是旧的」与「页面正常」长得一模一样。
func TestEntityPageDirtyScopePropagation(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 页面归属路径**未验证**", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	ctx := context.Background()

	const scopeA = types.Scope("scope-a")
	for _, p := range []*types.EntityPage{
		{EntityID: "e1", Scope: scopeA, Headline: "a 视角"},
		{EntityID: "e1", Scope: "", Headline: "全局视角"},
	} {
		if err := s.UpsertEntityPage(ctx, "ns", p); err != nil {
			t.Fatalf("写页面失败: %v", err)
		}
	}

	// 归属事实 → 只标该视角。
	if err := s.MarkEntityPageDirty(ctx, "ns", "e1", scopeA, pgBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	if g := mustPage(t, s, "ns", "e1", scopeA); !g.Dirty {
		t.Fatal("归属事实应把本视角的页面标脏")
	}
	if g := mustPage(t, s, "ns", "e1", ""); g.Dirty {
		t.Fatal("归属事实不该把别的视角标脏")
	}
	// 脏页清单也只列本视角。
	dirty, err := s.ListDirtyEntityPages(ctx, "ns", scopeA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0].Scope != scopeA {
		t.Fatalf("脏页清单失真: %+v", dirty)
	}
	if dirty, err = s.ListDirtyEntityPages(ctx, "ns", "", 10); err != nil {
		t.Fatal(err)
	} else if len(dirty) != 0 {
		t.Fatalf("全局视角不该列到归属视角的脏页: %+v", dirty)
	}

	// 全局事实 → 所有视角都标脏。
	if err := s.MarkEntityPageDirty(ctx, "ns", "e1", "", pgBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	for _, sc := range []types.Scope{scopeA, ""} {
		if g := mustPage(t, s, "ns", "e1", sc); !g.Dirty {
			t.Fatalf("全局事实变了，scope=%q 的页面也该标脏", sc)
		}
	}
}

// TestEntityPageDirtyCreatesPage 标脏时页面还不存在 → 自建一条空脏页。
//
// 晋升先于首次生成是常态，报「页面不存在」会把一次正常写入变成错误。
// 自建的那一条要落在**该归属**上：标 scope-a 的脏却建出全局页，
// 等于把「谁的事实变了」记错地方。
func TestEntityPageDirtyCreatesPage(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 页面归属路径**未验证**", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	ctx := context.Background()

	const scopeA = types.Scope("scope-a")
	if err := s.MarkEntityPageDirty(ctx, "ns", "e-new", scopeA, pgBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	got, err := s.GetEntityPage(ctx, "ns", "e-new", scopeA)
	if err != nil {
		t.Fatalf("标脏应自建页面: %v", err)
	}
	if !got.Dirty || got.Scope != scopeA {
		t.Fatalf("自建的脏页形状不对: %+v", got)
	}
	if _, err := s.GetEntityPage(ctx, "ns", "e-new", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("标归属的脏不该建出全局页, 实得: %v", err)
	}

	// 全局事实落在一个还没有任何页面的实体上 → 自建全局视角的空脏页。
	if err := s.MarkEntityPageDirty(ctx, "ns", "e-new2", "", pgBase()); err != nil {
		t.Fatalf("标脏失败: %v", err)
	}
	got2, err := s.GetEntityPage(ctx, "ns", "e-new2", "")
	if err != nil {
		t.Fatalf("标脏应自建页面: %v", err)
	}
	if !got2.Dirty || got2.Scope != "" {
		t.Fatalf("自建的全局脏页形状不对: %+v", got2)
	}
}

// TestEntityPageRegenTargetsScope 重生成写回哪一页、失败留不留脏。
//
// 失败仍要保持脏标记：清掉它会让这条页面永远不再进重生成队列，
// 而用户看到的是「页面存在、内容过时」——与「页面正常」长得一模一样。
func TestEntityPageRegenTargetsScope(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 页面归属路径**未验证**", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	ctx := context.Background()

	const scopeA = types.Scope("scope-a")
	for _, p := range []*types.EntityPage{
		{EntityID: "e1", Scope: scopeA, Headline: "旧"},
		{EntityID: "e1", Scope: "", Headline: "旧"},
	} {
		if err := s.UpsertEntityPage(ctx, "ns", p); err != nil {
			t.Fatalf("写页面失败: %v", err)
		}
	}

	// 失败：落在该视角那一行，且**保持脏**。
	if err := s.RecordEntityPageRegenFailure(ctx, "ns", "e1", scopeA, "模型超时", pgBase()); err != nil {
		t.Fatalf("记失败失败: %v", err)
	}
	got := mustPage(t, s, "ns", "e1", scopeA)
	if !got.Dirty || got.RegenFailure != "模型超时" {
		t.Fatalf("失败后该视角的页面形状不对: %+v", got)
	}
	if other := mustPage(t, s, "ns", "e1", ""); other.Dirty || other.RegenFailure != "" {
		t.Fatalf("失败不该落到别的视角: %+v", other)
	}

	// 成功：落正文、清脏、清失败原因。
	if err := s.ApplyEntityPageRegen(ctx, "ns", "e1", scopeA, &types.EntityPage{
		Headline: "新", Body: "正文", AtomIDs: []string{"a1"},
	}, pgBase()); err != nil {
		t.Fatalf("写回生成结果失败: %v", err)
	}
	got = mustPage(t, s, "ns", "e1", scopeA)
	if got.Dirty || got.RegenFailure != "" || got.Headline != "新" {
		t.Fatalf("生成成功后形状不对: %+v", got)
	}
	if got.Scope != scopeA {
		t.Fatalf("写回后视角失真: %q", got.Scope)
	}
	if other := mustPage(t, s, "ns", "e1", ""); other.Headline != "旧" {
		t.Fatalf("写回不该动别的视角: %+v", other)
	}
}

// mustPage 取一页，取不到就当场失败（夹具用）。
func mustPage(t *testing.T, s *Store, ns store.Namespace, entityID string, scope types.Scope) *types.EntityPage {
	t.Helper()
	p, err := s.GetEntityPage(context.Background(), ns, entityID, scope)
	if err != nil {
		t.Fatalf("取页面失败(entity=%q scope=%q): %v", entityID, scope, err)
	}
	return p
}
