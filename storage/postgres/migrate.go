package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 本文件：schema 迁移机制（与 SQLite 后端同一套机制、两份实现）。
//
// # 为什么现在才有
//
// 建表脚本一直是「CREATE TABLE IF NOT EXISTS」——**没有 ALTER**，所以
// 「加一列」这件事在本仓一直没有落地的路径。加了归属列（atoms.scope）之后
// 它就必须存在了：否则新代码按新形状读旧库，第一条事实卡查询就报
// 「column does not exist」，而那正是本仓明确要防的失效。
//
// # 机制
//
// 版本号记在 meta 表的 schema_version 键上（全局行，namespace=”），
// 迁移按版本号线性追加：每次打开库先读当前版本，把版本号更高的迁移依次跑一遍，
// 跑完把版本号写回去。
//
// # 每段迁移必须幂等
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的。
// 于是同一段迁移可能被跑第二次——故每段迁移自己判「要不要做」。
// 幂等不是风格偏好：迁移跑第二遍而失败的话，**库就打不开了**。
type migration struct {
	// version 迁移完成后的 schema 版本号。
	version int
	// apply 执行迁移。必须幂等。
	apply func(ctx context.Context, db *sql.DB) error
}

// migrations 按版本号线性追加。
//
// 新增一段的步骤：schemaVersion +1 → 在这里追加一段 → 写迁移。
var migrations = []migration{
	{version: 2, apply: migrateV2AtomScope},
	{version: 3, apply: migrateV3EntityPageScope},
}

// migrateV2AtomScope 给 atoms、raw_events、candidates 加归属列 scope 与索引。
//
// 三张表都要加，缺一张就会**在中途把归属丢掉**（理由见 SQLite 后端同处注释）：
// 事件是来路、候选是中途站、事实卡是归宿。
//
// 幂等做法：ADD COLUMN IF NOT EXISTS + CREATE INDEX IF NOT EXISTS。
func migrateV2AtomScope(ctx context.Context, db *sql.DB) error {
	for _, t := range []string{"atoms", "raw_events", "candidates"} {
		// DEFAULT '' 是必须的：老库里已有的行也得有值。
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE `+t+` ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("加 %s.scope 列失败: %w", t, err)
		}
	}
	// 索引归迁移管，不在建表脚本里：建表脚本每次打开都跑，
	// 而旧库（v1 形状）的表还没有这一列——建索引的语句会当场失败。
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

// migrateV3EntityPageScope 给 entity_pages 加归属列 scope，并把主键扩成三列。
//
// # 为什么要动主键
//
// 页面按「归属视角」分页：同一实体在不同归属下**各有一页**，
// 而旧主键 (namespace, entity_id) 把不同视角的页面挤成一行——
// 于是别家的事实会被揉进同一张画像里（那是内容泄漏，不是「少取」）。
//
// # 为什么主键约束名要动态查
//
// 约束名**不能写死**：库可能来自别处（拷来的、手工建过的），
// 它的主键约束未必叫约定的名字；而写死名字的 DROP CONSTRAINT
// 会当场失败，迁移失败 = 库打不开。故先从 pg_constraint 查出
// 约束名与它的列，再决定动不动。
//
// # 幂等
//
// 同一段迁移可能被跑第二遍（版本号被手工改过、被老版本覆盖过、
// 库从别处拷来）。判据是「当前主键的列里有没有 scope」：
// 有 → 已经扩过了，什么都不做。
func migrateV3EntityPageScope(ctx context.Context, db *sql.DB) error {
	// 迁移在事务里跑：PG 的 DDL 是事务性的，中途失败整段回滚。
	// 半改的表（列加了、主键没换）正是「库打不开」的形状。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 步骤一：加列。DEFAULT '' 是必须的——老库里已有的行也得有值，
	// 而那些行是全局视角的页面（scope 为空）。
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE entity_pages ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("加 entity_pages.scope 列失败: %w", err)
	}

	// 步骤二：主键从 (namespace, entity_id) 扩成 (namespace, entity_id, scope)。
	pkName, pkCols, err := primaryKeyOf(ctx, tx, "entity_pages")
	if err != nil {
		return err
	}
	switch {
	case pkName == "":
		// 表上没有主键：老脚本建表时一定带主键，故这是异常形状。
		// 补一个三列的；名字撞了就换一个（撞名会让 ADD CONSTRAINT 失败）。
		name, err := freeConstraintName(ctx, tx, "entity_pages", "entity_pages_pkey")
		if err != nil {
			return err
		}
		if err := addScopePrimaryKey(ctx, tx, name); err != nil {
			return err
		}
	case containsColumn(pkCols, "scope"):
		// 主键已经含 scope：这段迁移已经跑过了，跳过。
		// 这是幂等的关键——同一段迁移跑第二遍不能出事。
	default:
		// 旧主键删掉再加新的。删的是刚查出来的名字（不写死），
		// 加回去也用同一个名字：形状与新建的库一致。
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE entity_pages DROP CONSTRAINT `+quoteIdent(pkName)); err != nil {
			return fmt.Errorf("删 entity_pages 旧主键失败: %w", err)
		}
		if err := addScopePrimaryKey(ctx, tx, pkName); err != nil {
			return err
		}
	}

	// 步骤三：索引归迁移管。建表脚本每次打开都跑，而老库的表
	// 在跑完这段迁移之前还没有 scope 列——脚本里带 scope 的索引会当场失败。
	if _, err := tx.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS pages_scope_dirty ON entity_pages (namespace, scope, dirty)`); err != nil {
		return fmt.Errorf("建页面归属索引失败: %w", err)
	}
	return tx.Commit()
}

// querier 能查库的最小面：*sql.DB 与 *sql.Tx 都满足它。
//
// 用它而不是直接收 *sql.Tx，是为了让迁移逻辑与用例能共用同一段
// 「查主键形状」的代码——两处各写一份目录查询，那两份迟早漂移。
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// primaryKeyOf 查表的主键约束名与它的列（按列序）。
//
// 表名交给 to_regclass 解析（走 search_path），故不写死 schema 名。
// 返回空名表示这张表没有主键。
func primaryKeyOf(ctx context.Context, q querier, table string) (string, []string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT c.conname, a.attname
		 FROM pg_constraint c
		 JOIN pg_attribute a
		   ON a.attrelid = c.conrelid AND a.attnum = ANY(c.conkey)
		 WHERE c.contype = 'p' AND c.conrelid = to_regclass($1)
		 ORDER BY a.attnum`, table)
	if err != nil {
		return "", nil, fmt.Errorf("查 %s 的主键约束失败: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		name string
		cols []string
	)
	for rows.Next() {
		var cname, col string
		if err := rows.Scan(&cname, &col); err != nil {
			return "", nil, fmt.Errorf("读 %s 的主键约束失败: %w", table, err)
		}
		if name == "" {
			name = cname
		}
		cols = append(cols, col)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("读 %s 的主键约束失败: %w", table, err)
	}
	return name, cols, nil
}

// containsColumn 列集合里有没有这一列。
func containsColumn(cols []string, want string) bool {
	for _, c := range cols {
		if c == want {
			return true
		}
	}
	return false
}

// addScopePrimaryKey 把 entity_pages 的主键设成 (namespace, entity_id, scope)。
//
// 新主键是旧主键的**超集**（旧键唯一下新键也唯一），
// 所以老库里已有的行不会因为换键而撞唯一约束。
func addScopePrimaryKey(ctx context.Context, q querier, name string) error {
	if _, err := q.ExecContext(ctx,
		`ALTER TABLE entity_pages ADD CONSTRAINT `+quoteIdent(name)+
			` PRIMARY KEY (namespace, entity_id, scope)`); err != nil {
		return fmt.Errorf("加 entity_pages 新主键失败: %w", err)
	}
	return nil
}

// freeConstraintName 找一个没被占用的约束名。
//
// 撞名的后果是 ADD CONSTRAINT 失败 → 迁移失败 → 库打不开，
// 故宁可换一个名字：约束名对行为没有影响
// （ON CONFLICT 的仲裁索引靠**列集合**推断，不靠名字）。
func freeConstraintName(ctx context.Context, q querier, table, want string) (string, error) {
	for i := 0; i < 8; i++ {
		name := want
		if i > 0 {
			name = fmt.Sprintf("%s_scope%d", want, i)
		}
		var taken bool
		err := q.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_constraint
			 WHERE conrelid = to_regclass($1) AND conname = $2)`, table, name).Scan(&taken)
		if err != nil {
			return "", fmt.Errorf("查 %s 的约束名失败: %w", table, err)
		}
		if !taken {
			return name, nil
		}
	}
	return "", fmt.Errorf("找不到空闲的约束名（%s 上 %s 全被占用）", table, want)
}

// quoteIdent 给标识符加双引号。
//
// 约束名从系统目录里读出来，PG 保证它是合法标识符；仍要加引号，
// 因为不加引号的标识符会被折成小写，而加引号的保持原样——
// 两者指向不同的约束。
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
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
		`INSERT INTO meta (namespace, key, value) VALUES ('', 'schema_version', $1)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(v)); err != nil {
		return fmt.Errorf("写 schema_version 失败: %w", err)
	}
	return nil
}
