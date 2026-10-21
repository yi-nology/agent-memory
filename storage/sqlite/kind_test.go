package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yi-nology/agent-memory/types"
)

// 本文件：事实卡**时效类别**列（kind）的后端专属用例。
//
// # 为什么共享断言之外还要后端专属用例
//
// parity 那组断言守的是**跨后端行为一致**（类别往返、事件窗口的判据）。
// 而「老库升级」这件事**没有跨后端可比性**：两个后端的加列语法不同，
// 且各自都可能单独坏掉（SQLite 不许删被索引引用的列，
// 而列序只能由 ALTER 追加——这两条都只有在本后端才成立）。
// 故迁移路径各写一份。

// TestMigrationAtomKind 旧形状的库（没有时效类别列）打开后要能升上来。
//
// # 夹具怎么造「旧形状的库」
//
// **不复制一份旧 DDL**：复制的那份会与真 DDL 漂移，于是用例绿着而真库炸着。
// 这里的做法是「按新代码建库 → 把新列去掉 → 把版本号改回 6」——
// 得到的库与 v6 建出来的库形状相同，且不会随真 DDL 变化而失效。
func TestMigrationAtomKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kind.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// 先存一条老数据（类别未设 = 默认 = 长期经验）。
	if err := s.SaveAtom(context.Background(), "ns", &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小需要清理旧文件",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		OccurredAt: legacyBase(), CreatedAt: legacyBase(), UpdatedAt: legacyBase(),
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 变回「v6 形状」：去掉类别列与它的索引、版本号改回 6。
	dropKindShape(t, s)
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
	// 老数据的类别必须是**零值**（= 默认 = 长期经验）：
	// 零值落在「不淘汰」那一侧，升级不该让既有事实开始被淘汰
	// （那正是「升级一次丢一批老事实」的形状）。
	if got.Kind != types.FactDefault {
		t.Fatalf("老数据的时效类别应是零值（默认），实得 %q", got.Kind)
	}
	if !got.Kind.Durable() {
		t.Fatal("零值类别必须落在「不因时间失效」那一侧")
	}

	// 新列要能用：升级之后写入的卡能带上类别。
	ev := &types.AtomCard{
		ID: "a2", EntityID: "e1", Assertion: "昨天重启过一次",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		Kind:       types.FactEvent,
		OccurredAt: legacyBase(), CreatedAt: legacyBase(), UpdatedAt: legacyBase(),
	}
	if err := s2.SaveAtom(context.Background(), "ns", ev); err != nil {
		t.Fatalf("升级后写入失败: %v", err)
	}
	back, err := s2.GetAtom(context.Background(), "ns", "a2")
	if err != nil {
		t.Fatal(err)
	}
	if back.Kind != types.FactEvent {
		t.Fatalf("时效类别没存上: %q", back.Kind)
	}

	// 索引归迁移管：老库升级后必须真的有它（建表脚本里没有这一句）。
	if !indexExists(t, s2.db, "atoms_kind") {
		t.Fatal("升级后事实卡的类别索引没建出来")
	}

	// 迁移后的形状必须与新建的库一样：两条路（建表脚本 / 迁移）
	// 落在同一个形状上，否则「有的库能取到、有的库取不到」。
	fresh, err := OpenMemory("fresh-kind-" + t.Name())
	if err != nil {
		t.Fatalf("开新库失败: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	if !slices.Equal(tableShape(t, fresh.db, "atoms"), tableShape(t, s2.db, "atoms")) {
		t.Fatalf("atoms 的形状迁移后与新建的库不一致:\n新建: %v\n迁移后: %v",
			tableShape(t, fresh.db, "atoms"), tableShape(t, s2.db, "atoms"))
	}
}

// TestMigrationAtomKindIsIdempotent 迁移跑第二遍不报错。
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的
// ——同一段迁移可能被跑第二次，而迁移跑第二遍而失败的话
// **库就打不开了**，而那正是「升级一下记忆库」这种日常操作最不该出的事。
func TestMigrationAtomKindIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kind-twice.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(context.Background(), "ns", &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		OccurredAt: legacyBase(), CreatedAt: legacyBase(), UpdatedAt: legacyBase(),
	}); err != nil {
		t.Fatal(err)
	}
	// 假装这段迁移没跑过（版本号被手工改回 6）。
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '6' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 第二遍：列与索引都已经是新形状，迁移必须认出来并跳过。
	s2, err := OpenFile(path)
	if err != nil {
		t.Fatalf("第二次打开失败（迁移不幂等）: %v", err)
	}
	defer func() { _ = s2.Close() }()
	// 再直接跑一遍迁移函数本身（同一段迁移被跑第二次的最坏情形）。
	if err := migrateV7AtomKind(context.Background(), s2.db); err != nil {
		t.Fatalf("时效类别迁移跑第二遍失败（不幂等）: %v", err)
	}
	got, err := s2.GetAtom(context.Background(), "ns", "a1")
	if err != nil {
		t.Fatalf("第二遍之后数据丢了: %v", err)
	}
	if got.Assertion != "磁盘长期偏小" {
		t.Fatalf("第二遍之后数据变了: %+v", got)
	}
}

// dropKindShape 把库变回「没有时效类别列」的旧形状（v6 形状）。
//
// 不复制一份旧 DDL：旧形状由**当前形状的反面**推导（删列），
// 故它不会随真 DDL 变化而失效（抄一份旧 DDL 的用例会绿着，而真库炸着）。
func dropKindShape(t *testing.T, s *Store) {
	t.Helper()
	for _, stmt := range []string{
		// 先删索引：SQLite 不许删「被索引引用」的列。
		`DROP INDEX IF EXISTS atoms_kind`,
		`ALTER TABLE atoms DROP COLUMN kind`,
		`UPDATE meta SET value = '6' WHERE namespace = '' AND key = 'schema_version'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
}

// indexExists 索引在不在。
//
// 查 sqlite_master（系统表）而不是试一句会失败的查询：
// 「索引不存在」的报错信息由驱动决定，而这里要判的是「建没建出来」。
func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("查索引 %s 是否存在失败: %v", name, err)
	}
	return n > 0
}
