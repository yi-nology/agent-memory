# agent-memory

**跨会话的长期记忆内核。** 记忆是代码，LLM 只做单次提炼。

本仓只提供确定性记忆的机制面：**捕获 → 提炼 → 晋升 → 召回 → 存储 → 迁移**，
不认识任何宿主领域概念。宿主负责 agent 执行与模型调度，把事件喂进来、
把召回结果取走。这条分工对标 [TencentCloud/octop-memory](https://github.com/TencentCloud/octop-memory)
的自我定位，完整设计与裁定见 [`docs/AGENT_MEMORY_PLAN_20261019.md`](docs/AGENT_MEMORY_PLAN_20261019.md)。

> **要动手改这个仓？** 先读 [`docs/AI_GUIDE.md`](docs/AI_GUIDE.md)——
> 改造落点地图、改动后的验证路径、症状→病因索引。

```
RawEvent ──extraction──→ Candidate ──promotion──→ AtomCard ──dirty/regen──→ EntityPage
    └──episode extraction──→ Episode
```

## 现在有什么（v0.6.0）

| 面 | 状态 | 位置 |
|---|---|---|
| 领域结构单源 | ✅ | `types/` —— 捕获事件/候选/事实卡/实体/别名/页面/情节/审计条目 |
| 外部能力端口 | ✅ | `ports/` —— 模型调用/脱敏/注入防伪/词元计量，接口 + 纯标准库默认实现；**分桶配额与丢弃计数**（`ports.Budget`）与召回实现解耦，宿主自造召回可复用同一套；**排序打分器**（`ports.Ranker`）可注入换口径，缺省权重下**逐条等价于**原实现 |
| 持久化契约 | ✅ | `store/` —— 按消费面拆成 8 个小接口（`RawStore`/`CandidateStore`/`AtomStore`/`EntityStore`/`PageStore`/`EpisodeStore`/`JournalStore`/`MetaStore`），22 个模块的方向已登记；**归属维度**（记录级隔离）进过滤面，两条取数路都在**取数层**生效 |
| 架构门禁 | ✅ | `architecture-policy.yaml` + `tools/archcheck/`（**独立模块**，5 条规则 + 2 条阈值 / 18 个自证用例） |
| SQLite 后端 | ✅ | `storage/sqlite/` —— 11 张表 + FTS5 + 事务，**零 LLM 可跑**（单套表 + namespace 列隔离，非表前缀）；带**版本化迁移**（老库打开即升级，迁移幂等） |
| PostgreSQL 后端 | ✅ | `storage/postgres/` —— tsvector + GIN + 事务，与 SQLite **同一组行为断言** |
| 跨后端行为断言 | ✅ | `storage/parity/` —— **46 条断言，两个后端都真跑**（PG 对着真实服务器） |
| 召回流水线 | ✅ | `pipeline/recall/` —— 八段串行，**Bander 必需**、分桶配额、三路丢弃可观测、**取数两路**（查询命中 ∪ 高价值兜底）、可按宿主实体限定、**可按归属隔离**、可无捕获面运行、**排序可注入**、**可混合检索**（语义路 + RRF 融合，缺省零漂移）、**可查询理解**（时间窗/代词/实体）、**可打点**（P50/P95/token/缓存/丢弃原因） |
| 查询理解 | ✅ | `pipeline/query/` —— 相对时间窗（上周/昨天/最近三天）、代词共指、实体提示；**零值=不约束**，不改变未装时的行为 |
| 事实更新 | ✅ | `pipeline/promotion/` —— **取代**（同一个东西换了值，旧事实到期、版本号 +1）与**矛盾**（互相否定，送人工）分判；模型只做**路由**，作废与矛盾永远落人工 |
| 时序事实 | ✅ | `types/atom.go` —— 生效区间（`ValidFrom`/`ValidTo`）+ 版本号；老数据语义不变（`ValidFrom` 零值取 `OccurredAt`） |
| 图谱投影 | ✅ | `types/graph.go` + `pipeline/graph/` —— 关系边是**派生物**（溯源必需），多跳检索带归属与时间过滤 |
| 评测面 | ✅ | `evaluation/` —— 合成数据集 + Recall@k / Precision@k / nDCG@k / MRR / HitRate，**口径写死**、数据**可复现** |
| MCP 入口 | ✅ | `adapters/mcp/` —— stdio 上的 JSON-RPC，白名单四工具（capture/search/recall/stats），无逃逸口 |
| 注入防伪 | ✅ | `ports.MemoryBand` —— 转义全部 `<`，9 条攻击载荷用例钉住 |
| 脱敏 | ✅ | `ports.PatternRedactor` —— 形态兵底 + `guardx/logredact` 规则表，**掩码长度不泄露原值长度** |
| 捕获面 | ✅ | `pipeline/capture/` —— **落库前打码**（Redactor 必需）、运行期错误与空白正文拒收 |
| 提炼面 | ✅ | `pipeline/extractor/` —— 防稀释校验、非法枚举硬拒、无模型显式失败 |
| 晋升面 | ✅ | `pipeline/promotion/` —— 五检查（价值/证据/实体/重复/矛盾）+ **落库四步在一个事务里**，送人工是默认姿态 |
| 归一面 | ✅ | `pipeline/entity/` —— 按名查/登记别名/合并实体（合并**改归属**不是复制） |
| 页面/情节 | ✅ | `pipeline/page/` `pipeline/episode/` —— 页面重生成、情节与日/周/月周期摘要；**画像按归属分视角**（写入/读取/召回/导出全链带归属），晋升侧自动标脏 |
| 生命周期 | ✅ | `lifecycle/` —— 回收/合并/保留期/物理整理 |
| 模型适配器 | ✅ | `adapters/llm/` —— OpenAI 兼容端点，**纯标准库**（不引 SDK、不引 eino），空正文必须报错 |
| 迁移包 / bridge / CLI | ✅ | `operations/portable/`（.hmpkg，体检与导入走 `oekit/pkg/pack`）`adapters/bridge`（JSON-RPC）`adapters/cli` |
| 跨包端到端测试 | ✅ | `integration/` —— 主链走一遍（真实后端，不 mock 任何一层） |

> **v0.6.0 含破坏性变更**：`store` 的三个方法签名变了
> （`SearchAtoms` / `ListRaw` / `SearchRaw` 都要带过滤条件或归属），
> `application.Service.Search` 改成吃 `store.AtomFilter`。
> 下游跟着改的清单见 issue #10 / #11 的回写与 tag 的发布说明。

> **未发布（含破坏性变更，发布时并入下一版说明）**：
> `store.AtomFilter` 新增 `IDs` / `At`；`store` 新增 `EdgeStore` + `EdgeFilter`；
> `promotion.Verdict` 新增 `Actor` / `Superseded`，`promotion.Decision` 新增 `update`；
> `promotion.Config` 新增 `LLM` / `UpdateSimilarity` / `UpdateHintSimilarity`；
> `recall.Config` 新增 `Semantic` / `Analyzer` / `Metrics`；
> `recall.Request` 新增 `OccurredFrom` / `OccurredTo` / `At`；
> `ports.RankCandidate` 新增 `FusionScore`，`ports.RankWeights` 新增 `Fusion`；
> `application.Deps` 新增 `Promotion`；
> `types.AtomCard` 新增 `ValidFrom` / `ValidTo` / `Version`。
>
> 实体页面改成**按归属分视角**（逻辑主键 `namespace + entity_id + scope`），
> 故 `store.PageStore` 的六个方法都多了 `scope` 参数
> （`GetEntityPage` / `ListDirtyEntityPages` / `MarkEntityPageDirty` /
> `ApplyEntityPageRegen` / `RecordEntityPageRegenFailure`），
> `page.Regenerator` 的 `RegenerateDirty` / `RegenerateOne` 也多一个视角参数；
> `types.EntityPage` 新增 `Scope` 字段。**老库无需手工处理**：
> 两个后端都有**幂等迁移**（SQLite 重建表改主键，PG 改主键约束），
> 打开即升级。

> **PG 那一行不再是「未验证」**：v0.2.0 起 PG 后端是**完整实现**，
> 46 条 parity 断言对着**真实 PostgreSQL** 跑通，且 CI 里加了 `postgres` service
> 让它每次推送都真跑——「skipped 的 PG 用例不证明兼容」，所以它不许被 skip 掉。

## 设计取舍（先看这几条，其余都在方案文档里）

1. **核心零依赖，可机检。** 默认第三方包只许出现在 `storage/`（纯 Go SQLite 驱动）；
   其余模块只准 stdlib 与本仓，由门禁 `memory-core-stdlib-only` 在**源码 import 面**
   上执行。理由不是洁癖，是**降级路径**：没有模型时手工写入 + 词法召回必须能跑，
   没有数据库时领域模型必须能编译——核心一旦引了 SDK，这条路径就塌在
   「用户配了模型、装了数据库」的生产环境里，而不是在 CI 里。
   2026-10-19 起开了一个**受控开口**：纯 stdlib 的工具级模块（`oekit/pkg`、
   `oagentkit/textx`、`oagentkit/guardx/logredact`）可在 `architecture-policy.yaml`
   的 `allow_third_party` 段逐条登记后进核心——它们不引模型、不联网、不碰数据库，
   不破坏那条降级路径；每条登记带 `reason` + `until` + `removal`，缺一条加载即失败、
   到期即红。

2. **行为一致性由共享断言保证，不由各自的单测保证。** `storage/parity` 里 46 条断言，
   两个后端跑**同一份**，且**两个都必须真跑过**（PG 那个对着真实服务器）。
   各自单测都绿 ≠ 行为一致：软退役过滤漏一列、分页游标用
   `<=` 还是 `<`、时间窗闭开区间——这类不一致在各自单测里全都看不出来，
   而「换个部署形态召回结果就不变」是产品承诺。
   > 补 PG 实现时立刻撞出两条真缺陷：签名分隔符用 NUL（**PG 的 TEXT 禁止 0x00**，
   > 同一份签名在一个后端落库、另一个报编码错），以及重要度排序是字典序
   > （`TEXT` 列下 `'med' > 'low' > 'high'`，最重要的排最后）。
   > 两条都是 SQLite 侧永远绿的——**契约的可移植性要靠第二个实现来暴露**。

3. **没注入模型就必须报错。** 提炼、页面重生成、情节摘要在无模型时返回 `ErrNoLLM`，
   **不许返回空串假装成功**——空串会被下游当成「模型说没什么」，
   从而静默丢弃整批候选。无模型时手工 store + 词法召回照常工作。

4. **排序不是存储层的承诺。** 后端给出的是相关度顺序（bm25 之类），「哪条该进
   prompt」是召回层重排的职责。把 bm25 名次钉进跨后端断言，等于把实现细节写成
   产品契约——换个后端就假红，召回质量却一点没变。

5. **转义必须匹配实际在用的那条 band。** 实现期间曾把转义「借」给
   `guardx/injectband.SanitizeBody`（通用安全原语、单源、不重复造），用例当场打穿：
   它的 `tagRe` 是写死的、只认 `<system-reminder>` 家族，拿它守 `<memory-context>` 时
   伪造的闭合标签原样穿过。集成与依赖一并撤掉，转义归本仓 `ports.MemoryBand`
   （转义全部 `<`）——门禁这关守的是**安全性质**，不是代码量。详见方案 §4.3。
   > ⚠️ 但**脱敏**是另一回事：本仓借的是 `guardx/logredact` 的**规则表**（只认形态、
   > 与 band 无关），不是转义。借规则表 ≠ 借转义规则，这条界线别混。

6. **脱敏是必需依赖，不是可选项。** 记忆是留存资产，一旦落库就能被导出复制。
   `capture.New` 传 nil 直接失败——缺失的默认值不是宽容，是事故。兵底只认
   **形态**（已知凭据前缀、URL 内嵌凭据、key=value、长十六进制、私钥块），
   它是地板不是天花板，宿主必须叠自己的领域规则：凭据是唯一一类漏报即不可逆的。

7. **判定面能换成结构就别拿文本匹配。** 两处刻意偏离上游：
   召回结果**不**直接拼 prompt（过 `ports.MemoryBand` 身份防伪，记忆是跨会话
   跨用户的不可信内容）；`Provenance` 做成**类型**而不是 payload 里的字符串键。
   这两条各配一个「不接会怎样」的用例钉住——否则后人会照着上游改回去。

8. **上游修好了，自己那份兜底就得撤。** 脱敏的长度地板先后有两层本地兜底
   （声明式 `minLen` + 运行期预扫），而上游 `oekit/pkg` v0.17.0 已原生
   「短值整段全量打码」。本仓把两层**全部撤除**——留着它们不只是双源漂移，
   更危险的是让人误以为「这条是本仓保证的」，上游真回退时没人去查。
   撤除后该保证改由**依赖契约测试**钉住：把依赖降到修复前的版本，用例立刻变红。

9. **口径要单源，且钉在一处。** 检索切词与 token 折算都走上游，但本仓在
   `store` 层各留一个钉子（`LexicalTokens` / `FTS5Query`）——上游改了切法，
   本仓在这一处吸收并复核，而不是各处各自跟着变。
   钉子的必要性在 v0.4.0 被验证过一次：查询侧一度直接调上游而绕过钉子，
   两者当时行为一致（不是当下的错），但上游一改就是**静默漏命中**。

10. **归属是记录级的，过滤必须在取数层。** `Namespace` 是**整库级**隔离；
    多组共用一套库时还需要**记录级**归属（`types.Scope`）：
    空 = 全局可见（人人都该看到），非空 = 仅同归属可见。
    用 `Namespace` 表达第二层意味着「一条全局事实」得向每个组复制一份——
    写放大，且全局事实一新增就得回头补所有组的副本。那是不可行的建模。
    > ⚠️ 归属过滤**必须在取数层**（进 SQL，与 TopK 配额同一次求值）：
    > 交给调用方后置过滤的话，被别家占掉的配额已经花掉了，
    > 于是「该给我的那条」根本没进候选池——症状只是「召回变少了」，
    > 看着像排序问题，根因却在取数。召回层另有一道复查兜底（纵深防御）。

11. **排序要留注入缝，默认零漂移。** 打分器是宿主的事（走语义轨的宿主有自己的口径），
    没有注入缝它只能**绕开整条召回链自造一套**，两套排序口径并存，
    后续收敛都要付迁移成本。故 `ports.Ranker` 是端口，缺省实现是本仓的
    词面+重要度+新鲜度（权重 0.6/0.25/0.15）——
    缺省下与注入前**逐条等价**（`ports` 包的用例把旧公式重写一遍当期望值钉住）。
    契约只有一条要紧的：分数与候选**一一对应且同序**，长度不符即报错
    ——错位不会崩，只会静默换序，而换序在结果里完全看不出来。

## 开发

```bash
make check        # fmt + vet + test + 架构门禁 + tidy，提交前的完整闸
make test         # 只跑主模块测试
make tidy-check   # 核对主模块的 go.mod require 段
make architecture-check   # 只跑门禁（规则 + 门禁自证用例）
make release-check TAG=v0.5.1   # 按下游的真实路径验证已发布版本可被引进来
```

需要 Go 1.27+。全程 `CGO_ENABLED=0`（SQLite 走纯 Go 驱动，不依赖 cgo）。

> `make check` 里有**两段**门禁：主模块 + `tools/archcheck/`（独立模块，带一个 YAML
> 解析依赖）。主模块绿不代表门禁绿——门禁失效是静默的，所以两段都要跑。
> 架构门禁**自带单测**（`tools/archcheck/gate_test.go`）：它测的不是「好代码上通过」，
> 而是「坏代码上必须红」。它绿了才说明门禁本身没坏。

### 命令行面

```bash
memory capture --content "…"      # 捕获一条事件（落库前已打码）
memory extract [--session S]       # 从原始事件提炼候选（**要模型**）
memory promote [--entity ID]      # 判定候选并落成事实（**要模型**）
memory recall  <查询词>            # 召回并渲染
memory search  <查询词>            # 检索事实卡
memory stats                       # 计数
memory export --out <包路径>        # 导出成 .hmpkg
memory import --pkg <包路径>        # 导入
memory doctor --pkg <包路径>        # 检查包完整性

memory gc [--before 720h]          # 按龄回收原始事件（缺省保留 30 天）
memory consolidate                 # 合并同签名事实
memory retention                   # 退役证据全来自运行期错误的事实
memory vacuum                      # 物理整理（后端专属）
memory entity list                 # 列出实体
memory entity merge --to ID --from ID   # 合并两个实体（事实卡改归属）
memory entity alias --entity ID --alias A # 登记别名
memory episode list                # 列出情节
```

全局旗标（所有子命令通用）：`--db`（记忆库路径，缺省 `memory.sqlite`）、
`--namespace`（记忆库名，缺省 `default`）、`--host`（宿主标识，缺省 `cli`）、
`--scope`（记录级归属，缺省空=全局）。

`--scope` 在**写**的命令（`capture`）与**读**的命令（`recall` / `search`）
里是同一个旗标、两种含义：写的时候是「这条属于谁」，读的时候是「我要看谁的」
（空 = 不隔离；非空 = 只看「全局 + 同归属」）。

CLI 多数命令是**管理口**（看与搬），不注入模型；`extract` 与 `promote` 例外——
它们要模型，端点从环境变量读（`MEMORY_LLM_BASE_URL` / `MEMORY_LLM_API_KEY` /
`MEMORY_LLM_MODEL` / `MEMORY_LLM_TIMEOUT`），**密钥不进命令行**。
未配置模型时这两个命令显式报错，其余命令零影响。
页面重生成与周期摘要**不**走 CLI——它们要模型，走 bridge
（`memory.page.regen` / `memory.episode.extract` / `memory.episode.digest`），
未装配模型时返回 `CodeUnavailable` 而不是空结果。

### 跑 PG 的行为断言

PG parity 需要一个**真实服务器**（它测的是「约束建了没」「类型映射对不对」，
内存实现测不出来）：

```bash
export BQ_MEMORY_TEST_POSTGRES_DSN="postgres://user:pass@127.0.0.1:5432/postgres?sslmode=disable"
go test ./storage/postgres/ -count=1
```

没配这个环境变量时它会**显式跳过**并说明「PG 行为一致性未验证」——
跳过的用例不证明兼容。CI 里已配 `postgres` service，它每次推送都真跑。

## 边界

- **与 agent-harness 平级，不是父子。** 编排归 harness，记忆归本仓，两侧互不 import
  （门禁 `memory-no-harness-dependency` 强制）。要交换的东西走宿主。
  → 接线现状与宿主要做什么，见 [`docs/HOST_WIRING_20261020.md`](docs/HOST_WIRING_20261020.md)。
- **不认识领域。** 记忆认识「事实/实体/会话」，不认识「那台机器/那个集群/那次巡检」。
  宿主把运维对象投影成 `Entity` 交给本仓，门禁 `memory-no-domain-vocabulary` 强制。
- **向量索引默认关。** 上游自己也标为「默认关闭、测试全 mock、真实集成未验证」。
  但**缝留出来了**：`ports.Ranker` 可注入换排序口径，`types.AtomCard.Vector`
  是类型层的落脚点（默认不进持久化），`ports.Embedder` / `ports.VectorIndex`
  明确标注为 **reserved（当前无消费点）**——
  一个定义了却零消费者的导出接口，读起来像能力，实际不是，故写明。
- **写入面与召回面都不借 harness。** budget / pii / guardx 三处形态都对不上，且借道会
  同时撞破 `memory-no-harness-dependency` 与 `memory-core-stdlib-only` 两条红线。
  共享的是纪律，不是代码。详见 AGENTS.md「已定案、勿重复评估」。

## License

MIT
