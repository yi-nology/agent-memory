package ports

import "sort"

// 本文件：多路召回结果的融合（RRF）。
//
// # 为什么是 RRF 而不是「加权分数」
//
// 词面路的分数（bm25 类）没有上界，向量路的相似度在 [-1,1]，
// 直接相加要先归一——而归一依赖数据分布，
// 于是「换个库就得重调权重」，且调错了看不出来（只是排序变差）。
//
// RRF 只看**名次**：score(d) = Σ 1/(k + rank_i(d))。
// 于是：
//
//   - 跨路可比（名次无量纲）；
//   - 跨库稳定（不依赖分数分布）；
//   - 一条在两路都靠前的事实自然排到前面（两路都贡献分数）。
//
// # k 的作用：压住「头名通吃」
//
// k 越大，头名与后面的差距越小（名次的贡献被摊平）。
// 本仓取 60（社区常用的经验值）：头名仍然值钱，
// 但不至于「一路的第一名压过两路都在前列的那条」。
//
// # 这不是「排名的排名」
//
// RRF 的输入是**每一路自己的有序名单**，输出是融合后的有序名单。
// 它不懂内容、不重新打分，故它不会把「词面完全匹配但语义不相关」的东西捞回来——
// 那要靠各路自己捞对。

// DefaultRRFConstant RRF 的缺省 k。
const DefaultRRFConstant = 60.0

// FusedHit 一条融合后的命中。
type FusedHit struct {
	// ID 命中的标识。
	ID string
	// Score 融合分数（越大越靠前）。
	Score float64
	// Sources 各路的名次（第几路、第几名）。
	//
	// 留它是为了**可解释**：融合结果要能回答
	// 「这条为什么排在这里」——是「两路都进了前五」还是「某一路头名」。
	// 没有它，融合就成了一口黑箱，调不动。
	Sources []FusedSource
}

// FusedSource 一路里的名次。
type FusedSource struct {
	// Route 路标识（"lexical" / "semantic" / 宿主自定义）。
	Route string
	// Rank 名次（1 起）。
	Rank int
}

// RRF 用倒数排名融合多路名单。
//
// lists 是各路的**有序**标识名单（每路一个，元素不重复）。
// 返回按融合分数降序排好的名单；同分时保持**首次出现**的相对顺序
// （稳定排序：同分不该被随机换序，而换序在结果里看不出来）。
//
// 空名单的路直接跳过（没装语义面时那一路是空的，不是错）。
func RRF(lists map[string][]string, k float64) []FusedHit {
	if k <= 0 {
		k = DefaultRRFConstant
	}
	score := map[string]float64{}
	order := []string{}
	srcs := map[string][]FusedSource{}
	// 路之间按名字排序，保证同分时的次序与输入顺序无关（可复现）。
	routes := make([]string, 0, len(lists))
	for r := range lists {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	for _, route := range routes {
		for i, id := range lists[route] {
			if id == "" {
				continue
			}
			if _, seen := score[id]; !seen {
				order = append(order, id)
			}
			score[id] += 1 / (k + float64(i+1))
			srcs[id] = append(srcs[id], FusedSource{Route: route, Rank: i + 1})
		}
	}
	out := make([]FusedHit, 0, len(order))
	for _, id := range order {
		out = append(out, FusedHit{ID: id, Score: score[id], Sources: srcs[id]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}
