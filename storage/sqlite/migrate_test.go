package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/types"
)

// 本文件：schema 迁移的用例。
//
// # 为什么迁移要有用例
//
// 迁移是**一次性**的动作：写错了当场看不出来，要等下一次升级才炸——
// 而那一次升级往往发生在别人家的机器上。故它必须被用例钉住。
//
// # 夹具怎么造「旧形状的库」
//
// **不复制一份旧 DDL**：复制的那份会与真 DDL 漂移，于是用例绿着而真库炸着。
// 这里的做法是「按新代码建库 → 把新列去掉 → 把版本号改回 1」——
// 得到的库与旧版本建出来的库**形状相同**，且不会漂。

// TestMigrationAddsScopeColumn 旧形状的库（没有归属列）打开后要能升上来。
func TestMigrationAddsScopeColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// 先存一条老数据（归属为空 = 全局）。
	if err := s.SaveAtom(context.Background(), "ns", &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小需要清理旧文件",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		CreatedAt: legacyBase(), UpdatedAt: legacyBase(),
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 把它变回「v1 形状」：去掉归属列、去掉它的索引、版本号改回 1。
	dropScopeColumns(t, s)
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '1' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatalf("改版本号失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 重新打开：迁移必须把列加回来，且老数据还在。
	s2, err := OpenFile(path)
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
	// 新列要能用：升级之后写入的卡能带上归属。
	scoped := &types.AtomCard{
		ID: "a2", EntityID: "e1", Scope: "scope-a", Assertion: "内存占用偏高",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		CreatedAt: legacyBase(), UpdatedAt: legacyBase(),
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
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的
// （拷贝时带了旧版本号）——同一段迁移可能被跑第二次。
// 迁移跑第二遍而失败的话，**库就打不开了**，而那正是「升级一下记忆库」
// 这种日常操作最不该出的事。
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twice.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 把版本号改回 1（假装这次迁移没跑过），再打开一次。
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '1' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path); err != nil {
		t.Fatalf("第二次打开失败（迁移不幂等）: %v", err)
	}
}

// dropScopeColumns 把库变回「没有归属列」的旧形状。
func dropScopeColumns(t *testing.T, s *Store) {
	t.Helper()
	// 先删索引：SQLite 不许删「被索引引用」的列。
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS atoms_scope`,
		`DROP INDEX IF EXISTS raw_events_scope`,
		`ALTER TABLE atoms DROP COLUMN scope`,
		`ALTER TABLE raw_events DROP COLUMN scope`,
		`ALTER TABLE candidates DROP COLUMN scope`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
}

// legacyBase 夹具用的固定时刻。
func legacyBase() time.Time {
	return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
}
