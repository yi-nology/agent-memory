package evaluation

import (
	"context"
	"errors"
	"fmt"
)

// Retriever 评测用的召回器（窄接口：评测面不关心召回器怎么实现的）。
//
// 窄是有意的：评测只问「给一条查询，你返回哪些标识、什么顺序」。
// 取数、排序、过滤、预算怎么实现，是被评测方的事——
// 评测面一旦去管这些，它评的就不是「召回质量」而是「实现细节」。
//
// 实现方可以是本仓的召回流水线，也可以是宿主自造的召回链；
// 评测面与两者都不耦合。
type Retriever interface {
	// Recall 返回这一条用例的召回结果（标识有序）。
	//
	// 返回的标识应当是**事实卡 ID**（与用例标注同一套标识空间）。
	Recall(ctx context.Context, c Case) ([]string, error)
}

// Report 一次评测的报告。
type Report struct {
	// Dataset 数据集名。
	Dataset string
	// Metrics 聚合指标。
	Metrics Metrics
	// PerCase 逐用例明细（哪条命中了什么）。
	//
	// 顺序即用例顺序：同一份数据 + 同一个召回器 → 同一份报告。
	PerCase []CaseResult
}

// CaseResult 一条用例的评测明细。
//
// 明细存在的理由：聚合指标只说「整体准不准」，
// 而质量退化总是发生在**某几条**用例上——
// 「哪条漏了、漏的是哪条相关事实」才是能动手的信息。
type CaseResult struct {
	// ID 用例标识。
	ID string
	// Query 查询串（冗余存一份，报告才能自解释）。
	Query string
	// Retrieved 召回结果（有序，事实卡 ID）。
	Retrieved []string
	// Hits 命中的相关 ID（按召回出现顺序）。
	Hits []string
	// Missed 漏掉的相关 ID（相关集合减去召回结果，按相关集合原序）。
	Missed []string
	// FirstHitRank 第一条相关结果的排名（1 起；0=一条都没命中）。
	FirstHitRank int
	// Skipped 该用例是否被跳过（相关集合为空，无从判分）。
	Skipped bool
	// Metrics 本用例的指标（Skipped 时全零）。
	Metrics Metrics
}

// Run 跑一遍数据集（k 集合取 DefaultKs）。
func Run(ctx context.Context, ds Dataset, r Retriever) (*Report, error) {
	return RunWithKs(ctx, ds, r, DefaultKs)
}

// RunWithKs 跑一遍数据集，k 集合显式给定（为空时取 DefaultKs）。
func RunWithKs(ctx context.Context, ds Dataset, r Retriever, ks []int) (*Report, error) {
	if r == nil {
		return nil, errors.New("evaluation: 召回器为空")
	}
	ks = normalizeKs(ks)

	results := make([]CaseResult, 0, len(ds.Cases))
	for _, c := range ds.Cases {
		got, err := r.Recall(ctx, c)
		if err != nil {
			// 召回器报错**上抛**，不静默当 0 分。
			//
			// 静默 0 分的失效形态是「召回器炸了」与「召回很烂」长得一模一样：
			// 两者的报告都是一堆低分，排查时先怀疑算法，
			// 而真正的原因是召回链根本没跑起来。
			// 判据与本仓别处一致：消费方被迫拿一个不可信的数字去做决定，
			// 比直接崩掉更坏。
			return nil, fmt.Errorf("evaluation: 用例 %s 召回失败: %w", c.ID, err)
		}
		results = append(results, scoreCase(c, got, ks))
	}
	return &Report{
		Dataset: ds.Name,
		Metrics: aggregate(results, ks),
		PerCase: results,
	}, nil
}
