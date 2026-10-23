# AI 改造指南（agent-memory）

**给接手这个仓的 AI 代理看的。** 目标不是复述 README，而是回答三个问题：

1. 我要改的东西**落在哪个包**？
2. 改完之后**怎么证明它对**（而不是看起来对）？
3. 哪些改动会**当场被门禁打红**？

纪律原文在 [`../AGENTS.md`](../AGENTS.md)（那是「代理容易踩、且踩了会留下长期债的硬纪律」），
设计裁定在 [`AGENT_MEMORY_PLAN_20261019.md`](AGENT_MEMORY_PLAN_20261019.md)（50KB，人读版，不建议 AI 通读），
接线现状在 [`HOST_WIRING_20261020.md`](HOST_WIRING_20261020.md)（本仓与 agent-harness 怎么配合用）。
本文件是**改造地图**：先读它，再按指针去读具体文件。

> **四条读法，按你的任务挑一条**，别通读：
> - 要**加东西**（新端口、新后端、新流水线阶段）→ 读 §2 + §3
> - 要**改行为**（召回策略、判定阈值、脱敏规则）→ 读 §2 + §4
> - 要**修 bug / 排查** → 读 §5 的「症状 → 病因」索引
> - 要**接宿主 / 搞清与 agent-harness 的关系** → 读 [`HOST_WIRING_20261020.md`](HOST_WIRING_20261020.md)

---

## 1. 三十秒建立心智模型

```
宿主（编排在别的仓）
  │  喂事件 / 取召回结果
  ▼
capture ──→ extractor ──→ promotion ──→ store ──→ recall ──→ 宿主拼 prompt
（落库前打码）  （要模型）    （要模型·五检查）  （SQLite/PG）   （八段串行）
                                          ▲
                                    entity/page/episode
                                    （派生物与旁支）
```

一句话：**记忆是代码，LLM 只做单次提炼。** 本仓只提供确定性机制面，不认识任何宿主领域概念。

### 分层与「谁能 import 谁」

依赖方向**由 `architecture-policy.yaml` 强制**，不是约定：

```
types   （零依赖，领域结构单源）
  ↑
ports   （零依赖，外部能力接口 + 纯 stdlib 默认实现）
  ↑
store   （持久化契约，按消费面拆成 8 个小接口 + Store 聚合 + RawWriter 写入面）
  ↑
pipeline/*  ← lifecycle / operations
  ↑
application（唯一知道「谁跟谁怎么装」的地方）
  ↑
adapters/{cli,bridge,llm}  →  cmd/memory
```

**改代码前先问一句「我这次改动会不会让某个包 import 了不该 import 的东西」。**
会，就先改 `architecture-policy.yaml`（**policy 先行**），再写代码。

### 六条改不动的红线

| 红线 | 门禁规则 | 触发它会怎样 |
|---|---|---|
| 第三方包只许进 `storage/`（受控开口除外） | `memory-core-stdlib-only` | 非零退出 |
| 方言 SQL 只许出现在 `storage/` | `memory-store-single-source` | 非零退出 |
| 领域词（主机/集群/巡检/kubernetes…）不许进代码 | `memory-no-domain-vocabulary` | 非零退出 |
| 不得 import `agent-harness` | `memory-no-harness-dependency` | 非零退出 |
| 单类型方法数 ≤ 60、单文件 ≤ 800 行 | `threshold-god-object` | 非零退出 |
| 免检依赖必须登记 `reason`+`until`+`removal` | `memory-allowlist-expired` | 到期即红 |

> 红线里**只有前四条**是 policy 的 `rules:` 段写的；`threshold-god-object` 来自
> `thresholds:` 段，`memory-allowlist-expired` 是门禁**工具侧**自己算的
> （读 `allow_third_party` 条目的 `until`）。改门禁时别去 policy 里找它们。

⚠️ **领域词门禁会扫注释与测试数据**，且是纯文本匹配——
**连「解释禁词的那行注释」都会拦**。写这类说明只能说「本仓没有的某类基础设施对象」，
不能逐字复述词表。已从禁词表撤出的词：`namespace`（记忆隔离维度）、`node`、`host`。

---

## 2. 改动落点地图

**先查这张表再动手。** 绝大多数改造需求都能在表里找到一行。

| 你要改什么 | 落在哪 | 连带要改 | 门禁注意 |
|---|---|---|---|
| 领域结构（加字段/加枚举） | `types/` | 两个后端的 `storage/{sqlite,postgres}/` | `types` 零依赖，加字段要同步 `schema.go` + **迁移** |
| 记录级归属（多组共用一套库） | `types/scope.go` | `store.AtomFilter.Scope` + 两个后端的查询 + `pipeline/recall/gather.go` | 过滤**在取数层**（进 SQL）；签名必须含归属 |
| 实体画像（按归属分视角） | `types/page.go` | `store.PageStore` 全接口 + 两个后端（**加列 + 改主键 + 迁移**）+ `pipeline/page` + `pipeline/recall/gather.go` | 画像的 scope 是**键的一部分**（不是过滤面）；全局页只汇总全局事实 |
| schema 加列 | `storage/{sqlite,postgres}/schema.go` | `storage/{sqlite,postgres}/migrate.go` | 建表脚本**没有 ALTER**；迁移**必须幂等**；索引归迁移管 |
| 排序口径（打分器） | `ports/rank.go` | `pipeline/recall/rank.go` + `recall.Config.Ranker` | 缺省**零漂移**（旧公式在测试里重写一遍当期望值） |
| 模型调用/脱敏/防伪 | `ports/` | 注入它的 `application.Deps` | 新增第三方须登记 `allow_third_party` |
| 向量化/向量索引 | `ports/index.go` + `ports/vector.go` | `application/vector.go` + 两个存储后端（持久化索引） | **已接通**（有真的消费点）；语义强度归宿主、管线归内核 |
| 分桶配额与丢弃计数 | `ports/budget.go` | `pipeline/recall/recall.go` | `BucketID` 是**字符串**，别改成枚举 |
| 存储接口 | `store/store.go` | **两个后端 + `storage/parity/` 都要动** | 加接口方法 = parity 加断言，两个后端都真跑 |
| 检索口径钉子 | `store/lexical.go` | 两个后端的索引/查询侧 | `LexicalTokens` / `FTS5Query` 是**单源**，别绕过 |
| 召回策略 | `pipeline/recall/{recall,gather,rank,cache}.go` | `recall.Config` | 八段串行，改一段要重跑全链测试 |
| 捕获与脱敏接线 | `pipeline/capture/capture.go` | `ports/redact.go` | 顺序不可换：**先判收不收 → 再打码 → 最后落库** |
| 判定规则 | `pipeline/promotion/promotion.go` | `types/` 枚举 | 送人工是默认姿态，矛盾**不许**自动裁决 |
| 提炼（要模型） | `pipeline/extractor/extractor.go` | `types.Candidate` | 抽取与判定**必须分层** |
| 实体归一 | `pipeline/entity/entity.go` | `types/entity.go` | 合并是**改归属**，不是复制 |
| 查询理解（时间窗/代词/实体） | `pipeline/query/` | `pipeline/recall`（取数前置）+ `types/query.go` | 解析出的实体**只补画像**，不划事实范围 |
| 混合检索（语义 + RRF） | `ports/semantic.go` + `ports/rerank.go` | `pipeline/recall/hybrid.go` | 融合决定**池子**，排序归打分器；缺省权重 0 ⇒ 零漂移 |
| 向量轨（向量化 + 索引） | `ports/vector.go` + `ports/index.go` | `application/vector.go`（写/删向量）+ `application/service.go`（装配语义路）+ 两个存储后端（`atom_vectors` 表，持久化索引） | 语义强度归宿主、**管线**归内核；**归属必须进索引**（后置过滤会吃掉配额）；本仓的持久化索引是**暴力扫描**（小库够用），大规模注入宿主自己的向量库 |
| 事实更新（取代 vs 矛盾） | `pipeline/promotion/update.go` | `types/atom.go`（时序字段）+ `store.AtomFilter.At` | 模型只做**路由**；作废与矛盾落人工 |
| 时序事实（生效区间） | `types/atom.go` | 两个后端（加列 + 迁移）+ `store.AtomFilter.At` | `ValidFrom` 零值取 `OccurredAt`（老数据不变） |
| 关系边与多跳 | `types/graph.go` + `pipeline/graph/` | `store.EdgeStore` + 两个后端（建表 + 迁移） | 边是**派生物**；溯源（SourceAtomID）必需 |
| 评测面 | `evaluation/` | `integration/`（真召回链适配 + 基线护栏） | 口径写死在一处；合成数据必须可复现且带干扰项；**基线数字见 `docs/EVAL_BASELINE.md`** |
| token 效率 / 问答口径 | `evaluation/qa.go` | 宿主的 `Answerer` / `Judge`（判分用模型归宿主） | 「已测」与「未测」必须能分开；效率的「命中」用**检索口径**（不用「上下文充分」当分子） |
| 事实时效类别 | `types/atom.go`（`FactKind`） | 两个后端（`kind` 列 + 迁移）+ `lifecycle`（按龄退役）+ `store.AtomFilter.EventSince` | 零值落在**不淘汰**那一类；事件窗口与时间窗是**两个维度**（显式问历史时窗口让位） |
| 宽松词面路 | `store/lexical.go`（`FTS5QueryAny`）+ `store.SearchAtomsLoose` | `pipeline/recall/hybrid.go`（三路 RRF 融合） | 放宽的是**词的匹配方式**，不是**可见性**；别把 AND 改成 OR（噪声会灌进池子） |
| **给已发布的存储面加能力** | `store/optional.go`（可选能力面） | `pipeline/recall/gather.go`（探测 + 降级）+ `application/contract_test.go`（冻结清单） | 新能力**不许**加进 `store.AtomStore` / `recall.Reader` / `ports.*`——加方法 = 仓外实现者当天编译失败；走「独立小接口 + 运行时探测 + 降级路径」 |
| MCP 入口 | `adapters/mcp/` | `application.Service` | 白名单四工具，无逃逸口 |
| 生命周期 | `lifecycle/lifecycle.go` | — | 事实卡只软退役，不硬删 |
| 迁移包格式 | `operations/portable/portable.go` | `oekit/pkg/pack` | 写面不换 pack：改了等于**改包格式**，已发布包导不回来 |
| CLI 命令 | `adapters/cli/{cli,admin}.go` | — | `Run(args,stdout,stderr) int`，不是 `main()` |
| 向量索引补算 | `adapters/cli/cli.go`（`memory reindex`） | `ports.HashEmbedder` + 存储的向量面 | 存量事实没有向量 → 语义轨等于失灵；补算是幂等的 |
| 向量索引的**归属**维度 | `storage/{sqlite,postgres}/schema.go`（`atom_vectors.scope`） | 两个后端 + `ports.VectorIndex.Search` 的 scope 参数 | 归属过滤必须在**取数层**（进 SQL）；后置过滤会让别家占掉 topK 槽位 |
| 对外 RPC | `adapters/bridge/bridge.go` | `application.Service` | 方法名是 `memory.xxx` 白名单（现 8 个） |
| 宿主接入 | `application/service.go` | `adapters/*` | 服务面**只做五件事**：capture/extract/promote/recall/search |
| 依赖方向/豁免 | `architecture-policy.yaml` | `tools/archcheck/` | **policy 先行**；豁免缺字段**加载期即失败** |

---

## 3. 常见改造剧本

### 3.1 加一个存储后端（第三种方言）

最典型的「两处都别忘」改造。**漏一处的后果是假绿**：单个后端自己的单测全绿，
但「换个部署形态召回结果不变」这条承诺破了。

```
1. architecture-policy.yaml 加模块（id / roots / requires），requires 里带上 storage-parity
2. storage/<新后端>/  实现 store.Store 的 8 个子接口 + Transaction + Close
   （RawStore/CandidateStore/AtomStore/EntityStore/PageStore/EpisodeStore/JournalStore/MetaStore；
    另实现 store.RawWriter——那是捕获阶段只需要的那一小片写入面）
3. storage/<新后端>/schema.go  建表；方言 SQL 只许出现在这个目录
4. storage/parity/parity.go  加断言（**在共享断言里加，不是新后端自己写**）
5. storage/<新后端>/parity_test.go  挂 parity.Run(t, openXxx)
6. make architecture-check  →  跑 gate_test.go 确认门禁本身没坏
```

> **命名空间隔离：单套表 + namespace 列**，不是「每 namespace 一套表」。
> 理由写在 `storage/sqlite/schema.go` 顶部：实例已绑定一个记忆库，表前缀那条路
> 会让「跨 namespace 的体检与迁移」变成 N 遍 DDL。PG 侧同理是 namespace-first 键。

> **parity 断言是产品契约，不是实现细节。** 断言里**不许**断言 bm25 名次——
> 「哪条该进 prompt」是召回层重排的职责。把名次钉进跨后端断言，换个后端就假红。
>
> PG 后端必须对着**真实服务器**跑（`BQ_MEMORY_TEST_POSTGRES_DSN`）。
> 没配这个变量时它**显式 skip** 并说明「PG 行为一致性未验证」——
> **skipped 的用例不证明兼容**。CI 里配了 `postgres` service，别让它 skip。

### 3.2 加一个外部能力（端口）

```
1. ports/ 加接口 + 纯 stdlib 默认实现（NoopXxx 或 Xxx）
2. 没注入必须报错，不许返回空串假装成功
   —— 空串会被下游读成「模型说没什么」，静默丢弃整批候选
3. ports/index.go 的模式：ErrNoXxx 哨兵 + Noop 实现
4. 若该能力是必需的（脱敏 / 防伪）：在**构造期**拒绝 nil
```

必需的端口有三个，各自的构造期失败点已经钉住：
`capture.New` 传 nil Redactor 直接失败、`recall.New` 传 nil Bander 直接失败、
`application.New` 传 nil Bander 直接失败。**缺失的默认值不是宽容，是事故。**

### 3.3 改召回行为

召回是**八段串行**（`recall.go` 顶部注释有全景图）：
`router → 多源取数 → suppress（防回声）→ rerank → diversify → budget → thread_state → cache`

改之前先答三个问题：

1. **这次改动属于哪一段？** 动 `rank.go` 是排序，动 `gather.go` 是取数——
   **取数段与排序段改动的验收方式完全不同**（见下）。
2. **它会不会改变「哪条被捞出来」而不只是「谁排前面」？**
   查询词必须参与**取数**（`gather.collectAtoms` 走查询命中 ∪ 高价值兜底两路）。
   只在排序里响应查询词 = 候选池与「用户问了什么」无关，再相关的事实也进不来。
3. **丢弃可归因吗？** 改完之后 `Result.Drops`（按 `ports.DropReason` 分路）还说得清吗？
   `DropQuota`（该调分桶占比）与 `DropBudget`（该调总预算）**必须分开**，
   合成一个数就看不出是哪一种。

> **验收提醒**：**名次**是实现细节，别把它写成断言——断言「命中集合正确」即可。
> 但**方向不是**实现细节：`ports.Ranker` 的契约明写「分数越大越靠前」，
> 那是**对外契约**，必须在召回层断言（`TestHigherScoreComesFirst`）。
> 这两者的分界：后端给什么顺序是实现细节（所以 parity 不断言名次），
> 而「高分在前」是契约（所以召回层必须断言）。
>
> ⚠️ 这条边界曾经被人读反过：照字面执行「别断言名次」，
> 就会漏掉方向那条断言——而方向写反了，预算一紧**先丢最相关的事实**，
> 症状只是「召回少了」，看着像内容不够。
> （真实教训见 AGENTS.md「2026-10-21 三 P0 实弹」段。）

### 3.4 加一个 CLI 命令

```
1. adapters/cli/cli.go：Run() 里加 case → cmdXxx(...)
2. 命令逻辑放 cmdXxx，签名 (ctx, opt, stdout, stderr, ...) int
3. 库管理面（gc/consolidate/retention/vacuum/entity/episode）放 admin.go
4. 要模型的命令才装配 adapters/llm（按需依赖，其余命令零影响）
5. 密钥从环境变量读，不进命令行（密钥进命令行会进 shell 历史与进程表）
```

`Run(args, stdout, stderr) int` 而不是 `main()` 是刻意的：**命令逻辑可被单测直接调用，
不用起子进程**。写完记得补 `adapters/cli/cli_test.go` 风格的用例。

### 3.5 给**已发布**的存储面加能力（不破坏仓外实现者）

`store.*` / `recall.Reader` / `ports.*` 这些接口**仓外的宿主会自己实现**
（宿主仓 bianque：`var _ store.AtomStore = (*Store)(nil)`）。
在它们上面加方法 = **隐式 breaking**：上游发 tag 的当天，下游整个仓编译不过
（v0.8.0 加 `SearchAtomsLoose` 就这么打红了宿主仓，issue #12）。

⚠️ **「把大接口拆小」不解决这个问题**：拆开只是把一个大面换成几个小面，
任何一个被仓外实现的面，加方法仍然打红下游。

正确姿势（三条一起，缺一条就换一种失效形态）：

```
1. store/optional.go：落一个独立的小接口（可选能力面）
2. 消费方运行时探测：store.LooseSearcher(x) → (能力, 有没有)
3. 没有时走明确写下来的降级路径：少召回 + 可观测（不许静默）
```

判据：**这是「增强」还是「前提」？**
- 增强（如宽松检索：多话题查询的召回率）⇒ 降级（少召回，报出来）。
- 前提（如必需端口、捕获面）⇒ 缺了就**响亮失败**（装配期报错）。

两者都**不许**把增强当前提（宿主装不起就崩）。

改完跑 `go test ./application/` —— **契约冻结清单**在那里：
已发布接口的方法集被钉死，改它 = 一次有意的破坏性变更
（走大版本 + 发布说明点名「外部实现者需要改」）。
新能力走可选接口 ⇒ 清单不用动。

---

## 4. 改完之后：怎么证明它对

### 4.1 三级验证，从便宜到贵

```bash
export CGO_ENABLED=0
unset GOFLAGS                      # -mod=mod 会触发模块图重解析

go test ./<改动的包>/ -count=1      # ① 单包，最快
go test ./... -count=1             # ② 全仓
make check                         # ③ 完整闸：fmt + vet + test + 架构门禁 + tidy
```

`make check` 的顺序即成本递增，**提交前必须全绿**。

> **沙箱里的坑**：`GOCACHE` 会被前几轮失败尝试**污染**，
> 症状是「明明代码没问题却一直编译失败」。换一个新目录试试：
> `export GOCACHE=/tmp/am-cache-$(date +%s)`

### 4.2 反向验证（改行为时必做）

**用例绿了 ≠ 用例钉住了正确的东西。** 三种失效形态：

| 形态 | 现象 | 修法 |
|---|---|---|
| 假红 | 用例红了，但红在错的点上 | 看断言的**判别特征**选得对不对（选了「两边都有」的特征 = 永远红） |
| 假绿 | 用例绿了，但它在新实现下也绿 | **回退生产代码**，看用例是否变红。不红 = 它没钉住任何东西 |
| 没执行到 | 表驱动用例共用一个 `t.Fatalf`，第一条失败后面全没跑 | 改 `t.Run(tc.name, ...)` 逐条独立报告 |

**具体做法**：改完生产代码后，**手动回退那一段**，跑测试看它红不红。
红在正确的点上 = 钉住了；不红 = 这条用例是装饰。

### 4.3 门禁自证

`tools/archcheck/gate_test.go` 测的不是「好代码上通过」，而是**坏代码上必须红**：
域词（代码/注释/测试数据三处）、编排依赖、模块方向写反、pipeline 层出现 SQL、
核心引第三方包、白名单前缀混淆（`oekit/pkg` 不放行 `oekit/pkg-evil`）、
白名单到期、god object、policy 缺字段——**每条各一个用例，共 18 例**。
**它绿了才说明门禁本身没坏**——门禁最大的失败模式是假通过，它一声不吭退出 0。

改了门禁规则或 policy 之后：

```bash
make architecture-check   # 跑规则 + 跑 gate_test.go
```

### 4.4 提交前

```bash
make check
git add -- <具体路径>       # 不 add . —— 并发会话会把自己的文件带进来
git diff --cached --name-status | wc -l   # 核暂存区集合（**数行数**，别只看前几行）
git commit
git show --stat --name-status HEAD  # 提交后复核文件集合
```

> **并发会话的三种越界**：
> ①「塞进来」——对方 `git add` 了它的文件，你的提交会带上它；
> ②「抽走」——对方 `reset` 了你的暂存；
> ③「抢先提交」——对方先 `git commit`，把**整个暂存区**连同你的内容一起卷走，
> 你的提交变成「no changes added to commit」（内容没丢，但提交归属错乱）。
>
> 根治办法是**路径限定提交**：`git commit -- <路径>` 按工作树内容提交指定路径，
> 不依赖索引。注意坑：只列新路径时**删除侧会丢**，要同时列旧路径
> （`git commit -- old.go pkg/`）。
>
> ③ 的判法：提交返回「no changes added」**不等于失败**——先 `git show HEAD:<你的文件>`
> 核内容是否已落，再决定要不要重做。剔除他人内容用 `git reset --soft HEAD~1`
> + `git restore --staged <他人路径>`，**不动工作树**（他的在途内容原样保留）。

---

## 5. 症状 → 病因索引

排查时先在这里对号入座，能省掉大量阅读。

| 症状 | 病因 | 出处 |
|---|---|---|
| 事务里写不进数据 / 挂起不报错 | 用了**外层**的 store 而不是回调收到的那份 | `store/store.go` Transaction 注释 |
| 查询后下一条查询挂起 | 游标未关就发下一条 | `storage/{sqlite,postgres}/entity.go` |
| 「查到了」但对象是 nil，解引用 panic | `rows.Err()` 在 0 行时是 nil，**不存在被翻译成成功** | `storage/*/atom.go` |
| 门禁打红但代码没问题 | 领域词在**注释或测试数据**里 | `architecture-policy.yaml` 规则注释 |
| 门禁打红：换个词就好了 | **假红**——那个词在本仓语境里是承重的 | `memory-vocabulary-false-positive-guard` |
| 掩码长度泄露原值长度 | 自研掩码用等长星号串 | 用 `oekit/pkg/stringx.Masker` |
| 嵌套掩码 `«url-«secret-assignment:****»»` | 掩码类型名含敏感键名表里的词 | `ports/redact.go` 类型名约束 |
| 模型说「没什么」被当成成功 | 返回了空串而不是 `ErrNoLLM` | `ports/llm.go` |
| 记忆里的话被当系统指令执行 | 没包数据区 / 转义规则与实际 band 不配 | `ports/band.go` |
| 召回里留着的是最不相关的那条 | 排序方向与 `ports.Ranker` 契约反了（升序） | `pipeline/recall/rank.go` `sortByScore` |
| 预算一紧就丢最该留的事实 | 同上（分配按排序**逐条接收**，顺序即取舍） | `TestTightBudgetKeepsMostRelevant` |
| 换个归属就拿到上一归属的召回结果 | 缓存键漏了 `Scope`（跨租户泄漏） | `pipeline/recall/recall.go` `cacheKey` |
| 画像里出现了没提过的东西 | 画像不按归属分视角（摘要混进别家事实） | `types/page.go` `EntityPage.Scope` |
| `memory.page.regen` 安静地什么都不做 | 写入侧没标脏（派生标记没接线） | `application/service.go` `markPageDirty` |
| 问「上周那次」召不回上周的事 | 时间窗没进**取数层**（只改了词面） | `store.AtomFilter.OccurredFrom/To` |
| 查「上周」时把上周才写下的事实排除了 | 时间窗与生效区间混了（两个维度） | `types/atom.go` `ValidRange` |
| 一句代词把整轮事实清空了 | 解析出的实体被拿去划事实范围 | `pipeline/recall` `analyze` |
| 事实更新悄悄停了（记忆里留着两个版本） | 模型失败被静默吞掉 | `pipeline/promotion/update_llm.go` |
| 语义召回像是没生效 | 宿主只给了 Embedder 没给 Index（或反之），两件**都**给才启用 | `application/service.go` `recallCfg.Semantic` |
| 语义路命中率莫名偏低 | 事实退役了而向量还在索引里（死事实占槽位） | `application/vector.go` `vectorStore` |
| 语义召回像是没生效（且与归属有关） | 归属过滤被放在**后置**（topK 之后），别家占掉了配额 | `ports.VectorIndex.Search` 的 scope 参数 |
| 评测数字每次都不一样 | 合成数据用了全局 `rand`（被别的包推进） | `evaluation/synthetic.go` |
| 某实体的事实全丢了 | 实体合并时按 `ActiveOnly` 过滤了退役卡 | `store/store.go` ListAtomsByEntity 注释 |
| 召回结果偏少但看不出原因 | 丢弃没分路 | `Result.Drops` / `ports.Budget` |
| 多话题查询几乎打不中 | 词面路是**隐式 AND**（每个词都要在） | `store.FTS5QueryAny` + 宽松路（已在 v0.7.x 修，见 `docs/EVAL_BASELINE.md`） |
| 多话题查询打不中（且宿主自己实现了存储） | 宽松检索是**可选能力**，宿主没实现 ⇒ 召回回落到严格路 | `recall.Result.LooseSearchUnavailable` + 指标 `recall.loose.unavailable` |
| 升级上游当天，仓外实现者编译不过 | 新方法加进了**已发布**的接口（`store.AtomStore` 等） | `store/optional.go` 的契约演进规约 + `application/contract_test.go` 冻结清单 |
| 一次查询给不出答一道多事实问题的上下文 | 上下文充分率低（**部分匹配的事实进不来**） | `docs/EVAL_BASELINE.md` 的充分率与修法 |
| 画像桶恒为 0 | 画像参与了多样性裁剪被挤掉 | `TestBucketsAreIndependent` |
| 改了依赖方向门禁就红 | 没先改 policy（policy 先行） | `architecture-policy.yaml` |
| 中文子串检索 0 命中却「没报错」 | 切词口径漂了：FTS5 把连续汉字当**一个**词元 | `store/lexical.go` `LexicalTokens` |
| 预算看着够、实际超了 | 取整方向错：合总档把每段零头吞掉（**低估是静默的那一侧**） | `TestStaticEstimatorRoundingIsPerSegment` |
| 依赖升级后短凭据完整暴露 | 上游掩码的 Keep 档回退（本仓兜底已按纪律撤除） | `TestUpstreamMaskerShortHitIsFullyMasked` |
| 改依赖方向/白名单门禁就红 | policy 豁免缺 `reason`/`until`/`removal` 之一 | `tools/archcheck/policy.go` 加载期校验 |
| 提交里混进了别人的文件 / 提交内容被抽走 | 并发会话共用仓库：暂存区被别人动过 | AGENTS.md「发版」段与本文件 §4.4 |

---

## 6. 一份 60 秒上手路径

```bash
# 1. 看主链长什么样（五个包，每个包顶部的注释就是它的设计说明）
head -40 types/doc.go pipeline/capture/capture.go pipeline/extractor/extractor.go \
            pipeline/promotion/promotion.go pipeline/recall/recall.go

# 2. 看契约面（三个文件是全部的对外 API）
cat store/store.go ports/*.go

# 3. 跑通一次端到端
make test
go run ./cmd/memory capture --content "2026-10-20 部署了 v0.5.0" --explicit
go run ./cmd/memory stats

# 4. 看门禁在守什么
cat architecture-policy.yaml
```

**读代码的优先级**：包顶部注释 > 字段注释 > 实现。
这个仓的注释密度很高且写的是**为什么**，不是复述代码——先读注释能省掉大部分推理。

---

## 7. 已知限制（别当 bug 修）

| 限制 | 说明 |
|---|---|
| 领域词门禁是纯文本匹配 | 连解释禁词的那行注释都会拦；禁词不能出现在任何文本里 |
| 向量轨默认不启用 | 不给 Embedder/VectorIndex 就不启用（召回逐条回到纯词面）；本仓自带的哈希嵌入**只捕捉词面软匹配**，不是语义相似度 |
| 树（root/branch/leaf）暂不做 | 对运维域价值有限，等真出现「同一事实挂多锚点」再启 |
| `checkpoint` 不下沉到记忆 | 它服务「续跑而非重放」，记忆服务「记住事实」 |
| `pending` 不与活跃实体栈合并 | 表面都是跨轮状态，生命周期相反 |
| 迁移包写面不换 `pack` | 本仓清单多了两个溯源字段，换过去等于改包格式 |
| PG parity 需要真服务器 | 没配 DSN 时它**显式 skip**，跳过不证明兼容 |
| 画像：读者不隔离时只拿到全局页 | `Request.Scope` 为空，在**过滤面**是「不隔离」，在**画像面**是「全局视角」——刻意不同：全局页若也收进所有事实，它就是一张所有人都能看的、揉着别人内容的摘要（泄漏）。少一段摘要只是少点上下文，泄漏是内容事故 |
| 门禁的 `severity` 字段未生效 | policy 里声明了 `severity`，但门禁**只按 error 处理**，warn 与 error 行为相同 |
| 门禁 `exceptions` 的 `until` 只解析不比较 | 例外到期**不自动失效**；真要限时，得走 `allow_third_party` 那条路（那条会到期即红） |

---

## 8. 一次改造的检查清单

改完，逐条过：

- [ ] **落点对**？对照 §2 的表，没落到错包里
- [ ] **依赖方向没破**？新 import 在 policy 里被允许（没破就先改 policy）
- [ ] **必需依赖没变成可选**？新增的必需能力在构造期拒绝 nil
- [ ] **该有的两处都改了**？改存储接口 = 两个后端 + parity 三处
- [ ] **反向验证过了**？回退生产代码，用例会红在正确的点上
- [ ] **没有假绿**？新写的用例在旧实现下会红
- [ ] **表驱动用例逐条独立**？用 `t.Run` 报告，不是共享一个 `t.Fatalf`
- [ ] **门禁自证绿**？`make architecture-check`（含 `gate_test.go`）
- [ ] **`make check` 全绿**？
- [ ] **注释写的是「为什么」**？不是复述代码
- [ ] **提交只含自己的文件**？路径限定提交 + 提交后复核
