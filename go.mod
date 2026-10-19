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
//   - 核心零依赖：**只有 storage/ 允许第三方包**，其余模块只准 stdlib 与本仓
//     （门禁 memory-core-stdlib-only 强制，make tidy-check 复核）；
//   - 领域词（主机/集群/Pod/巡检/专家…）进本仓代码即红，由门禁强制；
//   - 新依赖方向先改 architecture-policy.yaml 再写代码（policy 先行）。

require modernc.org/sqlite v1.40.1

// 下面三条是离线可解析的前提：modernc.org/sqlite 的 go.mod 钉的是更早的
// 版本，本机模块缓存里没有那些精确版本，而 MVS 取「最大」——显式抬到缓存里
// 存在的版本即可，不改任何行为面。
require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20250620022241-b7579e27df2b // indirect
	golang.org/x/sys v0.49.0 // indirect
	modernc.org/libc v1.66.10 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
