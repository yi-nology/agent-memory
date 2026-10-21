# 三条集成线规划（2026-10-21）

本文是对三件事的规划：① `.hmpkg` 的 Go↔Python 互通；② 宿主（扁鹊）升级与画像团队维度；
③ OpenClaw 适配。

**规划前先把事实核了一遍**（下面每条「现状」都是实测/读源码得到的，不是推测）——
因为三件事里有两件的前提与直觉相反，先核再规划能省掉整轮返工。

---

## 轨道 A：`.hmpkg` Go↔Python 互通

### 现状（已核实）

**两侧的格式并不相同。** 原始文档里「两边使用相同的扩展名和格式版本号」这句话，
只对了一半：扩展名都是 `.hmpkg`、版本号都是 1 ✓，但**布局完全不同** ✗。

| | octop-memory（Python） | 本仓（Go） |
|---|---|---|
| 数据文件 | `data.jsonl.gz`（**单个** gzip 流） | **10 个**分类 `.jsonl`（未压缩） |
| 记录形状 | `{"v":1,"table":"atoms","data":{…}}` | 记录体**直接铺平** |
| manifest 键 | `pkg_version` / `source_host_kind` / `schema_version` / `tool_version` | `format_version` / `host` / `counts` / `files`（校验和） |
| 表清单 | 8 张（raw_events/candidates/entities/atoms/aliases/entity_pages/thread_active_entities/journal） | 10 类（多 episodes/digests，少 thread_active_entities） |

**但记录体本身几乎逐字段一致** ✓：`id / entity_id / candidate_id / raw_event_ids /
assertion / verbatim_quote / quote_event_id / search_terms / occurred_at / confidence /
importance` —— 同一设计的两个实现 ✓。本仓是**超集**（多 `scope`/`kind`/
`valid_from`/`valid_to`/`version` ✓）。

⇒ **结论：互通的第一件事不是「写测试」，是「格式对齐决策」✗**。
现在直接写互通测试，只会得到一个「两边都过、但导出的包对方读不了」的假绿 ✓。

### 路线选择

| 路线 | 做法 | 代价 | 对方要改吗 |
|---|---|---|---|
| **A1（推荐）** | 本仓加「octop 兼容导出/导入」：导出时写 `data.jsonl.gz` + `{v,table,data}` + octop 形状的 manifest；导入时能自动识别两种布局 | 本仓一个包两种写出面 + 一个布局嗅探 | **否** ✓（Python 侧零改动） |
| A2 | 只做单向（本仓 → octop） | 小 | 否 |
| A3 | 两边各写一个转换器 | 双份维护 ✗ | 是 |

选 **A1**：本仓的「读」侧已经有了完整的数据模型 ✓，只差**信封与布局**；
而 Python 侧已经有了能读 A1 输出的代码 ✓（它就是那个格式的作者 ✓）→ **对方零改动** ✓。

### 分期与验收

| 阶段 | 交付 | 验收（可在本仓内跑） | 状态 |
|---|---|---|---|
| A-0 | **字段映射表**（octop 字段 ↔ 本仓字段，缺的字段怎么填） | 写进 `operations/portable/octop.go` 的包注释（它是契约 ✓） | ✅ 已做 |
| A-1 | 本仓侧 **octop 格式写出**（`--format octop`） | 导出物含 manifest.json + data.jsonl.gz + {v,table,data} + snake_case 键名 ✓ | ✅ 已做 |
| A-2 | 本仓侧 **octop 格式读入**（`Import` 自动识别布局） | 自环：导出 → 读回 → **逐字段相等** ✓ | ✅ 已做 |
| A-3 | **黄金样本**（一个固定的小包，提交进 testdata） | 本仓侧用它跑导出/导入 ✓；**并把它交给 octop 侧跑一次**（跨仓对账 ✗ 需要对方配合 ✓） | ⏳ 待对方配合 |

### 跨仓协作的形态（现实一点）

**两边没有共享 CI** ✗ ⇒ 互通测试不能靠「CI 里互相跑」✓。
可行的形态是**黄金样本 + 一次性对账** ✓：

1. 本仓产出黄金包 `testdata/octop-golden.hmpkg` ✓（内容固定 ✓）；
2. octop 侧用它跑一次 `adopt` ✓ → 断言条数与关键字段 ✓ → **回报结果** ✓（贴在 issue/PR ✓）；
3. 反向：octop 侧也产一个黄金包 ✓ → 本仓侧用它跑 `import` ✓。
4. 之后**任何一侧改了布局**，黄金样本对账就会发现 ✓（靠人跑 ✓ —— 故在两侧的 README 里写明「改布局前先跑对账」✓）。

⚠️ 这条线的**独占价值**是真的 ✓（两家都实现了同一格式却没人证明过互通 ✓），
但**它是协作型的** ✗：本仓侧能做完 A-0…A-2 ✓，A-3 必须对方配合 ✓。

---

## 轨道 B：宿主（扁鹊）升级 + 画像团队维度

### 现状（已核实）

- 扁鹊钉的是 `agent-memory v0.6.0` ✓。
- **273 个文件**依赖 agent-memory ✓（`store` 229 处引用、`types` 223 处、`ports` 93 处、
  `pipeline/recall` 49 处、`application` 26 处……）→ 升版**不是改一行 go.mod** ✓，
  是一次**真实适配** ✓。
- 扁鹊有**自己的**画像表 `bq_memory_subject_page`（PK：`subject_kind + subject` ✓），
  并有自己的页面生命周期（`GetSubjectPage`/`SaveSubjectPage`/`MarkSubjectPageDirty`/
  `ListDirtySubjectPages` ✓）→ **它不是 agent-memory 的 `entity_pages` ✓**。

### 一处要纠正的旧说法 ⚠️

我此前说「宿主若要接画像团队维度**必须**升 v0.8.0」——**不准确** ✓。
按上面核实的事实：宿主用的是**自己的表** ✓，团队维度这件事
**给它自己的表加一列 `team`（PK 变成 subject_kind+subject+team）就能解决** ✓，
**不必**先升本仓 ✓。

升 v0.7.0+ 的收益是**另一件事** ✓：用内核的页面机制
（按归属分视角的生成 / 脏标记 / 归属隔离 / 审计 ✓），省掉自己维护一套 ✓。
那是**可选**的 ✓，不是前提 ✓。

### 破坏面清单（v0.6.0 → v0.8.0，逐条可核对）

**v0.7.0**
- `store.PageStore` 六个方法 + `scope` 参数 ✓（`GetEntityPage`/`ListDirtyEntityPages`/
  `MarkEntityPageDirty`/`ApplyEntityPageRegen`/`RecordEntityPageRegenFailure` ✓）；
- `recall.Reader` + `GetEntityPage(…, scope)` ✓；
- `types.EntityPage` + `Scope` ✓；`types.AtomCard` + `ValidFrom`/`ValidTo`/`Version` ✓；
- `promotion.Verdict` + `Actor`/`Superseded` ✓；`promotion.Decision` + `update` ✓；
  `promotion.Config` + `LLM`/`UpdateSimilarity`/`UpdateHintSimilarity` ✓；
- `recall.Config` + `Semantic`/`Analyzer`/`Metrics` ✓；
  `recall.Request` + `OccurredFrom`/`OccurredTo`/`At` ✓；
- `ports.RankCandidate` + `FusionScore` ✓；`ports.RankWeights` + `Fusion` ✓；
  `ports.VectorIndex` 的 `Search`/`Upsert` + `scope` ✓；
- `application.Deps` + `Promotion`/`Embedder`/`VectorIndex` ✓；
- `store.AtomFilter` + `IDs`/`At` ✓。

**v0.8.0**
- `store.AtomStore` + **`SearchAtomsLoose`** ✗（**实现召回链的宿主都要加** ✓）；
  `recall.Reader` 同样 + `SearchAtomsLoose` ✗；
- `store.AtomFilter` + `EventSince` ✓；`types.AtomCard` + `Kind` ✓；
- `lifecycle` + `Expire`/`WithEventRetention` ✓；`recall.Config` + `EventWindow` ✓。

### 分期（可分步，每步都能编译）

| 步 | 内容 | 谁受影响 | 验收 |
|---|---|---|---|
| B-0 | 只依赖 `types`/`store` 的层（273 文件的大头 ✓） | 几乎无感（**只加字段** ✓） | 编译 + 既有批次用例 ✓ |
| B-1 | 实现接口的层：`pipeline/page`/`pipeline/recall` 的假存储 + `application` 装配 | 十几个文件 ✓ | 编译 + 主链用例 ✓ |
| B-2 | 召回链的实现方（若宿主自造了召回器 ✓ → 加 `SearchAtomsLoose` ✓） | 看宿主有没有自造召回 ✓ | 编译 ✓ |
| B-3 | 跑宿主自己的回归（批次流程 ✓） | — | 宿主 CI 绿 ✓ |
| B-4（可选） | 画像切到内核页面机制（按归属分视角 ✓） | 画像那一块 ✓ | 团队隔离用例 ✓ |

⚠️ **不要把 B-4 混进升级** ✗：升级是「适配别人的新契约」✓，
切画像是「换实现」✓ —— 两件事的风险来源完全不同 ✓，混在一起出问题时分不清是谁的锅 ✓。

---

## 轨道 C：OpenClaw 适配

### 现状（已核实）

- **OpenClaw 认 MCP ✓**（多个第三方记忆产品用「MCP + hooks」接它 ✓；它的 gateway 监听 3111 ✓）。
- octop 的 OpenClaw 插件是 **Node/TS 包** ✓（`openclaw.plugin.json` + `package.json` ✓），
  做法是**拉起一个 bridge 子进程**暴露 tools ✓。
- OpenClaw 有 **hooks** ✓（`agent_end` 用来捕获 ✓）。

⇒ **对我们最省的路径是「MCP + hooks，零 Node 代码」** ✓✓：
octop 需要 Node 插件是因为它的运行时是 Python ✓；
我们是 Go 内核 ✓ → 一个二进制 + 一份配置就够 ✓。

### 需要产出

| 阶段 | 交付 | 验收 | 状态 |
|---|---|---|---|
| C-0 | **`memory mcp`**：stdio MCP 服务器入口（本仓已有 `adapters/mcp` 的协议层 ✓，缺的只是入口 ✓） | CLI 里跑一次真实协议往返（`initialize` → 有 `protocolVersion` ✓） | ✅ 已做 |
| C-1 | **OpenClaw 配置片段**（mcpServers + hooks ✓） | 配置能被 OpenClaw 读入 ✓（文档 + 片段 ✓） | ⏳ 待做 |
| C-2 | **捕获面接线**（hook → `memory capture` ✓ CLI 已有 ✓） | 一次 hook 触发后，库里多一条原始事件 ✓ | ⏳ 待做 |
| C-3 | **端到端验证脚本**（OpenClaw 里问一句 → 召回命中 ✓） | 脚本在有 OpenClaw 的机器上跑通 ✓ | ⏳ 待做 |

⚠️ **明确写下来：不需要写 Node 插件** ✓ —— 免得后人照着 octop 的做法去写一个 ✗
（那是它运行时形状决定的 ✓，不是协议要求 ✓）。

---

## 顺序建议

按「**独立可交付 + 对方依赖越少越先做**」排：

1. **C-0**（本仓独立可做 ✓，半天 ✓，且它让轨道 C 的其余部分变成「写配置」✓）；
2. **A-0 → A-2**（本仓独立可做 ✓，产出「格式对齐 + 双向读写 + 自环用例」✓；
   A-3 的跨仓对账可以并行发起 ✓）；
3. **B-0 → B-3**（宿主侧适配 ✓，工作量最大 ✓，但**不阻塞**上面两条 ✓）；
4. **B-4 / C-1 / C-2 / C-3**（按需 ✓）。

**互不阻塞** ✓：三条线分别落在「本仓 / 本仓+octop / 宿主 / 本仓+OpenClaw」✓，
可以并行推进 ✓。
