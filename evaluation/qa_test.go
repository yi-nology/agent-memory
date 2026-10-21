package evaluation

import (
	"context"
	"errors"
	"testing"
)

// 本文件：问答口径与 token 效率的用例。
//
// # 期望值全部手写
//
// 期望值若由实现自己算，就恒等于真。故这里的每个数字都写死在用例里，
// 旁边附一句「为什么是这个数」。

// qaRetriever 固定返回的召回器（本包已有另一个 fakeRetriever，名字让开）。
type qaRetriever struct {
	ids    []string
	byCase map[string][]string
	err    error
	calls  int
}

func (f *qaRetriever) Recall(_ context.Context, c Case) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if ids, ok := f.byCase[c.ID]; ok {
		return ids, nil
	}
	return f.ids, nil
}

// qaTokenRetriever 带 token 与正文的召回器。
type qaTokenRetriever struct {
	qaRetriever
	toks []int
	text []string
}

func (f *qaTokenRetriever) RecallTokenized(_ context.Context, c Case) ([]Retrieved, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	ids := f.ids
	if per, ok := f.byCase[c.ID]; ok {
		ids = per
	}
	out := make([]Retrieved, 0, len(ids))
	for i, id := range ids {
		r := Retrieved{ID: id}
		if i < len(f.toks) {
			r.Tokens = f.toks[i]
		}
		if i < len(f.text) {
			r.Text = f.text[i]
		}
		out = append(out, r)
	}
	return out, nil
}

// fakeAnswerer 回一句固定答案。
type fakeAnswerer struct {
	ans string
	err error
	// seenCtx 记下它看到的上下文（验「给的是正文不是标识」）。
	seenCtx []string
}

func (a *fakeAnswerer) Answer(_ context.Context, _ string, ctx []string) (string, error) {
	a.seenCtx = ctx
	if a.err != nil {
		return "", a.err
	}
	return a.ans, nil
}

// fakeJudge 按「答案包含期望」判对错。
type fakeJudge struct{ err error }

func (j fakeJudge) Correct(_ context.Context, _, answer, expected string) (bool, error) {
	if j.err != nil {
		return false, j.err
	}
	return answer == expected, nil
}

// 上下文充分性：支撑事实缺一条 → 不充分（充分率 0）。
func TestQASufficiency(t *testing.T) {
	ds := QADataset{Name: "qa", Cases: []QACase{{
		Case:       Case{ID: "c1", Query: "q", Relevant: []string{"a1", "a2"}},
		Question:   "问句",
		Answer:     "答案",
		SupportIDs: []string{"a1", "a2"},
	}}}
	// 只召回到 a1：a2 缺了 ⇒ 不充分。
	r := &qaRetriever{ids: []string{"a1"}}
	res, err := RunQA(context.Background(), ds, r, QAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.Sufficient != 0 {
		t.Fatalf("支撑事实缺一条时充分率应为 0，实得 %v", res.Metrics.Sufficient)
	}
	if len(res.PerCase[0].MissingSupport) != 1 || res.PerCase[0].MissingSupport[0] != "a2" {
		t.Fatalf("明细里应指出缺了哪条: %+v", res.PerCase[0].MissingSupport)
	}

	// 两条都在 ⇒ 充分。
	r2 := &qaRetriever{ids: []string{"a1", "a2"}}
	res2, err := RunQA(context.Background(), ds, r2, QAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Metrics.Sufficient != 1 {
		t.Fatalf("支撑事实齐备时充分率应为 1，实得 %v", res2.Metrics.Sufficient)
	}
}

// 答对率：配了回答器与判分器才测；没配就是「未测」而不是 0。
func TestQAAnswered(t *testing.T) {
	ds := QADataset{Name: "qa", Cases: []QACase{
		{
			Case:     Case{ID: "c1", Query: "q", Relevant: []string{"a1"}},
			Question: "问句", Answer: "答对", SupportIDs: []string{"a1"},
		},
		{
			Case:     Case{ID: "c2", Query: "q", Relevant: []string{"a2"}},
			Question: "问句", Answer: "另一个", SupportIDs: []string{"a2"},
		},
	}}
	// 两条都召回到（充分），但回答器只答对第一条 ⇒ 答对率 1/2 = 0.5。
	r := &qaRetriever{ids: []string{"a1", "a2"}}
	ans := &fakeAnswerer{ans: "答对"}
	res, err := RunQA(context.Background(), ds, r, QAOptions{
		Answerer: ans, Judge: fakeJudge{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Metrics.AnsweredMeasured {
		t.Fatal("配了回答器与判分器时答对率应被测")
	}
	if res.Metrics.Answered != 0.5 {
		t.Fatalf("答对率应为 1/2=0.5，实得 %v", res.Metrics.Answered)
	}
	// 上下文给的是**正文**不是标识。
	if len(ans.seenCtx) == 0 || ans.seenCtx[0] != "a1" {
		t.Fatalf("回答器看到的应是召回的正文: %+v", ans.seenCtx)
	}

	// 没配回答器 ⇒ 「未测」。
	res2, err := RunQA(context.Background(), ds, r, QAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Metrics.AnsweredMeasured {
		t.Fatal("没配回答器时答对率不该被测")
	}
}

// token 效率：手算三个数。
//
// 夹具：2 条用例，各召回 2 条，token 都是 [100, 50]；
// 两条用例的支撑事实都在各自的结果里（充分）。
//
//   - 总 token = 150 + 250 = 300；每次查询平均 = 300 / 2 = **150**。
//   - 「命中」= 上下文充分（该答所需的事实都在）的用例数 = **2**。
//   - 每千 token 命中 = 2 / (300 / 1000) = 2 / 0.3 ≈ **6.667**。
//   - 每命中一条花多少 token = 300 / 2 = **150**。
func TestEfficiencyHandCalculated(t *testing.T) {
	ds := QADataset{Name: "qa", Cases: []QACase{
		{
			Case:     Case{ID: "c1", Query: "q", Relevant: []string{"a1"}},
			Question: "q", Answer: "x", SupportIDs: []string{"a1"},
		},
		{
			Case:     Case{ID: "c2", Query: "q", Relevant: []string{"b1"}},
			Question: "q", Answer: "x", SupportIDs: []string{"b1"},
		},
	}}
	r := &qaTokenRetriever{
		qaRetriever: qaRetriever{byCase: map[string][]string{
			"c1": {"a1", "a2"},
			"c2": {"b1", "b2"},
		}},
		toks: []int{100, 50},
		text: []string{"甲的事实", "甲的事实2"},
	}
	res, err := RunQA(context.Background(), ds, r, QAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	e := res.Efficiency
	if !e.Measured {
		t.Fatal("召回器能报 token 时效率应被测")
	}
	if e.AvgTokensPerQuery != 150 {
		// 两条用例各召回 [100, 50] ⇒ 每次 150 token，平均也是 150。
		t.Fatalf("每次查询平均 token 应为 150，实得 %v", e.AvgTokensPerQuery)
	}
	if e.HitsPerKToken != 2/(300.0/1000) {
		t.Fatalf("每千 token 命中数算错: %v", e.HitsPerKToken)
	}
	if e.TokensPerHit != 150 {
		t.Fatalf("每命中一条的 token 应为 150，实得 %v", e.TokensPerHit)
	}
}

// 召回器报不了 token ⇒ 「未测」，而不是 0 冒充漂亮数字。
func TestEfficiencyUnmeasuredWithoutTokens(t *testing.T) {
	ds := QADataset{Name: "qa", Cases: []QACase{{
		Case:     Case{ID: "c1", Query: "q", Relevant: []string{"a1"}},
		Question: "q", Answer: "x", SupportIDs: []string{"a1"},
	}}}
	r := &qaRetriever{ids: []string{"a1"}}
	res, err := RunQA(context.Background(), ds, r, QAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Efficiency.Measured {
		t.Fatal("召回器报不了 token 时效率不该被测")
	}
}

// 召回器报错 → **上抛**，不静默当 0 分。
func TestQARetrieverErrorPropagates(t *testing.T) {
	ds := QADataset{Name: "qa", Cases: []QACase{{
		Case:     Case{ID: "c1", Query: "q", Relevant: []string{"a1"}},
		Question: "q", Answer: "x", SupportIDs: []string{"a1"},
	}}}
	r := &qaRetriever{err: errors.New("召回器炸了")}
	if _, err := RunQA(context.Background(), ds, r, QAOptions{}); err == nil {
		t.Fatal("召回器报错必须上抛：静默 0 分与「召回很烂」长得一模一样")
	}
}

// 问答视图与检索视图是**同一份语料**：相关集合与支撑集合一致。
func TestSyntheticQASharesCorpus(t *testing.T) {
	set := NewSynthetic(11, 3)
	if len(set.QA.Cases) != len(set.Dataset.Cases) {
		t.Fatalf("两个视图的用例数应一致: %d vs %d", len(set.QA.Cases), len(set.Dataset.Cases))
	}
	for i, qa := range set.QA.Cases {
		c := set.Dataset.Cases[i]
		if qa.ID != c.ID {
			t.Fatalf("第 %d 条用例标识对不上: %s vs %s", i, qa.ID, c.ID)
		}
		if len(qa.SupportIDs) != len(c.Relevant) {
			t.Fatalf("第 %d 条的支撑集合与相关集合应一致", i)
		}
		if qa.Answer == "" {
			t.Fatalf("第 %d 条的标准答案不该为空", i)
		}
	}
	// 可复现：同 seed 两份数据集逐条相等。
	other := NewSynthetic(11, 3)
	for i := range set.QA.Cases {
		if set.QA.Cases[i].Answer != other.QA.Cases[i].Answer {
			t.Fatalf("同 seed 的第 %d 条答案应相同", i)
		}
	}
}
