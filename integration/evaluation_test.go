package integration_test

import (
	"context"
	"testing"

	"github.com/yi-nology/agent-memory/evaluation"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：**评测面接到真召回链**。
//
// # 为什么这件事在 integration 里做
//
// evaluation 包**不依赖** pipeline/recall（policy 里刻意没给那条方向），
// 因为评测面要能评**任何**召回器——宿主自造的召回器也算。
// 故它只声明一个窄接口（evaluation.Retriever），由调用方提供实现。
//
// 而「用本仓的真召回链跑一遍合成数据集」这件事，既不是 evaluation 的事，
// 也不是召回包自己的事（召回包不该知道评测的存在）：
// 它是**接缝**——正是 integration 这个模块存在的理由。
//
// # 这条用例钉住什么
//
//  1. 评测面**能驱动真召回**（接口形状对得上）；
//  2. 词面召回在合成语料上**确实有召回率**（不是恒零）；
//  3. **可复现**：同 seed 跑两遍，指标逐条相同。
//
// 第 3 条最要紧：不可复现的评测等于没有评测——
// 而它的失效形态是静默的（数字照出，只是每次不一样）。

// evalRetriever 用真实召回链跑评测用例。
type evalRetriever struct {
	p  *recall.Pipeline
	ns store.Namespace
}

// Recall 跑一次真召回，返回事实卡 ID（按召回次序）。
func (r evalRetriever) Recall(ctx context.Context, c evaluation.Case) ([]string, error) {
	res, err := r.p.Recall(ctx, recall.Request{
		Namespace: r.ns, Query: c.Query, Scope: c.Scope,
		Entities: c.Entities, SessionID: c.SessionID, ThreadID: c.ThreadID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Snippets))
	for _, s := range res.Snippets {
		// 只算事实卡：画像与原始事件不是「相关事实」这个标注空间里的东西。
		if s.Source == recall.SourceAtom {
			out = append(out, s.RefID)
		}
	}
	return out, nil
}

// TestEvaluationOnSyntheticDataset 合成数据集跑真召回链。
func TestEvaluationOnSyntheticDataset(t *testing.T) {
	ctx := context.Background()
	set := evaluation.NewSynthetic(20261021, 12)

	s := openFresh(t)
	for _, f := range set.Facts() {
		if err := s.SaveAtom(ctx, ns, f); err != nil {
			t.Fatalf("灌库失败: %v", err)
		}
	}
	p, err := recall.New(recall.DefaultConfig(), s, ports.StaticEstimator{}, ports.MemoryBand{})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := evaluation.Run(ctx, set.Dataset, evalRetriever{p: p, ns: ns})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Metrics.Cases == 0 {
		t.Fatal("参与评分的用例数为 0：标注与语料对不上")
	}
	// 词面召回在合成语料上**应当**有召回率（恒零说明取数这条路断了）。
	if rep.Metrics.RecallAtK[10] <= 0 {
		t.Fatalf("词面召回在合成语料上应有召回率: %+v", rep.Metrics)
	}
	// 精确率不该是 1.0：合成语料里混了近义干扰，
	// 恒 1.0 说明干扰项根本没进候选池（那测不出任何东西）。
	if rep.Metrics.PrecisionAtK[10] >= 1 {
		t.Fatalf("精确率恒为 1 说明干扰项没进池，测不出东西: %+v", rep.Metrics)
	}
}

// TestEvaluationIsReproducible 同 seed 跑两遍，指标逐条相同。
func TestEvaluationIsReproducible(t *testing.T) {
	ctx := context.Background()
	run := func() evaluation.Metrics {
		set := evaluation.NewSynthetic(7, 8)
		s := openFresh(t)
		for _, f := range set.Facts() {
			if err := s.SaveAtom(ctx, ns, f); err != nil {
				t.Fatalf("灌库失败: %v", err)
			}
		}
		p, err := recall.New(recall.DefaultConfig(), s, ports.StaticEstimator{}, ports.MemoryBand{})
		if err != nil {
			t.Fatal(err)
		}
		rep, err := evaluation.Run(ctx, set.Dataset, evalRetriever{p: p, ns: ns})
		if err != nil {
			t.Fatal(err)
		}
		return rep.Metrics
	}
	a, b := run(), run()
	if a.RecallAtK[5] != b.RecallAtK[5] || a.NDCGAtK[5] != b.NDCGAtK[5] || a.MRR != b.MRR {
		t.Fatalf("同 seed 的两次评测结果不同：%+v vs %+v", a, b)
	}
}

// TestEvaluationReportsMissedRelevant 逐用例明细要能看出漏了哪些相关的。
//
// 判据：明细里必须能读到「哪些相关的事实没被召回」——
// 没有它，评测结果只是一堆数字，排查时无从下手。
func TestEvaluationReportsMissedRelevant(t *testing.T) {
	ctx := context.Background()
	set := evaluation.NewSynthetic(3, 4)
	s := openFresh(t)
	for _, f := range set.Facts() {
		if err := s.SaveAtom(ctx, ns, f); err != nil {
			t.Fatalf("灌库失败: %v", err)
		}
	}
	p, err := recall.New(recall.DefaultConfig(), s, ports.StaticEstimator{}, ports.MemoryBand{})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := evaluation.Run(ctx, set.Dataset, evalRetriever{p: p, ns: ns})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.PerCase) == 0 {
		t.Fatal("逐用例明细不该为空")
	}
	found := false
	for _, c := range rep.PerCase {
		for _, id := range c.Missed {
			if id != "" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("明细里应能看出漏了哪些相关的: %+v", rep.PerCase)
	}
	_ = types.Scope("")
}
