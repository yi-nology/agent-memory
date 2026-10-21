package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion 本后端的库形状版本号。
//
// 记在 meta 表的 schema_version 键里，迁移按版本号线性追加（见 migrate.go）。
const schemaVersion = 3

// schema 是本后端的建表脚本。
//
// 与 SQLite 后端的差异只在**类型映射**（见 postgres.go 的对照表）：
// 布尔用 BOOLEAN、时刻用 TIMESTAMPTZ、列表与载荷仍用 TEXT 存 JSON。
//
// 全文索引用 **tsvector 列 + GIN 索引**，不用虚表：PG 的虚表没有 SQLite 那种
// 「自动同步」的能力，正文与索引会漂——而漂了的索引表现为
// 「搜得到但打开是空的」或反过来，都不报任何错。
// 故正文（body）与词元向量（body_tsv）**在同一次写入里一起落**，
// 由写路径保证两者一致。
const schema = `
CREATE TABLE IF NOT EXISTS raw_events (
  namespace   TEXT NOT NULL,
  id          TEXT NOT NULL,
  host        TEXT NOT NULL DEFAULT '',
  session_id  TEXT NOT NULL DEFAULT '',
  thread_id   TEXT NOT NULL DEFAULT '',
  user_name   TEXT NOT NULL DEFAULT '',
  scope       TEXT NOT NULL DEFAULT '',
  ts          TIMESTAMPTZ,
  type        TEXT NOT NULL DEFAULT '',
  content     TEXT NOT NULL DEFAULT '',
  payload     TEXT NOT NULL DEFAULT '{}',
  provenance  TEXT NOT NULL DEFAULT '{}',
  body        TEXT NOT NULL DEFAULT '',
  body_tsv    tsvector,
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS raw_events_ts ON raw_events (namespace, ts DESC);
CREATE INDEX IF NOT EXISTS raw_events_fts ON raw_events USING GIN (body_tsv);

CREATE TABLE IF NOT EXISTS candidates (
  namespace            TEXT NOT NULL,
  id                   TEXT NOT NULL,
  raw_event_ids        TEXT NOT NULL DEFAULT '[]',
  candidate_type       TEXT NOT NULL DEFAULT '',
  status               TEXT NOT NULL DEFAULT '',
  title                TEXT NOT NULL DEFAULT '',
  assertion            TEXT NOT NULL DEFAULT '',
  verbatim_quote       TEXT NOT NULL DEFAULT '',
  quote_event_id       TEXT NOT NULL DEFAULT '',
  subject_name         TEXT NOT NULL DEFAULT '',
  subject_entity_type  TEXT NOT NULL DEFAULT '',
  target_entity_id     TEXT NOT NULL DEFAULT '',
  confidence           TEXT NOT NULL DEFAULT '',
  importance           TEXT NOT NULL DEFAULT '',
  recommended_action   TEXT NOT NULL DEFAULT '',
  promotion_reason     TEXT NOT NULL DEFAULT '',
  extractor_version    TEXT NOT NULL DEFAULT '',
  session_id           TEXT NOT NULL DEFAULT '',
  scope                TEXT NOT NULL DEFAULT '',
  created_at           TIMESTAMPTZ,
  decided_at           TIMESTAMPTZ,
  decided_by           TEXT NOT NULL DEFAULT '',
  payload              TEXT NOT NULL DEFAULT '{}',
  signature            TEXT NOT NULL DEFAULT '',
  body                 TEXT NOT NULL DEFAULT '',
  body_tsv             tsvector,
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS candidates_sig ON candidates (namespace, signature);
CREATE INDEX IF NOT EXISTS candidates_status ON candidates (namespace, status);
CREATE INDEX IF NOT EXISTS candidates_fts ON candidates USING GIN (body_tsv);

CREATE TABLE IF NOT EXISTS atoms (
  namespace       TEXT NOT NULL,
  id              TEXT NOT NULL,
  entity_id       TEXT NOT NULL DEFAULT '',
  scope           TEXT NOT NULL DEFAULT '',
  candidate_id    TEXT NOT NULL DEFAULT '',
  raw_event_ids   TEXT NOT NULL DEFAULT '[]',
  assertion       TEXT NOT NULL DEFAULT '',
  verbatim_quote  TEXT NOT NULL DEFAULT '',
  quote_event_id  TEXT NOT NULL DEFAULT '',
  search_terms    TEXT NOT NULL DEFAULT '[]',
  occurred_at     TIMESTAMPTZ,
  confidence      TEXT NOT NULL DEFAULT '',
  importance      TEXT NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ,
  updated_at      TIMESTAMPTZ,
  superseded_by   TEXT NOT NULL DEFAULT '',
  deprecated_at   TIMESTAMPTZ,
  provenance      TEXT NOT NULL DEFAULT '{}',
  signature       TEXT NOT NULL DEFAULT '',
  body            TEXT NOT NULL DEFAULT '',
  body_tsv        tsvector,
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS atoms_entity ON atoms (namespace, entity_id);
CREATE INDEX IF NOT EXISTS atoms_occurred ON atoms (namespace, occurred_at DESC);
CREATE INDEX IF NOT EXISTS atoms_live ON atoms (namespace, superseded_by, deprecated_at);
CREATE INDEX IF NOT EXISTS atoms_sig ON atoms (namespace, signature);
CREATE INDEX IF NOT EXISTS atoms_fts ON atoms USING GIN (body_tsv);

CREATE TABLE IF NOT EXISTS entities (
  namespace        TEXT NOT NULL,
  id               TEXT NOT NULL,
  entity_type      TEXT NOT NULL DEFAULT '',
  canonical_name   TEXT NOT NULL DEFAULT '',
  atom_count       INTEGER NOT NULL DEFAULT 0,
  created_at       TIMESTAMPTZ,
  last_promoted_at TIMESTAMPTZ,
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS entities_name ON entities (namespace, lower(canonical_name));

CREATE TABLE IF NOT EXISTS aliases (
  namespace   TEXT NOT NULL,
  alias       TEXT NOT NULL,
  entity_id   TEXT NOT NULL,
  entity_type TEXT NOT NULL DEFAULT '',
  created_by  TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ,
  PRIMARY KEY (namespace, alias)
);
CREATE INDEX IF NOT EXISTS aliases_entity ON aliases (namespace, entity_id);

CREATE TABLE IF NOT EXISTS thread_active_entities (
  namespace      TEXT NOT NULL,
  thread_id      TEXT NOT NULL,
  entity_id      TEXT NOT NULL,
  canonical_name TEXT NOT NULL DEFAULT '',
  last_seen      TIMESTAMPTZ,
  pinned         BOOLEAN NOT NULL DEFAULT FALSE,
  pin_time       TIMESTAMPTZ,
  PRIMARY KEY (namespace, thread_id, entity_id)
);
CREATE INDEX IF NOT EXISTS active_seen ON thread_active_entities (namespace, thread_id, last_seen DESC);

-- 实体页面：某个实体在某**归属视角**下的长文画像。
--
-- scope 是**逻辑主键的一部分**，不是过滤面：
-- 同一实体在不同归属下各有一页，因为「这个对象在我的视角下是什么样」
-- 与「在别人视角下是什么样」是两件事——事实卡已经按归属隔离了，
-- 而页面是事实的摘要，摘要若不分视角，等于把别家的事实揉进一张
-- 人人可见的画像里（那是内容泄漏，不是少取）。
-- scope 为空 = 全局视角，那一页只汇总全局事实。
--
-- ⚠️ 这里**不建**依赖 scope 列的索引：建表脚本每次打开都跑，
-- 而老库的表还没有这一列，脚本里那句建索引会当场失败，整个库都打不开。
-- 索引归迁移管（见 migrate.go 的 v3）。
CREATE TABLE IF NOT EXISTS entity_pages (
  namespace      TEXT NOT NULL,
  entity_id      TEXT NOT NULL,
  scope          TEXT NOT NULL DEFAULT '',
  headline       TEXT NOT NULL DEFAULT '',
  body           TEXT NOT NULL DEFAULT '',
  atom_ids       TEXT NOT NULL DEFAULT '[]',
  dirty          BOOLEAN NOT NULL DEFAULT FALSE,
  updated_at     TIMESTAMPTZ,
  last_regen_at  TIMESTAMPTZ,
  regen_failure  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (namespace, entity_id, scope)
);
CREATE INDEX IF NOT EXISTS pages_dirty ON entity_pages (namespace, dirty);

CREATE TABLE IF NOT EXISTS episodes (
  namespace      TEXT NOT NULL,
  id             TEXT NOT NULL,
  session_id     TEXT NOT NULL DEFAULT '',
  title          TEXT NOT NULL DEFAULT '',
  summary        TEXT NOT NULL DEFAULT '',
  decisions      TEXT NOT NULL DEFAULT '[]',
  emotions       TEXT NOT NULL DEFAULT '[]',
  occurred_at    TIMESTAMPTZ,
  created_at     TIMESTAMPTZ,
  raw_event_ids  TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS episodes_occurred ON episodes (namespace, occurred_at DESC);
CREATE INDEX IF NOT EXISTS episodes_session ON episodes (namespace, session_id);

CREATE TABLE IF NOT EXISTS digests (
  namespace   TEXT NOT NULL,
  period      TEXT NOT NULL,
  period_key  TEXT NOT NULL,
  markdown    TEXT NOT NULL DEFAULT '',
  episode_ids TEXT NOT NULL DEFAULT '[]',
  window_start TIMESTAMPTZ,
  window_end   TIMESTAMPTZ,
  generated_at TIMESTAMPTZ,
  PRIMARY KEY (namespace, period, period_key)
);

CREATE TABLE IF NOT EXISTS journal (
  namespace TEXT NOT NULL,
  id        TEXT NOT NULL,
  at        TIMESTAMPTZ,
  kind      TEXT NOT NULL DEFAULT '',
  subject   TEXT NOT NULL DEFAULT '',
  actor     TEXT NOT NULL DEFAULT '',
  summary   TEXT NOT NULL DEFAULT '',
  detail    TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS journal_at ON journal (namespace, at DESC);
CREATE INDEX IF NOT EXISTS journal_subject ON journal (namespace, subject, at DESC);

CREATE TABLE IF NOT EXISTS meta (
  namespace TEXT NOT NULL,
  key       TEXT NOT NULL,
  value     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (namespace, key)
);
`

// ensureSchema 建表、跑迁移、记版本。
//
// 顺序不能换：先建表（老库的表已存在，IF NOT EXISTS 让它变成空操作），
// 再跑迁移（迁移要 ALTER 老库的表），最后记版本号。
//
// schema_version 记在全局行（namespace=”）上：它描述的是**库**的形状，
// 不是某个记忆库的内容，故不按 namespace 分行。
func ensureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	cur, err := schemaVersionOf(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= cur {
			continue
		}
		if err := m.apply(ctx, db); err != nil {
			return fmt.Errorf("迁移到 schema v%d 失败: %w", m.version, err)
		}
		if err := setSchemaVersion(ctx, db, m.version); err != nil {
			return err
		}
	}
	return setSchemaVersion(ctx, db, schemaVersion)
}
