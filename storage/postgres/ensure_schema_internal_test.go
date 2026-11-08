package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：需要**白盒**访问的 PG 用例（unexported 方法与 schema 初始化内部形态）。
// 对外行为断言在 parity 套件里；本文件补 parity 造不出/够不到的两条：
//
//   - 活跃实体淘汰的条件删除复核 pinned（#22）：黑盒造不出「SELECT 之后、
//     DELETE 之前被并发钉上」的确定形态；
//   - ensureSchema 的 advisory 锁串行化（#24）：锁被占用时 Open 必须等待。

// openInternal 每个用例开一份干净库（与 parity_test 的 openPG 同款形状：
// 建新库而不是清表，彻底隔离）。白盒用例拿到的 Store 可触内部方法。
func openInternal(t *testing.T) *Store {
	t.Helper()
	if !Configured() {
		t.Skipf("未配置 %s：PG 行为**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	dsn := Dsn()
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("连不上 PG: %v", err)
	}
	name := "am_internal_" + fmt.Sprint(time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		_ = admin.Close()
		t.Fatalf("建测试库失败: %v", err)
	}
	testDSN, err := WithDatabase(dsn, name)
	if err != nil {
		_ = admin.Close()
		t.Fatalf("拼测试库 DSN 失败: %v", err)
	}
	s, err := Open(testDSN)
	if err != nil {
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
		_ = admin.Close()
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
		_ = admin.Close()
	})
	return s
}

// TestEvictDeleteRechecksPinned 条件删除必须在 DELETE 时复核 pinned：
// 「SELECT 后才被钉上」的行删不掉，「未钉」的行照删（#22，与 SQLite 同款白盒）。
func TestEvictDeleteRechecksPinned(t *testing.T) {
	s := openInternal(t)
	ctx := context.Background()
	ns := store.Namespace("ns-wb")
	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	if err := s.UpsertActiveEntity(ctx, ns, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-pin", CanonicalName: "刚确认的", LastSeen: base,
	}); err != nil {
		t.Fatal(err)
	}
	// SELECT 拿到受害者之后、DELETE 执行之前，行被并发钉上。
	if err := s.UpsertActiveEntity(ctx, ns, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-pin", CanonicalName: "刚确认的",
		LastSeen: base, Pinned: true, PinTime: base,
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.deleteUnpinnedActiveEntity(ctx, ns, "th", "e-pin")
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
	// 解除钉住之后，条件删除恢复工作。
	if err := s.UpsertActiveEntity(ctx, ns, &types.ActiveEntity{
		ThreadID: "th", EntityID: "e-pin", CanonicalName: "刚确认的",
		LastSeen: base, Pinned: false,
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err = s.deleteUnpinnedActiveEntity(ctx, ns, "th", "e-pin"); err != nil {
		t.Fatal(err)
	} else if !deleted {
		t.Fatal("解除钉住后应能被条件删除删掉")
	}
}

// TestEnsureSchemaWaitsForAdvisoryLock advisory 锁被占用时 Open 必须在锁上等待，
// 锁释放后成功打开——多进程并发开库在 schema 初始化上串行化（#24）。
//
// 这条是确定性的：修复后 Open 在 pg_advisory_xact_lock 上阻塞，持锁事务
// 不回滚它就不可能返回；没有锁的话 Open 在空库上瞬间完成。
func TestEnsureSchemaWaitsForAdvisoryLock(t *testing.T) {
	if !Configured() {
		t.Skipf("未配置 %s：PG 行为**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	dsn := Dsn()

	// 持锁的对象是一份刚建好的干净库（Open 的 ensureSchema 会对它做完整初始化）。
	// 另一条连接上持住 advisory 锁（事务级，先不提交/回滚）。
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("连不上 PG: %v", err)
	}
	defer func() { _ = admin.Close() }()
	name := "am_lock_" + fmt.Sprint(time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	testDSN, err := WithDatabase(dsn, name)
	if err != nil {
		t.Fatalf("拼测试库 DSN 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
	})

	holder, err := sql.Open("postgres", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('agent-memory-schema'))`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		s2, err := Open(testDSN)
		if err == nil {
			err = s2.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("advisory 锁被占用时 Open 不该完成（ensureSchema 未在锁上串行化）: %v", err)
	case <-time.After(200 * time.Millisecond):
		// 还没返回 = 在等锁，正确。
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("锁释放后 Open 应成功: %v", err)
	}
}
