package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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

// TestMigrationEntityPageScope 旧形状的库（页面表没有归属列、主键只有两列）
// 打开后要能升上来，且老数据不丢。
//
// 页面表这一段的迁移比「加一列」重：主键多了一列，而 SQLite 不能改主键，
// 只能重建表——重建漏了任何一步都是「库打不开了」。
func TestMigrationEntityPageScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page-scope.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	// 先存一条老数据（归属为空 = 全局视角）。
	if err := s.UpsertEntityPage(context.Background(), "ns", &types.EntityPage{
		EntityID: "e1", Headline: "旧画像", Body: "正文", AtomIDs: []string{"a1"},
		UpdatedAt: legacyBase(),
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 把它变回「v2 形状」：去掉归属列、主键退回两列、版本号改回 2。
	rebuildEntityPagesOldShape(t, s)
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '2' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatalf("改版本号失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 重新打开：迁移必须把列加回来、把主键扩成三列，且老数据还在。
	s2, err := OpenFile(path)
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	got, err := s2.GetEntityPage(context.Background(), "ns", "e1", "")
	if err != nil {
		t.Fatalf("升级后读老数据失败: %v", err)
	}
	if got.Headline != "旧画像" || got.Body != "正文" {
		t.Fatalf("老数据读出来变了: %+v", got)
	}
	// 老行搬过去时归属是空串 = 全局视角。
	if got.Scope != "" {
		t.Fatalf("老数据的视角应是全局（空），实得 %q", got.Scope)
	}

	// 主键扩成三列：同一 entity_id 的两个视角不撞唯一键。
	for _, sc := range []types.Scope{"scope-a", "scope-b"} {
		if err := s2.UpsertEntityPage(context.Background(), "ns", &types.EntityPage{
			EntityID: "e1", Scope: sc, Headline: string(sc), UpdatedAt: legacyBase(),
		}); err != nil {
			t.Fatalf("写 %q 视角失败: %v", sc, err)
		}
	}
	for _, sc := range []types.Scope{"scope-a", "scope-b", ""} {
		page, err := s2.GetEntityPage(context.Background(), "ns", "e1", sc)
		if err != nil {
			t.Fatalf("取 %q 视角失败: %v", sc, err)
		}
		if page.Scope != sc {
			t.Fatalf("视角字段失真: 期望 %q 实得 %q", sc, page.Scope)
		}
	}

	// 迁移后的表形状必须与新建的库一样：迁移重建出来的那张表
	// 就是此后这个库实际用的表，两者不同形的话「有的库能取到、有的库取不到」。
	fresh, err := OpenMemory("fresh-shape-" + t.Name())
	if err != nil {
		t.Fatalf("开新库失败: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	freshShape := tableShape(t, fresh.db, "entity_pages")
	migratedShape := tableShape(t, s2.db, "entity_pages")
	if !slices.Equal(freshShape, migratedShape) {
		t.Fatalf("迁移后的表形状与新建的库不一致:\n新建: %v\n迁移后: %v", freshShape, migratedShape)
	}
}

// TestMigrationEntityPageScopeIsIdempotent 迁移跑第二遍不报错。
//
// 重建表那段若不幂等（每次都重建、或每次都加列），
// 第二次打开就会失败——而失败的后果是「库打不开了」。
func TestMigrationEntityPageScopeIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "page-scope-twice.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEntityPage(context.Background(), "ns", &types.EntityPage{
		EntityID: "e1", Scope: "scope-a", Headline: "画像", UpdatedAt: legacyBase(),
	}); err != nil {
		t.Fatal(err)
	}
	// 假装这段迁移没跑过（版本号被手工改回 2）。
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '2' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 第二遍：表已经是新形状，迁移必须认出来并跳过重建。
	s2, err := OpenFile(path)
	if err != nil {
		t.Fatalf("第二次打开失败（迁移不幂等）: %v", err)
	}
	defer func() { _ = s2.Close() }()
	page, err := s2.GetEntityPage(context.Background(), "ns", "e1", "scope-a")
	if err != nil {
		t.Fatalf("第二遍之后数据丢了: %v", err)
	}
	if page.Headline != "画像" {
		t.Fatalf("第二遍之后数据变了: %+v", page)
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
	path := filepath.Join(t.TempDir(), "temporal.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	occurred := legacyBase()
	// 先存一条老数据（生效区间未设 = 按发生时刻算）。
	if err := s.SaveAtom(context.Background(), "ns", &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小需要清理旧文件",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		OccurredAt: occurred, CreatedAt: occurred, UpdatedAt: occurred,
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 变回「v3 形状」：去掉三列与时效类别列、删掉边表、版本号改回 3。
	dropPostV3Shape(t, s)
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '3' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatalf("改版本号失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 重新打开：迁移必须把列加回来、把边表建出来，且老数据还在。
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
	// 版本号：老行由迁移补上，列的默认值是 1。
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
		CreatedAt: legacyBase(),
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

	// 迁移后的形状必须与新建的库一样：两条路（建表脚本 / 迁移）
	// 落在同一个形状上，否则「有的库能取到、有的库取不到」。
	fresh, err := OpenMemory("fresh-temporal-" + t.Name())
	if err != nil {
		t.Fatalf("开新库失败: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	for _, table := range []string{"atoms", "entity_edges"} {
		freshShape := tableShape(t, fresh.db, table)
		migratedShape := tableShape(t, s2.db, table)
		if !slices.Equal(freshShape, migratedShape) {
			t.Fatalf("%s 的形状迁移后与新建的库不一致:\n新建: %v\n迁移后: %v",
				table, freshShape, migratedShape)
		}
	}
}

// TestMigrationAtomTemporalIsIdempotent 迁移跑第二遍不报错。
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的
// ——同一段迁移可能被跑第二次，而迁移跑第二遍而失败的话
// **库就打不开了**，而那正是「升级一下记忆库」这种日常操作最不该出的事。
func TestMigrationAtomTemporalIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temporal-twice.sqlite")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(context.Background(), "ns", &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		CreatedAt: legacyBase(), UpdatedAt: legacyBase(),
	}); err != nil {
		t.Fatal(err)
	}
	// 假装这两段迁移没跑过（版本号被手工改回 3）。
	if _, err := s.db.Exec(
		`UPDATE meta SET value = '3' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 第二遍：列与表都已经是新形状，迁移必须认出来并跳过。
	s2, err := OpenFile(path)
	if err != nil {
		t.Fatalf("第二次打开失败（迁移不幂等）: %v", err)
	}
	defer func() { _ = s2.Close() }()
	got, err := s2.GetAtom(context.Background(), "ns", "a1")
	if err != nil {
		t.Fatalf("第二遍之后数据丢了: %v", err)
	}
	if got.Assertion != "磁盘长期偏小" {
		t.Fatalf("第二遍之后数据变了: %+v", got)
	}
}

// dropPostV3Shape 把库变回「v3 形状」（没有生效区间三列、没有时效类别列、没有关系边表）。
//
// 不复制一份旧 DDL：旧形状由**当前形状的反面**推导（删列、删表），
// 故它不会随真 DDL 变化而失效（抄一份旧 DDL 的用例会绿着，而真库炸着）。
//
// ⚠️ 时效类别列（kind，v7 加的）**也要去掉**：它比生效区间三列**更新**，
// 而 ALTER 只能把新列追加到末尾——留着它，迁移补列时新列会排在它后面，
// 于是「迁移后的形状」与「新建的形状」列序不同（两者形状不一致的形状差异）。
func dropPostV3Shape(t *testing.T, s *Store) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE atoms DROP COLUMN valid_from`,
		`ALTER TABLE atoms DROP COLUMN valid_to`,
		`ALTER TABLE atoms DROP COLUMN version`,
		// 先删索引：SQLite 不许删「被索引引用」的列。
		`DROP INDEX IF EXISTS atoms_kind`,
		`ALTER TABLE atoms DROP COLUMN kind`,
		`DROP TABLE IF EXISTS entity_edges`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
}

// tableColumn 表的一列定义。
type tableColumn struct {
	name    string
	typ     string
	notNull bool
	def     sql.NullString
	pk      int
}

// tableColumns 读表的列定义。
//
// 用 pragma_table_info（表值函数）读，拿到的是**现库实际的形状**，
// 而不是某份抄下来的 DDL——抄下来的那份会与真 DDL 漂移。
func tableColumns(t *testing.T, db *sql.DB, table string) []tableColumn {
	t.Helper()
	rows, err := db.Query(
		`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("读 %s 的列失败: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []tableColumn
	for rows.Next() {
		var c tableColumn
		if err := rows.Scan(&c.name, &c.typ, &c.notNull, &c.def, &c.pk); err != nil {
			t.Fatalf("读 %s 的列定义失败: %v", table, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读 %s 的列定义失败: %v", table, err)
	}
	return out
}

// tableShape 表的形状指纹（列名、类型、非空、默认值、主键序）。
//
// 两张表比指纹就比出了形状是否一致。
func tableShape(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	var out []string
	for _, c := range tableColumns(t, db, table) {
		def := ""
		if c.def.Valid {
			def = c.def.String
		}
		notNull := "0"
		if c.notNull {
			notNull = "1"
		}
		out = append(out, fmt.Sprintf("%s|%s|notnull=%s|default=%s|pk=%d",
			c.name, c.typ, notNull, def, c.pk))
	}
	return out
}

// rebuildEntityPagesOldShape 把 entity_pages 变回「没有归属列、主键少一列」的旧形状。
//
// 不复制一份旧 DDL：列的集合、类型、默认值、主键序都从**现库的表**读出来，
// 只把归属列去掉——于是旧形状永远由当前代码推导，不会与真 DDL 漂移
// （抄一份旧 DDL 的用例会绿着，而真库炸着）。
func rebuildEntityPagesOldShape(t *testing.T, s *Store) {
	t.Helper()
	cols := tableColumns(t, s.db, "entity_pages")
	var (
		defs   []string
		names  []string
		pkCols []string
	)
	type pkCol struct {
		name string
		ord  int
	}
	var pks []pkCol
	for _, c := range cols {
		if c.name == "scope" {
			continue
		}
		def := c.name + " " + c.typ
		if c.notNull {
			def += " NOT NULL"
		}
		if c.def.Valid {
			def += " DEFAULT " + c.def.String
		}
		defs = append(defs, def)
		names = append(names, c.name)
		if c.pk > 0 {
			pks = append(pks, pkCol{name: c.name, ord: c.pk})
		}
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i].ord < pks[j].ord })
	for _, p := range pks {
		pkCols = append(pkCols, p.name)
	}
	colList := strings.Join(names, ", ")
	stmts := []string{
		`CREATE TABLE entity_pages_oldshape (` + strings.Join(defs, ", ") +
			`, PRIMARY KEY (` + strings.Join(pkCols, ", ") + `))`,
		`INSERT INTO entity_pages_oldshape (` + colList + `) SELECT ` + colList + ` FROM entity_pages`,
		`DROP TABLE entity_pages`,
		`ALTER TABLE entity_pages_oldshape RENAME TO entity_pages`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
}
