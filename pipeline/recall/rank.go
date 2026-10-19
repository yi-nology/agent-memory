package recall

import (
	"sort"
	"strings"
	"time"

	"github.com/oekit/pkg/container"

	"github.com/yi-nology/agent-memory/types"
)

// suppress 防回声：本轮刚说过的话不再进 prompt。
//
// 三道闸，判据全是结构化的：
//
//  1. **本会话的原始事件**：用户刚说的话回灌给用户，模型会当成"重复强调"，
//     从而过度加权这一轮。它还在上下文里，不必再塞一遍。
//  2. **运行期错误来源**：合成出来的报错文本不是"发生过的事"，它进记忆库
//     是灾难（见 types.Provenance.RuntimeError）。这里再挡一道是纵深防御——
//     捕获层已经挡过，但召回面不应该假设上游一定干净。
//  3. **同一实体同一断言**：同一件事换个说法再记一遍时，两张卡都说得通，
//     但一起进 prompt 等于把同一句话说两遍，挤掉了别的实体的事实。
//
// 刻意**不做**的：不按相似度做语义去重。那需要向量或模型，成本与不确定性都不小，
// 而这里要的只是「别把明显重复的东西塞两遍」。
func (p *Pipeline) suppress(in []*candidate, req Request) ([]*candidate, int) {
	seen := container.NewSet[string]()
	dup := 0
	out := in[:0:0]
	for _, c := range in {
		if c.dedupKey != "" {
			if seen.Contains(c.dedupKey) {
				dup++ // 闸 3
				continue
			}
			seen.Add(c.dedupKey)
		}
		if c.Source == SourceRaw && req.SessionID != "" && c.rawSession == req.SessionID {
			continue // 闸 1
		}
		if c.rawRuntimeError {
			continue // 闸 2
		}
		out = append(out, c)
	}
	return out, dup
}

// rank 重排：相关度 + 重要度 + 新鲜度。
//
// # 为什么不用向量
//
// 上游把向量检索放在可选面，且自承真实集成未验证；本仓的判断相同——
// 先把「该捞哪几层」「怎么排」这件事做对，语义相似度是第二阶段的替换件，
// 不是第一阶段的前提。相关度用词面重叠，是够用且零成本的基线。
//
// # 三项权重是经验值，不是标定值
//
// 调它之前先看 Result.Dropped：预算被打满时，提高某一项的权重只是在
// 三者之间挪位置，不改变「装不下」这个事实。
func rank(cands []*candidate, query string, now time.Time) {
	terms := tokenize(query)
	for _, c := range cands {
		c.score = 0.6*lexical(c.plain, terms) +
			0.25*float64(c.Importance.Rank())/3 +
			0.15*recency(c.OccurredAt, now)
	}
	sortByScore(cands)
}

// lexical 词面重叠度：命中词数 / 查询词数。
//
// 匹配的是去包裹的原文（candidate.plain）而不是带标签的 Body：标签词「记忆」
// 「画像」每次都出现在每一段里，算进去会让所有片段得分虚高且完全同分。
func lexical(body string, terms []string) float64 {
	if len(terms) == 0 || body == "" {
		return 0
	}
	hit := 0
	for _, t := range terms {
		if strings.Contains(body, t) {
			hit++
		}
	}
	return float64(hit) / float64(len(terms))
}

// recency 新鲜度：近一天的取 1，线性衰减到 30 天取 0。
//
// 刻意用线性而非指数：记忆的时效判断是粗粒度的（「这是刚发生的还是老账」），
// 指数衰减会在头几小时里把排序几乎完全交给新鲜度，而那恰恰是噪声最大的时段。
func recency(at, now time.Time) float64 {
	if at.IsZero() {
		return 0
	}
	age := now.Sub(at)
	switch {
	case age <= 0:
		return 1
	case age >= 30*24*time.Hour:
		return 0
	default:
		return 1 - age.Seconds()/(30*24*3600)
	}
}

// tokenize 粗分词：按空白与标点切，中文整段保留（召回查询本来就短，
// 不值得引分词器；整段包含匹配对短查询反而更稳）。
func tokenize(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ',' || r == '，' ||
			r == '?' || r == '？' || r == '!' || r == '！' || r == '。' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// diversify 限制单个实体的贡献条数。
//
// # 超出的片段直接出局，不「沉底」
//
// 初版是把它们挪到候选尾部保留着，理由是「沉底比删除可查」——**那个理由不成立**：
// 预算宽裕时沉底的片段会原样回来，于是上限在**最常见的情形下根本不生效**，
// 而上限的意义恰恰是「不管预算多宽裕，一个实体都不许主导」。
// 出局的片段会体现在 Result.Dropped 里，可观测性一点没丢。
func diversify(cands []*candidate, perEntityMax int) ([]*candidate, int) {
	if perEntityMax <= 0 {
		return cands, 0
	}
	seen := map[string]int{}
	kept := cands[:0:0]
	dropped := 0
	for _, c := range cands {
		// 画像桶不参与多样性裁剪：它是那个实体的摘要，裁掉它等于裁掉整个实体。
		// 本条是踩出来的——初版把它算进配额，于是「该实体事实多」时画像恒被挤掉，
		// 而画像恰好是事实太多时最该保留的那一句。
		if c.EntityID == "" || c.bucket == bucketHeadline {
			kept = append(kept, c)
			continue
		}
		if seen[c.EntityID] >= perEntityMax {
			dropped++
			continue
		}
		seen[c.EntityID]++
		kept = append(kept, c)
	}
	return kept, dropped
}

func sortByScore(cands []*candidate) {
	// 用标准库的稳定排序。手写插入排序在这里曾是「不引第三方包到核心」的
	// 产物——而 sort 是**标准库**，那条理由本来就不成立。
	//
	// 稳定性必须保留：同分的片段之间的相对顺序是上游定的（存储层按重要度
	// 排过），换成不稳定排序会在同分时悄悄换序，而那在结果里看不出来。
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score < cands[j].score })
}

var _ = types.ImportanceMed
