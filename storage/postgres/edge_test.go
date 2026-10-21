package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：关系边存取的后端专属用例（与 SQLite 后端同一套语义、两份实现）。
//
// 跨后端的**行为**由 storage/parity 钉住（两个后端跑同一份断言）；
// 这里只钉**后端自己**的形状与边缘：幂等 upsert、缺失即不存在、
// 空来源不许清理。
//
// ⚠️ 这些用例对着**真实服务器**跑：内存实现测不出「约束没建」「类型映射错」
// 这类问题，而它们恰恰是换后端时最常见的故障。

// TestUpsertEdgeReplacesRow 同一标识的边再写一次是**替换**，不是新增。
//
// 投影层重算一条边时给的是新值，而边没有「合并」语义。
func TestUpsertEdgeReplacesRow(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 行为**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	ctx := context.Background()

	created := pgBase()
	if err := s.UpsertEdge(ctx, "ns", &types.EntityEdge{
		ID: "e1", FromEntityID: "a", ToEntityID: "b", Relation: "关系甲",
		Weight: 2, CreatedAt: created,
	}); err != nil {
		t.Fatalf("写边失败: %v", err)
	}
	if err := s.UpsertEdge(ctx, "ns", &types.EntityEdge{
		ID: "e1", FromEntityID: "a", ToEntityID: "c", Relation: "关系乙",
		Weight: 3, CreatedAt: created.Add(time.Hour),
	}); err != nil {
		t.Fatalf("覆盖写边失败: %v", err)
	}

	got, err := s.GetEdge(ctx, "ns", "e1")
	if err != nil {
		t.Fatalf("读边失败: %v", err)
	}
	if got.ToEntityID != "c" || got.Relation != "关系乙" || got.Weight != 3 {
		t.Fatalf("覆盖写没生效: %+v", got)
	}
	// created_at 是**首次**落库的时刻：重投影不改变「这条边存在多久了」。
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("建边时刻不该被覆盖写改掉，实得 %v", got.CreatedAt)
	}

	all, err := s.ListEdges(ctx, "ns", store.EdgeFilter{})
	if err != nil {
		t.Fatalf("列边失败: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("同一标识应只有一条边，实得 %d 条", len(all))
	}
}

// TestGetEdgeMissing 取不到就是取不到。
//
// 消费方一律 errors.Is 判 store.ErrNotFound——让底层错误漏到上层，
// 就等于逼着每个调用点各写一次判断。
func TestGetEdgeMissing(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 行为**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	if _, err := s.GetEdge(context.Background(), "ns", "不存在"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("缺失的边应判 ErrNotFound，实得 %v", err)
	}
}

// TestDeleteEdgesBySourceRejectsEmptySource 空来源必须被拒，且一条都不能删。
//
// 空来源会命中「没有溯源的边」（source_atom_id 为空的那批），
// 一次清理就把它们全删了——那是不可逆的数据损失。
func TestDeleteEdgesBySourceRejectsEmptySource(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 行为**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	ctx := context.Background()

	for _, e := range []*types.EntityEdge{
		{ID: "e1", FromEntityID: "a", ToEntityID: "b", Relation: "关系甲", CreatedAt: pgBase()},
		{ID: "e2", FromEntityID: "b", ToEntityID: "c", Relation: "关系甲", CreatedAt: pgBase()},
		{ID: "e3", FromEntityID: "c", ToEntityID: "d", Relation: "关系甲",
			SourceAtomID: "a1", CreatedAt: pgBase()},
	} {
		if err := s.UpsertEdge(ctx, "ns", e); err != nil {
			t.Fatalf("写边失败(%s): %v", e.ID, err)
		}
	}

	if err := s.DeleteEdgesBySource(ctx, "ns", ""); err == nil {
		t.Fatal("空来源必须被拒：它会误删没有溯源的边")
	}
	all, err := s.ListEdges(ctx, "ns", store.EdgeFilter{})
	if err != nil {
		t.Fatalf("列边失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("被拒的清理不该删掉任何边，实得 %d 条", len(all))
	}
}

// TestEdgeNamespaceIsolation 命名空间之间互不可见。
//
// 一个库服务多个记忆库时，隔离是最容易在「忘了加 WHERE 子句」处破的。
func TestEdgeNamespaceIsolation(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 行为**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	ctx := context.Background()

	if err := s.UpsertEdge(ctx, "ns-a", &types.EntityEdge{
		ID: "e1", FromEntityID: "a", ToEntityID: "b", Relation: "关系甲", CreatedAt: pgBase(),
	}); err != nil {
		t.Fatalf("写边失败: %v", err)
	}
	if _, err := s.GetEdge(ctx, "ns-b", "e1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("B 库读到了 A 库的边: %v", err)
	}
	got, err := s.ListEdges(ctx, "ns-b", store.EdgeFilter{})
	if err != nil {
		t.Fatalf("列边失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("B 库不该列出任何边，实得 %d 条", len(got))
	}
}
