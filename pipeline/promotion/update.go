package promotion

import (
	"context"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：事实更新判定（ADD / UPDATE / DELETE / NOOP 中的 UPDATE 一支）。
//
// # 为什么要有「取代」这一支
//
// 事实卡是**原子断言**，而现实里同一个东西会变：
//
//	旧：机器甲使用版本 1.32
//	新：机器甲已升级到版本 1.36
//
// 两条断言**没有否定关系**（「使用」与「已升级到」不矛盾），
// 故现有的否定词矛盾检查看不见它们——而它们说的是同一件事。
// 结果是两条都留在库里，都参与召回，模型看到两个互相打架的版本。
//
// # 为什么判据是「差异落在值上」
//
// 取代的语义是「同一个东西换了值」，故判据是**形态**的：
// 两边词元的差集里有含数字的词元（版本号、容量、数量、编号……）。
//
// 刻意**不用词表**：词表是领域知识，而本仓不认识任何领域。
// 形态判据只要求「这一段文字里的数字变了」——
// 它不知道「版本」是什么，只知道数字动了。
//
// # 为什么它排在矛盾检查**之前**
//
// 两者都用「同一主体 + 高度相似」做前提。
// 矛盾（互相否定）必须**送人工**（本仓纪律：模型和规则都分不清
// 「已修复」与「未修复」哪个是新的）；
// 而取代（同一个东西换了值）可以自动做。
// 顺序颠倒的话，「已修复/未修复」会被当成「值变了」而自动取代——
// 那正是被明令禁止的自动裁决。
//
// # 自动取代的门槛比送人工更高
//
// 取代会让旧事实**失效**，比送人工更重，故相似度阈值更高
// （UpdateSimilarity 0.7 vs ConflictSimilarity 0.6）：
// 「说得像」不够，还得「像到只差一个值」。

// updateHint 取代判定的中间结果。
type updateHint struct {
	// old 可能被取代的既有事实。
	old *types.AtomCard
	// confident 规则判据命中（差异落在值上）——不必问模型。
	//
	// 两档的区别即「规则能定」与「规则定不了」：
	// confident 的直接取代；不 confident 的要问模型（或者不取代）。
	confident bool
}

// checkUpdate 检「可能与候选说同一件事」的既有事实。
//
// 三档判据（相似度从高到低）：
//
//	≥ UpdateSimilarity 且差异落在值上 → 高置信度取代（confident）
//	≥ UpdateSimilarity 但差异判不清     → 要问模型
//	≥ UpdateHintSimilarity              → 可能同主体，也要问模型
//	<  UpdateHintSimilarity              → 不是同一件事，不看
//
// 归属不同的卡**不可比**：那是两个环境里的事实
// （「A 组的是 1.32」与「B 组的是 1.36」不构成取代关系）。
func (p *Promoter) checkUpdate(ctx context.Context, ns store.Namespace, entityID string,
	c *types.Candidate) (*updateHint, error) {
	atoms, err := p.store.ListAtoms(ctx, ns, store.AtomFilter{
		EntityIDs:  []string{entityID},
		ActiveOnly: true,
		Scope:      c.Scope,
		Limit:      50,
	})
	if err != nil {
		return nil, err
	}
	var best *updateHint
	bestSim := 0.0
	for _, a := range atoms {
		if a.Scope != c.Scope {
			continue
		}
		sim := similarity(a.Assertion, c.Assertion)
		if sim < p.cfg.UpdateHintSimilarity {
			continue
		}
		if negationDiffers(a.Assertion, c.Assertion) {
			// 矛盾 → 交给 checkConflict 送人工，不自动裁决。
			continue
		}
		if sim >= p.cfg.UpdateSimilarity && differsInValue(a.Assertion, c.Assertion) {
			return &updateHint{old: a, confident: true}, nil
		}
		// 判不清的那一档：留**最像**的那条。
		if sim > bestSim {
			bestSim = sim
			best = &updateHint{old: a}
		}
	}
	return best, nil
}

// decideUpdate 处置「可能同主体」的情形。
//
// # 处置表
//
//	高置信度（规则判出差异落在值上）→ 取代
//	模型说 update                   → 取代
//	模型说 noop                     → 并入
//	模型说 delete / conflict        → 送人工
//	模型说 add / 没装模型           → 按普通晋升处理（新增一条事实）
//
// 两条「重」的动作（作废已存事实、裁决矛盾）永远落在人工手里：
// 模型能触发的只有「取代」与「并入」。
func (p *Promoter) decideUpdate(ctx context.Context, ns store.Namespace, c *types.Candidate,
	entityID string, ev *types.RawEvent, hint *updateHint) (*Verdict, error) {
	if hint.confident {
		return p.updateToAtom(ctx, ns, c, entityID, ev, hint.old)
	}
	if p.cfg.LLM == nil {
		// 没装模型 = 没有这一支判定：按普通晋升处理。
		//
		// 刻意**不**送人工：那会让「没配模型」的宿主突然多出一堆待人处置的候选，
		// 而降级路径（没模型也能记事实）必须保持原样。
		return nil, nil
	}
	rel, reason, err := p.askModel(ctx, c, hint.old)
	if err != nil {
		return nil, err
	}
	switch rel {
	case relationUpdate:
		return p.updateToAtom(ctx, ns, c, entityID, ev, hint.old)
	case relationNoop:
		return p.merge(ctx, ns, c, hint.old)
	case relationDelete, relationConflict:
		return &Verdict{Decision: DecisionReview, Check: "update", EntityID: entityID,
			Reason: "与既有事实的关系待人工判定：" + reason}, nil
	default:
		// add：两条说的是不同的事 → 走普通晋升路径。
		return nil, nil
	}
}

// updateToAtom 新事实取代旧事实。
//
// # 四步写，顺序与 promoteToAtom 同源
//
// 新事实入库 → 旧事实退役（有效期到此刻）→ 实体计数 → 候选落定 → 审计条目。
//
// 「先写新事实」的理由与晋升路径一样：若后面失败，库里会多一条新事实
// （它本身是对的，只是旧事实还没退役），而反过来（先退役旧事实）失败时，
// 那条新事实会**永远丢**——候选已判为「已取代」但库里没有它，
// 读取侧完全看不出来。
//
// # 旧事实是「到期」而不是「删除」
//
// 旧事实的 ValidTo 设成此刻：它「到这一刻为止有效」。
// 于是「现在的状态」与「历史上曾经的状态」能同时被查到——
// 而只软退役（SupersededBy）的话，旧事实只会变成「查得到但不算数」，
// 追问「当初为什么这么判」时反而丢了它**什么时候对过**这个信息。
func (p *Promoter) updateToAtom(ctx context.Context, ns store.Namespace, c *types.Candidate,
	entityID string, ev *types.RawEvent, old *types.AtomCard) (*Verdict, error) {
	now := p.now()
	atom := p.newAtom(c, entityID, ev, old.VersionOrOne()+1, now)
	if err := p.store.SaveAtom(ctx, ns, atom); err != nil {
		return nil, err
	}
	// 旧事实：软退役 + 有效期到新事实生效为止。
	old.ValidTo = now
	old.SupersededBy = atom.ID
	old.DeprecatedAt = time.Time{} // 互斥：被取代就不该同时是「人工作废」
	old.UpdatedAt = now
	if err := p.store.SaveAtom(ctx, ns, old); err != nil {
		return nil, err
	}
	if err := p.store.BumpEntityAtomCount(ctx, ns, entityID, 1); err != nil {
		return nil, err
	}
	if err := p.store.UpdateCandidateStatus(ctx, ns, c.ID,
		types.CandidatePending, types.CandidatePromoted, p.cfg.Actor,
		"取代既有事实 "+old.ID); err != nil {
		return nil, err
	}
	if err := p.store.AppendJournal(ctx, ns, &types.JournalEntry{
		ID: "journal-update-" + atom.ID, At: now,
		Kind: types.JournalSupersede, Subject: atom.ID, Actor: p.cfg.Actor,
		Summary: "候选 " + c.ID + " 取代既有事实 " + old.ID + "（同一主体，值变了）",
	}); err != nil {
		return nil, err
	}
	return &Verdict{Decision: DecisionUpdate, EntityID: entityID, Check: "update",
		Reason: "与既有事实说的是同一件事，值变了", Atom: atom, Superseded: old}, nil
}

// differsInValue 两条断言的差异里有没有「值」变了。
//
// 判据：两边词元的差集里有含数字的词元。
//
// 差集里**同时**有非值词并不否决取代：
// 「X 使用 1.32」与「X 已升级到 1.36」的差集是 {使用, 已升级到, 1.32, 1.36}，
// 它仍然是一次取代（主体与属性都没变，变的是值）。
// 差得太远的情形由相似度阈值挡住。
func differsInValue(a, b string) bool {
	ta, tb := tokens(a), tokens(b)
	setA := map[string]int{}
	for _, t := range ta {
		setA[t]++
	}
	setB := map[string]int{}
	for _, t := range tb {
		setB[t]++
	}
	for t, n := range setB {
		if setA[t] < n && hasValueToken(t) {
			return true
		}
	}
	for t, n := range setA {
		if setB[t] < n && hasValueToken(t) {
			return true
		}
	}
	return false
}

// hasValueToken 这个词元是不是「值」（含数字）。
func hasValueToken(t string) bool {
	for _, r := range t {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// newAtom 造一张事实卡。
//
// 晋升（新增）与取代（更新）共用它：两条路径的差别只在
// 「版本号」与「生效时刻」上，别的地方一样——
// 而「不一样的地方」散在两处，正是迟早会漂的地方。
func (p *Promoter) newAtom(c *types.Candidate, entityID string, ev *types.RawEvent,
	version int, validFrom time.Time) *types.AtomCard {
	now := p.now()
	return &types.AtomCard{
		// ID 由候选 ID 派生：同一条候选被重复晋升时命中同一张卡
		// （upsert 语义），不会凭空多出一条同签名的重复事实。
		ID:            "atom-" + c.ID,
		EntityID:      entityID,
		CandidateID:   c.ID,
		RawEventIDs:   c.RawEventIDs,
		Assertion:     c.Assertion,
		VerbatimQuote: c.VerbatimQuote,
		QuoteEventID:  c.QuoteEventID,
		SearchTerms:   searchTerms(c.Assertion),
		OccurredAt:    occurredAt(c, ev),
		Confidence:    c.Confidence,
		Importance:    c.Importance,
		CreatedAt:     now,
		UpdatedAt:     now,
		// 归属继承自候选（候选的归属又继承自证据事件）：
		// 归属在**捕获时**定下，中途不让模型参与。
		Scope: c.Scope,
		// 来源凭证取自**证据事件**：审计链要能从事实回到那条事件，
		// 而事件才带 origin/host/tool 这些字段。
		Provenance: ev.Provenance,
		// 时序：生效时刻与版本号。
		//
		// 生效时刻零值时取 OccurredAt（老数据不变语义），
		// 版本号零值当 1。
		ValidFrom: validFrom,
		Version:   version,
	}
}
