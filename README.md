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
| 持久化契约 | ✅ | `store/` —— 按消费面拆成小接口，16 个模块的方向已登记 |
| 架构门禁 | ✅ | `architecture-policy.yaml` + `tools/archcheck/`（**独立模块**） |
| SQLite/PG 后端 | ⏳ M1 | `storage/` |
| 流水线五段 | ⏳ M2–M4 | `pipeline/` |
| 迁移包 / bridge / CLI | ⏳ M5 | `operations/` `adapters/` |

## 设计取舍（先看这三条，其余都在方案文档里）

1. **核心零依赖，可机检。** 主模块 `go.mod` 的 require 段**为空**，`make tidy-check`
   逐行核对。门禁工具单独成模块，只为一个 YAML 库——这样「零核心依赖」是
   `go list -m all` 能验的事实，不是口号。

2. **没注入模型就必须报错。** 提炼、页面重生成、情节摘要在无模型时返回 `ErrNoLLM`，
   **不许返回空串假装成功**——空串会被下游当成「模型说没什么」，
   从而静默丢弃整批候选。无模型时手工 store + 词法召回照常工作。

3. **判定面能换成结构就别拿文本匹配。** 两处刻意偏离上游：
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
