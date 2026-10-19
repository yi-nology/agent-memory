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
//   - 核心零依赖：默认只准 stdlib 与本仓；受控放行的纯 stdlib 工具级模块
//     在 architecture-policy.yaml 的 allow_third_party 段逐条登记
//     （带 reason + until + removal），由门禁 memory-core-stdlib-only 强制，
//     make tidy-check 复核 go.mod 面；
//   - 领域词（主机/集群/Pod/巡检/专家…）进本仓代码即红，由门禁强制；
//   - 新依赖方向先改 architecture-policy.yaml 再写代码（policy 先行）。

// 下面这条是离线可解析的前提：modernc.org/sqlite 的 go.mod 钉的是更早的
// 版本，本机模块缓存里没有那些精确版本，而 MVS 取「最大」——显式抬到缓存里
// 存在的版本即可，不改任何行为面。
//
// ⚠️ 这里的第三方包分两类，**纪律不同**：
//
//   - lib/pq：数据库驱动，只出现在 storage/ 下（核心零依赖纪律的存储层例外）；
//   - oekit/pkg、oagentkit/textx、oagentkit/guardx：纯 stdlib 工具级模块，
//     进了核心，故必须在 architecture-policy.yaml 的 allow_third_party 段
//     逐条登记理由与到期日（门禁加载期校验，缺一条即失败）。
//
// 曾经设想「借 guardx/injectband.SanitizeBody 做转义」，实测它是写死的 tagRe、
// 只认 <system-reminder> 家族：拿它守 <memory-context> 这条数据区，伪造的
// 闭合标签原样穿过——见 ports/band.go 与本文件
// docs/AGENT_MEMORY_PLAN_20261019.md §四 的偏离记录。
// 故**转义**归本仓自己所有；guardx 现在只被用在凭据脱敏规则表上。
require (
	github.com/lib/pq v1.10.9
	github.com/oagentkit/guardx v0.8.0
	github.com/oagentkit/textx v0.6.0
	github.com/oekit/pkg v0.16.0
	modernc.org/sqlite v1.40.1
)

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
