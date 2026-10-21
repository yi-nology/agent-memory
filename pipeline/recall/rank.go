package recall

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/oekit/pkg/container"

	"github.com/yi-nology/agent-memory/ports"
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

// rank 重排：按打分器给的分数排序。
//
// # 打分器可注入，默认**零漂移**
//
// Config.Ranker 为 nil 时用 ports.LexicalRanker{}（词面 + 重要度 + 新鲜度，
// 权重 0.6/0.25/0.15）——与打分器这个端口出现之前**逐条等价**。
// 宿主要换口径（走语义轨）时注入自己的打分器即可，
// 不必绕开整条召回链自造一套——两套排序口径并存，后续收敛都要付迁移成本。
//
// # 为什么默认仍不用向量
//
// 上游把向量检索放在可选面，且自承真实集成未验证；本仓的判断相同——
// 先把「该捞哪几层」「怎么排」这件事做对，语义相似度是第二阶段的替换件，
// 不是第一阶段的前提。相关度用词面重叠，是够用且零成本的基线。
// （缝留出来了：宿主要接语义轨，注入打分器就行。）
//
// # 打分失败一律上抛
//
// 不许静默回退到默认打分器：静默回退的失效形态是「排序悄悄变回老样子」，
// 而调用方看不出发生了什么。
func (p *Pipeline) rank(ctx context.Context, cands []*candidate, query string, now time.Time) error {
	views := make([]ports.RankCandidate, len(cands))
	for i, c := range cands {
		views[i] = ports.RankCandidate{
			RefID:          c.RefID,
			Source:         string(c.Source),
			EntityID:       c.EntityID,
			Role:           c.Role,
			Body:           c.plain, // 未包裹的正文：标签词每段都有，算进去会拉平得分
			Importance:     string(c.Importance),
			ImportanceRank: c.Importance.Rank(),
			OccurredAt:     c.OccurredAt,
			Tokens:         c.Tokens,
			// 融合分交给打分器：排序口径是它的事，
			// 内核只负责把「这一条在多路融合里排第几」如实告诉它。
			FusionScore: c.fusion,
		}
	}
	scores, err := p.cfg.Ranker.Score(ctx, query, views, now)
	if err != nil {
		return err
	}
	if len(scores) != len(cands) {
		// 分数与候选错位不会崩，只会静默换序——而换序在结果里完全看不出来
		//（内容都还在，只是次序变了）。故这里响亮失败。
		return fmt.Errorf("打分器返回的分数条数与候选数不符（%d vs %d）", len(scores), len(cands))
	}
	for i, c := range cands {
		c.score = scores[i]
	}
	sortByScore(cands)
	return nil
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

// sortByScore 按分数排序：**分数大的在前**。
//
// # 方向必须与 ports.Ranker 的契约一致
//
// 契约写得很死：「分数越大越靠前」（见 ports/rank.go）。
// 而本函数初版写的是升序（`score < score`）——方向**反了**。
//
// # 为什么这个方向的错误特别贵
//
// 它不是「排序不好看」：排序之后紧接着是 diversify 与 allocate，
// 而 allocate 是**按顺序逐条接收、装不下就丢弃**的。
// 于是升序排出来的第一名恰恰是**最不相关**的那条：
// 预算一紧，先被丢掉的是最该保留的事实，而留在 prompt 里的是最不相干的。
//
// 更糟的是它**看不出来**：内容一条不少，只是次序变了、丢的换了人。
// 事实卡的顺序、条数看着都正常，只有「召回得准不准」变了——
// 而那正是最容易被归因为「模型不行」的一类症状。
//
// 升序在别处是安全的（分数越低越靠前也行，只要契约写的是那样），
// 这里不行：本仓的契约是「分数越大越靠前」，实现必须跟着契约走。
func sortByScore(cands []*candidate) {
	// 用标准库的稳定排序。手写插入排序在这里曾是「不引第三方包到核心」的
	// 产物——而 sort 是**标准库**，那条理由本来就不成立。
	//
	// 稳定性必须保留：同分的片段之间的相对顺序是上游定的（存储层按重要度
	// 排过），换成不稳定排序会在同分时悄悄换序，而那在结果里看不出来。
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
}

var _ = types.ImportanceMed
