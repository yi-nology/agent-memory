package evaluation

import (
	"context"
	"fmt"
)

// 本文件：token 效率口径、问答口径、行为（决策）口径。
//
// # 为什么要有这三套口径
//
// 检索口径（Recall@k / nDCG@k …）答的是「该找的找到了吗」，
// 但它**不答**「为此花了多少」——而后者才是生产环境里真正的约束：
// 同样准不准，塞进 prompt 的 token 差三倍就是三倍的钱。
//
// 竞品把这件事摆在标题上（同等准确率下每次召回只花 ~7k token，
// 而全上下文要 25k+），本包把它做成**可算的指标**而不是叙事。
//
// # 三套口径的分工
//
//   - 检索口径：找得准不准（已有）。
//   - token 效率：找同样的东西花了多少（成本）。
//   - 问答/行为口径：**记忆到了之后有没有用上**（最终效果）。
//
// 第三套最要紧：前两套都是代理指标——召回了 10 条相关事实，
// 不代表模型能据此答对题。问答口径把「上下文 → 答案」这一段接上，
// 判分用的模型由**宿主**给（语义强度归宿主，管线归内核）。
//
// # 口径仍然写死在本包
//
// 答案由宿主模型生成，但**怎么算对**由本包定（Judge 是接口，
// 实现可以宿主给；不配就退到「上下文充分性」这一确定性判据）。

// Retrieved 一条召回结果。
type Retrieved struct {
	// ID 事实卡标识（与用例标注同一套标识空间）。
	ID string
	// Tokens 这条内容占用的 token（0 = 未计量）。
	Tokens int
	// Text 这条内容的正文（供回答器用；可空 = 只给标识）。
	Text string
}

// TokenizedRetriever 能报出每条结果 token 数与正文的召回器（**可选**）。
//
// # 为什么是可选接口，而不是改 Retriever 的签名
//
// token 效率这个指标只在「知道每条内容多大」时才算得出，
// 而不是每个召回器都报得出（自造的召回器未必有计量器）。
// 强改签名会让所有实现方为一个**可选**指标付出改造代价，
// 而多数人只会把 Tokens 填 0 —— 于是指标算出来是「未计量」，
// 却被当成「效率很好」看。
//
// 故：能报就报，报不了就**明说没测**（Efficiency.Measured=false）。
type TokenizedRetriever interface {
	Retriever
	// RecallTokenized 返回这一条用例的召回结果（带 token 与正文）。
	RecallTokenized(ctx context.Context, c Case) ([]Retrieved, error)
}

// Efficiency token 效率指标。
//
// # 三个数是一件事的三个面
//
//   - AvgTokensPerQuery：每次查询带出去的 token（**成本**）。
//   - HitsPerKToken：每千 token 命中多少条相关事实（**效率**，越高越好）。
//   - TokensPerHit：每命中一条相关事实花多少 token（**单位收益成本**，越低越好）。
//
// 只看第一个会被「少召回」骗过去（什么都不召回，成本最低）；
// 只看后两个会被「召回巨量内容」骗过去。故三个一起给。
type Efficiency struct {
	// Measured 是否真的测了（召回器能报 token 才测）。
	//
	// 刻意与「效率很好」分开：没测就是没测，
	// 不能让 0 冒充一个漂亮的效率数。
	Measured bool
	// AvgTokensPerQuery 每次查询平均带出的 token。
	AvgTokensPerQuery float64
	// HitsPerKToken 每千 token 命中多少条相关事实。
	HitsPerKToken float64
	// TokensPerHit 每命中一条相关事实花多少 token（0 = 一条没命中）。
	TokensPerHit float64
}

// QACase 问答式用例（公开基准那种形态：一个问题 + 一个答案 + 支撑它的事实）。
type QACase struct {
	// Case 内嵌的检索用例（查询串、相关集合、归属、实体提示…）。
	Case
	// Question 问题（人话形态；可与 Query 不同——
	// Query 是检索串，Question 是给人看的问法）。
	Question string
	// Answer 标准答案（判分用）。
	Answer string
	// SupportIDs 支撑这条答案的事实（**充分性**判据）。
	//
	// 为什么要有它：没有它就答不了「上下文够不够」——
	// 而「上下文够不够」是**确定性**的判据（不依赖模型），
	// 是问答口径在没配模型时唯一能给的数。
	SupportIDs []string
}

// QADataset 一组问答用例。
type QADataset struct {
	// Name 数据集名。
	Name string
	// Cases 用例集（顺序即评测顺序）。
	Cases []QACase
}

// Answerer 用召回上下文回答问题（**宿主的能力**）。
//
// # 为什么模型在宿主那边
//
// 判分与回答的质量取决于宿主选的模型与提示词，而本仓不该替宿主选。
// 本仓只保证「同一份上下文 + 同一个回答器 → 同一个答案」的可复现前提
// （上下文由本包按次序拼好，不掺别的东西）。
type Answerer interface {
	// Answer 用给定的上下文回答问题。
	//
	// context 是召回结果按次序拼好的**正文**（不是标识）——
	// 给标识等于让模型自己猜库里有什么。
	Answer(ctx context.Context, question string, context []string) (string, error)
}

// Judge 判答案对不对（规则或模型，宿主给）。
type Judge interface {
	// Correct 判断 answer 是否答对了 question（expected 是标准答案）。
	Correct(ctx context.Context, question, answer, expected string) (bool, error)
}

// QAMetrics 问答口径的指标。
type QAMetrics struct {
	// Cases 参与评分的问答用例数。
	Cases int
	// Sufficient 上下文充分率：该答所需的事实**都在**上下文里的用例比例。
	//
	// 这是**确定性**判据（不依赖模型）：支撑事实缺一条就不足。
	// 它的意义是「记忆到了之后有没有可能用上」。
	Sufficient float64
	// AnsweredMeasured 是否测了「答对率」。
	//
	// 没配 Answerer/Judge 时它是 false（而不是 0）——
	// 「没测」与「全答错」必须能分开。
	AnsweredMeasured bool
	// Answered 答对率（仅当 AnsweredMeasured）。
	Answered float64
	// AvgTokens 每次问答带出的平均 token（0 = 未计量）。
	AvgTokens float64
}

// QAOptions 问答评测的选项。
type QAOptions struct {
	// Answerer 回答器（可 nil：只测上下文充分性）。
	Answerer Answerer
	// Judge 判分器（可 nil：不测答对率）。
	//
	// 给了 Answerer 却没给 Judge 是不完整的：答了但没法判，
	// 那种情况按「没测答对率」处理（AnsweredMeasured=false），
	// 而不是把答案丢掉。
	Judge Judge
	// Ks 截断（为空 = DefaultKs）。
	Ks []int
}

// QACaseResult 一条问答用例的明细。
type QACaseResult struct {
	// ID 用例标识。
	ID string
	// Question 问题。
	Question string
	// Retrieved 召回结果（有序）。
	Retrieved []string
	// Tokens 这次带出去的 token 合计。
	Tokens int
	// Hits 召回结果里**相关事实**的条数（与检索口径的命中同义）。
	//
	// ⚠️ 它与「充分」**不是一回事**：充分 = 支撑事实**全部**在上下文里
	// （问答口径的严判据）；命中 = 捞到了多少条相关的（检索口径）。
	// token 效率用**命中**当收益分子（用的是检索口径那套），
	// 用充分当分子会让「每命中一条的 token」大出一个量级——而那个数
	// 测的是「上下文够不够答」，不是效率。
	Hits int
	// MissingSupport 上下文里**缺**的支撑事实。
	MissingSupport []string
	// Sufficient 上下文是否充分。
	Sufficient bool
	// Answer 模型给的答案（没配回答器时为空）。
	Answer string
	// Correct 答案是否正确（仅当测了）。
	Correct bool
	// Skipped 该用例是否被跳过（支撑集合为空，无从判分）。
	Skipped bool
}

// QAResult 问答评测的结果。
type QAResult struct {
	// Dataset 数据集名。
	Dataset string
	// Metrics 问答口径的指标。
	Metrics QAMetrics
	// Efficiency token 效率。
	Efficiency Efficiency
	// PerCase 逐用例明细。
	PerCase []QACaseResult
}

// RunQA 跑一遍问答数据集。
//
// # 三个口径一次算齐
//
// 检索口径（Recall@nDCG@…）在 CaseResult 里；本函数额外给
// 问答口径（充分性 / 答对率）与 token 效率。
//
// # 答不出来时**不猜**
//
// 上下文不足的用例：充分性记 false，答对率按「未测」处理
// （答了也是瞎答）。把「上下文不够」混进「答错」里，
// 就分不清是召回的锅还是模型的锅——而这两者的处置完全不同。
func RunQA(ctx context.Context, ds QADataset, r Retriever, opts QAOptions) (*QAResult, error) {
	if r == nil {
		return nil, fmt.Errorf("evaluation: 召回器为空")
	}
	tr, tokenized := r.(TokenizedRetriever)

	perCase := make([]QACaseResult, 0, len(ds.Cases))
	var (
		totalTokens int
		sufficient  int
		answered    int
		answeredCnt int
		measured    bool
	)

	for _, c := range ds.Cases {
		var (
			got  []Retrieved
			ids  []string
			err  error
			toks int
		)
		if tokenized {
			got, err = tr.RecallTokenized(ctx, c.Case)
			if err != nil {
				return nil, fmt.Errorf("evaluation: 用例 %s 召回失败: %w", c.ID, err)
			}
			for _, r := range got {
				ids = append(ids, r.ID)
				toks += r.Tokens
			}
		} else {
			ids, err = r.Recall(ctx, c.Case)
			if err != nil {
				return nil, fmt.Errorf("evaluation: 用例 %s 召回失败: %w", c.ID, err)
			}
		}

		res := QACaseResult{
			ID:        c.ID,
			Question:  c.Question,
			Retrieved: ids,
			Tokens:    toks,
		}
		totalTokens += toks

		// 支撑事实的齐备性（充分性）——**确定性**判据。
		have := map[string]bool{}
		for _, id := range ids {
			have[id] = true
		}
		// 检索命中数（效率指标的收益分子）。
		rel := map[string]bool{}
		for _, id := range c.Relevant {
			rel[id] = true
		}
		for _, id := range ids {
			if rel[id] {
				res.Hits++
			}
		}
		missing := 0
		for _, id := range c.SupportIDs {
			if !have[id] {
				res.MissingSupport = append(res.MissingSupport, id)
				missing++
			}
		}
		if len(c.SupportIDs) > 0 {
			res.Sufficient = missing == 0
			if res.Sufficient {
				sufficient++
			}
		} else {
			// 没有支撑标注 → 无从判充分性：跳过这条用例，不进分母。
			res.Skipped = true
		}

		// 答对率：上下文充分 + 配了回答器与判分器才测。
		if !res.Skipped && res.Sufficient && opts.Answerer != nil && opts.Judge != nil {
			ctxText := contextTexts(got, ids)
			ans, aerr := opts.Answerer.Answer(ctx, c.Question, ctxText)
			if aerr != nil {
				return nil, fmt.Errorf("evaluation: 用例 %s 回答失败: %w", c.ID, aerr)
			}
			res.Answer = ans
			ok, jerr := opts.Judge.Correct(ctx, c.Question, ans, c.Answer)
			if jerr != nil {
				return nil, fmt.Errorf("evaluation: 用例 %s 判分失败: %w", c.ID, jerr)
			}
			res.Correct = ok
			answeredCnt++
			if ok {
				answered++
			}
			measured = true
		}

		perCase = append(perCase, res)
	}

	m := QAMetrics{
		Cases:            len(ds.Cases),
		Sufficient:       ratio(sufficient, len(ds.Cases)),
		AnsweredMeasured: measured,
		AvgTokens:        float64(totalTokens) / float64(maxInt(len(ds.Cases), 1)),
	}
	if measured {
		m.Answered = float64(answered) / float64(answeredCnt)
	}

	return &QAResult{
		Dataset:    ds.Name,
		Metrics:    m,
		Efficiency: efficiencyOf(perCase),
		PerCase:    perCase,
	}, nil
}

// contextTexts 拼出给回答器看的上下文（正文优先；没有正文就用标识）。
func contextTexts(got []Retrieved, ids []string) []string {
	out := make([]string, 0, len(ids))
	if len(got) > 0 {
		for _, r := range got {
			if r.Text != "" {
				out = append(out, r.Text)
				continue
			}
			out = append(out, r.ID)
		}
		return out
	}
	out = append(out, ids...)
	return out
}

// efficiencyOf 算 token 效率。
func efficiencyOf(cases []QACaseResult) Efficiency {
	var tokens, hits int
	measured := false
	for _, c := range cases {
		if c.Tokens > 0 {
			measured = true
		}
		tokens += c.Tokens
		// 「命中」= 召回结果里相关事实的条数（检索口径那套）。
		//
		// ⚠️ 不用「上下文充分」当分子：充分要求**全部**支撑事实都在，
		// 在多事实问题上极罕见，于是「每命中一条的 token」会大出一个量级——
		// 那个数测的是「上下文够不够答」，不是效率。两个口径不许混。
		hits += c.Hits
	}
	e := Efficiency{Measured: measured}
	if len(cases) == 0 {
		return e
	}
	e.AvgTokensPerQuery = float64(tokens) / float64(len(cases))
	if tokens > 0 {
		e.HitsPerKToken = float64(hits) / (float64(tokens) / 1000)
		if hits > 0 {
			e.TokensPerHit = float64(tokens) / float64(hits)
		}
	}
	return e
}

// ratio a/b（b 为 0 时记 0）。
func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// maxInt 两数取大。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
