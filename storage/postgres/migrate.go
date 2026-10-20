package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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
