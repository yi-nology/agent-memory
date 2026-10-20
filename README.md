# agent-memory

**跨会话的长期记忆内核。** 记忆是代码，LLM 只做单次提炼。

本仓只提供确定性记忆的机制面：**捕获 → 提炼 → 晋升 → 召回 → 存储 → 迁移**，
不认识任何宿主领域概念。宿主负责 agent 执行与模型调度，把事件喂进来、
把召回结果取走。这条分工对标 [TencentCloud/octop-memory](https://github.com/TencentCloud/octop-memory)
的自我定位，完整设计与裁定见 [`docs/AGENT_MEMORY_PLAN_20261019.md`](docs/AGENT_MEMORY_PLAN_20261019.md)。

```
RawEvent ──extraction──→ Candidate ──promotion──→ AtomCard ──dirty/regen──→ EntityPage
    └──episode extraction──→ Episode
```

## 现在有什么（v0.4.0）

| 面 | 状态 | 位置 |
|---|---|---|
| 领域结构单源 | ✅ | `types/` —— 捕获事件/候选/事实卡/实体/别名/页面/情节/审计条目 |
| 外部能力端口 | ✅ | `ports/` —— 模型调用/向量化/向量索引/脱敏/注入防伪，接口 + 纯标准库默认实现；**分桶配额与丢弃计数**（`ports.Budget`）与召回实现解耦，宿主自造召回可复用同一套 |
| 持久化契约 | ✅ | `store/` —— 按消费面拆成小接口，22 个模块的方向已登记 |
| 架构门禁 | ✅ | `architecture-policy.yaml` + `tools/archcheck/`（**独立模块**，5 条规则 / 18 个自证用例） |
| SQLite 后端 | ✅ | `storage/sqlite/` —— 11 张表 + FTS5 + 事务，**零 LLM 可跑** |
| PostgreSQL 后端 | ✅ | `storage/postgres/` —— tsvector + GIN + 事务，与 SQLite **同一组行为断言** |
| 跨后端行为断言 | ✅ | `storage/parity/` —— **31 条断言，两个后端都真跑**（PG 对着真实服务器） |
| 召回流水线 | ✅ | `pipeline/recall/` —— 八段串行，**Bander 必需**、分桶配额、三路丢弃可观测、**取数两路**（查询命中 ∪ 高价值兜底）、可按宿主实体限定、可无捕获面运行 |
| 注入防伪 | ✅ | `ports.MemoryBand` —— 转义全部 `<`，9 条攻击载荷用例钉住 |
| 脱敏 | ✅ | `ports.PatternRedactor` —— 形态兵底 + `guardx/logredact` 规则表，**掩码长度不泄露原值长度** |
| 捕获面 | ✅ | `pipeline/capture/` —— **落库前打码**（Redactor 必需）、运行期错误与空白正文拒收 |
| 提炼面 | ✅ | `pipeline/extractor/` —— 防稀释校验、非法枚举硬拒、无模型显式失败 |
| 晋升面 | ✅ | `pipeline/promotion/` —— 五检查（价值/证据/实体/重复/矛盾）+ **落库四步在一个事务里**，送人工是默认姿态 |
| 归一面 | ✅ | `pipeline/entity/` —— 按名查/登记别名/合并实体（合并**改归属**不是复制） |
| 页面/情节 | ✅ | `pipeline/page/` `pipeline/episode/` —— 页面重生成、情节与日/周/月周期摘要 |
| 生命周期 | ✅ | `lifecycle/` —— 回收/合并/保留期/物理整理 |
| 模型适配器 | ✅ | `adapters/llm/` —— OpenAI 兼容端点，**纯标准库**（不引 SDK、不引 eino），空正文必须报错 |
| 迁移包 / bridge / CLI | ✅ | `operations/portable/`（.hmpkg，体检与导入走 `oekit/pkg/pack`）`adapters/bridge`（JSON-RPC）`adapters/cli` |
| 跨包端到端测试 | ✅ | `integration/` —— 主链走一遍（真实后端，不 mock 任何一层） |

> **PG 那一行不再是「未验证」**：v0.2.0 起 PG 后端是**完整实现**，
> 31 条 parity 断言对着**真实 PostgreSQL** 跑通，且 CI 里加了 `postgres` service
> 让它每次推送都真跑——「skipped 的 PG 用例不证明兼容」，所以它不许被 skip 掉。

## 设计取舍（先看这三条，其余都在方案文档里）

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

1. **行为一致性由共享断言保证，不由各自的单测保证。** `storage/parity` 里 31 条断言，
   两个后端跑**同一份**，且**两个都必须真跑过**（PG 那个对着真实服务器）。
   各自单测都绿 ≠ 行为一致：软退役过滤漏一列、分页游标用
   `<=` 还是 `<`、时间窗闭开区间——这类不一致在各自单测里全都看不出来，
   而「换个部署形态召回结果就不变」是产品承诺。
   > 补 PG 实现时立刻撞出两条真缺陷：签名分隔符用 NUL（**PG 的 TEXT 禁止 0x00**，
   > 同一份签名在一个后端落库、另一个报编码错），以及重要度排序是字典序
   > （`TEXT` 列下 `'med' > 'low' > 'high'`，最重要的排最后）。
   > 两条都是 SQLite 侧永远绿的——**契约的可移植性要靠第二个实现来暴露**。

2. **没注入模型就必须报错。** 提炼、页面重生成、情节摘要在无模型时返回 `ErrNoLLM`，
   **不许返回空串假装成功**——空串会被下游当成「模型说没什么」，
   从而静默丢弃整批候选。无模型时手工 store + 词法召回照常工作。

3. **排序不是存储层的承诺。** 后端给出的是相关度顺序（bm25 之类），「哪条该进
   prompt」是召回层重排的职责。把 bm25 名次钉进跨后端断言，等于把实现细节写成
   产品契约——换个后端就假红，召回质量却一点没变。

4. **转义必须匹配实际在用的那条 band。** 实现期间曾把转义「借」给
   `guardx/injectband.SanitizeBody`（通用安全原语、单源、不重复造），用例当场打穿：
   它的 `tagRe` 是写死的、只认 `<system-reminder>` 家族，拿它守 `<memory-context>` 时
   伪造的闭合标签原样穿过。集成与依赖一并撤掉，转义归本仓——门禁这关守的是
   **安全性质**，不是代码量。详见方案 §4.3。

5. **脱敏是必需依赖，不是可选项。** 记忆是留存资产，一旦落库就能被导出复制。
   `capture.New` 传 nil 直接失败——缺失的默认值不是宽容，是事故。兵底只认
   **形态**（已知凭据前缀、URL 内嵌凭据、key=value、长十六进制、私钥块），
   它是地板不是天花板，宿主必须叠自己的领域规则：凭据是唯一一类漏报即不可逆的。

6. **判定面能换成结构就别拿文本匹配。** 两处刻意偏离上游：
   召回结果**不**直接拼 prompt（过 `guardx/injectband` 身份防伪，记忆是跨会话
   跨用户的不可信内容）；`Provenance` 做成**类型**而不是 payload 里的字符串键。
   这两条各配一个「不接会怎样」的用例钉住——否则后人会照着上游改回去。

7. **上游修好了，自己那份兜底就得撤。** 脱敏的长度地板先后有两层本地兜底
   （声明式 `minLen` + 运行期预扫），而上游 `oekit/pkg` v0.17.0 已原生
   「短值整段全量打码」。本仓把两层**全部撤除**——留着它们不只是双源漂移，
   更危险的是让人误以为「这条是本仓保证的」，上游真回退时没人去查。
   撤除后该保证改由**依赖契约测试**钉住：把依赖降到修复前的版本，用例立刻变红。

8. **口径要单源，且钉在一处。** 检索切词与 token 折算都走上游，但本仓在
   `store` 层各留一个钉子（`LexicalTokens` / `FTS5Query`）——上游改了切法，
   本仓在这一处吸收并复核，而不是各处各自跟着变。
   钉子的必要性在 v0.4.0 被验证过一次：查询侧一度直接调上游而绕过钉子，
   两者当时行为一致（不是当下的错），但上游一改就是**静默漏命中**。

## 开发

```bash
make check        # fmt + vet + test + 架构门禁，提交前的完整闸
make test         # 只跑主模块测试
make tidy-check   # 核对主模块仍然零依赖
```

需要 Go 1.27+。全程 `CGO_ENABLED=0`（SQLite 走纯 Go 驱动，不依赖 cgo）。

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

memory gc [--before 720h]          # 按龄回收原始事件
memory consolidate                 # 合并同签名事实
memory retention                   # 退役证据全来自运行期错误的事实
memory vacuum                      # 物理整理（后端专属）
memory entity list                 # 列出实体
memory entity merge --to ID --from ID   # 合并两个实体（事实卡改归属）
memory entity alias --entity ID --alias A # 登记别名
memory episode list                # 列出情节
```

CLI 多数命令是**管理口**（看与搬），不注入模型；`extract` 与 `promote` 例外——
它们要模型，端点从环境变量读（`MEMORY_LLM_BASE_URL` / `MEMORY_LLM_API_KEY` /
`MEMORY_LLM_MODEL` / `MEMORY_LLM_TIMEOUT`），**密钥不进命令行**。
未配置模型时这两个命令显式报错，其余命令零影响。
页面重生成与周期摘要仍由宿主触发。

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
- **不认识领域。** 记忆认识「事实/实体/会话」，不认识「那台机器/那个集群/那次巡检」。
  宿主把运维对象投影成 `Entity` 交给本仓，门禁 `memory-no-domain-vocabulary` 强制。
- **向量索引默认关。** 上游自己也标为「默认关闭、测试全 mock、真实集成未验证」。

## License

MIT
