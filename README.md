# agent-memory

**跨会话的长期记忆内核。** 记忆是代码，LLM 只做单次提炼。

本仓只提供确定性记忆的机制面：**捕获 → 提炼 → 晋升 → 召回 → 存储 → 迁移**，
不认识任何宿主领域概念。宿主负责 agent 执行与模型调度，把事件喂进来、
把召回结果取走。这条分工对标 [TencentCloud/octop-memory](https://github.com/TencentCloud/octop-memory)
的自我定位，完整设计与裁定见 [`docs/AGENT_MEMORY_PLAN_20261019.md`](docs/AGENT_MEMORY_PLAN_20261019.md)。

```
RawEvent ──extraction──→ Candidate ──promotion──→ AtomCard ──dirty/regen──→ EntityPage
    └──episode extraction──→ Episode              └──AtomID 引用──→ tree leaf
```

## 现在有什么（批次 M0）

| 面 | 状态 | 位置 |
|---|---|---|
| 领域结构单源 | ✅ | `types/` —— 捕获事件/候选/事实卡/实体/页面/树/情节/审计条目 |
| 外部能力端口 | ✅ | `ports/` —— 模型调用/向量化/向量索引，全部是接口 |
| 持久化契约 | ✅ | `store/` —— 按消费面拆成 9 个小接口，17 个模块的方向已登记 |
| 架构门禁 | ✅ | `architecture-policy.yaml` + `tools/archcheck/`（**独立模块**，5 条规则 / 12 个自证用例） |
| SQLite 后端 | ✅ | `storage/sqlite/` —— 11 张表 + FTS5 + 事务，**零 LLM 可跑** |
| 跨后端行为断言 | ✅ | `storage/parity/` —— 29 条断言，SQLite 已接上并全绿 |
| PostgreSQL 后端 | ⏳ M6 | `storage/postgres/` —— 接线位就位，**实现未落，行为一致性未验证** |
| 召回流水线 | ✅ | `pipeline/recall/` —— 八段串行，**Bander 必需**、分桶配额、三路丢弃可观测 |
| 注入防伪 | ✅ | `ports.MemoryBand` —— 转义全部 `<`，9 条攻击载荷用例钉住 |
| 捕获面 | ✅ | `pipeline/capture/` —— **落库前打码**（Redactor 必需）、运行期错误拒收 |
| 提炼面 | ✅ | `pipeline/extractor/` —— 防稀释校验、非法枚举硬拒、无模型显式失败 |
| 晋升面 | ✅ | `pipeline/promotion/` —— 五检查（价值/证据/实体/重复/矛盾），送人工是默认姿态 |
| 页面/情节 | ⏳ M4 | `pipeline/` |
| 生命周期 | ⏳ M6 | `lifecycle/` |
| 迁移包 / bridge / CLI | ⏳ M5 | `operations/` `adapters/` |

> ⚠️ 上表里 PG 那行的 ⏳ 是有意的：它的测试是**跳过**的，而**跳过的用例不证明兼容**。
> 「SQLite 与 PG 行为一致」这句话目前还没有被验证过。

## 设计取舍（先看这三条，其余都在方案文档里）

1. **核心零依赖，可机检。** 第三方包只许出现在 `storage/`（纯 Go SQLite 驱动）；
   其余模块只准 stdlib 与本仓，由门禁 `memory-core-stdlib-only` 在**源码 import 面**
   上执行。理由不是洁癖，是**降级路径**：没有模型时手工写入 + 词法召回必须能跑，
   没有数据库时领域模型必须能编译——核心一旦引了 SDK，这条路径就塌在
   「用户配了模型、装了数据库」的生产环境里，而不是在 CI 里。

1. **行为一致性由共享断言保证，不由各自的单测保证。** `storage/parity` 里 29 条断言，
   两个后端跑**同一份**。各自单测都绿 ≠ 行为一致：软退役过滤漏一列、分页游标用
   `<=` 还是 `<`、时间窗闭开区间——这类不一致在各自单测里全都看不出来，
   而「换个部署形态召回结果就不变」是产品承诺。

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

## 开发

```bash
make check        # fmt + vet + test + 架构门禁，提交前的完整闸
make test         # 只跑主模块测试
make tidy-check   # 核对主模块仍然零依赖
```

需要 Go 1.25+。全程 `CGO_ENABLED=0`（SQLite 走纯 Go 驱动，不依赖 cgo）。

## 边界

- **与 agent-harness 平级，不是父子。** 编排归 harness，记忆归本仓，两侧互不 import
  （门禁 `memory-no-harness-dependency` 强制）。要交换的东西走宿主。
- **不认识领域。** 记忆认识「事实/实体/会话」，不认识「那台机器/那个集群/那次巡检」。
  宿主把运维对象投影成 `Entity` 交给本仓，门禁 `memory-no-domain-vocabulary` 强制。
- **向量索引默认关。** 上游自己也标为「默认关闭、测试全 mock、真实集成未验证」。

## License

MIT
