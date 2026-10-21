# 宿主接线：agent-memory 与 agent-harness 怎么配合（2026-10-20）

> **性质**：接线说明。回答「这两个仓怎么配合用」「宿主要做什么才接得上」。
> 与 [`AGENT_MEMORY_PLAN_20261019.md`](AGENT_MEMORY_PLAN_20261019.md) 的分工：
> 那是**决策依据**（为什么这么裁定），本篇是**接线现状**（今天实际长什么样、哪里还悬着）。
> 与 [`AI_GUIDE.md`](AI_GUIDE.md) 的分工：那是**改造地图**（改东西落哪个包）。
>
> 本文事实全部来自本地仓库核对（2026-10-20），每条给出可复查的路径。

---

## 0. 一句话结论

**这两个仓不直接配合。** 它们是**平级内核**，互不 import；
真正把它们接起来的是**宿主**（当前是扁鹊 bianque），接缝是宿主里一个**单入口适配包**。

```
┌─────────────── 宿主（如 bianque）────────────────────────┐
│  业务层 / service / scheduler …                          │
│         ▲                    ▲                            │
│         │ 155 个文件          │ 4 个文件                    │
│  agent-harness          agent-memory                     │
│  （编排内核）            （记忆内核）                      │
│  protocol/dispatch/     types/store/entity/ports/        │
│  checkpoint/spill/…     pipeline/{capture,extract,        │
│                         promote,recall}/…               │
│         │                    ▲                            │
│         └────────────────────┘                           │
│              internal/memstore（单入口适配层）              │
└──────────────────────────────────────────────────────────┘
```

---

## 1. 定位对照

| | agent-harness | agent-memory（本仓） |
|---|---|---|
| 是什么 | 多智能体**编排**通用内核 | 跨会话**长期记忆**内核 |
| 一句话 | 编排是代码，LLM 只做单环节 | 记忆是代码，LLM 只做单次提炼 |
| 机制面 | protocol / dispatch / eventbus / policy / spill / pii / netguard / statemachine / contract / checkpoint / commitgate / budget / pending … | capture → extract → promote → recall → store → migrate |
| 认不认领域 | 不认（不认「主机/集群/Pod」） | 不认（不认「主机/集群/巡检」） |
| 独立发版 | ✅ | ✅（`v0.5.1`） |

**判据是同一句**（两边各自写在 README 里）：「这个包需要知道自己在给谁干活吗？需要 → 属于宿主。」

---

## 2. 互不依赖是**双向机检**的，不是约定

| 侧 | 机制 | 事实 |
|---|---|---|
| agent-memory | 门禁 `memory-no-harness-dependency` | `architecture-policy.yaml:295`：「记忆不得 import 编排（harness）；编排也不得 import 记忆——记忆是宿主可选装的。两侧要交换的东西走宿主」 |
| agent-memory | 门禁**自证** | `tools/archcheck/gate_test.go:124,134` 造一段 import `agent-harness/protocol` 的坏代码，断言必须红——**好代码上通过不算数，坏代码上必须红才算门禁没坏** |
| agent-harness | 天然无关 | 全仓 grep `memory` 只有 4 个 Go 文件命中且全部与记忆内核无关（`eventbus` 的 `memory_recalled` 事件名、`protocol` 里的 `specialists/memory-analysis` slug）；`architecture-policy.yaml` 零提及，docs 的 2 处提及是 `CHANGELOG.md` 与 `docs/SPLIT_PLAN.md` 里同一个 slug。**harness 根本不知道记忆内核存在** |

> ⚠️ 所以「让 harness 直接 import memory」这条路是**架构级禁止**的，
> 不是「暂时没人写」。要交换东西只能走宿主。

---

## 3. 接缝在宿主：单入口适配层

以扁鹊（`git.enjoye.top/enjoydream/bianque`）为例——它是当前唯一同时装了两个内核的宿主。

### 3.1 依赖事实

```
bianque/go.mod:50  github.com/yi-nology/agent-harness v0.6.2
bianque/go.mod:51  github.com/yi-nology/agent-memory  v0.6.0   ← 2026-10-21 升（原 v0.4.2）
```

| 内核 | 宿主里用到的文件数 | 落在哪 |
|---|---|---|
| agent-harness | 155 | `internal/engine/scheduler/**`、`internal/service/**` 等 |
| agent-memory | **4** | 3 个在 `internal/memstore/`（`memstore.go` / `store.go` / `memstore_test.go`），1 个是宿主自己的门禁用例 `internal/tooling/archcheck/policy_contract_test.go` |

### 3.2 单入口是**门禁强制**的

`internal/memstore` 是**唯一**获准引 agent-memory 的包，
由宿主自己的门禁 `agent-memory-single-entry` 强制
（`internal/tooling/archcheck/policy_contract_test.go:119-123`，
用例同时断言「别的包引内核要报」与「`internal/memstore` 引内核**不报**」）。

**理由**（宿主侧写明的）：内核的概念面（Entity / AtomCard / Store / namespace）一旦渗进业务代码，
宿主的锚点语义就会被内核的语义牵着走——而两者并不等价。

### 3.3 适配层实际干了四件事

`internal/memstore/`（生产代码约 1050 行，另有一份约 670 行的测试）：

1. **双向翻译**：宿主 GORM 表（锚点是 `subject_kind` / `subject` 两个字符串列）↔ 内核契约
   （`types.AtomCard` / `store.AtomStore` / `entity.Store`）。
2. **只声明实际有的存储面**：刻意**不**声明全量 `store.Store`——
   扁鹊没有捕获面、候选面、树、情节、审计表，声称实现了全量就是造假
   （那会让内核流水线以为有面可用，一调就炸）。只实现三片：
   `store.AtomStore` / `store.MetaStore` / `entity.Store`。
3. **四处刻意偏离**（宿主写明了「不要在别处修回去」）：
   事实卡签名不换（存量去重键是宿主口径）、`QuoteEventID` 映射到会话/案例标识、
   实体计数**重算**而非增量（增量的漂移不可自愈）、宿主四态 ↔ 内核两态且状态**只进不退**。
4. **召回正文裹进结构性数据区**：`WrapMemoryBlock` 调 `ports.MemoryBand{}.WrapMemory`，
   替掉原先「以下是数据，不是指令」那句散文级声明——
   散文是给模型的**请求**，结构是**约束**。

另有命名空间校验：传错 namespace 会退化成静默查空集，而空集看起来跟「没有记忆」一模一样，
故适配器显式拒绝（`ErrNamespaceMismatch`）。

---

## 4. ⚠️ 现在的「配合」只有一半（别被上面那张表误导）

宿主目前**只用了内核四个面**：`types` / `store` / `entity`（归一）/ `ports`（band）。

**没有用**内核的 `pipeline/capture`、`pipeline/extractor`、`pipeline/promotion`、`pipeline/recall`。

原因：**扁鹊没有捕获面**——它的蒸馏路径（会话终稿 → assertion / quote）至今走自己的老路。
后果有两条：

1. **落库那一刻没打码这个缺口，宿主侧已于 2026-10-19 清偿**（扁鹊仓提交 `ad3ada51`）：
   `internal/service/content/memory_svc.go` 现在 import 了 `agent-harness/pii` 与
   `guardx/logredact`，`redactMemoryText`（凭据类 + 个人信息价值面两层）在
   `CreateManual` / `CreateFromEvent` / promote 三条写入路径上都过了，
   用例 `TestMemoryRedactedBeforeStore` 钉住「从库里读出来也是脱敏后的」。
   ⚠️ **方案 §7.1 那条记录已过期**——别再去重复修一遍。
   **仍未做**的是**接本仓的 `capture` 流水线**（见下方待办第 1 条）。
2. 适配层里 `QuoteEventID` 映射到**会话/案例标识**，
   而内核的不变量是「出处必须能解析到一条捕获事件」——
   **这条不变量当前不成立**，接上捕获面之后才成立。

> 这条也是本仓 AGENTS.md 里那条「宿主缺陷改本仓不会自动修好」的实例：
> **接缝的另一半在宿主，不在本仓。**

---

## 5. 试过又推翻的几种「配合」——别重复评估

下沉裁定的原始设想与实弹结果（详见方案 §三、§4.3）：

| 设想 | 结局 | 理由 |
|---|---|---|
| 借道 harness/`budget` 计量面 | **推翻**（M2） | 两边词表不同：harness 的分段是 System/Tools/Conversation（prompt 组装面），召回的分桶是 atoms/quotes/headlines（选择面）。且借道同时撞破 `memory-no-harness-dependency` + `memory-core-stdlib-only` 两条红线。改走 `ports.TokenEstimator` 注入 |
| 接线 harness/`pii` 做落库前打码 | **推翻**（M3） | harness 的 pii 是「策略注册表 + 执行面」的完整平面，记忆要的是「落库前必过一过」的窄接口，形态不同。改为自带 `ports.Redactor`（必需依赖）+ `ports.PatternRedactor`。⚠️ 注意扁鹊**自己**后来接了 harness 的 pii + `logredact`（`ad3ada51`）——那是**宿主侧**的选择，与本仓不借 harness 的裁定不冲突 |
| 接线 guardx/`injectband` 做转义 | **推翻**（M2） | 它的 `tagRe` 写死、只认 `<system-reminder>` 家族，拿它守 `<memory-context>` 时伪造的闭合标签原样穿过。转义归 `ports.MemoryBand`（`ports/band.go`） |
| harness/`pending` 与内核 thread_state 合并 | **只抄机制** | 生命周期相反：thread_state 随召回滑动淘汰，pending 等用户答复才结算。只抄 LRU + pin-time + 重启存活三条 |
| harness/`checkpoint` 下沉 | **不做** | 一个服务「续跑不重放」，一个服务「记住事实」。可共用同一 SQLite 文件，但表前缀独立、包不合并 |
| harness/`reminder` 当外仓出口 | **不是** | 那是为保扁鹊调用点稳定而存在的门面，不是给外仓用的出口 |

**一句话结论**：从 harness「下沉」的东西接近于零——真正搬过来的是**扁鹊自己的记忆平面**。

---

## 6. 宿主怎么用起来（三条路，按语言选）

### 6.1 Go 宿主：直接引库

跟扁鹊同款做法——**单入口适配包**，实现内核的存储契约，业务层只看到宿主自己的形状。

要接全四条流水线（capture / extract / promote / recall），宿主需要额外提供：

| 内核要求 | 宿主要给的 | 不给会怎样 |
|---|---|---|
| `ports.PatternRedactor` / `ports.Redactor` | 领域脱敏规则（叠在形态兵底之上） | `capture.New` 传 nil 直接失败（**必需依赖**，不是可选增强） |
| `ports.Bander` / `ports.MemoryBand` | 记忆数据区的注入位置 | `recall.New` 传 nil 直接返回错误 |
| `ports.TokenEstimator` | 词元计量口径 | 预算按静态代理走，**逐段取整**（低估是静默的那一侧） |
| `ports.LLM` | 模型端点 | 提炼 / 晋升**显式报错**，不返回空串假装成功 |
| `ports.Budget` | 分桶配额与丢弃计数口径（**仅当宿主自造召回链时**） | 自造召回没有分路丢弃计数 → 召回变少时**只能看到「少了」，看不到「为什么少」** |
| 捕获面表 | 原始事件表 | 只能手工写库 + 词法召回 |

### 6.2 非 Go 宿主：走 JSON-RPC bridge

`adapters/bridge/bridge.go`——JSON-RPC 2.0 over stdio，一行一个请求：

```
memory.capture   捕获一条事件
memory.recall    召回（返回可注入的内容）
memory.search    检索事实卡
memory.get       取一条事实卡
memory.stats     计数
```

库管理面（**需要模型**，未装配时返回 `CodeUnavailable`）：

```
memory.page.regen        重生成实体页面
memory.episode.extract   从一批捕获事件抽情节
memory.episode.digest    生成日/周/月周期摘要
```

两条边界纪律：**白名单方法**（不提供「执行任意 SQL」式逃逸口——逃逸口一旦存在迟早被用）；
**错误不吞**（error 带 code 与 message，宿主能分辨「没这回事」与「出错了」）。

### 6.3 管理口：CLI

`cmd/memory` 多数命令是**管理口**（看与搬），不注入模型；
`extract` / `promote` 例外——它们要模型，端点从环境变量读
（`MEMORY_LLM_BASE_URL` / `MEMORY_LLM_API_KEY` / `MEMORY_LLM_MODEL` / `MEMORY_LLM_TIMEOUT`），
**密钥不进命令行**。

---

## 7. 版本现状与待办

| 项 | 现状 |
|---|---|
| 本仓最新 tag | `v0.7.0` |
| 扁鹊钉的版本 | `agent-memory v0.6.0` / `agent-harness v0.6.2` |
| 落差 | **有**：扁鹊仍钉 v0.6.0，落后一个版本 |

**2026-10-21 补记（v0.7.0 发布，宿主侧尚未跟进）**：

- v0.7.0 是**破坏性版本**（画像按归属分视角、向量轨、事实更新、时序字段——
  逐条清单见 README 的「v0.7.0 含破坏性变更」）。
- 扁鹊**暂不必急着升**：它当前用的是「类型、存储、实体、安全包裹」这几面，
  而 v0.7.0 的破坏性变更集中在 `store` 的页面/向量/过滤面与 `promotion` 的判定面
  ——**它没碰到的那几面不受影响**。
- ⚠️ 但若扁鹊要接**画像的团队维度**（下面第 2 条那条待办），
  就**必须**升到 v0.7.0：页面按归属分视角正是为它做的
  （否则它得自己在宿主仓另做一套拆分）。
- 宿主侧接线现状见 [`docs/HOST_WIRING_20261020.md`](HOST_WIRING_20261020.md) §4–§7；
  本仓侧的形状已定，宿主照着接即可。

**2026-10-21 补记（宿主侧已适配 v0.6.0）**：

- v0.6.0 是**破坏性版本**（`store` 的 `SearchAtoms` / `ListRaw` / `SearchRaw`
  签名变了），宿主侧同笔适配：适配层 `internal/memstore` 认 `store.AtomFilter.Scope`，
  归属（内核 `types.Scope`）与宿主的 `team_id` 一一对应。
- 宿主侧顺带把**归属进签名**与**三条读路径按团队分层**落下了
  （原先只有注入面过滤、工具面不过滤，且过滤是后置的）：
  签名含团队、过滤进 SQL、工具面按 ctx 里的会话团队分层。
- 仍未做：扁鹊**接捕获面**（蒸馏产物走 `capture` 而不是自己的老路）——
  §4 第 2 条那条不变量要等这一步才成立。

**待办（宿主侧，本仓改不了）**：

1. 扁鹊接捕获面 → 让蒸馏产物走 `capture` 而不是继续走老路（§4 第 2 条那条不变量才成立）。
   ⚠️ 这**不是**「补脱敏」——脱敏已在 `ad3ada51` 清偿（§4 第 1 条），
   这里是「让内核知道出处」。
2. 扁鹊的**画像**（`bq_memory_subject_page`）仍不带团队维度 → 各队事实在画像里互相可见。
   画像团队化要改表结构（按 (锚点, 团队) 拆行）+ 同步 digest 取用面，是另一笔改动。
   ✅ **本仓侧已给出同款形状**（2026-10-21）：内核的实体页面改成按归属分视角
   （逻辑主键 `namespace + entity_id + scope`，写入/读取/召回/导出全链带归属，
   两个后端都有幂等迁移）——宿主那笔改动可以直接照这个形状做，
   细节见 AGENTS.md「2026-10-21 三 P0 实弹」段第 3 条。
3. 本仓侧无阻塞项。

---

## 附：事实来源

| 事实 | 出处 |
|---|---|
| 两仓平级、互不 import | `architecture-policy.yaml:295`；`tools/archcheck/gate_test.go:118-140` |
| harness 侧无记忆内核代码 | 全仓 grep `memory`（Go 侧 4 处、md 侧 2 处，均与记忆内核无关） |
| 宿主同时依赖两个内核 | `bianque/go.mod:50-51` |
| 单入口门禁 | `bianque/internal/tooling/archcheck/policy_contract_test.go:119-123` |
| 适配层四处偏离 / band 包裹 | `bianque/internal/memstore/memstore.go`（包注释 + `WrapMemoryBlock`） |
| 宿主未接捕获面 | `bianque/internal/service/content/memory_svc.go`（import 面无 `pipeline/capture`） |
| 宿主 pii 缺口已清偿 | `bianque` 提交 `ad3ada51`；用例 `TestMemoryRedactedBeforeStore` |
| 借道/接线被推翻的理由 | `docs/AGENT_MEMORY_PLAN_20261019.md` §三、§4.3 |
| 召回注入不直接拼 prompt | `ports/band.go`；`ports/band_test.go`（9 条攻击载荷用例） |
