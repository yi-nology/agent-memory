# agent-memory 立项方案（2026-10-19）

> 性质：**决策依据文档**。回答三个问题——octop-memory 是什么、扁鹊记忆平面差在哪、
> agent-harness 哪些该下沉到新模块。文中所有 octop 事实均由 `git clone --depth 1`
> 核对 `TencentCloud/octop-memory` @ `ed11d54`（v1.0.2）得出，未采信二手描述。
> 关联：`docs/RAG_ASSESSMENT_20261008.md`（知识检索面，与本方案**不是同一件事**，
> 见 §5.3）、`agent-harness/docs/SPLIT_PLAN.md`（拆分方法论，本方案沿用其判定标准）。

## TL;DR

1. **不是从零抽**。扁鹊已有分层记忆平面（批次 80/81/115，`bq_memory_atom` +
   `bq_memory_subject_page`），其 IDL 注释明写「对标 Octop octop-memory 的事实卡」
   ——它是 octop 的一个**子集**，缺的是**宿主外的抽象边界**与 octop 后半程能力。
2. **从 agent-harness「下沉」的东西接近于零**。逐包核完 23 个包后裁定：
   **1 个借道**（budget 的计量面）、**2 个接线**（pii 落库前打码、injectband 召回防注入）、
   **1 个只抄机制不合并**（pending ↔ thread_state）、**其余 16 个包全留 harness**。
   真正要搬的是**扁鹊自己的记忆平面**。
3. **本仓有两处强于 octop，必须写进设计**：octop 的 privacy 面只有
   `store_raw_content=false` 这类**开关式丢弃**、没有内容级打码；recall 出来的内容
   直接拼 prompt、**不过身份防伪**。记忆跨会话持久化 + 可导出成包 = 新增泄漏面与
   注入面，这两处我们不能跟着抄。
4. **顺手发现一个现存缺陷**：记忆面没有接 pii（`memory_svc.go` 的 import 面可证），
   会话终稿蒸馏出的 `assertion`/`quote` 逐字落库。建议**单列一批先修**，不等新模块。
5. **下一步 M0 建仓**（go.mod + 域词门禁 + types + store 接口），依赖面已核实可离线取到
   `modernc.org/sqlite v1.23.1`（纯 Go，满足 `CGO_ENABLED=0`）。

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
| vector index | ⚠️ 有 Milvus RAG 面（**另一件事**） | `ports` | **刻意不做**，见 §5.3 |

---

## 二、包布局与依赖方向

```
agent-memory/
├── types/          AtomCard/Candidate/RawEvent/Entity/Alias/EntityPage/
│                   Episode/MemoryNode/ActiveEntity/JournalEntry
├── ports/          LLM / Embedder / VectorIndex —— 全是接口，零实现
├── store/          Store 接口 + 事务语义（对标 MemoryBackend Protocol）
├── storage/        sqlite（FTS5 + 表前缀 ns）/ postgres（ns-first 键 + FTS）
├── pipeline/       capture → extractor → promotion → recall → page → episode
├── lifecycle/      gc / consolidate / vacuum / retention
├── operations/     portable 包 + import/export
├── application/    Runtime 装配 + config + 路径投影 + 宿主文件对账
├── adapters/       bridge（JSON-RPC over stdio）/ cli
└── policy/         architecture-policy.yaml
```

**依赖方向（单向、无环）**：

```
adapters → application → pipeline → store(接口) ← storage(实现)
                             ↓          ↑
                           types ←─── ports(接口)
lifecycle / operations 与 pipeline 平级
```

**零核心依赖纪律**（octop 的核心零依赖在 Go 侧的等价物）：
核心不引 LLM SDK、不引 embedding SDK、不引 HTTP 框架、不引编排包。
只用 stdlib + `database/sql` + `modernc.org/sqlite`（**纯 Go，满足本仓
`CGO_ENABLED=0` 硬要求**；该版本 `v1.23.1` 已在本地模块缓存，离线可建仓）。

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
| `reminder` | 30 | **接线（改引 guardx，不经本门面）** | recall 内容跨会话/跨用户/跨宿主 = **不可信内容**，直拼 prompt = 提示注入。核心 `injectband` 已在 guardx/tier 0，agent-memory **直接引 guardx**；`harness/reminder` 这个门面是为保扁鹊调用点稳定而存在的，不是给外仓用的出口 |
| `pending` | 62 | **不合并，只抄机制** | octop 的 `thread_active_entities` 回答「『那个』指谁」= 召回期代词消解栈；pending 是「上一轮问了什么」= 跨轮挂起态载体。表面都是跨轮状态，**生命周期相反**（前者随召回滑动淘汰，后者等用户答复才结算）。同 AGENTS.md「意图两个 LLM 选路面不合并」的判据。但 thread_state 的 **LRU + pin-time + 重启存活**三条机制值得整体抄 |
| `checkpoint` | 545 | **不下沉** | octop 把它与长期记忆同库，是因为它的宿主是 LangGraph。本仓 checkpoint 是跨进程 CAS + SeqGate，服务「续跑不重放」，记忆服务「记住事实」；且 AGENTS.md 已记本仓链是**含并行分支的 DAG**，`BuildPlan` 的线性连续前缀模型不成立。可共用同一个 SQLite 文件/连接池，但**表前缀独立、包不合并** |
| `spill` | 251 | **不下沉（语义相反）** | spill 管输出溢出的截断落盘，记忆管值得留的。预算面互补（预算不够先 recall 补料、再 spill 截断），实现不共享 |
| `offload` | 589 | **暂不接** | 附件内容寻址落盘。记忆要不要存附件 = 多模态进上下文那天再定（同 AGENTS.md 已记的 offload 未引理由） |
| 其余 16 个包 | ~8.6k | **不属记忆，全留 harness** | `protocol`/`eventbus`/`statemachine`/`commitgate`/`contract`/`dispatch`/`ptc`/`watchdog`/`delta`/`drain`/`netguard`/`policy`/`toolfail`/`tooldisclose`/`guardset`——编排与安全面。`delta`/`pending` 已被 harness 抽过一次；再抽进 memory 等于把编排机制拆到两个内核仓，破坏 harness 的单一定位 |

**一句话结论**：从 harness「下沉」的东西接近于零——**真正要搬的是扁鹊自己的记忆平面**
（`repo_memory.go` + `memory_svc.go` 的机制面），harness 侧只有 1 个借道 + 2 个接线。

---

## 四、我们比 octop 强 / 必须补的三处

> 三处偏离全部有「不接会怎样」的用例钉住。其中两处（转义借 guardx、脱敏兵底的形态
> 覆盖）在实现期间各被用例打穿过一次，过程与结论记在 §4.3 与 ports/redact.go。

| 处 | octop 现状 | 本仓要求 |
|---|---|---|
| **落库前脱敏** | privacy 只有开关式丢弃，无内容级打码 | capture 管线挂 pii；**打码在落库前**，否则导出即泄漏 |
| **召回注入防伪** | recall 输出直接拼 prompt，无身份防伪 | recall 结果统一过 `guardx/injectband`；记忆内容标记为数据区，不得被当指令 |

这三处不是「对齐 octop」，是**明确偏离 octop**。写进 `types`/`ports`/`recall` 的包注释，
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
`store.Store`。M2 兑现这条：召回只用得到 5 个存储方法，于是声明 `recall.Reader`。
量化收益：召回的单元测试写 5 个方法而不是 40 个，且「召回依赖了存储的哪些面」
在类型上直接可见。

---

## 五、刻意不做（防止重复评估）

| 不做 | 理由 |
|---|---|
| **vector index** | octop 自己都默认关闭、测试全 fake、自承真实集成未验证。扁鹊已有 Milvus RAG 面但**那是知识检索**（`RAG_ASSESSMENT` 已定案：生产实态是纯 TF-IDF 本地面、语料 97% 是 mock 残留）。记忆的向量面等本地面稳定后再议 |
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

---

## 七、风险与未决

### 7.1 现存缺陷（**宿主侧，仍未修**）

> ⚠️ **本仓（M3）已把接线点做出来了，但宿主那条路还是通的。**
> `capture.Capture` 会先打码再落库——**前提是宿主把事件交给 capture**。
> 宿主现有的蒸馏路径（`internal/service/memory_svc.go`）仍然把断言与逐字引用
> 原样落库，那条路径上没有任何脱敏接线。**改本仓不会自动修好它**，
> 需要在宿主侧单列一批把蒸馏产物改走 capture。

**记忆面没有接 pii**。证据：`internal/service/memory_svc.go` 的 import 面只有
`runner`/`llm`/`model`/`repo`/eino/`gorm`，**无 pii**。会话终稿蒸馏出的
`assertion` 与**逐字 `quote`** 直接落 `bq_memory_atom`。
删除权有（`repo_userdata.go` 的 `memory_atoms_derived` 覆盖用户数据导出/删除），
但**落库那一刻没有打码**。这是记忆平面从「单会话内缓存」变成「跨会话持久资产」后
才出现的缺口，且 octop 同样没解——所以它不会因为「对标上游」而自动被修掉。

### 7.2 待拍板

| 事项 | 选项 | 影响 |
|---|---|---|
| portable `.hmpkg` 做不做 | A 全量对标 / B 降级为 JSON export-import / C 不做 | octop 的迁移面服务「记忆在宿主间搬家」；扁鹊目前是单宿主 + team 维度，**没有第二个宿主**。无第二宿主时 B 已够，A 是为未来付费 |
| 召回是否走 vector | A 不做 / B 接 Milvus | 见 §5；建议 A，等 RAG 本地面稳 |

### 7.3 双后端成本

octop 的 PG 实现 2,416 行 + 真实 `TEST_POSTGRES_DSN` 门禁。扁鹊记忆面现役在 PG 上，
若 M0–M2 只做 SQLite，**M7 切流前必须补 PG**，否则是行为面回退。故 §六 把 PG 骨架放进 M1。

---

## 附：本文事实来源

| 事实 | 来源 |
|---|---|
| octop 全部能力面/分层/数据流/门禁 | 本地 `git clone --depth 1` @ `ed11d54`，逐文件读 `README.md` / `AGENTS.md` / `types.py` / `pipeline/**` / `storage/backends/**` / `application/runtime.py` |
| octop 33,479 行 / 280 文件 / 86 测试 | `find src -name '*.py' \| xargs wc -l`、`git ls-files \| wc -l`、`ls tests \| wc -l` |
| 扁鹊记忆平面体量与常量 | `wc -l`、`memory_svc.go:29-45`、`idl/memory.thrift` |
| 记忆面无 pii 接线 | `memory_svc.go` import 面（未引 pii） |
| harness 24 包行数 | `ls $d/*.go \| grep -v _test \| wc -l`（排除测试与 `.toolgate-work/`） |
| `modernc.org/sqlite v1.23.1` 可离线取到 | `bianque/go.mod:242` + 本地模块缓存 |
