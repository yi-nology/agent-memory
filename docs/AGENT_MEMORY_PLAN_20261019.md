# agent-memory 立项方案（2026-10-19）

> 性质：**决策依据文档**。回答三个问题——octop-memory 是什么、扁鹊记忆平面差在哪、
> agent-harness 哪些该下沉到新模块。文中所有 octop 事实均由 `git clone --depth 1`
> 核对 `TencentCloud/octop-memory` @ `ed11d54`（v1.0.2）得出，未采信二手描述。
> 关联：`bianque/docs/RAG_ASSESSMENT_20261008.md`（知识检索面，与本方案**不是同一件事**，
> 见 §五）、`agent-harness/docs/SPLIT_PLAN.md`（拆分方法论，本方案沿用其判定标准）。

## TL;DR

1. **不是从零抽**。扁鹊已有分层记忆平面（批次 80/81/115，`bq_memory_atom` +
   `bq_memory_subject_page`），其 IDL 注释明写「对标 Octop octop-memory 的事实卡」
   ——它是 octop 的一个**子集**，缺的是**宿主外的抽象边界**与 octop 后半程能力。
2. **从 agent-harness「下沉」的东西接近于零**。逐包核完 24 个包后裁定：
   **0 个借道**（budget 原判「借道计量面」，M2 实弹推翻）、**0 个接线**
   （pii 原判「接线」，M3 实弹推翻；injectband 原判「接线」，M2 实弹推翻并撤掉集成）、
   **1 个只抄机制不合并**（pending ↔ thread_state）、**其余 16 个包全留 harness**。
   真正要搬的是**扁鹊自己的记忆平面**。
   ⚠️ 三处裁定都被实弹推翻了，理由见 §三 表内与 §4.3——**别按本文最初的裁定动手**。
3. **本仓有两处强于 octop，必须写进设计**：octop 的 privacy 面只有
   `store_raw_content=false` 这类**开关式丢弃**、没有内容级打码；recall 出来的内容
   直接拼 prompt、**不过身份防伪**。记忆跨会话持久化 + 可导出成包 = 新增泄漏面与
   注入面，这两处我们不能跟着抄。
4. **顺手发现一个现存缺陷**：记忆面没有接 pii（`memory_svc.go` 的 import 面可证），
   会话终稿蒸馏出的 `assertion`/`quote` 逐字落库。建议**单列一批先修**，不等新模块。
   ✅ **宿主侧已于 2026-10-19 清偿**（扁鹊仓提交 `ad3ada51`：在「构造事实卡」这个
   动作上收口，先脱敏再算签名，用例 `TestMemoryRedactedBeforeStore` 钉住）。
   仍未做的是**接入本仓的 `capture` 流水线**——见 §7.1 与
   [`HOST_WIRING_20261020.md`](HOST_WIRING_20261020.md) §4。
5. **下一步 M0 建仓**（go.mod + 域词门禁 + types + store 接口），依赖面已核实可离线取到
   `modernc.org/sqlite`（纯 Go，满足 `CGO_ENABLED=0`；现钉 `v1.40.1`）。
   ✅ M0–M6 已落地（当前 `v0.5.1`），M7 见 §6.1–§6.3。

---

## 一、三方底牌

### 1.1 octop-memory 是什么

**定位**（README 原话）："Your host manages agent execution and model scheduling;
Octop Memory handles capture, extraction, recall, storage, and migration."

| 形态 | 事实 |
|---|---|
| 语言/依赖 | Python 3.12+，**核心零依赖**（stdlib + `sqlite3`/FTS5）；PG、向量、LangGraph checkpoint、CLI 均为可选 |
| 体量 | `src/` 33,479 行 Python，280 文件，86 个测试文件 |
| 存储 | SQLite（表前缀隔离 namespace）+ PostgreSQL（namespace-first 键），11 张表 + FTS |
| 模型 | **不内置**。抽取/晋升检查/页重生成都用注入的 `LLMClient`（`ports/llm`），无模型时手工 store + 词法召回照样能用 |
| 向量 | Chroma/Qdrant 接口存在但**默认关闭、测试全为 fake**，README 自承真实集成未验证 |

**分层**（README 的 Code architecture 直译）：

```
adapters(bridge/cli/dashboard) → application(MemoryRuntime/config/文件投影)
        → pipeline(extractor/promotion/recall/page/episode/lifecycle)
        → storage(sqlite/postgres/checkpoint/vector)；ports 只放接口
```

**长期记忆数据流**：

```
RawEvent ──extraction──→ Candidate ──promotion──→ AtomCard ──dirty/regen──→ EntityPage
    └──episode extraction──→ Episode              └──atom_id 引用──→ tree leaf
```

树是**组织视图**，leaf 只引用 atom，不是第二份副本、也不是独立召回源。

**召回全链**（`recall_for_prompt`，八段串行）：
`router → multi-source → rerank → diversify → suppress → budget → thread_state → cache/timeout`

- router：默认路由 `atom` + `raw`，解析出实体时加 `page_headline`，配了向量再加 `vector`；
  默认 raw 兜底策略下**持久命中抑制 raw**，当前会话/线程的 raw **不进 prompt**（防回声）。
- budget：CJK 2 字符/token、ASCII 4 字符/token 的静态代理（±10-20%），
  **分桶配额 atoms 70% / quotes 20% / page_headline 10%**，总量缺省 1500 token。
- thread_state：`thread_active_entities` 活跃实体栈，**LRU 淘汰 + pin-time + 重启存活**，
  回答「『那个』指谁」。

**晋升 5 检查**（`promotion/checks.py`，全是纯函数 + 注入协议）：

| # | 检查 | 注入协议 |
|---|---|---|
| 1 | 长期价值 `check_value` | — |
| 2 | 证据 `check_evidence` | `EvidenceLookup` |
| 3 | 实体归一 `check_entity` | `EntityResolver` + `LLMEscalationHook` |
| 4 | 重复 `check_duplicate` | 存储侧 `find_atom_by_signature` |
| 5 | 矛盾 `check_conflict` | 否定词表（与抽取器共用同一份词表） |

晋升结果不止「过/不过」：可 promote / merge / drop，**留 review 队列给用户处置**。

**治理面**（对标本仓的架构门禁，值得抄的两条）：
- **双后端 parity 门禁**：任何 public 读写、schema、SQL、事务、FTS 改动，
  两种后端同步维护 + 同一组行为断言，且 PG 用**真实测试服务器**（`TEST_POSTGRES_DSN`），
  「skip 的 PG 用例不证明兼容」。
- **provenance 结构化**：合成的运行期错误必须标 `payload.octop_runtime_error=true`，
  自动捕获据此排除；**在线过滤只看结构化来源，不做意图关键词黑名单**。
  这一条与本仓 AGENTS.md「不要用裸数字/关键词匹配做判定」是同一条纪律的不同面。

### 1.2 扁鹊已有的记忆平面

| 面 | 位置 | 体量 |
|---|---|---|
| IDL | `idl/memory.thrift` | 93 行 |
| 生成码 | `biz/model/memory/memory.go` | 3,823 行 |
| 服务 | `internal/service/memory_svc.go` | 896 行 |
| 仓储 | `internal/repo/repo_memory.go` | — |
| 模型 | `internal/model/memory.go` | — |
| 表 | `bq_memory_atom` / `bq_memory_subject_page`（批次八十一） | — |
| 注入面 | `runner.go:117` `memoryRecall(sessionID, host, cluster)`；`assemble.go` 段头 | — |

关键常量（`memory_svc.go:29-45`）：蒸馏模型 `platform/memory-distill`、
召回预算 `1200` runes、上限 `12` 条、相似合并阈值 `0.45`、扫描上限 `300`、Top `10`。

**已具备**：会话终稿蒸馏 → 断言抽取 → 规则优先晋升（签名去重 / 高相似合并 /
疑似矛盾送审 `needs_review` / `superseded_by` 软退役）→ 按 subject 召回注入
（带防回声段头）→ `memory_search` 工具 → 管理面 CRUD。

**领域绑定**（这是要拆掉的部分）：`subject_kind ∈ {host, cluster}`、
`source_case_id`（CaseStep 是宿主资产）、`team_id`、存储走 gorm/PG。

### 1.3 逐项对标

| octop 能力 | 扁鹊现状 | agent-memory 落点 | 判定 |
|---|---|---|---|
| RawEvent 捕获层 | ❌ 无显式事件层（终稿直接蒸馏） | `types.RawEvent` + `pipeline/capture` | **抽** |
| Candidate 提炼（LLM 注入 + 防稀释解析） | ✅ Distill | `pipeline/extractor` | 搬 |
| 晋升 1 长期价值 | ❌ | `pipeline/promotion` | 补 |
| 晋升 2 证据校验 | ⚠️ 只存 quote，不校验 | `pipeline/promotion` | 补 |
| 晋升 3 **实体归一** | ❌（host/cluster 两态硬编码） | `types.Entity/Alias` + `pipeline/promotion` | 补（**最大缺口**） |
| 晋升 4 去重 | ✅ 签名去重 | `pipeline/promotion` | 搬 |
| 晋升 5 矛盾送审 | ✅ `needs_review` + 软退役 | `pipeline/promotion` | 搬 |
| AtomCard（superseded/deprecated/search_terms） | ✅ `bq_memory_atom` | `types.AtomCard` | 搬 |
| Entity + Alias 归一 | ❌ | `types` + `pipeline/promotion` | 补 |
| EntityPage 脏标记 + 重生成 | ⚠️ 有 `bq_memory_subject_page`，无脏标记/重生成/失败态 | `pipeline/page` | 补机制 |
| Episode + 日/周/月 digest | ❌ | `pipeline/episode` | 补 |
| Journal | ⚠️ auditlog 部分覆盖 | `store` + `lifecycle` | 补 |
| recall router（atom/raw/page_headline/vector） | ❌（只有 subject 过滤） | `pipeline/recall` | 补 |
| recall rerank + diversify | ⚠️ importance 单键排序 | `pipeline/recall` | 补 |
| recall suppress（防回声） | ✅ 段头单源 | `pipeline/recall` | 搬 + 补 |
| recall **budget 分桶配额** | ⚠️ 手搓 1200 runes + 12 条 | `pipeline/recall` + **借道 harness/budget** | 借道 |
| recall thread_state（代词消解栈） | ❌ | `pipeline/recall` | 补 |
| recall cache + timeout | ❌ | `pipeline/recall` | 补 |
| SQLite FTS5 + PG 双后端 + parity 门禁 | ⚠️ gorm PG，无 FTS | `storage/` | 补 |
| namespace 隔离 | ⚠️ 单列 `team_id` | `storage/` | 补 |
| portable `.hmpkg`（pack/adopt/doctor） | ❌ | `operations/portable` | 补（**待拍板**，见 §7.2） |
| bridge JSON-RPC | ❌（走 hz HTTP + MCP 工具） | `adapters/bridge` | 补 |
| 虚拟路径投影（atom/page/raw ↔ `.md`） | ❌ | `application` | 补 |
| 宿主手改文件对账（`MEMORY.md`/`USER.md`） | ❌ | `application` | 补 |
| provenance 排除运行期错误 | ❌ | `types` + `capture` | 补 |
| vector index | ⚠️ 有 Milvus RAG 面（**另一件事**） | `ports` | **刻意不做**，见 §五 |

---

## 二、包布局与依赖方向

```
agent-memory/
├── types/          AtomCard/Candidate/RawEvent/Entity/Alias/EntityPage/
│                   Episode/ActiveEntity/JournalEntry/DigestRecord/Provenance
│                   （**不声明 MemoryNode**——树刻意不做，留着它等于让人以为
│                    那是个待启用的能力）
├── ports/          LLM / Embedder / VectorIndex / Redactor / Bander /
│                   TokenEstimator / Budget —— 接口 + 纯 stdlib 默认实现
├── store/          8 个消费面小接口 + Store 聚合 + RawWriter + 事务语义
│                   （对标 MemoryBackend Protocol）+ lexical.go 口径钉子
├── storage/        sqlite（FTS5 + 单套表 + namespace 列）/ postgres（ns-first 键 + FTS）
├── pipeline/       capture → extractor → promotion → recall → page → episode → entity
├── lifecycle/      gc / consolidate / vacuum / retention
├── operations/     portable 包 + import/export
├── application/    Service 装配（服务面只做 capture/extract/promote/recall/search）
├── adapters/       bridge（JSON-RPC over stdio）/ cli / llm（OpenAI 兼容端点）
├── cmd/memory/     CLI 入口
├── integration/    跨包端到端测试
├── tools/archcheck/ 门禁工具（**独立模块**，好让 YAML 依赖不进核心）
└── architecture-policy.yaml   规则源（声明面 + 可执行面）
```

**依赖方向（单向、无环）**：

```
adapters → application → pipeline → store(接口) ← storage(实现)
                             ↓          ↑
                           types ←─── ports(接口)
lifecycle / operations 与 pipeline 平级
```

> 方向的**权威来源是 `architecture-policy.yaml` 的 `modules` 段**（22 个模块登记），
> 不是这张图。改依赖方向先改 policy（policy 先行），门禁按 policy 判。

**零核心依赖纪律**（octop 的核心零依赖在 Go 侧的等价物）：
核心不引 LLM SDK、不引 embedding SDK、不引 HTTP 框架、不引编排包。
只用 stdlib + `database/sql` + `modernc.org/sqlite`（**纯 Go，满足本仓
`CGO_ENABLED=0` 硬要求**；现钉 `v1.40.1`，含 FTS5，实测 SQLite 3.50.4）。
第三方进核心须在 policy 的 `allow_third_party` 段逐条登记（见 §8.4）。

**门禁**（对标 harness 的 `harness-no-domain-vocabulary`）：

| 规则 | 判据 |
|---|---|
| `memory-no-domain-vocabulary` | `host`/`cluster`/`pod`/`case`/巡检/专家/主机/集群/命名空间 等词进代码即红（注释与测试数据同样计） |
| `memory-no-harness-dependency` | agent-memory 不得 import agent-harness——两者是**平级内核**，不是父子 |
| `memory-store-single-source` | 方言 SQL 只许出现在 `storage/`，pipeline 不得出现 SQL |

**宿主投影手法**（沿用 SPLIT_PLAN §一.4 的 `EdgeSource` 同构解法）：
扁鹊的 `subject_kind: host|cluster` 换成通用 `Entity{Type, CanonicalName, Aliases}`，
由**宿主把运维对象投影成 entity**。这一步把「记忆平面认识运维对象」翻成
「宿主把运维对象交给记忆平面」，是整个迁移里工作量最大、收益也最大的一步。

---

## 三、下沉裁定：agent-harness 哪些该下沉

**判定标准沿用 SPLIT_PLAN**：*这个包需要知道自己在给谁干活吗？* 需要 → 留宿主。

四类判定：**下沉**（实现搬走）/ **借道**（不搬，单向依赖）/ **接线**（两仓共接同一机制，
不是搬）/ **不动**。

| 包 | 非测试行 | 裁定 | 理由 |
|---|---|---|---|
| `budget` | 756 | ~~**借道**~~ → **实装改为：不借道，召回自带分桶配额器**（2026-10-19 M2 实弹推翻） | 原判据是「同一个分段账本，两个作用域」。**实现时发现两条都对**：① 两边词表不同——harness 的分段是 System/Tools/Conversation（prompt 组装面），召回的分桶是 atoms/quotes/headlines（选择面）；② 借道会同时撞破本仓两条门禁红线（`memory-no-harness-dependency` + `memory-core-stdlib-only`）。共享的是**纪律**（先计量、再在上限内分配），不是代码。`pipeline/recall` 自带 `allocate()`，计量走 `ports.TokenEstimator` 注入 |
| `pii` | 827 | ~~**接线**~~ → **实装改为：不借道，记忆自带 `ports.Redactor` 端口 + 结构性兵底**（2026-10-19 M3 实弹） | octop 的 privacy 只有 `store_raw_content`/`store_tool_payloads` 两个开关式丢弃，**没有内容级打码**。记忆跨会话持久化 + 可导出成包 = 新增泄漏面。裁定：capture 管线挂脱敏处置面，**落库前打码**（不是注入前打码——落库后打码等于已泄漏）。这是本仓强于 octop 的一处。

**实弹（M3）**：与 budget 同理不借道——harness 的 pii 是「策略注册表 + 执行面」的完整平面，而记忆要的是一个「落库前必过一过」的窄接口，两者是不同形态，且借道撞破同样的两条门禁红线。改为 `ports.Redactor`（**必需依赖**，`capture.New` 传 nil 直接失败）+ `ports.PatternRedactor`（只认形态的兵底）。兵底的收录标准三条同时成立：① 不认识领域词；② 误报代价只是少几个字符；③ **漏报代价不可逆**（凭据一旦落库，要靠轮换补救，而轮换往往来不及） |
| `reminder` | 30 | ~~**接线（改引 guardx，不经本门面）**~~ → **实装改为：不借，转义归本仓 `ports.MemoryBand`**（2026-10-19 M2 实弹推翻，见 §4.3） | recall 内容跨会话/跨用户/跨宿主 = **不可信内容**，直拼 prompt = 提示注入。原判据是「核心 `injectband` 已在 guardx/tier 0，直接引 guardx」——**实装时被用例打穿**：它的 `tagRe` 写死、只认 `<system-reminder>` 家族，拿它守 `<memory-context>` 时伪造的闭合标签原样穿过。集成与依赖一并撤掉，转义归本仓（转义全部 `<`，一次挡住两类伪造） |
| `pending` | 62 | **不合并，只抄机制** | octop 的 `thread_active_entities` 回答「『那个』指谁」= 召回期代词消解栈；pending 是「上一轮问了什么」= 跨轮挂起态载体。表面都是跨轮状态，**生命周期相反**（前者随召回滑动淘汰，后者等用户答复才结算）。同 AGENTS.md「意图两个 LLM 选路面不合并」的判据。但 thread_state 的 **LRU + pin-time + 重启存活**三条机制值得整体抄 |
| `checkpoint` | 545 | **不下沉** | octop 把它与长期记忆同库，是因为它的宿主是 LangGraph。本仓 checkpoint 是跨进程 CAS + SeqGate，服务「续跑不重放」，记忆服务「记住事实」；且 AGENTS.md 已记本仓链是**含并行分支的 DAG**，`BuildPlan` 的线性连续前缀模型不成立。可共用同一个 SQLite 文件/连接池，但**表前缀独立、包不合并** |
| `spill` | 251 | **不下沉（语义相反）** | spill 管输出溢出的截断落盘，记忆管值得留的。预算面互补（预算不够先 recall 补料、再 spill 截断），实现不共享 |
| `offload` | 589 | **暂不接** | 附件内容寻址落盘。记忆要不要存附件 = 多模态进上下文那天再定（同 AGENTS.md 已记的 offload 未引理由） |
| 其余 16 个包 | ~8.6k | **不属记忆，全留 harness** | `protocol`/`eventbus`/`statemachine`/`commitgate`/`contract`/`dispatch`/`ptc`/`watchdog`/`delta`/`drain`/`netguard`/`policy`/`toolfail`/`tooldisclose`/`guardset`——编排与安全面。`delta`/`pending` 已被 harness 抽过一次；再抽进 memory 等于把编排机制拆到两个内核仓，破坏 harness 的单一定位 |

**一句话结论**：从 harness「下沉」的东西接近于零——**真正要搬的是扁鹊自己的记忆平面**
（`repo_memory.go` + `memory_svc.go` 的机制面）。
⚠️ harness 侧最终是 **0 个借道 + 0 个接线**（三处原裁定都被实弹推翻，见上表）。
「共享的是纪律，不是代码」这条是本节最贵的结论。

---

## 四、我们比 octop 强 / 必须补的两处

> 这两处偏离全部有「不接会怎样」的用例钉住。其中一处（转义借 guardx）实现期间
> 被用例当场打穿，过程与结论记在 §4.3 与 ports/redact.go。
> ⚠️ 标题曾写成「三处」——第三处（召回的存储依赖面收窄成 `recall.Reader`）
> 是**本仓自己的工程纪律**，不是对 octop 的偏离，故不计入本表。

| 处 | octop 现状 | 本仓要求 |
|---|---|---|
| **落库前脱敏** | privacy 只有开关式丢弃，无内容级打码 | capture 管线挂脱敏处置面；**打码在落库前**，否则导出即泄漏 |
| **召回注入防伪** | recall 输出直接拼 prompt，无身份防伪 | recall 结果统一过 `ports.MemoryBand` 数据区；记忆内容标记为数据区，不得被当指令 |

这两处不是「对齐 octop」，是**明确偏离 octop**。写进 `types`/`ports`/`recall` 的包注释，
并各配一个把「不接会怎样」的用例钉住——否则后人会看着 octop 把它们改回去。

### 4.3 转义必须匹配**实际在用的那条 band**（M2 实弹，代价是一个被拆掉的集成）

方案 §三 原写「召回注入防伪 → 改引 guardx，不经 harness 门面」。M2 实现时照做了：
建 `integrations/guardx`，把 `ports.Bander` 接到 `injectband.SanitizeBody` 上——
理由是「转义是通用安全原语，单源在 guardx，本仓不重复造」。

**用例当场打穿了它**。`injectband.SanitizeBody` 的实现是

```go
func SanitizeBody(body string) string {
    return tagRe.ReplaceAllStringFunc(body, func(m string) string {
        return strings.Replace(m, "<", "&lt;", 1)
    })
}
```

`tagRe` 是写死的、**只匹配 `<system-reminder…>` 家族**。它守得住的只有它自己那条
band；拿它守 `<memory-context>` 时，一句 `</memory-context>` 原样穿过——攻击者提前
闭合数据区把内容搬出去，还顺手在后面自称系统注入。

> **教训（值得单独立一条纪律）**：转义规则与「实际在用的那条 band」是**一对**。
> 借 A 的规则去守 B，等于没有转义。「不重复造轮子」在这里必须让位给
> 「转义必须匹配实际 band」——门禁这关守的是**安全性质**，不是代码量。

处置：**集成与依赖一并撤掉**，转义归 `ports.MemoryBand` 自己所有（转义全部 `<`，
一次同时挡住两类伪造）。`ports/band_test.go` 留 9 条攻击载荷用例钉住这件事，
并在文件头写明「曾打算借 guardx，被本用例打穿」。

### 4.4 召回的依赖面收窄成 `recall.Reader`（M2 兑现自己的纪律）

`store/store.go` 的包注释写着「阶段只声明自己需要的子接口」，但 M1 的后端全量实现了
`store.Store`。M2 兑现这条：召回只用得到 5 个存储方法，于是声明 `recall.Reader`
（`ListAtoms` / `SearchAtoms` / `GetEntityPage` / `UpsertActiveEntity` /
`EvictActiveEntities`）。量化收益：召回的单元测试写 5 个方法而不是 40 个，
且「召回依赖了存储的哪些面」在类型上直接可见。

原始事件面**另立** `recall.RawReader`（可选，1 个方法），理由见 §7.2.1
第二条「无捕获面形态」——那是**契约洗白**问题，不是接口数量问题。

### 4.5 夹具是为实现量身写的：实现错了夹具也会绿（深度评审的元教训，issue #7）

> 这条来自 2026-10-19 的深度评审（结论见 issue #7）。前面那些缺陷都是**点状**的，
> 这一条是**元教训**——它解释为什么其中一条（中文子串检索失效）能在跨后端断言
> 套件下**全绿**。

`storage/parity` 是本仓质量最高的资产（跨后端共享行为断言、与实现同仓）。
但它的**夹具一度是照着实现写的**：

```
评审当时：  "磁盘 剩余 2G"   ← 汉字之间有空格，unicode61 正好切成两个词元
            "磁盘长期偏小"     ← 真实中文形态，但只用在去重签名上，从未进过检索断言
```

于是「unicode61 把连续汉字算作一个词元、查「磁盘」搜不到「磁盘长期偏小」」
这条缺陷在 parity 套件下**全绿**。门禁侧有「坏代码必须红」的自证用例，
**parity 夹具侧没有对应的「坏实现必须红」**。

> **纪律**：每加一条跨后端断言，问一次——
> **这条断言在实现是错的时候会不会红？**
> 夹具是为实现量身写的，实现错了夹具也会绿。

**已落地的修法**：parity 的中文夹具一律用**真实形态**（句子内部没有空格分词），
于是 unicode61 把连续汉字当一个词元这件事直接暴露成 0 命中。
改完之后该缺陷在 parity 套件下变红，随后被修掉（issue #4），
用例即 `storage/parity/parity_atoms.go` 里那条中文检索断言。

**延伸两处同族**（都是同一条纪律的变体）：

1. **断言在两种情形下结果相同时，断言等于没断言**。例：某处把
   「查不到东西」当断言，而被测层把错误吞成空结果——于是
   「配得对」与「出错被吞」两种情形的可观测结果相同。
   修法：断言必须挑一个**「两种情形结果不同」**的输入。
2. **断言写成「实现现在的样子」时，实现错了断言也绿**。断言要写
   「应该怎样」，不是「现在怎样」。

### 4.6 记录级归属与排序注入缝（2026-10-20，issue #10 / #11）

两条都是**下游实测报上来的缺口**，形状不同，判据同源：
**「读起来像有那个能力，实际没有」**。

#### 4.6.1 记录级归属（issue #10）

**现象**：记忆模型只有整库级的 `Namespace` 隔离，没有**记录级**的归属。
下游要「全局事实 + 本组事实」两层语义，只能在取数之后**手工过滤**——
而手工过滤必然会漏一处（下游实测：注入面过滤了，检索工具面没过滤；
两处读同一张表、同一个字段，一处过滤一处不过滤）。

**为什么不能拿 Namespace 表达第二层**：那意味着「一条全局事实」得向每个组
复制一份——写放大，且全局事实一新增就得回头补所有组的副本。
那是不可行的建模，不是配置问题。

**裁定**：

1. `types.Scope` 是**记录级归属**，与 `Namespace`（整库级）正交：
   空 = 全局可见，非空 = 仅同归属可见。归属由宿主定义（本仓不枚举其种类）。
2. 归属**在捕获时定下**，事件带着它走，候选与事实卡从它继承，中途不让模型参与。
3. 归属**进签名**（`types.AtomSignature`）：签名不含归属的话，
   跨归属的重复检测会把一组的卡并进另一组的卡——那是内容泄漏，不是去重。
   为此全局卡的签名保持与加归属之前**逐字相同**（签名是存起来的列）。
4. 过滤**在取数层**（进 SQL，与 TopK 配额同一次求值）。后置过滤时，
   被别家占掉的配额已经花掉了，「该给我的那条」根本没进候选池——
   症状只是「召回变少了」，看着像排序问题，根因却在取数。
   召回层另有一道复查兜底（纵深防御：漏过滤的失效形态是**泄漏**不是少取）。
5. 过滤条件的**空值语义 = 不隔离**（与 `EntityIDs` 空=不限一致）。
   两种语义都自洽，失效方向相反（忘传 → 泄漏 / 少取）；
   本仓取「不隔离」，理由是同一份过滤条件里不该有两种空值语义。
6. 原始事件面**同样**按归属隔离：它是召回的兜底源，一次就取 `MaxRaw` 条，
   漏一条就是把别组的对话塞进 prompt。

**顺带逼出来的一件事**：本仓的建表脚本**没有 ALTER**，
所以「加一列」一直没有落地路径。加归属列逼出了**版本化迁移**
（`storage/{sqlite,postgres}/migrate.go`）：版本号记在 meta 表，
迁移按版本号线性追加，**每段必须幂等**，索引归迁移管。

#### 4.6.2 排序注入缝与向量位（issue #11）

**现象**：召回的排序是包级函数 + 写死权重（0.6/0.25/0.15），
`ports.Embedder` / `ports.VectorIndex` 是**死端口**（定义了但全模块零消费者），
`AtomCard` 上没有向量字段——走语义轨的下游只能**绕开整条链自造**，
于是两套排序口径并存。

**裁定**（**不**是「现在上向量轨」——那个判断本仓做得对）：

1. **排序可插拔**：`ports.Ranker` 是端口，缺省实现 `ports.LexicalRanker`
   （词面+重要度+新鲜度）。缺省下与注入前**逐条等价**——
   钉法是「把旧公式在测试里重写一遍当期望值」。
   契约只有一条要紧的：分数与候选**一一对应且同序**，长度不符即报错
   （错位只会静默换序，而换序在结果里完全看不出来）。
2. **向量有类型层的落脚点**：`types.AtomCard.Vector`（可选、默认不进持久化）。
   本仓两个后端**不落向量**——带着向量入库**响亮报错**，
   因为静默丢掉的话症状是「相似度检索永远空」，而它与「存错了地方」毫无相似之处。
3. **死端口标注 reserved**：`ports.Embedder` / `ports.VectorIndex` 的注释里
   写明「当前无消费点」。**一个定义了却零消费者的导出接口，
   读起来像能力，实际不是**——故写明，别让后人照着它接线。

**边界不变**：本仓仍**不消费**向量轨（§五）。「不接」说的是本仓不消费，
不是不许下游走语义轨——缝留出来就够了。

---

## 五、刻意不做（防止重复评估）

| 不做 | 理由 |
|---|---|
| **vector index** | octop 自己都默认关闭、测试全 fake、自承真实集成未验证。扁鹊已有 Milvus RAG 面但**那是知识检索**（`RAG_ASSESSMENT` 已定案：生产实态是纯 TF-IDF 本地面、语料 97% 是 mock 残留）。记忆的向量面等本地面稳定后再议。⚠️ 2026-10-20 补：**缝留出来了**（`ports.Ranker` 可注入、`types.AtomCard.Vector` 有落脚点、两个向量端口标注 reserved）——见 §4.6.2 |
| **tree root/branch/leaf** | 对运维域价值有限（host/cluster 两态够用）。tree 在 octop 里的价值是给「个人助理」组织大量实体用的；等真出现「同一事实挂多锚点」再启 |
| **LangGraph checkpoint 对标物** | 见 §3 的 checkpoint 裁定 |
| **dashboard** | octop 的 dashboard 直接访问 SQLite，README 自承「是既有例外、不是新模块模板」。本仓控制台另有其道，不复制这个反面样本 |

---

## 六、落地批次

| 批 | 内容 | 完成判据 |
|---|---|---|
| **M0** | 建仓：`go.mod` + policy 门禁 + `types` 全量结构 + `store`/`ports` 接口 + 空实现 | 门禁实跑红过一次（塞域词必红）；核心包 `go test` 绿 |
| **M1** | SQLite 后端：11 张表 + FTS5 + 事务 + 零 LLM 的 store/recall。**同时落 PG 骨架** | SQLite+PG 同组行为断言（对标 octop parity 门禁） |
| **M2** | 召回全链八段 + 防回声 + injectband + **budget 借道** | 「不接 injectband 会被注入」用例钉住 |
| **M3** | extractor + promotion 5 检查 + **pii 接线** | 落库前打码用例（断言存储层拿不到明文） |
| **M4** | Entity/Alias 归一 + EntityPage 脏标记重生成 + episode/digest + journal | 代词消解栈 LRU/pin-time/重启存活三用例 |
| **M5** | portable 包 + bridge JSON-RPC + CLI | `pack/adopt/doctor` 三条闭环（**范围待 §7.2 拍板**） |
| **M6** | lifecycle（gc/consolidate/vacuum/retention）+ provenance 排除运行期错误 | provenance 只看结构化来源，不含意图关键词黑名单 |
| **M7** | 扁鹊切流：`subject_kind` → Entity 投影 + `repo_memory` → `store` 适配层（**留门面保调用点**） | 扁鹊全量 test + e2e 绿 |

### 6.1 M7 的实际拆法（2026-10-19 落地）

M7 拆成两刀，因为**召回切流是面向生产 prompt 的行为面变更**，与「接缝铺好」
不是同一件事：

| 刀 | 内容 | 状态 |
|---|---|---|
| **M7-b1** | ① 本仓发版 v0.1.0（宿主按版本号引入，零本地 replace）；② 宿主新增实体表/别名表 + `subject_kind`→Entity 投影（标识**派生**非自增，可跨宿主重算）；③ 宿主 GORM 记忆面实现内核 `store.AtomStore` + `store.MetaStore` + `pipeline/entity.Store` 三片契约；④ 四条写入路径挂实体投影；⑤ 架构门禁加两条规则锁「内核依赖只经适配层进入」 | ✅ 宿主仓已落地 |
| **M7-b2** | 扁鹊召回改走内核 `pipeline/recall` 八段链 | ❌ **裁定不做**（2026-10-19，见 §6.2） |
| **M7-b3** | 扁鹊召回注入改用内核 `ports.MemoryBand` 数据区标记（结构性隔离，替掉散文级声明） | ✅ 宿主仓已落地 |

### 6.2 M7-b2「召回切流」裁定：**不做**（2026-10-19）

M7-b1 落地后，**M7 按 §六 的定义已达成**（`subject_kind` → Entity 投影 +
`repo_memory` → `store` 适配层，留门面保调用点，完成判据=扁鹊全量 test + e2e 绿）。
原计划的「M7-b2 = 召回改走内核八段链」经实查**不做**，三条理由：

1. **两边的召回模型相反，不是同一件事的两种实现**。内核（以及它对标的上游
   octop）都是**查询驱动**：`router` 默认路由 `atom` + `raw`，atom 源是**全库**，
   解析出实体时只**加** `page_headline`（见 §三 召回全链）。扁鹊是**锚点驱动**：
   只看当前会话锚点那个对象的事实 **+ 全局事实**。把扁鹊套进内核链，要么全库
   事实被注进诊断 prompt（把别的对象的事实也讲给模型，是噪声甚至误导），要么
   把锚点过滤藏进适配层的 `ListAtoms` 里——那是**契约洗白**（内核
   `store.AtomFilter` 的语义被悄悄改写，内核自己不知道）。

2. **收益对扁鹊几乎为零**。八段里对扁鹊有增量的是：去重（扁鹊写入期已按签名
   去重）、多样性裁剪（扁鹊锚定单个对象 + 全局事实，裁不掉什么）、防回声
   （扁鹊没有捕获面，无可回声）、bm25 相关性（单个对象的二十来条事实，重要性
   + 时序排序已经够用）。**真正有增量的是数据区包裹**——它单独做了（M7-b3）。

3. **全局事实在内核形状里没有位置**。扁鹊的召回刻意含**全局事实**
   （`subject_kind = global`，对一切对象生效）。内核的 `AtomCard.EntityID` 空串
   表示「无归属」，而 `store.AtomFilter.EntityIDs` 是**与**过滤——一旦按实体
   限定，全局事实会被一起滤掉。要接就得给内核加「含无归属事实」的语义，那是
   **内核语义变更**，不是接线。

**顺带记一条内核自己的契约空洞**（留给需要它的宿主，本次不改内核）：
`store.AtomFilter.EntityIDs`（存储层**已实现**过滤，见 `storage/sqlite/atom.go`）
与 `recall.Request.Entities`（宿主解析出的实体）**都存在，却在召回路径上互不相干**
——`gather` 只把 `Entities` 用于 `page_headline` 源，从不把它接到 atom 过滤上。
后果：**锚点驱动的宿主无法用内核链把事实限定在某个实体上**。这是内核的缺口，
不是扁鹊的；补法是给 `recall.Config` 加一个显式开关（不能「有 Entities 就限定」：
宿主解析实体的两种场合语义相反——「本轮提到的实体」是给画像桶补页，
「本会话的锚点实体」才是给事实卡划范围）。本次无消费方，不做。

**另一条同源的空洞**（同样留给宿主）：`recall.Reader` 要求宿主实现 `ListRaw`
（捕获面），而**只有采集原始事件的主机**才有捕获面——只蒸馏终稿的主机没有。
本仓的 `capture` 阶段（落库前打码）正是给这类主机准备的，但召回链没有「无捕获面」
的降级形态。


M7-b1 落定三条**刻意偏离**（写在宿主适配层的包注释里，勿在别处"修"回去）：

1. **事实卡签名不换**。存量唯一索引是 `repo.MemorySignature`（锚点+kind+断言），
   内核的 `AtomCard.Signature()` 是（实体+断言），语义不同——换了就是存量去重键
   变更，同一件事会多出一张卡。
2. **`QuoteEventID` 不是捕获事件标识**。内核不变量要求「出处能解析到一条捕获
   事件」，宿主没有捕获面，故映射到宿主会话/案例标识——**不满足该不变量**，
   接入 capture 面后才成立。
3. **实体计数不增量维护**。内核契约给的是 `BumpEntityAtomCount(delta)`，宿主
   实现为**重算**：增量的漂移不可自愈（漏一次加一次就永远差一，且无任何症状），
   重算永远正确。

宿主侧依赖地板的两处**非自主**抬升（本地 replace `../agent-harness` 随上游发版
被抬，已按房规精确同步、零 tidy）：`oagentkit/guardx` v0.4.0→v0.4.1→v0.5.0、
`oagentkit/agent` v0.8.0→v0.10.0 等八模块。

**地板抬升的连带面**：`oekit/pkg` v0.15.1 把 `uidgen` 挪进了该模块的 `kitx/uidgen`
子目录，模块路径不变、只是子路径变了。于是宿主源码里四处旧导入
`github.com/oekit/pkg/uidgen` 在这个版本**不存在**——只抬 `go.mod` 版本而不改
import 路径，仓库会停在「编译不过」，且报错（`cannot find module providing
package`）完全指不到真正成因（**包被拆了**，不是模块找不到）。已同批修掉并提交
（宿主仓 `129c6c03`，每文件一行 import）。

### 6.3 接缝自查发现的四条（2026-10-19，宿主侧）

M7-b1/b3 落地后对适配层做了一轮逐行自查，查出四条**契约面**的问题。四条都在宿主
侧修，但其中一条是**内核的契约空洞**，内核自己改不改要另行评估（本次不改）。

1. **归属迁移是 `entity.Store` 的明文契约**。内核 `pipeline/entity` 的接口注释
   写得很直白：「`SaveAtom` 写回事实卡（合并时要改归属）」，`entity.Resolver.Merge`
   的实现就是改 `a.EntityID` 后 `SaveAtom` 回去。宿主适配器原本**只改内容不改归属**
   ——后果是归一合并在宿主侧**静默失效**：卡留在旧实体上，症状是「两个实体各自
   记着同一条事实」，在内容上完全看不出来。修：锚点变了要改
   `(subject_kind, subject)`、按**存量口径**重算签名（签名含锚点，不重算则去重键
   与归属对不上，同一条事实在新锚点下会多出一张卡）、实体投影跟着迁。

2. **第三条内核契约空洞（本次新增记录）**：`store.AtomStore.SaveAtom` 的**退役
   状态是「随卡写回」的**。内核 SQLite 实现是
   `deprecated_at = excluded.deprecated_at, superseded_by = excluded.superseded_by`
   （`storage/sqlite/atom.go`）——即**入卡不带退役信息，就会把退役清空**。内核自己
   的读改写路径（晋升/生命周期）都带得全，但**跨宿主适配器一旦在映射时丢了退役
   信息，已退役的事实就会被复活**。宿主形状尤其危险：它没有独立的「退役时刻」
   列，只能拿 `updated_at` 代偿，而那一列**可能为零**（手工造的行、老库回填）。
   宿主侧对策两条：映射时退役时刻必须有**非零兜底**（`updated_at` → `created_at`
   → 纪元时刻——零值在 `Active()` 眼里就是「从未退役」）；写回时**状态只进不退**
   （库里那份是权威，只有来件显式带退役信息才允许改状态）。

   这条对**任何**接内核的宿主都成立，与宿主形状无关：读出来的卡**必须**带全退役
   字段再写回去，否则就是复活。

3. **宿主四态 ↔ 内核两态**（新增第四条刻意偏离）。宿主事实卡有四态
   （`active` / `needs_review` / `superseded` / `deprecated`），内核事实卡只有两个
   退役字段（`SupersededBy` + `DeprecatedAt`）。映射：`active`↔活跃；
   `needs_review`↔`DeprecatedAt`（待裁决**不进召回集**，与退役同效——宿主侧的
   召回只取 `active`，把它当活跃等于让待裁决的卡绕过人审直接进 prompt）；
   `superseded`↔`SupersededBy`；`deprecated`↔`DeprecatedAt`。回写**不还原**
   `needs_review`（内核形状里没有这一态），状态**只进不退**。
   宿主侧单枚举列还表达不了内核的「既被推翻又被人工作废」两态组合——
   内核那两列是独立的，宿主这一列只能存一个，取 `superseded` 优先（两者都非
   活跃，取哪个都不影响「不参与召回」这个判断）。

4. **来源性质的映射要双向可逆**。宿主 `created_by` 只有三档
   （`distill` / `user` / `event`），内核 `Provenance.Origin` 有五档
   （model / operator / user / external / tool）。宿主原本把内核写的卡一律记成
   `event`（外部事件沉淀）——**审计链指错人**：内核写出的卡来自蒸馏/晋升链，应记
   `distill`。修法是加 `createdByOf`（`originOf` 的**逆**映射），并让「没标来源」
   按内核写卡的实际路径取 `distill`。
   往返口径：`model`→`distill`→`model`、`operator`→`user`→`operator`、
   `external`→`event`→`external` 三档**无损**；`user`→`user`→`operator`、
   `tool`→`event`→`external` 两档**有损**（宿主无槽位，回读时归到相邻档）。
   写明在这里，免得后来人以为宿主能无损承载来源性质。

---

## 七、风险与未决

### 7.1 现存缺陷（宿主侧：**已清偿，但捕获面仍未接**）

> ✅ **宿主侧已于 2026-10-19 清偿**（宿主仓提交 `ad3ada51`）。
>
> 修法不是把蒸馏产物改走 capture（那要先发布本仓并让宿主依赖它，是 M7 的事），
> 而是**在宿主自己的「构造事实卡」这个动作上收口**：CreateManual /
> CreateFromEvent / promote 三条写入路径都经过它，用宿主既有的两层脱敏
> （logredact 凭据类 + pii 个人信息价值面），与告警/审计/工单三条出口面同款口径。
>
> 落 service 层而不是 repo：架构策略的 repo 模块 requires 不含 pii
> （repo 不得上溯业务面），service 已有 pii。
>
> **先脱敏再算签名**（签名由 repo 用落库的断言现算）：签名的用途是对**存下来的**
> 事实去重，而存下来的是脱敏后的文本。副作用是「两条断言只差一个密钥」会被判成
> 同一条——那正是我们要的：差别只在我们要不存的密钥上。
>
> 用例 `TestMemoryRedactedBeforeStore` 钉住「从库里读出来也是脱敏后的」，
> 不靠内存态。

**记忆面没有接 pii**。证据：`internal/service/memory_svc.go` 的 import 面只有
`runner`/`llm`/`model`/`repo`/eino/`gorm`，**无 pii**。会话终稿蒸馏出的
`assertion` 与**逐字 `quote`** 直接落 `bq_memory_atom`。
删除权有（`repo_userdata.go` 的 `memory_atoms_derived` 覆盖用户数据导出/删除），
但**落库那一刻没有打码**。这是记忆平面从「单会话内缓存」变成「跨会话持久资产」后
才出现的缺口，且 octop 同样没解——所以它不会因为「对标上游」而自动被修掉。

### 7.2 待拍板

| 事项 | 选项 | 影响 | 裁定 |
|---|---|---|---|
| portable `.hmpkg` 做不做 | A 全量对标 / B 降级为 JSON export-import / C 不做 | octop 的迁移面服务「记忆在宿主间搬家」；扁鹊目前是单宿主 + team 维度，**没有第二个宿主**。无第二宿主时 B 已够，A 是为未来付费 | **已做 A**（2026-10-19 落地并发布，v0.2.0 起）。当时按「B 已够」做的取舍，后来实测迁移面确有两个消费方，A 的成本没有想象中高 |
| 召回是否走 vector | A 不做 / B 接 Milvus | 见 §5；建议 A，等 RAG 本地面稳 | **A，维持**（见 §5；上游自承「真实集成未验证」，本仓的向量面同样默认关） |

### 7.2.1 补记：范围与形态两条召回契约（2026-10-19，issue #8）

评审挖出两条**契约空洞**，都已补上，裁定如下。

**一、事实卡按宿主实体限定——`recall.Config.RestrictAtomsToEntities`（缺省 false）**

`store.AtomFilter.EntityIDs` 与 `recall.Request.Entities` 都存在，却在召回路径上互不相干：
宿主传了实体，内核照收不误，召回回来的是**全库事实**。

但**不能无条件接上过滤**：`Request.Entities` 这个字段同时承载两种**语义相反**的用法——

- 「本轮**提到**的实体」：用来补画像页，**不该**收窄事实范围；
- 「本会话的**锚点**实体」：用来给事实卡划范围。

内核从字段本身**区分不了**二者。故给一个**显式开关**由宿主声明
「这一轮的 Entities 就是事实边界」，缺省保持不过滤（既有调用方一行不改）。
接错了的后果是「本轮随口提到的一个对象，把整个会话的事实集清空」。

**空值语义是要点**：开关为真而 `Entities` 为空 → **返回空集**，不是全库。
「显式声明了范围但范围为空」与「没声明范围」是两种不同的意图；
退化成全库会把「限定」变成「装饰」——而宿主正是靠它挡全库事实的。

**二、无捕获面形态——`recall.Config.NoCaptureFace`（缺省 false）**

只有**采集原始事件**的宿主才有捕获面；只蒸馏终稿的宿主没有。
让那种宿主实现 `ListRaw` 只能给一个「永远返回空集」的桩——
那是**契约洗白**（声称支持捕获面，实际不支持），
而内核会据此在 raw 源上做出「有原始事件」的假设。

故把 `ListRaw` 从 `recall.Reader` 拆出来成为**可选**的 `recall.RawReader`：

- 存储实现了它 → raw 源照常进路由；
- 没实现而配置又要 raw 源 → `recall.New` **在构造期报错**（不是静默空集）；
- `NoCaptureFace = true` → raw 源**不出现在路由里**，且该开关**优先于** `Sources`。

**三、库管理面挂 bridge（2026-10-19 补记）**

页面重生成、情节抽取、周期摘要**都需要模型**，而服务面刻意不装它们
（见 `application` 的包注释）。它们此前在本仓内**没有跨进程入口**——
跨语言宿主（不必是 Go）触达不到。

裁定：挂到 **bridge**（`memory.page.regen` / `memory.episode.extract` /
`memory.episode.digest`），以独立的 `bridge.Management` 结构装配，
**不动服务面**。未装配时那三个方法返回 `CodeUnavailable`（能力未装配），
而不是「装了个永远返回空集的桩」。

---

### 7.3 双后端成本

octop 的 PG 实现 2,416 行 + 真实 `TEST_POSTGRES_DSN` 门禁。扁鹊记忆面现役在 PG 上，
若 M0–M2 只做 SQLite，**M7 切流前必须补 PG**，否则是行为面回退。故 §六 把 PG 骨架放进 M1。

> **2026-10-19 补记**：PG 后端已从骨架落地为**完整实现**，31 条 parity 断言
> 对着真实 PostgreSQL 跑通，CI 加了 `postgres` service 每次推送都跑。
> 宿主改走本仓 PG 后端的三步见 `storage/postgres` 的包注释
> 「宿主如何改走本仓的 PG 后端」。
> 补 PG 实现时立刻撞出两条跨后端缺陷：**签名分隔符用 NUL**（PG 的 TEXT 禁止 0x00，
> 同一份签名在一个后端落库、另一个报编码错）与**重要度排序是字典序**
> （`TEXT` 列下 `'med' > 'low' > 'high'`，最重要的排最后）——
> **两条都是 SQLite 侧永远绿的**：契约的可移植性要靠第二个实现来暴露。

---

## 八、oekit / oagentkit 矩阵复用裁定（2026-10-19）

> 触发：以 <https://oekit.github.io/llms.txt> 与 <https://oagentkit.github.io/llms.txt>
> 两份清单为输入，评估能否用这两个矩阵简化本仓业务逻辑，并把矩阵不满足的地方
> 提为上游 issue。

### 8.1 先说结论

**「全面简化」这个目标本身不成立**，原因不是矩阵不够好，而是**本仓的业务逻辑
恰好是这两个矩阵不覆盖的那一层**。

- oekit / oagentkit 是**通用基础设施矩阵**（微工具集、HTTP/RPC、缓存、并发、
  熔断、日志、模型客户端、MCP、检索、Agent 编排）。它们**没有记忆内核**——
  捕获 / 提炼 / 晋升 / 召回 / 实体归一 / 页面重生成 / 情节摘要 / 迁移包 /
  生命周期这一整套语义，两个矩阵里一行都没有。
- 本仓 10317 行非测试代码里，真正属于「通用轮子」的是约 800~1000 行
  （TTL 缓存、手写排序、两份保序去重、手写日期边界、围栏剥离、掩码生成、
  CLI 分发样板、JSON-RPC 帧、zip 容器 + 清单 + 体检）。
  （体量数字记于 2026-10-19；此后本仓继续长，**现约 13k 行非测试代码**——
  判据是「通用轮子占比有限」，不是具体行数。）

故本轮的实际产出是：**采纳 6 处严格等价的替换 + 1 处真实能力补齐**，
**并把矩阵的缺口提为 8 条上游 issue**。业务语义一行未动——那部分本来就不该
被通用库接管。

### 8.2 依赖地板（决定谁能进核心的硬事实）

门禁 `memory-core-stdlib-only` 查的是**源码 import 面**，但依赖地板得先看模块
的 `go.mod`（模块级 require 会进模块图，即便不参与构建）。逐模块核对结果：

| 模块 | 版本 | 模块级 require | import 面 | 能否进核心 |
|---|---|---|---|---|
| `oekit/pkg` | v0.15.1 | **空** | 纯 stdlib | ✅ |
| `oekit/concurrency` | v0.17.1 | `oekit/pkg`、`golang.org/x/time` | 除 ratelimit/retry-httpx 外纯 stdlib | ⚠️ 仅个别包 |
| `oekit/core` | v0.9.2 | hertz、yaml.v3 | chain/transport/lifecycle/governance/network 纯 stdlib | ⚠️ 仅个别包 |
| `oekit/logx` | v0.7.1 | `oekit/core`（→hertz） | 根包纯 stdlib | ⚠️ |
| `oekit/timertask` | v0.1.9 | redis、logx… | **同包内带 redis** | ❌ |
| `oagentkit/textx` | v0.5.0 | `oekit/pkg` | 纯 stdlib | ✅ |
| `oagentkit/runtime` | v0.7.0 | `oekit/concurrency` | 随 concurrency | ⚠️ |
| `oagentkit/guardx` | v0.5.0 | `oekit/logx`、`mvdan.cc/sh/v3` | logredact/fence/egress/permgate 纯 stdlib | ⚠️ 单包可入 |
| `oagentkit/{llm,rag,obs,toolgate,skillkit}` | — | eino、milvus、hertz、yaml… | — | ❌ |
| `oagentkit/queue` | v0.6.0 | lib/pq | worker 后端无关、pglease 绑定 PG | ⚠️ |

> **本表是 2026-10-19 那一刻的依赖地板快照**——判据（模块级 require 与 import 面
> 的差别）仍然有效，**版本号已经过时**。本仓当前实际钉的是
> `oekit/pkg v0.17.0` / `oagentkit/textx v0.6.1` / `oagentkit/guardx v0.8.2`
> （见 `go.mod`），三个都在 policy 的 `allow_third_party` 段登记着。
> 重新评估某个模块能否进核心时，**看当下的 `go.mod`，别照抄本表的版本号**。

两条由此得出的纪律：

1. **矩阵不是「纯 stdlib 的集合」**。llms.txt 的措辞容易让人以为整包都是零依赖，
   实际 `logx → core → hertz`、`guardx → logx + mvdan.cc/sh` 都会把第三方拖进
   模块图。**判据要落到具体包的 import 面**。
2. **「包级不可拆」是真实存在的形态**：`timertask` 的本地后端与 Redis 后端同包，
   `import timertask` 即拉 redis——「有本地后端」在依赖面上并不存在。

### 8.3 采纳（6 处，均已落地并有用例钉住）

| # | 采纳 | 落点 | 等价性依据 |
|---|---|---|---|
| 1 | `oekit/pkg/timex` 的期初边界 | `pipeline/episode.Window` | `BeginningOfDay/Week/Month` 与原手写实现**逐字等价**（周一起始自然周、UTC）；`to` 侧**不取** `EndOfX`——那三个是闭区间末纳秒，而本仓窗口是半开区间 |
| 2 | `oekit/pkg/container.Set` | `pipeline/recall`（suppress）、`lifecycle`、`pipeline/promotion` | 替掉 1 处手写 `map[string]bool` 与 2 份保序去重；保序由切片维持，Set 只当「已见集合」 |
| 3 | `oekit/pkg/encoding.Sha256Hex` | `operations/portable` | 替掉 2 处手写 sha256+hex |
| 4 | `oagentkit/guardx/logredact` | `ports.PatternRedactor` | **补齐四类形态**（Authorization 头、裸 Bearer、裸 JWT、截断私钥）与四个键名；与本仓兵底**互补而非包含** |
| 5 | `oagentkit/textx/textutil.StripCodeFence` | `pipeline/extractor`、`pipeline/page` | 与原手写围栏剥离**逐条等价**；页面侧顺带补上围栏容错（原先每次模型重生成都失败一次） |
| 6 | `oekit/pkg` 门禁白名单机制 | `tools/archcheck` + policy | 见 §8.4 |

顺带（非 kit 驱动，但同批改）：`pipeline/recall` 的手写插入排序换成标准库
`sort.SliceStable`——原注释写的理由是「不引第三方包到核心」，而 `sort` **是标准库**，
那条理由本来就不成立。稳定性保留（同分片段的相对顺序是上游定的）。

### 8.4 门禁改动：受控开口而非废除

「核心零依赖」这条纪律的出发点是「SDK 那一类会拆掉降级路径」，而**纯 stdlib 的
工具级模块不拆**。故开的不是废除，是**受控开口**：

- policy 的 `memory-core-stdlib-only` 规则下新增 `allow_third_party` 段，
  每条登记 `path` + `reason` + `until` + `removal`，可选 `paths` 限目录；
- 门禁侧：路径匹配卡在路径分隔符上（`oekit/pkg` 不顺带放行 `oekit/pkg-evil`）、
  **到期即红**（`memory-allowlist-expired`）、**缺字段在加载期即失败**
  （缺字段的失效形态是「放行生效且永不过期」，危险的那侧）、
  **有白名单却无执行体在加载期即失败**（那是死条款）；
- 门禁自测新增 6 条，每条都测**坏代码上必须红**。

### 8.5 不采纳（含理由，防重复评估）

| 候选 | 不采纳的理由 |
|---|---|
| `oekit/pkg/cache.LocalCache` | ① `TTL>0` 时起后台 goroutine，撞「不引入宿主看不见的常驻件」；② 读命中返回**共享引用**，本仓必须返回副本（否则第二次命中即脏数据）；③ 淘汰是写序而非 LRU，热点靠读续命的场景不对。→ 已提 issue |
| `oekit/pkg/errors`（`BizError`/`Wrap`） | `BizError` 带 HTTP 语义，本仓是内核不是 HTTP 服务；`Chain`/`In`/`Append` 与标准库 `errors.Join` 重叠。存储层现有 `fmt.Errorf("…: %w", err)` 风格已一致 |
| `oagentkit/textx/textutil.EstTokensCJK` | 系数写死（≈1.5 CJK 字/token、3 ASCII 字符/token），与 octop 口径（2/4）差 **1.7 倍**——换过去等于同预算下召回条数少四成，是产品行为变更。→ 已提 issue |
| `oagentkit/textx/textutil.Similarity` | 二元组 Jaccard ≠ 本仓的词元 Jaccard，同一对「已修复/未修复」的分数从 0.5 掉到 0.33，矛盾检测的阈值 0.6 判定会翻转 |
| `oagentkit/guardx/injectband` | 早已裁定不借（转义规则与实际在用的 band 是一对，借错等于没有转义）。**本轮借的是 logredact 的规则表，与转义无关**——这条界线要守住 |
| `oagentkit/{llm,obs,rag}` | 本仓的模型与向量化能力一律 `ports` 注入，内核不拥有模型客户端；向量面刻意不做 |
| `oekit/concurrency/retry` | 本仓 SQLite 已设 `PRAGMA busy_timeout = 5000`，忙等问题已就地解决，引入退避重试是重复投资 |
| `oagentkit/queue` | 实测 `worker` 是**后端无关**的（`TaskQueue`/`LeaseStore` 由调用方实现），无缺口；且本仓候选表无「running + 心跳」态，与队列模型不对位 |
| `oekit/core/lifecycle.Manager` | 进程级关停编排；本仓 CLI 是短命的、bridge 读到 EOF 即退，用不上。而 `lifecycle` 包名撞车会让人分不清 `lifecycle.GC` 与 `lifecycle.Manager` |

### 8.6 提给上游的 issue（8 条）

| 仓 | issue | 要点 |
|---|---|---|
| oekit/pkg | [#24](https://github.com/oekit/pkg/issues/24) | 缺「归档容器」原语（zip + manifest + 每文件摘要 + 格式版本 + 体检） |
| oekit/pkg | [#25](https://github.com/oekit/pkg/issues/25) | `LocalCache` 缺「无后台 goroutine」模式、缺「读命中返回副本」、淘汰非 LRU |
| oekit/pkg | [#26](https://github.com/oekit/pkg/issues/26) | `stringx` 缺「正则 + 形态标签 + 保定位线索」的脱敏原语 |
| oekit/pkg | [#27](https://github.com/oekit/pkg/issues/27) | 缺可单测的 CLI 子命令分发原语 |
| oekit/timertask | [#14](https://github.com/oekit/timertask/issues/14) | 本地后端与 Redis 后端同包，import 即拉 redis |
| oagentkit/textx | [#3](https://github.com/oagentkit/textx/issues/3) | `EstTokensCJK` 折算系数写死且不可配 |
| oagentkit/textx | [#4](https://github.com/oagentkit/textx/issues/4) | 缺 CJK 逐字成词元 + 拉丁按词切的双语 tokenizer |
| oagentkit/guardx | [#3](https://github.com/oagentkit/guardx/issues/3) | 规则表不可扩展/不可组合，且缺「保定位线索的不可逆掩码」 |

**2026-10-20 补记**：上表八条**全部已修并发布**（oekit/pkg v0.16.0–v0.17.0、
oagentkit/textx v0.6.0–v0.6.1、oagentkit/guardx v0.6.0–v0.8.2）。
本仓又提了三条，同样已修：

| 仓 | issue | 要点 | 修法 |
|---|---|---|---|
| oekit/pkg | [#31](https://github.com/oekit/pkg/issues/31) | `stringx.Masker` 的 Keep 档在匹配长度 ≤ 2×Keep 时把原值完整暴露（头尾两段拼回原文） | v0.17.0 起按**实际命中长度**判定：n < Keep×4 一律整段全量打码 |
| oekit/pkg | [#32](https://github.com/oekit/pkg/issues/32) | `pack.Inspect` 的「条数不符」核对在 counts 键与文件名不匹配时被**静默跳过** | v0.17.0 起先按文件名精确匹配、再按基名宽松匹配，仍匹配不上则**显式报出「条数未核对」** |
| oekit/pkg | [#33](https://github.com/oekit/pkg/issues/33) | `pack.Writer` 缺时钟注入，清单的 `created_at` 写死 `time.Now()` | v0.17.0 起 `NewWriter(w, ver, WithClock(now))` |

这三条的共同形状值得记：**「本该跑的检查没跑，却报通过」**。
凡是被跳过的检查，都必须让调用方看得见。

### 8.7 顺带修掉的两个既有缺陷

1. **掩码类型名撞敏感键名表**（`ports/redact.go`）。掩码形如 `«kind:****»`，
   而 `kind` 后跟冒号：类型名 `url-credential` 里的 `credential` **本身就在
   敏感键名表里**，于是 key=value 那条规则会把自己的掩码再打码一次，产出
   `«url-«secret-assignment:****»»`。明文照样没了，但**审计失真**——读掩码的
   人看到的形态标签和事实不是一回事。改名 `url-userinfo`，并加用例
   `TestRedactorOutputHasNoNestedMask` 把「掩码必须惰性」变成可执行的约束。
2. **门禁自身的三处**（`tools/archcheck`）：`severity` 字段声明了但**从未被执行**
   （`warn` 与 `error` 行为相同）；`exceptions` 的 `until` 只解析不比较
   （到期不豁免）；白名单的前缀匹配若用朴素 `HasPrefix`，`oekit/pkg` 会顺带
   放行 `oekit/pkg-evil`。本轮修的是第三件（白名单从设计起就卡在分隔符上），
   前两件记录在此待处置。

---

## 附：本文事实来源

| 事实 | 来源 |
|---|---|
| octop 全部能力面/分层/数据流/门禁 | 本地 `git clone --depth 1` @ `ed11d54`，逐文件读 `README.md` / `AGENTS.md` / `types.py` / `pipeline/**` / `storage/backends/**` / `application/runtime.py` |
| octop 33,479 行 / 280 文件 / 86 测试 | `find src -name '*.py' \| xargs wc -l`、`git ls-files \| wc -l`、`ls tests \| wc -l` |
| 扁鹊记忆平面体量与常量 | `wc -l`、`memory_svc.go:29-45`、`idl/memory.thrift` |
| 记忆面无 pii 接线（**当时**） | `memory_svc.go` import 面（未引 pii）——⚠️ 已于 2026-10-19 由扁鹊仓 `ad3ada51` 清偿 |
| harness 24 包行数 | `ls $d/*.go \| grep -v _test \| wc -l`（排除测试与 `.toolgate-work/`） |
| `modernc.org/sqlite` 可离线取到 | `bianque/go.mod:242` + 本地模块缓存（当时 `v1.23.1`，现钉 `v1.40.1`） |
