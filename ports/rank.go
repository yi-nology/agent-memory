package ports

import (
	"context"
	"strings"
	"time"
)

// 本文件：排序（打分）的外部能力端口。
//
// # 为什么要把它做成端口
//
// 排序策略是**宿主的事**：本仓的基线是「词面重叠 + 重要度 + 新鲜度」的线性加权，
// 权重是经验值（0.6/0.25/0.15）。宿主若要走语义轨（向量相似度），
// 没有注入缝就只能**绕开整条召回链自造一套**——
// 而两套排序口径并存，后续无论往哪边收敛都要付迁移成本。
//
// 端口给出的正是那条缝：**不接也完全能用**（默认打分器），接了就换口径。
//
// # 本仓自己在用
//
// 通用件的验收标准是「本仓自己在用」——本仓召回的默认打分器就是
// LexicalRanker，故这不是「已实现但没接线的抽象」。

// RankCandidate 参与排序的一条候选（**公开给打分器看的形状**）。
//
// 刻意做成结构而不是 map：打分器是宿主写的，字段名拼错要有编译期信号。
//
// # 为什么不带 types 的结构
//
// ports 是零内部依赖的（见 architecture-policy.yaml），
// 故这里不能出现 types.Importance 之类的东西——
// 重要度用**档位名 + 档位秩**两个平铺字段表达：
// 档位名给宿主看得懂，档位秩（0..3）给默认打分器直接用，
// 而「哪个档位对应几」的真源始终在 types.Importance.Rank() 那一处。
type RankCandidate struct {
	// RefID 溯源标识（事实卡 ID / 事件 ID / 实体 ID）。
	RefID string
	// Source 来源面。
	Source string
	// EntityID 归属实体（raw 可能为空）。
	EntityID string
	// Role 渲染角色。
	Role string
	// Body 正文——**未包裹**的形态。
	//
	// 为什么给未包裹的：包裹后带防伪标签（ports.MemoryBand），
	// 那些标签词每段都有，算进去会让所有候选的得分虚高且完全同分。
	Body string
	// Importance 重要度档位名（宿主看得懂）。
	Importance string
	// ImportanceRank 重要度档位的秩（0..3，越大越重要）。
	//
	// 由调用方从领域结构算出（真源在 types.Importance.Rank()），
	// 打分器不必知道本仓的档位枚举。
	ImportanceRank int
	// OccurredAt 发生时刻。
	//
	// 刻意与「写进来的时刻」分开：三天前说「磁盘只剩 2G」是昨天发生的事实，
	// 两者混同会让新鲜度排序把「新写进来的旧事实」排到最前。
	OccurredAt time.Time
	// Tokens 该段占用的估算 token。
	Tokens int
}

// Ranker 打分器：给候选打分，分数越大越靠前。
//
// # 契约
//
// 返回的分数与传入的候选**一一对应且同序**（第 i 个分数是第 i 条候选的）。
// 长度不符一律报错——分数与候选错位不会崩，只会静默换序，
// 而换序在结果里完全看不出来（内容都还在，只是次序变了）。
//
// 分数的**绝对值**没有意义（各家的量纲不同），只有**相对大小**有意义。
// 故实现方不必把分数归一到 [0,1]，本仓也不做归一。
//
// # 错误怎么办
//
// 打分失败一律**上抛**，不许静默回退到默认打分器：
// 静默回退的失效形态是「排序悄悄变回老样子」，而调用方看不出发生了什么。
type Ranker interface {
	Score(ctx context.Context, query string, cands []RankCandidate, now time.Time) ([]float64, error)
}

// RankWeights 三项权重（词面 / 重要度 / 新鲜度）。
//
// 刻意**不做**「零值=缺省」那一套：那样就没法把某一项调成 0，
// 而「不要新鲜度」「只看语义分」恰恰是最常见的两种调法。
// 故用构造函数给全值（见 NewRankWeights）。
type RankWeights struct {
	// Lexical 词面重叠的权重。
	Lexical float64
	// Importance 重要度的权重。
	Importance float64
	// Recency 新鲜度的权重。
	Recency float64
}

// DefaultRankWeights 缺省权重（0.6 / 0.25 / 0.15）。
//
// 这三个数是**经验值，不是标定值**。
// 调它之前先看召回结果的丢弃计数：预算被打满时，提高某一项的权重只是在
// 三者之间挪位置，不改变「装不下」这个事实。
func DefaultRankWeights() RankWeights {
	return RankWeights{Lexical: 0.6, Importance: 0.25, Recency: 0.15}
}

// NewRankWeights 造一组权重。
//
// 三项**不归一**：绝对值随权重变化没关系，只有相对大小有意义。
func NewRankWeights(lexical, importance, recency float64) RankWeights {
	return RankWeights{Lexical: lexical, Importance: importance, Recency: recency}
}

// LexicalRanker 默认打分器：词面重叠 + 重要度 + 新鲜度。
//
// 零值（LexicalRanker{}）用缺省权重 0.6/0.25/0.15——
// 故本仓召回在未注入打分器时的行为与注入前**逐条等价**。
type LexicalRanker struct {
	// Weights 三项权重。
	//
	// 零值 LexicalRanker{} = 缺省权重（见 DefaultRankWeights）；
	// 一旦给了非零的权重，就**按给的算**（其中的 0 是字面的 0，
	// 不补缺省）——否则「不要新鲜度」这种最常见的调法就没法表达。
	Weights RankWeights
}

// weights 取生效的权重。
func (r LexicalRanker) weights() RankWeights {
	if r.Weights == (RankWeights{}) {
		return DefaultRankWeights()
	}
	return r.Weights
}

// Score 给候选打分。
//
// # 为什么切词在这里做一次
//
// 查询词只切一次，**不是每条候选切一次**：候选池有几十条，
// 每条切一遍纯属浪费。切词规则是「按空白与标点切，中文整段保留」
// （召回查询本来就短，不值得引分词器；整段包含匹配对短查询反而更稳）。
func (r LexicalRanker) Score(_ context.Context, query string, cands []RankCandidate,
	now time.Time) ([]float64, error) {
	w := r.weights()
	terms := tokenize(query)
	out := make([]float64, len(cands))
	for i, c := range cands {
		out[i] = w.Lexical*lexical(c.Body, terms) +
			w.Importance*float64(c.ImportanceRank)/3 +
			w.Recency*recency(c.OccurredAt, now)
	}
	return out, nil
}

// lexical 词面重叠度：命中词数 / 查询词数。
//
// 匹配的是**去包裹的原文**（RankCandidate.Body）而不是带标签的 Body：
// 标签词「记忆」「画像」每次都出现在每一段里，算进去会让所有片段得分虚高且完全同分。
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

// tokenize 粗分词：按空白与标点切，中文整段保留。
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
