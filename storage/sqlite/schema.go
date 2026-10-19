// Package sqlite 记忆存储的 SQLite 后端（纯 Go 驱动，CGO_ENABLED=0 可用）。
//
// # 为什么用单套表 + namespace 列，而不是「每 namespace 一套表」
//
// 上游 octop-memory 用表前缀隔离（`{ns}_atoms`）。那是「一个 backend 实例绑定
// 一个 namespace」的必然结果：既然实例已经绑定，就没必要每行再存一遍。
//
// 本仓的 store.Namespace 是**显式参数**（见 store/store.go），一个后端服务多个
// 记忆库，于是表前缀这条路直接断掉：每个 namespace 都要跑一遍 DDL、
// 都要在迁移时逐个改，而「这个实体在全库有几种叫法」这类跨库问题无从问起。
// 单套表 + namespace 列让跨 namespace 的体检与迁移成为一条普通 SQL。
//
// 代价是每条查询都要带 ns 条件——这个代价用索引前缀补：
// 所有主键与热点索引都以 (namespace, ...) 起头。
//
// # 本文件只放 DDL
//
// 方言 SQL 全部留在本包（门禁 memory-store-single-source 强制）。schema 版本
// 记在 meta 表的 schema_version 键里，迁移按版本号线性追加。
package sqlite

// schemaVersion 当前 schema 版本。新增迁移时 +1，并在 migrations 里追加一段。
const schemaVersion = 1

// schema 建表语句。
//
// 一律 `IF NOT EXISTS`：本后端的打开是幂等的——同一目录被两个进程打开是常态
// （回宿主跑的命令、另一个测试），而「第二个进程因为表已存在就打不开库」不该
// 是一种失败模式。
const schema = `
CREATE TABLE IF NOT EXISTS meta (
  namespace TEXT NOT NULL,
  key       TEXT NOT NULL,
  value     TEXT NOT NULL,
  PRIMARY KEY (namespace, key)
);

-- 捕获事件：记忆流程的素材。payload/provenance 存 JSON（SQLite 无原生 JSON 列
-- 类型，但 FTS5 与索引都不碰这两列，代价为零）。
CREATE TABLE IF NOT EXISTS raw_events (
  namespace      TEXT NOT NULL,
  id             TEXT NOT NULL,
  host           TEXT NOT NULL DEFAULT '',
  session_id     TEXT NOT NULL DEFAULT '',
  thread_id      TEXT NOT NULL DEFAULT '',
  user           TEXT NOT NULL DEFAULT '',
  ts             INTEGER NOT NULL,
  type           TEXT NOT NULL,
  content        TEXT NOT NULL DEFAULT '',
  payload        TEXT NOT NULL DEFAULT '{}',
  provenance     TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS raw_events_ts      ON raw_events (namespace, ts DESC);
CREATE INDEX IF NOT EXISTS raw_events_session ON raw_events (namespace, session_id, ts DESC);
CREATE INDEX IF NOT EXISTS raw_events_thread  ON raw_events (namespace, thread_id, ts DESC);

CREATE VIRTUAL TABLE IF NOT EXISTS raw_fts USING fts5(
  namespace UNINDEXED, event_id UNINDEXED, body, tokenize='unicode61'
);

-- 提炼候选：可丢弃的中间态。signature 是重复检测的键（见 types.AtomCard.Signature）。
CREATE TABLE IF NOT EXISTS candidates (
  namespace           TEXT NOT NULL,
  id                  TEXT NOT NULL,
  raw_event_ids       TEXT NOT NULL DEFAULT '[]',
  candidate_type      TEXT NOT NULL,
  status              TEXT NOT NULL,
  title               TEXT NOT NULL DEFAULT '',
  assertion           TEXT NOT NULL DEFAULT '',
  verbatim_quote      TEXT NOT NULL DEFAULT '',
  quote_event_id      TEXT NOT NULL DEFAULT '',
  subject_name        TEXT NOT NULL DEFAULT '',
  subject_entity_type TEXT NOT NULL DEFAULT '',
  target_entity_id    TEXT NOT NULL DEFAULT '',
  confidence          TEXT NOT NULL DEFAULT '',
  importance          TEXT NOT NULL DEFAULT '',
  recommended_action  TEXT NOT NULL DEFAULT '',
  promotion_reason    TEXT NOT NULL DEFAULT '',
  extractor_version   TEXT NOT NULL DEFAULT '',
  session_id          TEXT NOT NULL DEFAULT '',
  created_at          INTEGER NOT NULL,
  decided_at          INTEGER,
  decided_by          TEXT NOT NULL DEFAULT '',
  payload             TEXT NOT NULL DEFAULT '{}',
  signature           TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS candidates_status ON candidates (namespace, status, created_at DESC);
CREATE INDEX IF NOT EXISTS candidates_sig    ON candidates (namespace, signature);

CREATE VIRTUAL TABLE IF NOT EXISTS candidate_fts USING fts5(
  namespace UNINDEXED, candidate_id UNINDEXED, body, tokenize='unicode61'
);

-- 事实卡：长期记忆的真源。superseded_by 与 deprecated_at 至多其一非空。
CREATE TABLE IF NOT EXISTS atoms (
  namespace       TEXT NOT NULL,
  id              TEXT NOT NULL,
  entity_id       TEXT NOT NULL DEFAULT '',
  candidate_id    TEXT NOT NULL DEFAULT '',
  raw_event_ids   TEXT NOT NULL DEFAULT '[]',
  assertion       TEXT NOT NULL DEFAULT '',
  verbatim_quote  TEXT NOT NULL DEFAULT '',
  quote_event_id  TEXT NOT NULL DEFAULT '',
  search_terms    TEXT NOT NULL DEFAULT '[]',
  occurred_at     INTEGER NOT NULL,
  confidence      TEXT NOT NULL DEFAULT '',
  importance      TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  superseded_by   TEXT NOT NULL DEFAULT '',
  deprecated_at   INTEGER,
  provenance      TEXT NOT NULL DEFAULT '{}',
  signature       TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS atoms_entity      ON atoms (namespace, entity_id);
CREATE INDEX IF NOT EXISTS atoms_importance  ON atoms (namespace, importance);
CREATE INDEX IF NOT EXISTS atoms_occurred    ON atoms (namespace, occurred_at DESC);
CREATE INDEX IF NOT EXISTS atoms_live        ON atoms (namespace, superseded_by, deprecated_at);
CREATE INDEX IF NOT EXISTS atoms_sig         ON atoms (namespace, signature);

CREATE VIRTUAL TABLE IF NOT EXISTS atom_fts USING fts5(
  namespace UNINDEXED, atom_id UNINDEXED, body, tokenize='unicode61'
);

CREATE TABLE IF NOT EXISTS entities (
  namespace         TEXT NOT NULL,
  id                TEXT NOT NULL,
  entity_type       TEXT NOT NULL DEFAULT '',
  canonical_name    TEXT NOT NULL DEFAULT '',
  atom_count        INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  last_promoted_at  INTEGER,
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS entities_name ON entities (namespace, canonical_name);

CREATE TABLE IF NOT EXISTS aliases (
  namespace   TEXT NOT NULL,
  alias       TEXT NOT NULL,
  entity_id   TEXT NOT NULL,
  entity_type TEXT NOT NULL DEFAULT '',
  created_by  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (namespace, alias)
);
CREATE INDEX IF NOT EXISTS aliases_entity ON aliases (namespace, entity_id);

-- 实体页面（派生物）：dirty 标脏、regen 落生成、failure 记失败原因。
CREATE TABLE IF NOT EXISTS entity_pages (
  namespace    TEXT NOT NULL,
  entity_id    TEXT NOT NULL,
  headline     TEXT NOT NULL DEFAULT '',
  body         TEXT NOT NULL DEFAULT '',
  atom_ids     TEXT NOT NULL DEFAULT '[]',
  dirty        INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL,
  last_regen_at INTEGER,
  regen_failure TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (namespace, entity_id)
);
CREATE INDEX IF NOT EXISTS entity_pages_dirty ON entity_pages (namespace, dirty, updated_at);

-- 记忆树：组织视图。leaf 只持 atom_id（不复制正文，见 types/node.go）。
CREATE TABLE IF NOT EXISTS memory_nodes (
  namespace       TEXT NOT NULL,
  id              TEXT NOT NULL,
  parent_id       TEXT,
  level           TEXT NOT NULL,
  content         TEXT NOT NULL DEFAULT '',
  topic           TEXT,
  conversation_id TEXT,
  atom_id         TEXT,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  metadata        TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS nodes_parent ON memory_nodes (namespace, parent_id);
CREATE INDEX IF NOT EXISTS nodes_level  ON memory_nodes (namespace, level);
CREATE INDEX IF NOT EXISTS nodes_atom   ON memory_nodes (namespace, atom_id);

-- 活跃实体栈（召回期代词消解）：LRU 淘汰 + 钉住，跨进程重启存活。
CREATE TABLE IF NOT EXISTS thread_active_entities (
  namespace      TEXT NOT NULL,
  thread_id      TEXT NOT NULL,
  entity_id      TEXT NOT NULL,
  canonical_name TEXT NOT NULL DEFAULT '',
  last_seen      INTEGER NOT NULL,
  pinned         INTEGER NOT NULL DEFAULT 0,
  pin_time       INTEGER,
  PRIMARY KEY (namespace, thread_id, entity_id)
);
CREATE INDEX IF NOT EXISTS active_seen ON thread_active_entities (namespace, thread_id, last_seen DESC);

CREATE TABLE IF NOT EXISTS episodes (
  namespace    TEXT NOT NULL,
  id           TEXT NOT NULL,
  session_id   TEXT NOT NULL DEFAULT '',
  title        TEXT NOT NULL DEFAULT '',
  summary      TEXT NOT NULL DEFAULT '',
  decisions    TEXT NOT NULL DEFAULT '[]',
  emotions     TEXT NOT NULL DEFAULT '[]',
  occurred_at  INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  raw_event_ids TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS episodes_occurred ON episodes (namespace, occurred_at DESC);
CREATE INDEX IF NOT EXISTS episodes_session  ON episodes (namespace, session_id);

CREATE TABLE IF NOT EXISTS digests (
  namespace    TEXT NOT NULL,
  period       TEXT NOT NULL,
  period_key   TEXT NOT NULL,
  markdown     TEXT NOT NULL DEFAULT '',
  episode_ids  TEXT NOT NULL DEFAULT '[]',
  window_start INTEGER NOT NULL,
  window_end   INTEGER NOT NULL,
  generated_at INTEGER NOT NULL,
  PRIMARY KEY (namespace, period, period_key)
);

-- 审计条目：追加面（append-only），与业务表分离。
CREATE TABLE IF NOT EXISTS journal (
  namespace TEXT NOT NULL,
  id        TEXT NOT NULL,
  at        INTEGER NOT NULL,
  kind      TEXT NOT NULL,
  subject   TEXT NOT NULL DEFAULT '',
  actor     TEXT NOT NULL DEFAULT '',
  summary   TEXT NOT NULL DEFAULT '',
  detail    TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS journal_at      ON journal (namespace, at DESC);
CREATE INDEX IF NOT EXISTS journal_subject ON journal (namespace, subject, at DESC);
`
