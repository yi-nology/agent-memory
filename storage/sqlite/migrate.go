package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// 本文件：schema 迁移机制。
//
// # 为什么现在才有
//
// 建表脚本一直是「CREATE TABLE IF NOT EXISTS」——**没有 ALTER**，所以
// 「加一列」这件事在本仓一直没有落地的路径。加了归属列（atoms.scope）之后
// 它就必须存在了：否则新代码按新形状读旧库，第一条事实卡查询就报
// 「no such column」，而那正是本仓明确要防的失效（旧形状的库被新代码按新形状读）。
//
// # 机制
//
// 版本号记在 meta 表的 schema_version 键上（全局行，namespace=”），
// 迁移按版本号线性追加：每次打开库先读当前版本，把版本号更高的迁移依次跑一遍，
// 跑完把版本号写回去。
//
// # 每段迁移必须幂等
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的
// （拷贝时带了旧版本号）。于是同一段迁移可能被跑第二次——
// 故每段迁移自己判「要不要做」（见 migrateV2AtomScope、migrateV3EntityPageScope）。
// 幂等不是风格偏好：迁移跑第二遍而失败的话，**库就打不开了**，
// 而那正是「升级一下记忆库」这种日常操作最不该出的事。
type migration struct {
	// version 迁移完成后的 schema 版本号。
	version int
	// apply 执行迁移。必须幂等。
	apply func(ctx context.Context, db *sql.DB) error
}

// migrations 按版本号线性追加。
//
// 新增一段的步骤：schemaVersion +1 → 在这里追加一段 → 写迁移。
// 顺序不能反：迁移要按版本号跑，版本号是排序依据。
var migrations = []migration{
	{version: 2, apply: migrateV2AtomScope},
	{version: 3, apply: migrateV3EntityPageScope},
	{version: 4, apply: migrateV4AtomTemporal},
	{version: 5, apply: migrateV5EntityEdges},
	{version: 6, apply: migrateV6AtomVectors},
}

// migrateV2AtomScope 给 atoms、raw_events、candidates 加归属列 scope 与索引。
//
// 三张表都要加，缺一张就会**在中途把归属丢掉**：
//
//   - 原始事件是归属的**来路**（事件带着归属走）；
//   - 候选是**中途站**（提炼出来、还没晋升）；
//   - 事实卡是归属的**归宿**。
//
// 少了候选那一列，归属在「提炼→晋升」之间断掉：
// CLI 的提炼路径从库里读事件、提成候选、落库，候选再被晋升成事实——
// 中间那一站没存归属，于是这一组的事实就变成了全局的（**泄漏**）。
//
// 幂等做法：先查列在不在，不在才 ALTER；索引用 IF NOT EXISTS。
func migrateV2AtomScope(ctx context.Context, db *sql.DB) error {
	for _, t := range []struct{ table, column string }{
		{"atoms", "scope"},
		{"raw_events", "scope"},
		{"candidates", "scope"},
	} {
		has, err := hasColumn(ctx, db, t.table, t.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		// DEFAULT '' 是必须的：老库里已有的行也得有值，
		// 而 NOT NULL 没有默认值的加列在有数据的表上会直接失败。
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE `+t.table+` ADD COLUMN scope TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("加 %s.scope 列失败: %w", t.table, err)
		}
	}
	// 索引归迁移管，不在建表脚本里——理由见 schema.go 里 atoms 那段的注释。
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS atoms_scope ON atoms (namespace, scope)`,
		`CREATE INDEX IF NOT EXISTS raw_events_scope ON raw_events (namespace, scope)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("建归属索引失败（%s）: %w", stmt, err)
		}
	}
	return nil
}

// migrateV4AtomTemporal 给 atoms 加生效区间与版本号三列。
//
// # 为什么时序是**区间**而不是只软退役
//
// 软退役（SupersededBy / DeprecatedAt）只说「这条不算数了」，
// 追问「当初为什么这么判」时它就丢了「它**什么时候对过**」这个信息：
// 「现在磁盘满了」与「三天前磁盘满了」在只软退役的模型里是同一张卡，
// 而它们是两条事实。故事实带一个生效区间（从哪一刻成立、到哪一刻止），
// 于是「现在的状态」与「历史上曾经的状态」能同时被查到。
//
// # 为什么加列必须走迁移
//
// 建表脚本是 `CREATE TABLE IF NOT EXISTS`：老库的 atoms 表已存在，
// 脚本对它是**空操作**，新列不会凭空出现。于是新代码按新形状读旧库，
// 第一条事实卡查询就报「no such column」——那正是本仓要防的失效。
//
// # 幂等
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的，
// 于是同一段迁移可能被跑第二遍——而迁移跑第二遍而失败的话
// **库就打不开了**。故加列前先查列在不在（hasColumn）：不在才 ALTER。
//
// # 为什么这一段不建索引
//
// 生效判据是 COALESCE(...) 的**表达式**（显式生效点，没有就退回发生时刻），
// 普通索引吃不上；而过滤前先按 (namespace, entity_id) / (namespace, scope)
// 收窄候选集，那两个索引已经在了。
func migrateV4AtomTemporal(ctx context.Context, db *sql.DB) error {
	for _, c := range []struct{ column, ddl string }{
		{"valid_from", `ALTER TABLE atoms ADD COLUMN valid_from INTEGER NOT NULL DEFAULT 0`},
		{"valid_to", `ALTER TABLE atoms ADD COLUMN valid_to INTEGER`},
		{"version", `ALTER TABLE atoms ADD COLUMN version INTEGER NOT NULL DEFAULT 1`},
	} {
		has, err := hasColumn(ctx, db, "atoms", c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		// DEFAULT 0 是必须的：老库里已有的行也得有值，
		// 而 NOT NULL 没有默认值的加列在有数据的表上会直接失败。
		if _, err := db.ExecContext(ctx, c.ddl); err != nil {
			return fmt.Errorf("加 atoms.%s 列失败: %w", c.column, err)
		}
	}
	return nil
}

// migrateV5EntityEdges 建关系边表与它的索引。
//
// # 为什么要有这一段
//
// 边表是**新表**：新库由建表脚本建，老库（v3 形状）没有这张表。
// 迁移补上它，两个来源的库必须落在**同一个形状**上——
// 否则「有的库能投影边、有的库不能」，症状是「边存不进去」，
// 而失败点离病因很远。
//
// # 幂等
//
// DDL 与索引都用 IF NOT EXISTS（DDL 与建表脚本共用同一份，见 schema.go）：
// 同一段迁移可能被跑第二遍（版本号被手工改过、被老版本覆盖过、
// 库从别处拷来），而迁移跑第二遍而失败的话**库就打不开了**。
func migrateV5EntityEdges(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, entityEdgesDDL); err != nil {
		return fmt.Errorf("建关系边表失败: %w", err)
	}
	return nil
}

// migrateV6AtomVectors 建向量索引表与它的归属索引。
//
// # 为什么要有这一段
//
// 向量索引表是**新表**：新库由建表脚本建，老库（v5 形状）没有这张表。
// 迁移补上它，两个来源的库必须落在**同一个形状**上——
// 否则「有的库能语义召回、有的库不能」，症状是「换了库语义路就空了」。
//
// # 幂等
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的，
// 于是同一段迁移可能被跑第二次——而迁移跑第二遍而失败的话
// **库就打不开了**，而那正是「升级一下记忆库」这种日常操作最不该出的事。
//
// 故表在不在先查（sqlite_master）：不在才建，在就跳过。
// 索引单独建（IF NOT EXISTS）：「表在而索引不在」是可能发生的形状
// （迁移被手工改过版本号），而缺索引的后果是「归属过滤退化成全表扫描」——
// 症状只是慢，没人会去查。
func migrateV6AtomVectors(ctx context.Context, db *sql.DB) error {
	exists, err := tableExists(ctx, db, "atom_vectors")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := db.ExecContext(ctx, atomVectorsDDL); err != nil {
			return fmt.Errorf("建向量索引表失败: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx, atomVectorsIndexDDL); err != nil {
		return fmt.Errorf("建向量归属索引失败: %w", err)
	}
	return nil
}

// tableExists 表在不在。
//
// 查 sqlite_master（系统表）而不是试一句 SELECT：
// 「表不存在」的报错信息由驱动决定，而迁移要判的是「要不要建」，
// 不是「建表语句会不会失败」。
func tableExists(ctx context.Context, q rowQuerier, table string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("查表 %s 是否存在失败: %w", table, err)
	}
	return n > 0, nil
}

// rowQuerier 能查库的东西。*sql.DB 与 *sql.Tx 都满足它。
//
// 用接口而不是只认 *sql.DB：迁移的形状判定必须在**事务里**做
// （重建表那段整段都得在事务里），而事务里拿到的是 *sql.Tx。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// entityPagePK 实体页面表的主键列（按主键序）。
var entityPagePK = []string{"namespace", "entity_id", "scope"}

// entityPagesNewShape 重建后 entity_pages 的新形状。
//
// ⚠️ 这一段 DDL 与 schema.go 里的那份**必须同形**：迁移重建出来的表
// 就是此后这个库实际用的表，两者不一致的话迁移后的库与新建的库行为不同
// （而症状是「有的库能取到、有的库取不到」这种没法一眼归因的差异）。
// 用例 TestMigrationEntityPageScope 拿形状指纹把两者钉在一起。
const entityPagesNewShape = `
CREATE TABLE entity_pages_new (
  namespace     TEXT NOT NULL,
  entity_id     TEXT NOT NULL,
  scope         TEXT NOT NULL DEFAULT '',
  headline      TEXT NOT NULL DEFAULT '',
  body          TEXT NOT NULL DEFAULT '',
  atom_ids      TEXT NOT NULL DEFAULT '[]',
  dirty         INTEGER NOT NULL DEFAULT 0,
  updated_at    INTEGER NOT NULL,
  last_regen_at INTEGER,
  regen_failure TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (namespace, entity_id, scope)
)`

// migrateV3EntityPageScope 给实体页面表加归属列，并把主键扩成三列。
//
// # 为什么归属是键的一部分而不是过滤面
//
// 页面是事实卡的摘要，而事实卡已经按归属隔离了。摘要若不分视角，
// 等于把别家的事实揉进一张全局画像里——那是**内容泄漏**，不是「少取」：
// 画像是人读的一整段话，读者分不清哪句属于自己。
// 所以归属进主键：同一实体在不同归属下**各有一页**，互相取不到。
//
// # 两步，为什么不能一步
//
// SQLite 不能 ALTER 主键，只能重建表：
//  1. 先加 scope 列（NOT NULL 且默认空串：老库里已有的行也得有值，
//     而 NOT NULL 没有默认值的加列在有数据的表上会直接失败）；
//  2. 再重建表，把主键换成 (namespace, entity_id, scope)，老行搬过去
//     （scope 取刚加的那列，老行自然是空串，即全局视角）。
//
// # 为什么整段在一个事务里
//
// 重建是「建新表 → 搬数据 → 删旧表 → 改名」四步。中间失败会留下半张表
// （旧表没了、新表还没改名），而那正是「库打不开了」——
// 迁移失败的后果不该是库报废，只能是「升级没成功，库还是原来那个形状」。
// 故这两步放在同一个事务里：任何一步出错都整体回滚。
//
// # 幂等
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的，
// 于是同一段迁移可能被跑第二遍——而迁移跑第二遍而失败的话**库就打不开了**。
// 故两处都要自己判「要不要做」：
//   - 加列前查列在不在（hasColumn）；
//   - 重建前读主键列，已是新形状就跳过（primaryKeyColumns）。
func migrateV3EntityPageScope(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 步骤一：加归属列。
	has, err := hasColumn(ctx, tx, "entity_pages", "scope")
	if err != nil {
		return err
	}
	if !has {
		// DEFAULT '' 是必须的：老库里已有的行也得有值，
		// 而 NOT NULL 没有默认值的加列在有数据的表上会直接失败。
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE entity_pages ADD COLUMN scope TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("加 entity_pages.scope 列失败: %w", err)
		}
	}

	// 步骤二：主键扩列（重建表）。
	pk, err := primaryKeyColumns(ctx, tx, "entity_pages")
	if err != nil {
		return err
	}
	if len(pk) == 0 {
		// 表不存在（建表脚本保证它在，但别赌）：没什么可迁的。
		return tx.Commit()
	}
	if !slices.Equal(pk, entityPagePK) {
		for _, stmt := range []string{
			entityPagesNewShape,
			`INSERT INTO entity_pages_new
				(namespace, entity_id, scope, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure)
			 SELECT namespace, entity_id, scope, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure
			 FROM entity_pages`,
			`DROP TABLE entity_pages`,
			`ALTER TABLE entity_pages_new RENAME TO entity_pages`,
			// 删表会把挂在它身上的索引一起带走，故改名后要把它建回来。
			// 这一条索引只用到 (namespace, dirty, updated_at)，不依赖新列，
			// 所以它能一直留在建表脚本里（新库老库都建得出来）。
			`CREATE INDEX IF NOT EXISTS entity_pages_dirty ON entity_pages (namespace, dirty, updated_at)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("重建 entity_pages 失败（%s）: %w", stmt, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}

// primaryKeyColumns 表的主键列，按主键序排。
//
// 用 pragma_table_info 的 pk 列：pk>0 的行按 pk 值排就是主键的列序。
// 「主键已是哪几列」是「要不要重建表」的判据，故这个读数必须准。
func primaryKeyColumns(ctx context.Context, q rowQuerier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return nil, fmt.Errorf("查 %s 的主键失败: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读 %s 的主键列失败: %w", table, err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// hasColumn 表里有没有这一列。
//
// 用 pragma_table_info（表值函数）而不是 PRAGMA table_info：
// 后者要按行读，前者能直接当子查询用。
func hasColumn(ctx context.Context, q rowQuerier, table, column string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("查 %s 的列失败: %w", table, err)
	}
	return n > 0, nil
}

// schemaVersionOf 读当前库的形状版本号。
//
// 读不到（老库没有这一行）一律当 0：0 的含义是「比任何迁移都早」，
// 于是所有迁移都会被跑一遍——而每段迁移都是幂等的，多跑一遍不会出事。
//
// 版本号**读不出来**（不是数字）时同样当 0：宁可多跑一遍幂等迁移，
// 也不要因为一个脏值就跳过迁移——跳过迁移的代价是「新代码按新形状读旧库」。
func schemaVersionOf(ctx context.Context, db *sql.DB) (int, error) {
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE namespace = '' AND key = 'schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读 schema 版本失败: %w", err)
	}
	n, convErr := strconv.Atoi(v)
	if convErr != nil {
		return 0, nil
	}
	return n, nil
}

// setSchemaVersion 写库形状版本号。
func setSchemaVersion(ctx context.Context, db *sql.DB, v int) error {
	if _, err := db.ExecContext(ctx,
		`INSERT INTO meta (namespace, key, value) VALUES ('', 'schema_version', ?)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(v)); err != nil {
		return fmt.Errorf("写 schema_version 失败: %w", err)
	}
	return nil
}
