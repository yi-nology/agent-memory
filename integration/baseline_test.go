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

// 本文件：**召回质量的基线**。
//
// # 这条用例是干什么的
//
// 它把一组合成的、标注好的事实灌进**真召回链**，跑一遍评测，
// 然后把三套口径的数字**打出来**并**钉住下限**。
//
// 两个作用：
//
//  1. **可复现的数字**：以前「召回准不准」只能靠感觉；
//     现在有一组可重复算出来的数字，任何排序/融合/查询理解的改动
//     都能拿它对比（跑 `go test ./integration/ -run TestRecallBaseline -v` 即可复现）。
//  2. **回归护栏**：下限不是摆设——改了召回链让指标掉下去，这条用例会红。
//
// # 为什么下限而不是「与上次完全相等」
//
// 完全相等太脆：任何无关的小改动（词表、顺序、默认参数）都会让数字抖动，
// 于是大家开始习惯性地忽略它。给下限是折中：
// 明显退化会红，而正常波动不会。
//
// # 这组数字的**前提**要说清
//
// 合成集的「相关」是按**词面共享**标的，故它衡量的是词面召回的上限，
// 不衡量语义召回——语义那一路要用真模型 Embedder 才算数
// （本仓自带的哈希嵌入只捕捉词面软匹配）。
// 故这组数字是**下限基线**，不是「我们比谁强」的结论。

// baselineDataset 基线用的合成集规模。
const baselineDataset = 40

// TestRecallBaseline 召回质量的基线：跑真召回链，出三套口径的数字。
func TestRecallBaseline(t *testing.T) {
	ctx := context.Background()
	set := evaluation.NewSynthetic(20261021, baselineDataset)

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
	r := evalRetriever{p: p, ns: ns}

	// 检索口径（Recall@nDCG@… / MRR / HitRate）。
	rep, err := evaluation.Run(ctx, set.Dataset, r)
	if err != nil {
		t.Fatal(err)
	}
	// 问答口径（上下文充分性）+ token 效率。
	qa, err := evaluation.RunQA(ctx, set.QA, r, evaluation.QAOptions{})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("基线（合成集 seed=20261021，用例 %d 条）：", baselineDataset)
	t.Logf("  检索口径：Recall@1=%.3f Recall@3=%.3f Recall@5=%.3f Recall@10=%.3f",
		rep.Metrics.RecallAtK[1], rep.Metrics.RecallAtK[3],
		rep.Metrics.RecallAtK[5], rep.Metrics.RecallAtK[10])
	t.Logf("             nDCG@5=%.3f MRR=%.3f HitRate=%.3f",
		rep.Metrics.NDCGAtK[5], rep.Metrics.MRR, rep.Metrics.HitRate)
	t.Logf("  问答口径：上下文充分率=%.3f（答对率未测：本用例没配回答器与判分器）",
		qa.Metrics.Sufficient)
	t.Logf("  token 效率：每次查询平均 %.1f token，每千 token 命中 %.3f 条，每命中一条 %.1f token",
		qa.Efficiency.AvgTokensPerQuery, qa.Efficiency.HitsPerKToken, qa.Efficiency.TokensPerHit)

	// ── 下限（回归护栏）──
	//
	// 数字取自本机实测值再往下留一截余量：退化到明显不可接受才红，
	// 而正常波动不红。
	if got := rep.Metrics.RecallAtK[5]; got < 0.45 {
		t.Fatalf("Recall@5 基线护栏：实得 %.3f，低于 0.45", got)
	}
	if got := rep.Metrics.NDCGAtK[5]; got < 0.55 {
		t.Fatalf("nDCG@5 基线护栏：实得 %.3f，低于 0.55", got)
	}
	if got := rep.Metrics.HitRate; got < 0.90 {
		t.Fatalf("HitRate 基线护栏：实得 %.3f，低于 0.90", got)
	}
	// 上下文充分率当前极低（见 docs/EVAL_BASELINE.md 的分析：
	// 词面路是隐式 AND，多话题查询几乎打不中），
	// 故这里的护栏只挡「彻底失效」，不挡小幅波动。
	if got := qa.Metrics.Sufficient; got < 0.01 {
		t.Fatalf("上下文充分率基线护栏：实得 %.3f，低于 0.01（彻底失效）", got)
	}
	if !qa.Efficiency.Measured {
		t.Fatal("token 效率应被测：真召回链报得出每条结果的 token")
	}
	// token 成本的上限护栏：召回的预算就是 1500（recall.DefaultConfig），
	// 每次查询带出去的 token 不该超过它太多。
	if got := qa.Efficiency.AvgTokensPerQuery; got <= 0 || got > 2000 {
		t.Fatalf("每次查询的 token 成本应在 (0, 2000] 区间，实得 %.1f", got)
	}
}

// TestBaselineIsReproducible 基线数字必须**可复现**。
//
// 不可复现的基线等于没有基线：数字每次都不一样，
// 于是「这次改动让指标变好了」这句话没人能验证。
func TestBaselineIsReproducible(t *testing.T) {
	ctx := context.Background()
	run := func() (float64, float64) {
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
		return rep.Metrics.RecallAtK[5], rep.Metrics.NDCGAtK[5]
	}
	// ⚠️ 分开接：Go 里多返回值的调用**不能**混在元组里（`run(), run()` 编译不过）。
	a1, a2 := run()
	b1, b2 := run()
	if a1 != b1 || a2 != b2 {
		t.Fatalf("两次跑的数字应逐条相同: %v/%v vs %v/%v", a1, a2, b1, b2)
	}
	_ = store.Namespace("")
	_ = types.Scope("")
}
