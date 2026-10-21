package recall

import (
	"context"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：多路召回（混合检索）与 RRF 融合。
//
// # 两路各干什么
//
//   - **词面路**：FTS/词面重叠。找的是「字面像」的那几条。
//   - **语义路**：宿主注入的面（ports.SemanticReader）。找的是「意思像」的那几条。
//
// 两路都只给**标识与次序**，不给事实卡——事实卡一律回存储层取，
// 故语义路**不**能成为第二个真源（它只是「多给一批候选」）。
//
// # 融合解决什么
//
// 融合（RRF）决定**候选池里有什么**（哪些事实进来了），
// 而**谁排前面**归打分器。两者分开是有意的：
// 混在一起就会出现两套排序口径并存（融合排好的序被重排器覆盖），
// 后续收敛都要付迁移成本。
//
// # 没装语义面时，这里**一行都不生效**
//
// 融合分数会一路传到打分器（ports.RankCandidate.FusionScore），
// 但缺省打分器的融合权重是 0 ⇒ 分数贡献为 0 ⇒ 行为与装之前**逐条一致**。
// 这条「零漂移」由 ports/rank_test.go 钉住。

// collectSemantic 语义路：语义面给的命中 → 事实卡。
//
// # 归属过滤在两层
//
// 存储层按 AtomFilter.Scope 过滤（语义面自己也要过滤，见 ports.SemanticReader），
// 这里再复查一次（纵深防御）：语义面是**宿主写的**，
// 万一它漏了过滤条件，别家的事实会混进候选池，
// 而那在结果里完全看不出来（就是一条说得通的事实）。
//
// # 命中次序
//
// 语义面给的是「按相似度排好的标识」，故取回事实卡后要**按那个次序**重排，
// 否则语义路的次序信息就丢了（融合吃的就是名次）。
func (p *Pipeline) collectSemantic(ctx context.Context, req Request, limit int) ([]*types.AtomCard, error) {
	if p.cfg.Semantic == nil {
		return nil, nil
	}
	hits, err := p.cfg.Semantic.Search(ctx, string(req.Namespace), req.Query, string(req.Scope), limit)
	if err != nil {
		// 语义面失败**上抛**：静默忽略的话，混合检索悄悄退化成纯词面，
		// 而宿主以为自己在用语义轨——症状是「召回质量莫名下降」，
		// 而原因藏在一个被吞掉的错误里。
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(hits))
	seen := map[string]bool{}
	for _, h := range hits {
		if h.ID == "" || seen[h.ID] {
			continue
		}
		seen[h.ID] = true
		ids = append(ids, h.ID)
	}
	atoms, err := p.store.ListAtoms(ctx, req.Namespace, store.AtomFilter{
		IDs:          ids,
		Scope:        req.Scope,
		ActiveOnly:   true,
		OccurredFrom: req.OccurredFrom,
		OccurredTo:   req.OccurredTo,
		At:           req.At,
	})
	if err != nil {
		return nil, err
	}
	// 按语义面的次序重排，并做可见性复查。
	byID := make(map[string]*types.AtomCard, len(atoms))
	for _, a := range atoms {
		if !a.Active() || !a.Scope.VisibleTo(req.Scope) {
			continue
		}
		byID[a.ID] = a
	}
	out := make([]*types.AtomCard, 0, len(ids))
	for _, id := range ids {
		if a, ok := byID[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

// fuse 融合两路，返回按融合分降序排好的候选。
//
// 没有语义路时**原样返回**（连次序都不动）——
// 那是「没装语义面」的主路径，它必须与装之前逐条一致。
//
// 融合用 ports.RRF：它只看名次，故跨路可比、跨库稳定。
func (p *Pipeline) fuse(ctx context.Context, lexical []*candidate, semantic []*types.AtomCard) []*candidate {
	if len(semantic) == 0 {
		return lexical
	}
	lexIDs := make([]string, 0, len(lexical))
	lexIndex := make(map[string]*candidate, len(lexical))
	for _, c := range lexical {
		lexIDs = append(lexIDs, c.RefID)
		lexIndex[c.RefID] = c
	}
	semIDs := make([]string, 0, len(semantic))
	semIndex := make(map[string]*types.AtomCard, len(semantic))
	for _, a := range semantic {
		semIDs = append(semIDs, a.ID)
		semIndex[a.ID] = a
	}
	fused := ports.RRF(map[string][]string{
		"lexical":  lexIDs,
		"semantic": semIDs,
	}, ports.DefaultRRFConstant)

	// 融合出的次序里，词面路的候选沿用（它已带正文与去重键）；
	// 语义路独有的候选要**造**一个（正文取回的事实）。
	out := make([]*candidate, 0, len(fused))
	for _, f := range fused {
		if c, ok := lexIndex[f.ID]; ok {
			c.fusion = f.Score
			out = append(out, c)
			continue
		}
		a, ok := semIndex[f.ID]
		if !ok {
			continue
		}
		c, ok, err := p.snippet(ctx, SourceAtom, a.ID, a.EntityID, "记忆", a.Assertion,
			a.Importance, a.OccurredAt)
		if err != nil {
			// 造片段失败（计量失败）→ 上抛：与别处一致，不静默丢内容。
			return lexical
		}
		if !ok {
			continue
		}
		c.bucket = bucketAtom
		c.dedupKey = a.EntityID + "\x00" + a.Signature()
		c.fusion = f.Score
		out = append(out, c)
	}
	return out
}
