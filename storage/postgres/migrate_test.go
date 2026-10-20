package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/types"
)

// 本文件：schema 迁移的用例（与 SQLite 后端同一套机制、两份实现）。
//
// 理由见 sqlite/migrate_test.go：迁移是**一次性**的动作，写错了当场看不出来。

// TestMigrationAddsScopeColumn 旧形状的库（没有归属列）打开后要能升上来。
func TestMigrationAddsScopeColumn(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	if err := s.SaveAtom(context.Background(), "ns", &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小需要清理旧文件",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		CreatedAt: pgBase(), UpdatedAt: pgBase(),
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 变回「v1 形状」：去掉归属列、版本号改回 1。
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS atoms_scope`,
		`DROP INDEX IF EXISTS raw_events_scope`,
		`ALTER TABLE atoms DROP COLUMN scope`,
		`ALTER TABLE raw_events DROP COLUMN scope`,
		`ALTER TABLE candidates DROP COLUMN scope`,
		`UPDATE meta SET value = '1' WHERE namespace = '' AND key = 'schema_version'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 重新打开：迁移必须把列加回来，且老数据还在。
	s2, err := Open(tempDSN(t, dsn, dbName))
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.GetAtom(context.Background(), "ns", "a1")
	if err != nil {
		t.Fatalf("读旧数据失败: %v", err)
	}
	if got.Assertion != "磁盘长期偏小需要清理旧文件" {
		t.Fatalf("旧数据读出来变了: %+v", got)
	}
	if got.Scope != "" {
		t.Fatalf("老数据的归属应是全局（空），实得 %q", got.Scope)
	}
	scoped := &types.AtomCard{
		ID: "a2", EntityID: "e1", Scope: "scope-a", Assertion: "内存占用偏高",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		CreatedAt: pgBase(), UpdatedAt: pgBase(),
	}
	if err := s2.SaveAtom(context.Background(), "ns", scoped); err != nil {
		t.Fatalf("升级后写入失败: %v", err)
	}
	back, err := s2.GetAtom(context.Background(), "ns", "a2")
	if err != nil {
		t.Fatal(err)
	}
	if back.Scope != "scope-a" {
		t.Fatalf("归属没存上: %q", back.Scope)
	}
}

// TestMigrationIsIdempotent 迁移跑第二遍不报错。
func TestMigrationIsIdempotent(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '1' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(tempDSN(t, dsn, dbName))
	if err != nil {
		t.Fatalf("第二次打开失败（迁移不幂等）: %v", err)
	}
	_ = s2.Close()
}

// ── 夹具 ──

// openTempDB 造一个干净的临时库（返回库名，删库时要用）。
func openTempDB(t *testing.T, dsn string) (*Store, string) {
	t.Helper()
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("连不上 PG: %v", err)
	}
	name := "am_migrate_" + fmt.Sprint(time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		_ = admin.Close()
		t.Fatalf("建临时库失败: %v", err)
	}
	_ = admin.Close()
	s, err := Open(tempDSN(t, dsn, name))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	return s, name
}

// tempDSN 拼临时库的 DSN。
func tempDSN(t *testing.T, dsn, name string) string {
	t.Helper()
	out, err := WithDatabase(dsn, name)
	if err != nil {
		t.Fatalf("拼 DSN 失败: %v", err)
	}
	return out
}

// dropTempDB 删掉临时库。
func dropTempDB(t *testing.T, dsn, name string) {
	t.Helper()
	if name == "" {
		return
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		return
	}
	defer func() { _ = admin.Close() }()
	_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
}

// pgBase 夹具用的固定时刻。
func pgBase() time.Time {
	return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
}
