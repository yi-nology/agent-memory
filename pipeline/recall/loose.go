package recall

import (
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：宽松词面检索这条**可选能力**的接线。
//
// # 它是可选的
//
// 宽松检索（命中任一词就算）是**增强**：它让多话题查询打不中的问题缓解。
// 它不是**前提**：宿主没实现那个能力时，召回链必须照跑（少召回一些）。
//
// 契约面在 store 包（`store.AtomLooseSearcher`）——
// 之所以不落在 `recall.Reader` 里：那张面**仓外的宿主会实现**，
// 往它上面加方法 = 仓外实现者当天编译失败（issue #12）。
//
// # 三条要件（缺一不可）
//
//  1. **探测**（New 里做一次）：`store.LooseSearcher`；
//  2. **降级**：没有就整条路不存在，召回回落到严格路（见 gather.collectLoose）；
//  3. **可观测**：`Result.LooseSearchUnavailable` + 指标
//     `recall.loose.unavailable`（指标名集中定义在 recall.go 的常量段）。
//
// 缺第三条 ⇒ 静默降级：症状只是「多话题查询打不中」，看着像内容不够，
// 而病因是「宿主没实现那个接口」——两者处置完全不同。

// probeLooseSearcher 探测宿主有没有宽松检索能力。
//
// 探测在**构造期**做一次，不在每次召回时做：
// 能力不会中途长出来，而每次召回都断言一次是白花的开销
// （还会在指标里灌重复的计数）。
//
// 返回 nil = 没有 ⇒ 调用方走降级路径。
func probeLooseSearcher(s Reader) store.AtomLooseSearcher {
	ls, _ := store.LooseSearcher(s)
	return ls
}

// markDegraded 记一次「降级过的召回」。
//
// 口径：**每次召回**记一次（缓存命中的那次也算——它端出来的就是降级结果）。
//
// ⚠️ 为什么不记在取数段：那样记出来的是「降级取数次数」，
// 而缓存命中时根本不取数 ⇒ 同一个问题在两次口径下数字不同，
// 看板上的曲线就没法解释（宿主会问「为什么刚才还有缓存命中却没涨」）。
func (p *Pipeline) markDegraded(req Request) {
	if p.loose == nil {
		p.metrics().Incr(MetricLooseSearchUnavailable, 1)
	}
	// 语义路的全局读者检索不可用（issue #62）：只对**空读者**的召回记。
	// 非空读者走「全局 + 同归属」，语义面的冻结语义本来就覆盖，无降级可言。
	if p.semanticGlobalDegraded(req.Scope) {
		p.metrics().Incr(MetricSemanticGlobalUnavailable, 1)
	}
}

// semanticGlobalDegraded 本轮召回的语义路是否处在「全局读者降级」形态：
// 空读者 + 装了语义面 + 语义面没实现 ports.GlobalSemanticReader。
func (p *Pipeline) semanticGlobalDegraded(scope types.Scope) bool {
	return scope == "" && p.cfg.Semantic != nil && p.semGlobal == nil
}
