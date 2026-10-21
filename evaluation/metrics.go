package evaluation

import (
	"math"
	"sort"
)

// Metrics 一组用例的评测结果。
//
// # 口径写死在这里，别在调用点各算一套
//
// 评测的公信力全靠**口径一致**：同一组数字，换一套算法再算一遍就得对不上，
// 而对不上时没人知道是哪边错了——于是「指标变好了」这句话就没有意义。
// 故本包把每个指标的定义（分母、截断、跳过规则）写死在这里，
// 想改口径就改这一处，并让新旧数字**不可比**（宁可作废重来，也不要混着看）。
//
// # 聚合方式是宏平均
//
// 各指标先按用例算，再对用例取算术平均：每条用例等权。
// 不按「命中条数汇总 / 相关条数汇总」那种微平均，是因为那样相关条数多的
// 用例会主导总分——一条用例挂二十条相关事实，它就能把整体分数抬起来，
// 而这与「这次改动让每条查询都更准了」不是一回事。
type Metrics struct {
	// Cases 参与评分的用例数。
	//
	// 相关集合为空的用例被跳过（无从判分），**不进这个数**。
	Cases int
	// RecallAtK 各 k 的召回率（命中的相关条数 / 相关条数总数）。
	RecallAtK map[int]float64
	// PrecisionAtK 各 k 的精确率。
	PrecisionAtK map[int]float64
	// NDCGAtK 各 k 的归一化折损累计增益。
	NDCGAtK map[int]float64
	// MRR 平均倒数排名（第一条相关结果排名的倒数，取平均）。
	MRR float64
	// HitRate 至少命中一条相关结果的用例比例。
	HitRate float64
}

// DefaultKs 缺省评测的 k 集合。
//
// 固定成一组而不让调用点每次现选：k 一变，指标就不可比，
// 而「两串不可比的数字被并排看」是最容易骗过自己的比较方式。
var DefaultKs = []int{1, 3, 5, 10}

// Score 给一条用例的召回结果打分。
//
// retrieved 是召回结果（有序）；ks 为空时用 DefaultKs。
// 相关集合为空的用例在这里返回全零（跳过由运行器处理）。
func Score(c Case, retrieved []string, ks []int) Metrics {
	return scoreCase(c, retrieved, ks).Metrics
}

// scoreCase 给一条用例打分，并攒出逐用例明细。
//
// 这里是**全部口径的落点**：相关判定、重复计数、k 截断都在这一处，
// 别在别处再写一遍同样的算式——两处各写一份，改一处就漂，
// 而漂了之后的数字仍然「看起来正常」。
func scoreCase(c Case, retrieved []string, ks []int) CaseResult {
	ks = normalizeKs(ks)
	res := CaseResult{
		ID:        c.ID,
		Query:     c.Query,
		Retrieved: append([]string(nil), retrieved...),
	}

	rel := make(map[string]bool, len(c.Relevant))
	for _, id := range c.Relevant {
		rel[id] = true
	}
	if len(rel) == 0 {
		// 相关集合为空 → 无从判分。这条用例**不进任何分母**：
		// 把它算成 0 分等于说「这条查询一条都没召回对」，
		// 而它压根没有「对」可言；混进分母只会让指标随数据集里
		// 标注缺失的用例数量漂移。
		res.Skipped = true
		res.Metrics = emptyMetrics(ks)
		return res
	}

	// gains[i] 表示第 i+1 条结果是否相关。
	//
	// 同一 ID 重复出现**只按首次出现计**：重复召回是召回器的缺陷，
	// 不该被指标当成额外收益（否则一个把同一条事实返回五遍的召回器
	// 会在精确率与 nDCG 上得高分）。
	seen := map[string]bool{}
	gains := make([]bool, len(retrieved))
	for i, id := range retrieved {
		if !rel[id] || seen[id] {
			continue
		}
		seen[id] = true
		gains[i] = true
		res.Hits = append(res.Hits, id)
		if res.FirstHitRank == 0 {
			res.FirstHitRank = i + 1
		}
	}
	// 漏掉的相关：按相关集合的原序列出，便于对照标注逐条看。
	for _, id := range c.Relevant {
		if !seen[id] {
			res.Missed = append(res.Missed, id)
		}
	}

	m := Metrics{
		Cases:        1,
		RecallAtK:    make(map[int]float64, len(ks)),
		PrecisionAtK: make(map[int]float64, len(ks)),
		NDCGAtK:      make(map[int]float64, len(ks)),
	}
	for _, k := range ks {
		hits := countHits(gains, k)
		// Recall@k：分母是相关条数总数（相关为空者已在上游跳过）。
		m.RecallAtK[k] = float64(hits) / float64(len(rel))
		// Precision@k：分母**固定是 k**，不是 min(k, 返回条数)。
		// 召回器只返回 3 条而 k=10 时精确率会偏低——这是口径的一部分，
		// 不是 bug：口径写死是为了跨实现可比，而「返回不足 k 条」
		// 本身就是召回质量的一部分，不该被悄悄抹平。
		m.PrecisionAtK[k] = float64(hits) / float64(k)
		m.NDCGAtK[k] = ndcgAtK(len(rel), gains, k)
	}
	if res.FirstHitRank > 0 {
		m.MRR = 1 / float64(res.FirstHitRank)
	}
	res.Metrics = m
	return res
}

// countHits 前 k 条里的相关条数（去重后的命中数）。
func countHits(gains []bool, k int) int {
	n := 0
	for i := 0; i < len(gains) && i < k; i++ {
		if gains[i] {
			n++
		}
	}
	return n
}

// ndcgAtK 算一条用例的 nDCG@k。
//
// gains[i] 表示第 i+1 条结果是否相关（已去重）；
// relCount 是相关条数总数（IDCG 的来源）。
//
// 定义：
//
//   - DCG@k = Σ (2^rel_i − 1) / log2(i+1)，i 从 1 起，rel_i∈{0,1}；
//   - IDCG@k = 同一公式作用在「相关条数排在最前」的理想排序上，
//     相关条数 > k 时**按 k 截断**（理想排序里第 k+1 条本来就进不了前 k）；
//   - nDCG@k = DCG@k / IDCG@k；IDCG 为 0 时记 0。
//
// 截断那条是必须的：不截断的话，相关条数越多 IDCG 越大，
// 于是「相关条数多的用例」天然 nDCG 更低——那不是排序的过错，是分母的过错。
func ndcgAtK(relCount int, gains []bool, k int) float64 {
	if k < 1 {
		return 0
	}
	dcg := 0.0
	for i := 0; i < len(gains) && i < k; i++ {
		if !gains[i] {
			continue
		}
		dcg += relGain(1) / math.Log2(float64(i+2))
	}
	m := relCount
	if m > k {
		m = k
	}
	idcg := 0.0
	for j := 0; j < m; j++ {
		idcg += relGain(1) / math.Log2(float64(j+2))
	}
	if idcg == 0 {
		// 相关集合为空时 IDCG 为 0，此时比值无定义。
		// 记 0 而不是 NaN：NaN 会在聚合里传播，
		// 让整份报告的数字全废——而这里的语义本就是「无从判分」。
		return 0
	}
	return dcg / idcg
}

// relGain 相关性增益：2^rel − 1。
func relGain(rel int) float64 {
	return math.Pow(2, float64(rel)) - 1
}

// aggregate 把逐用例明细聚合成一份指标。
//
// 聚合顺序固定为**用例顺序**（求和后除以用例数）：
// 浮点加法不可交换，若让 map 迭代顺序漏进来，
// 同一份数据两次聚合可能差在最后几位——那正是「不可复现」的温床。
func aggregate(results []CaseResult, ks []int) Metrics {
	ks = normalizeKs(ks)
	m := emptyMetrics(ks)

	scored, hit := 0, 0
	mrrSum := 0.0
	for _, r := range results {
		if r.Skipped {
			continue
		}
		scored++
		if r.FirstHitRank > 0 {
			hit++
		}
		mrrSum += r.Metrics.MRR
		for _, k := range ks {
			m.RecallAtK[k] += r.Metrics.RecallAtK[k]
			m.PrecisionAtK[k] += r.Metrics.PrecisionAtK[k]
			m.NDCGAtK[k] += r.Metrics.NDCGAtK[k]
		}
	}
	m.Cases = scored
	if scored == 0 {
		// 一条都没参与评分：各指标记 0（不是 NaN）。
		return m
	}
	for _, k := range ks {
		m.RecallAtK[k] /= float64(scored)
		m.PrecisionAtK[k] /= float64(scored)
		m.NDCGAtK[k] /= float64(scored)
	}
	m.MRR = mrrSum / float64(scored)
	m.HitRate = float64(hit) / float64(scored)
	return m
}

// emptyMetrics 造一份全零指标（各 k 都在）。
func emptyMetrics(ks []int) Metrics {
	m := Metrics{
		RecallAtK:    make(map[int]float64, len(ks)),
		PrecisionAtK: make(map[int]float64, len(ks)),
		NDCGAtK:      make(map[int]float64, len(ks)),
	}
	for _, k := range ks {
		m.RecallAtK[k] = 0
		m.PrecisionAtK[k] = 0
		m.NDCGAtK[k] = 0
	}
	return m
}

// normalizeKs 归一 k 集合：去掉非正数、去重、升序。
//
// 升序不是为了好看，而是为了让报告里的指标永远按同一个顺序出现——
// 同一份报告读两遍，数字得在同一个地方。
func normalizeKs(ks []int) []int {
	if len(ks) == 0 {
		ks = DefaultKs
	}
	out := make([]int, 0, len(ks))
	seen := make(map[int]bool, len(ks))
	for _, k := range ks {
		if k < 1 || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Ints(out)
	if len(out) == 0 {
		// 全是非正数：退回缺省，不让除零发生。
		out = append(out, DefaultKs...)
	}
	return out
}
