package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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

// TestMigrationAddsEntityPageScope 旧形状的库（页面表没有归属列、主键只有两列）
// 打开后要能升上来，且老数据不能丢。
//
// # 旧形状怎么造
//
// **按当前代码建库，再把它改回旧形状**——不抄一份旧 DDL：
// 抄的那份会与真 DDL 漂移，于是用例绿着而真库炸着。
// 改回去的动作（删列、退主键、改版本号）都是「当前形状的反面」，
// 故它们不会随真 DDL 变化而失效。
func TestMigrationAddsEntityPageScope(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	if err := s.UpsertEntityPage(context.Background(), "ns", &types.EntityPage{
		EntityID: "e1", Headline: "磁盘长期偏小",
	}); err != nil {
		t.Fatalf("写页面失败: %v", err)
	}
	// 变回「v2 形状」：主键退回两列、去掉归属列、版本号改回 2。
	pkName := pkConstraintName(t, s.db, "entity_pages")
	for _, stmt := range []string{
		`ALTER TABLE entity_pages DROP CONSTRAINT ` + quoteIdent(pkName),
		`ALTER TABLE entity_pages ADD CONSTRAINT ` + quoteIdent(pkName) + ` PRIMARY KEY (namespace, entity_id)`,
		// 列还被主键引用着，故必须先换主键再删列。
		`ALTER TABLE entity_pages DROP COLUMN scope`,
		`UPDATE meta SET value = '2' WHERE namespace = '' AND key = 'schema_version'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 重新打开：迁移必须把列加回来、把主键扩成三列，且老数据还在。
	s2, err := Open(tempDSN(t, dsn, dbName))
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.GetEntityPage(context.Background(), "ns", "e1", "")
	if err != nil {
		t.Fatalf("读旧页面失败: %v", err)
	}
	if got.Headline != "磁盘长期偏小" {
		t.Fatalf("旧数据读出来变了: %+v", got)
	}
	if got.Scope != "" {
		t.Fatalf("老数据的视角应是全局（空），实得 %q", got.Scope)
	}
	if cols := pkColumns(t, s2.db, "entity_pages"); len(cols) != 3 || cols[2] != "scope" {
		t.Fatalf("主键没扩成三列: %v", cols)
	}
	// 主键扩了，同一实体才能在不同视角下各有一页（旧的会撞唯一约束）。
	for _, sc := range []types.Scope{"scope-a", ""} {
		if err := s2.UpsertEntityPage(context.Background(), "ns", &types.EntityPage{
			EntityID: "e1", Scope: sc, Headline: "视角 " + string(sc),
		}); err != nil {
			t.Fatalf("扩主键后写入失败(scope=%q): %v", sc, err)
		}
	}
	// 迁移跑第二遍不报错（幂等）。
	if err := migrateV3EntityPageScope(context.Background(), s2.db); err != nil {
		t.Fatalf("迁移跑第二遍失败（不幂等）: %v", err)
	}
	if cols := pkColumns(t, s2.db, "entity_pages"); len(cols) != 3 {
		t.Fatalf("第二遍迁移后主键形状变了: %v", cols)
	}
	if got, err := s2.GetEntityPage(context.Background(), "ns", "e1", "scope-a"); err != nil {
		t.Fatalf("第二遍迁移后读页面失败: %v", err)
	} else if got.Headline != "视角 scope-a" {
		t.Fatalf("第二遍迁移后数据变了: %+v", got)
	}
	// 再打开一次也必须是干净的（版本号已到 3，迁移不该再动）。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(tempDSN(t, dsn, dbName))
	if err != nil {
		t.Fatalf("再次打开失败: %v", err)
	}
	_ = s3.Close()
}

// TestFreshSchemaHasPageScopeShape 新建的库必须**直接就是**新形状：
// 主键含归属列、归属索引在位。
//
// 为什么单独钉：建表脚本（新库）与迁移（老库）是两条路，
// 两条路必须落在同一个形状上——否则「新库能跑、老库跑不通」这类问题
// 要等到用户升级时才炸。
func TestFreshSchemaHasPageScopeShape(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	if cols := pkColumns(t, s.db, "entity_pages"); len(cols) != 3 || cols[2] != "scope" {
		t.Fatalf("新库的主键不是三列: %v", cols)
	}
	var hasIdx bool
	if err := s.db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM pg_indexes
		 WHERE tablename = 'entity_pages' AND indexname = 'pages_scope_dirty')`).Scan(&hasIdx); err != nil {
		t.Fatalf("查索引失败: %v", err)
	}
	if !hasIdx {
		t.Fatal("新库缺少归属索引 pages_scope_dirty")
	}
}

// TestMigrationAtomTemporal 旧形状的库（事实卡没有生效区间三列、没有关系边表）
// 打开后要能升上来，且老数据不丢、语义不变。
//
// # 旧形状怎么造
//
// **按当前代码建库，再把它改回旧形状**——不抄一份旧 DDL：
// 抄的那份会与真 DDL 漂移，于是用例绿着而真库炸着。
// 改回去的动作（删列、删表、改版本号）都是「当前形状的反面」，
// 故它们不会随真 DDL 变化而失效。
func TestMigrationAtomTemporal(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	occurred := pgBase()
	// 先存一条老数据（生效区间未设 = 按发生时刻算）。
	if err := s.SaveAtom(context.Background(), "ns", &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小需要清理旧文件",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		OccurredAt: occurred, CreatedAt: occurred, UpdatedAt: occurred,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 变回「v3 形状」：去掉三列、删掉边表、版本号改回 3。
	for _, stmt := range []string{
		`ALTER TABLE atoms DROP COLUMN valid_from`,
		`ALTER TABLE atoms DROP COLUMN valid_to`,
		`ALTER TABLE atoms DROP COLUMN version`,
		`DROP TABLE IF EXISTS entity_edges`,
		`UPDATE meta SET value = '3' WHERE namespace = '' AND key = 'schema_version'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 重新打开：迁移必须把列加回来、把边表建出来，且老数据还在。
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
	// 老数据的生效区间是「未设」：语义等价于改动前（按发生时刻算）。
	if !got.ValidFrom.IsZero() || !got.ValidTo.IsZero() {
		t.Fatalf("老数据的生效区间应是未设，实得 %v ~ %v", got.ValidFrom, got.ValidTo)
	}
	from, to := got.ValidRange()
	if !from.Equal(occurred) || !to.IsZero() {
		t.Fatalf("老数据的生效区间应退回发生时刻，实得 %v ~ %v", from, to)
	}
	if !got.EffectiveAt(occurred) {
		t.Fatal("老数据在发生时刻应成立")
	}
	if got.Version != 1 {
		t.Fatalf("老数据的版本号应为 1，实得 %d", got.Version)
	}

	// 新列要能用：升级之后写入的卡能带上生效区间与版本号。
	scoped := &types.AtomCard{
		ID: "a2", EntityID: "e1", Assertion: "内存占用偏高",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		OccurredAt: occurred, CreatedAt: occurred, UpdatedAt: occurred,
		ValidFrom: occurred, ValidTo: occurred.Add(time.Hour), Version: 3,
	}
	if err := s2.SaveAtom(context.Background(), "ns", scoped); err != nil {
		t.Fatalf("升级后写入失败: %v", err)
	}
	back, err := s2.GetAtom(context.Background(), "ns", "a2")
	if err != nil {
		t.Fatal(err)
	}
	if !back.ValidFrom.Equal(occurred) || !back.ValidTo.Equal(occurred.Add(time.Hour)) {
		t.Fatalf("生效区间没存上: %v ~ %v", back.ValidFrom, back.ValidTo)
	}
	if back.Version != 3 {
		t.Fatalf("版本号没存上: %d", back.Version)
	}

	// 边表也要在：老库升级后能投影边。
	if err := s2.UpsertEdge(context.Background(), "ns", &types.EntityEdge{
		ID: "e-1", FromEntityID: "e1", ToEntityID: "e2", Relation: "关系甲",
		CreatedAt: pgBase(),
	}); err != nil {
		t.Fatalf("升级后写边失败: %v", err)
	}
	edge, err := s2.GetEdge(context.Background(), "ns", "e-1")
	if err != nil {
		t.Fatalf("升级后读边失败: %v", err)
	}
	if edge.FromEntityID != "e1" || edge.ToEntityID != "e2" {
		t.Fatalf("边的两端失真: %+v", edge)
	}

	// 迁移跑第二遍不报错（幂等）：两段迁移都自己判「要不要做」。
	if err := migrateV4AtomTemporal(context.Background(), s2.db); err != nil {
		t.Fatalf("时序迁移跑第二遍失败（不幂等）: %v", err)
	}
	if err := migrateV5EntityEdges(context.Background(), s2.db); err != nil {
		t.Fatalf("边表迁移跑第二遍失败（不幂等）: %v", err)
	}
	if got, err := s2.GetAtom(context.Background(), "ns", "a1"); err != nil {
		t.Fatalf("第二遍迁移后读事实卡失败: %v", err)
	} else if got.Assertion != "磁盘长期偏小需要清理旧文件" {
		t.Fatalf("第二遍迁移后数据变了: %+v", got)
	}
}

// TestFreshSchemaMatchesMigratedShape 新建的库与迁移后的库形状一致。
//
// 建表脚本（新库）与迁移（老库）是两条路，两条路必须落在同一个形状上——
// 否则「新库能跑、老库跑不通」这类问题要等到用户升级时才炸。
// 断言用**列集合 + 主键形状**比对：迁移出来的表此后就是这个库实际用的表，
// 两者不同形的话「有的库能取到、有的库取不到」。
func TestFreshSchemaMatchesMigratedShape(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	// 变回「v3 形状」再打开一次，让它走一遍迁移。
	for _, stmt := range []string{
		`ALTER TABLE atoms DROP COLUMN valid_from`,
		`ALTER TABLE atoms DROP COLUMN valid_to`,
		`ALTER TABLE atoms DROP COLUMN version`,
		`DROP TABLE IF EXISTS entity_edges`,
		`UPDATE meta SET value = '3' WHERE namespace = '' AND key = 'schema_version'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}
	migrated, err := Open(tempDSN(t, dsn, dbName))
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	defer func() { _ = migrated.Close() }()

	// 新库：按当前建表脚本建的库。
	fresh, freshName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, freshName)
	defer func() { _ = fresh.Close() }()

	for _, table := range []string{"atoms", "entity_edges"} {
		freshShape := tableShapePG(t, fresh.db, table)
		migratedShape := tableShapePG(t, migrated.db, table)
		if !slices.Equal(freshShape, migratedShape) {
			t.Fatalf("%s 的形状迁移后与新建的库不一致:\n新建: %v\n迁移后: %v",
				table, freshShape, migratedShape)
		}
		freshPK := pkColumns(t, fresh.db, table)
		migratedPK := pkColumns(t, migrated.db, table)
		if !slices.Equal(freshPK, migratedPK) {
			t.Fatalf("%s 的主键迁移后与新建的库不一致:\n新建: %v\n迁移后: %v",
				table, freshPK, migratedPK)
		}
	}
}

// tableShapePG 表的形状指纹（列名、类型、可空、默认值，按列序）。
//
// 读的是**现库实际的形状**（系统目录），而不是某份抄下来的 DDL——
// 抄下来的那份会与真 DDL 漂移。
func tableShapePG(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT column_name, data_type, is_nullable, coalesce(column_default, '')
		FROM information_schema.columns
		WHERE table_name = $1
		ORDER BY ordinal_position`, table)
	if err != nil {
		t.Fatalf("读 %s 的列失败: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var (
			name, typ, nullable, def string
		)
		if err := rows.Scan(&name, &typ, &nullable, &def); err != nil {
			t.Fatalf("读 %s 的列定义失败: %v", table, err)
		}
		out = append(out, fmt.Sprintf("%s|%s|nullable=%s|default=%s", name, typ, nullable, def))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读 %s 的列定义失败: %v", table, err)
	}
	return out
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

// pkConstraintName 查表的主键约束名（用例用来把库改回旧形状）。
func pkConstraintName(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	name, _, err := primaryKeyOf(context.Background(), db, table)
	if err != nil {
		t.Fatalf("查 %s 的主键约束失败: %v", table, err)
	}
	if name == "" {
		t.Fatalf("%s 上没有主键约束", table)
	}
	return name
}

// pkColumns 查表的主键列（用例用来断言主键形状）。
func pkColumns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	_, cols, err := primaryKeyOf(context.Background(), db, table)
	if err != nil {
		t.Fatalf("查 %s 的主键列失败: %v", table, err)
	}
	return cols
}
