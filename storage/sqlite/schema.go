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
//
// 7 = 事实卡的生效区间与版本号（v4）+ 关系边表（v5）+ 向量索引表（v6）
//   - 事实卡的时效类别列（v7）。
const schemaVersion = 7

// atomVectorsDDL 向量索引表的建表语句。
//
// 建表脚本与迁移共用**同一份** DDL（见 migrate.go 的 v6）：
// 两份各写一遍的话，改了一处就会漂，而漂了的失效形态是
// 「有的库有这张表、有的库没有」——症状是「语义召回像是没生效」，
// 而那在数据上完全看不出来。
//
// 向量是**派生物**：真源是事实卡（向量是事实的函数），
// 索引丢了可以从事实卡重算，事实卡丢了索引救不回来。
// 故这一面只有「落向量与读向量」，没有任何改写事实的入口。
//
// scope 是**这条向量的归属**（与事实卡同口径）：空 = 全局可见，
// 非空 = 仅同归属可见。检索时归属过滤**进 SQL**（见 misc.go 的 Search）。
//
// vec 是 float32 的**小端字节序列**（不是 JSON 串、不是文本）：
// 文本往返会丢精度（十进制 → float64 → float32 两次舍入），
// 而向量要逐位可复现——同一批记忆在不同进程里算出的向量必须逐位相同。
const atomVectorsDDL = `
CREATE TABLE IF NOT EXISTS atom_vectors (
  namespace  TEXT NOT NULL,
  id         TEXT NOT NULL,
  scope      TEXT NOT NULL DEFAULT '',
  dim        INTEGER NOT NULL,
  vec        BLOB NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (namespace, id)
);
`

// atomVectorsIndexDDL 向量索引表的归属索引。
//
// **归属过滤进 SQL**（见 misc.go 的 Search），故这条索引是取数路径的一部分：
// 没有它，检索就是「整个命名空间全扫 + 逐条判归属」——
// 而归属过滤的价值恰恰在于「别家的数据不进候选池」。
//
// 索引**归迁移管**（见 migrate.go 的 v6）：建表脚本每次打开都跑，
// 而这张表是**新表**——脚本对老库是空操作（表还不存在时由脚本建出来，
// 建出来之后索引自然跟着建；表已存在时两条语句都是空操作）。
const atomVectorsIndexDDL = `
CREATE INDEX IF NOT EXISTS atom_vectors_scope ON atom_vectors (namespace, scope);
`

// entityEdgesDDL 关系边表的建表语句。
//
// 建表脚本与迁移共用**同一份** DDL（见 migrate.go 的 v5）：
// 两份各写一遍的话，改了一处就会漂，而漂了的失效形态是
// 「有的库有这张表、有的库没有」——症状出现在「边查不到」，离病因很远。
//
// 实体关系边（**派生物**）：边的真源是事实卡，投影丢了可以重投影。
//
// 归属与事实卡同口径：scope 非空且不等于读者的边**一条都不给**。
// 生效区间与事实卡同口径：**左闭右开**（valid_to 那一刻已不成立）。
//
// ⚠️ 这里的「未设」编码是 0（valid_from/valid_to 都是）：
// 边是投影物，它**存在**的时刻（created_at）就是它生效的时刻，
// 故未设生效点的边按 created_at 参与比较。
const entityEdgesDDL = `
CREATE TABLE IF NOT EXISTS entity_edges (
  namespace      TEXT NOT NULL,
  id             TEXT NOT NULL,
  from_entity_id TEXT NOT NULL,
  to_entity_id   TEXT NOT NULL,
  relation       TEXT NOT NULL,
  scope          TEXT NOT NULL DEFAULT '',
  valid_from     INTEGER NOT NULL DEFAULT 0,
  valid_to       INTEGER NOT NULL DEFAULT 0,
  weight         REAL NOT NULL DEFAULT 0,
  source_atom_id TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (namespace, id)
);
CREATE INDEX IF NOT EXISTS entity_edges_from  ON entity_edges (namespace, from_entity_id);
CREATE INDEX IF NOT EXISTS entity_edges_to    ON entity_edges (namespace, to_entity_id);
CREATE INDEX IF NOT EXISTS entity_edges_scope ON entity_edges (namespace, scope);
`

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
  scope          TEXT NOT NULL DEFAULT '',
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
  scope               TEXT NOT NULL DEFAULT '',
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
-- scope 是记录级归属（空=全局可见，非空=仅同归属可见，见 types/scope.go）。
--
-- ⚠️ 这一列的**索引**不在本文件里，而在迁移里（见 migrate.go）：
-- 本文件的 DDL 每次打开都跑，而旧库（v1 形状）的 atoms 表还没有这一列——
-- 建索引的语句会当场失败，整个库就都打不开了。
--
-- ⚠️ 生效区间那三列的**形状变更**同样归迁移管（见 migrate.go 的 v4）：
-- 老库的 atoms 表没有这三列，而本文件的 DDL 对老库是**空操作**
-- （CREATE TABLE IF NOT EXISTS），新列不会凭空出现——
-- 于是新代码按新形状读旧库，第一条事实卡查询就报「no such column」。
--
-- 三列的「未设」编码（**每种含义各一种编码，别混**）：
--   valid_from 的 0  = 未设 = 按发生时刻算（老数据的语义不变）；
--   valid_to 的 NULL = 未设 = 仍有效（与 deprecated_at 的 NULL 同款约定）。
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
  occurred_at     INTEGER NOT NULL,
  confidence      TEXT NOT NULL DEFAULT '',
  importance      TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  superseded_by   TEXT NOT NULL DEFAULT '',
  deprecated_at   INTEGER,
  provenance      TEXT NOT NULL DEFAULT '{}',
  signature       TEXT NOT NULL DEFAULT '',
  valid_from      INTEGER NOT NULL DEFAULT 0,
  valid_to        INTEGER,
  version         INTEGER NOT NULL DEFAULT 1,
  -- 时效类别（见 types.FactKind）：空串 = 默认 = 按长期经验处理，**不因时间失效**。
  --
  -- 零值落在「不淘汰」那一侧是刻意的：既有事实卡没有这个维度，
  -- 而升级不该让它们开始被淘汰——零值若落在「会被淘汰」的一侧，
  -- 升级一次就丢一批老事实。故 DEFAULT '' 而不是 DEFAULT 'event'。
  --
  -- ⚠️ 这一列的**形状变更与索引**都归迁移管（见 migrate.go 的 v7）：
  -- 本文件的 DDL 每次打开都跑，而旧库的 atoms 表还没有这一列——
  -- 建索引的语句会当场失败，整个库就都打不开了。
  --
  -- ⚠️ 这一列**必须排在最后**：迁移用 ALTER TABLE ADD COLUMN 补列，
  -- 而 ALTER 只能把新列追加到末尾——建表脚本若把它插在中间，
  -- 新建的库与迁移后的库形状就不同了（列序不同），
  -- 而形状差异的后果是「有的库能取到、有的库取不到」。
  kind            TEXT NOT NULL DEFAULT '',
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
--
-- scope 是**页面视角**，且是逻辑主键的一部分（不是过滤面）：
-- 同一个实体在不同归属下各有一页，因为页面是事实的摘要——
-- 摘要若不分视角，等于把别家的事实揉进一张全局画像里（内容泄漏），
-- 而画像是人读的一整段话，读者分不清哪句属于自己。
--
-- ⚠️ 主键里多了一列，于是这一列的形状变更归迁移管（见 migrate.go）：
-- 本文件的 DDL 每次打开都跑，而旧库的表还是两列主键——
-- 主键改不了，只能重建表，而重建必须成段落在事务里。
CREATE TABLE IF NOT EXISTS entity_pages (
  namespace    TEXT NOT NULL,
  entity_id    TEXT NOT NULL,
  scope        TEXT NOT NULL DEFAULT '',
  headline     TEXT NOT NULL DEFAULT '',
  body         TEXT NOT NULL DEFAULT '',
  atom_ids     TEXT NOT NULL DEFAULT '[]',
  dirty        INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL,
  last_regen_at INTEGER,
  regen_failure TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (namespace, entity_id, scope)
);
CREATE INDEX IF NOT EXISTS entity_pages_dirty ON entity_pages (namespace, dirty, updated_at);
` + entityEdgesDDL + `
` + atomVectorsDDL + atomVectorsIndexDDL + `

-- 记忆树：组织视图。leaf 只持 atom_id（不复制正文，见 types/node.go）。

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
