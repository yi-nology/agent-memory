module github.com/yi-nology/agent-memory

go 1.27

// agent-memory：跨会话的长期记忆内核。
//
// 定位：**记忆是代码，LLM 只做单次提炼**——本仓只提供确定性记忆的机制面
// （捕获 / 提炼 / 晋升 / 召回 / 存储 / 迁移），**不认识任何宿主领域概念**。
// 宿主负责 agent 执行与模型调度，把事件喂进来、把召回结果取走。
//
// 与 agent-harness 是**平级内核**，不是父子：编排归 harness，记忆归本仓，
// 两侧互不 import（见 architecture-policy.yaml 的 memory-no-harness-dependency）。
//
// 纪律：
//   - 提交前跑 `make check`（build + vet + test + architecture-check）；
//   - 核心零依赖：不引 LLM SDK / embedding SDK / HTTP 框架，只用 stdlib +
//     database/sql + 纯 Go SQLite 驱动；
//   - 领域词（主机/集群/Pod/巡检/专家…）进本仓代码即红，由门禁强制；
//   - 新依赖方向先改 architecture-policy.yaml 再写代码（policy 先行）。
//
// # 依赖面
//
// **M0 阶段 require 段为空**，这是刻意的：领域模型与端口契约必须是纯 stdlib 的，
// 这样「无模型、无数据库」的降级路径可以被完整编译与测试（方案 §1.1 对标的
// 「零核心依赖、装了包不等于配好了」）。存储后端引入的纯 Go SQLite 驱动
// 在 M1 随实现一并进入 require 段。

