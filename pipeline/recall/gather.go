package recall

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var (
	errNilStore  = errors.New("recall: 存储未装配")
	errNilBander = errors.New("recall: 必须装配 Bander：召回内容是不可信内容，不裹数据区不得进 prompt")
	// errNoRawSource 配置要 raw 源，但存储没有捕获面。
	//
	// 这不是「配错了，忍一忍」——它意味着宿主被迫给 ListRaw 一个
	// 「永远返回空集」的桩（契约洗白），而内核会据此在 raw 源上
	// 做出「有原始事件」的假设。故让它在**构造期**炸。
	errNoRawSource = errors.New("recall: 配置要 raw 源，但存储没有捕获面（未实现 recall.RawReader）：" +
		"要么实现 RawReader，要么设 Config.NoCaptureFace = true，要么从 Sources 里去掉 SourceRaw")
)

// hasSource 判断来源是否在配置里。
func hasSource(list []Source, want Source) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// withoutSource 从来源列表里去掉某个来源。
func withoutSource(list []Source, drop Source) []Source {
	out := make([]Source, 0, len(list))
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// gather 按路由决定的面取候选。
//
// 路由规则（缺省口径）：
//   - 恒有 atom（召回主力）与 raw（兜底）；
//   - 宿主解析出实体时才加 page_headline——它是固定成本，收益要话题锁定才兑现。
//
// 关键：**ActiveOnly 恒置位**。漏掉它意味着已退役与已作废的事实照进 prompt，
// 而那种失效在数据上完全看不出来（内容看着是对的，只是过时了）。
func (p *Pipeline) gather(ctx context.Context, req Request, limit int, plan types.QueryPlan) ([]*candidate, error) {
	var out []*candidate

	if hasSource(p.cfg.Sources, SourceAtom) {
		atoms, err := p.collectAtoms(ctx, req, limit)
		if err != nil {
			return nil, err
		}
		for _, a := range atoms {
			c, ok, err := p.snippet(ctx, SourceAtom, a.ID, a.EntityID, "记忆", a.Assertion,
				a.Importance, a.OccurredAt)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // 空白断言：没有可注入的内容，跳过而不是崩掉
			}
			c.bucket = bucketAtom
			c.dedupKey = a.EntityID + "\x00" + a.Signature()
			out = append(out, c)
		}
	}

	if hasSource(p.cfg.Sources, SourceRaw) && p.raw != nil {
		raws, err := p.raw.ListRaw(ctx, req.Namespace, req.Scope, time.Time{},
			store.Page{Limit: p.cfg.MaxRaw})
		if err != nil {
			return nil, err
		}
		for _, e := range raws {
			// 归属复查（纵深防御）：存储层已经按归属过滤了原始事件，
			// 这里再挡一道——原始事件是**对话原文**，漏一条就是
			// 把别人说过的话塞进当前 prompt。
			if !e.Scope.VisibleTo(req.Scope) {
				continue
			}
			c, ok, err := p.snippet(ctx, SourceRaw, e.ID, "", "历史", e.Content,
				types.ImportanceLow, e.Timestamp)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // 空白正文：没有可注入的内容
			}
			c.bucket = bucketQuote
			c.dedupKey = "raw\x00" + e.ID
			c.rawSession = e.SessionID
			c.rawRuntimeError = e.Provenance.RuntimeError
			out = append(out, c)
		}
	}

	// 画像源用**宿主给的 + 查询理解解析出的**实体。
	//
	// 刻意不把解析出的实体塞进 req.Entities：那个字段同时承载
	// 「本轮提到的实体」（补画像）与「会话锚点实体」（划事实范围）
	// 两种语义相反的用法，后者要宿主用 RestrictAtomsToEntities 显式声明。
	// 让一句代词去划事实范围，等于让「那个」清空整轮事实。
	pageEnts := mergeEntities(req.Entities, plan.Entities)
	if hasSource(p.cfg.Sources, SourcePageHeadline) && len(pageEnts) > 0 {
		for _, id := range pageEnts {
			page, err := p.pageOf(ctx, req, id)
			if err != nil {
				return nil, err
			}
			if page == nil || page.Headline == "" {
				continue
			}
			c, ok, err := p.snippet(ctx, SourcePageHeadline, id, id, "画像", page.Headline,
				types.ImportanceMed, page.UpdatedAt)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // 空白画像：实体还没有实质画像
			}
			c.bucket = bucketHeadline
			c.dedupKey = "headline\x00" + id
			out = append(out, c)
		}
	}

	return out, nil
}

// pageOf 取某个实体的画像（按读者的归属视角）。
//
// # 取本页，找不到退到全局页
//
// 读者有自己的归属时，先取他那一页；那一页还没生成过（晋升先于首次生成是常态），
// 就退到全局页。
//
// 退到全局页是**安全**的方向：全局页只汇总全局事实，
// 所以退过去是「少一点内容」，绝不是「别人的内容」。
// 反过来（把别家那一页退给读者）就是内容泄漏。
//
// # 读者不隔离时（归属为空）只能拿到全局页
//
// 归属为空在本仓的过滤面里是「不隔离」（见 store.AtomFilter.Scope），
// 而页面按视角分页：全局页只汇总全局事实。
// 于是「不隔离的读者」拿到的是**全局事实的摘要**，不是全部事实的摘要。
//
// 这是刻意的取舍：让全局页收进所有事实，它就会成为一张所有人都能看的、
// 揉进了别家事实的摘要——那是泄漏。少一段摘要只是少一点上下文，
// 泄漏是内容层面的事故。两者不可比，故取保守的一侧。
func (p *Pipeline) pageOf(ctx context.Context, req Request, entityID string) (*types.EntityPage, error) {
	page, err := p.store.GetEntityPage(ctx, req.Namespace, entityID, req.Scope)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil {
		// 归属复查（纵深防御）：存储层是按 (实体, 视角) 精确取键的，
		// 这里再确认一次可见性——画像是整段人读的文字，
		// 混进别人的一段话比混进一条事实更难被读者分辨。
		if !page.Scope.VisibleTo(req.Scope) {
			return nil, nil
		}
		return page, nil
	}
	if req.Scope == "" {
		return nil, nil // 本视角的页面不存在，且没有更上一级可退
	}
	global, err := p.store.GetEntityPage(ctx, req.Namespace, entityID, "")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil // 实体还没有页面是常态（晋升先于首次生成），不是错
		}
		return nil, err
	}
	return global, nil
}

// collectAtoms 取事实卡候选：**查询命中 ∪ 高价值兜底**。
//
// # 为什么是两路而不是一路
//
// 早先这里只取一路（按重要度+新鲜度的前 N 条），查询词只参与**重排**。
// 后果：候选池与「用户问了什么」无关——再相关的事实，只要排在池子之外，
// 就永远进不了 prompt，而重排再准也无济于事。库越大，召回越不准。
//
// 现在两路各管一件事：
//
//   - 路 A 按**查询词**检索（「用户明确问的那件事」）；
//   - 路 B 按**重要度/新鲜度**取（「最近与最重要的那些事」）。
//
// 路 B 保留的理由：召回不总是有查询词（Request.Query 可以为空），
// 而且「最近发生的事」本身就是要召回的——用户问「那个盘的事」时，
// 上一轮刚发生的相关事实即便字面不匹配也值得进 prompt。
//
// 两路合并后统一重排（rank），重排的排序键与路序无关。
//
// # 退役事实不进池
//
// 路 A 的检索**故意**能搜到已退役的事实（「当初为什么这么判」要能追问，
// 见 store 的软退役约定），所以这里必须显式滤掉——
// 否则一条被推翻的事实会照进 prompt，而它在数据上完全看不出异常。
func (p *Pipeline) collectAtoms(ctx context.Context, req Request, limit int) ([]*types.AtomCard, error) {
	// 显式限定时：事实范围 = Request.Entities。
	scope := p.entityScope(req)
	if scope != nil && len(scope) == 0 {
		// 显式声明了范围但范围是空的 → 这一轮**不该带任何事实**。
		// 退化成全库会把「限定」变成「装饰」——
		// 而宿主正是靠这个开关把全库事实挡在 prompt 之外的。
		return nil, nil
	}
	var entityIDs []string
	if scope != nil {
		entityIDs = req.Entities
	}

	var hits []*types.AtomCard
	if strings.TrimSpace(req.Query) != "" {
		var err error
		hits, err = p.store.SearchAtoms(ctx, req.Namespace, req.Query, store.AtomFilter{
			Scope:        req.Scope,
			EntityIDs:    entityIDs,
			ActiveOnly:   true,
			OccurredFrom: req.OccurredFrom,
			OccurredTo:   req.OccurredTo,
			At:           req.At,
			EventSince:   p.eventSince(req),
			Limit:        limit,
		})
		if err != nil {
			return nil, err
		}
	}
	base, err := p.store.ListAtoms(ctx, req.Namespace, store.AtomFilter{
		ActiveOnly:   true,
		Scope:        req.Scope,
		EntityIDs:    entityIDs,
		OccurredFrom: req.OccurredFrom,
		OccurredTo:   req.OccurredTo,
		At:           req.At,
		EventSince:   p.eventSince(req),
		Limit:        limit,
	})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(hits)+len(base))
	out := make([]*types.AtomCard, 0, len(hits)+len(base))
	add := func(a *types.AtomCard) {
		if a == nil || seen[a.ID] {
			return
		}
		if !a.Active() {
			return // 退役/作废的事实不参与召回
		}
		// 归属复查（纵深防御）：存储层已经按归属过滤了，
		// 这里再挡一道——万一某个后端把过滤条件漏了，
		// 后果是**别家的环境事实进了 prompt**，而那在结果里完全看不出来。
		//
		// 实体范围同理：限定在这一侧再兜一道。
		if !a.Scope.VisibleTo(req.Scope) {
			return
		}
		if scope != nil && !scope[a.EntityID] {
			return
		}
		seen[a.ID] = true
		out = append(out, a)
	}
	for _, a := range hits {
		add(a)
	}
	for _, a := range base {
		add(a)
	}
	return out, nil
}

// collectLoose 宽松词面路：命中**任一**词的事实。
//
// # 为什么与严格路**并列**而不是替代它
//
// 严格路是隐式 AND：「磁盘 内存 清理」里只有同时含三个词的事实才算命中，
// 而一条只含「磁盘」的事实对这条查询明明是相关的。
// 基线数字把这件事量出来了（docs/EVAL_BASELINE.md）：Recall@3 之后不再上升。
//
// 直接把 AND 改成 OR 也不对：噪声一次性灌进池子，
// 而池内排序靠打分器（词面重叠）——于是「命中任一词」与「词面重叠高」高度相关，
// 噪声反而排前面。
//
// 故另加一路：两路的名次交给融合器（RRF）合成——
// 完全匹配的那条在**两路都出现**（融合分最高），部分匹配的那些从这一路进来。
//
// # 归属与时序过滤在这一层同样生效
//
// 与严格路同一套条件（进 SQL，与 TopK 同一次求值）——
// 放宽的是**词的匹配方式**，不是**可见性**。
func (p *Pipeline) collectLoose(ctx context.Context, req Request, limit int) ([]*types.AtomCard, error) {
	if strings.TrimSpace(req.Query) == "" {
		return nil, nil
	}
	scope := p.entityScope(req)
	if scope != nil && len(scope) == 0 {
		return nil, nil
	}
	var entityIDs []string
	if scope != nil {
		entityIDs = req.Entities
	}
	atoms, err := p.store.SearchAtomsLoose(ctx, req.Namespace, req.Query, store.AtomFilter{
		Scope:        req.Scope,
		EntityIDs:    entityIDs,
		ActiveOnly:   true,
		OccurredFrom: req.OccurredFrom,
		OccurredTo:   req.OccurredTo,
		At:           req.At,
		EventSince:   p.eventSince(req),
		Limit:        limit,
	})
	if err != nil {
		return nil, err
	}
	// 可见性复查（纵深防御）：这一路的过滤在存储层已生效，
	// 但它比严格路**宽松**（命中任一词即入），故复查一遍更该做。
	out := make([]*types.AtomCard, 0, len(atoms))
	for _, a := range atoms {
		if !a.Active() || !a.Scope.VisibleTo(req.Scope) {
			continue
		}
		if scope != nil && !scope[a.EntityID] {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// eventSince 事件类事实的默认召回窗口下界。
//
// 零值 = 不限。两条让位规则：
//   - 窗口没配（零值）→ 不限；
//   - **用户显式问了历史**（请求带了 OccurredFrom/OccurredTo）→ 不限。
//     显式查询优先于默认策略：用户问「上周那次」时，
//     默认窗口不该把那条挡在外面。
func (p *Pipeline) eventSince(req Request) time.Time {
	if p.cfg.EventWindow <= 0 {
		return time.Time{}
	}
	if !req.OccurredFrom.IsZero() || !req.OccurredTo.IsZero() {
		return time.Time{}
	}
	return p.now().Add(-p.cfg.EventWindow)
}

// entityScope 事实卡的范围。
//
// 返回 nil = **不限定**（默认）；返回空但非 nil 的 map = 显式限定为空 → 空集。
// 两者不可混：把「不限定」与「限定为空」都表达成空 slice，
// 就没有办法区分「不限」与「这一轮不该带事实」。
func (p *Pipeline) entityScope(req Request) map[string]bool {
	if !p.cfg.RestrictAtomsToEntities {
		return nil
	}
	scope := make(map[string]bool, len(req.Entities))
	for _, e := range req.Entities {
		scope[e] = true
	}
	return scope
}

// snippet 造一个**已包裹**的候选片段。
//
// 包裹在这一层发生、而不是渲染时：片段一旦被造出来就是「可以进 prompt 的形态」，
// 让它以明文形态存在等于给下游留了一条忘记包裹的路。
//
// # 第二个返回值：「有没有这一条」
//
// 空正文**不是错误，是「没有」**——所以它有自己一个返回值，而不是塞进 error。
//
// 早先这个位置返回 (nil, nil)，而三个调用点直接写 c.bucket = ...，
// 于是一条空白正文的事件就让召回链解引用 nil 崩掉。空正文能落库是常态
// （捕获层当时不校验正文非空），于是「一条空事件 → 之后每次召回都崩」。
//
// 现在两道防线：类型层拒绝空白正文（types.ErrRawEmptyContent），
// 这里再挡一次，挡的是**绕过捕获层直接写库**的那条路径。
func (p *Pipeline) snippet(ctx context.Context, src Source, refID, entityID, role, body string,
	imp types.Importance, at time.Time) (*candidate, bool, error) {
	if strings.TrimSpace(body) == "" {
		return nil, false, nil
	}
	wrapped := p.bander.WrapMemory(body)
	n, err := p.est.Estimate(ctx, wrapped)
	if err != nil {
		return nil, false, err
	}
	return &candidate{
		Snippet: Snippet{
			Source: src, RefID: refID, EntityID: entityID, Role: role,
			Body: wrapped, Importance: imp, OccurredAt: at, Tokens: n,
		},
		plain: body,
	}, true, nil
}

// pushEntities 把本次召回命中的实体推进活跃实体栈。
//
// 两条入栈路径（缺省口径）：
//  1. 召回命中的实体（事实卡带出来的）；
//  2. 宿主已解析出的实体。
//
// 推进发生在**重排之后、取用之前**——用的是排过序的前几条，
// 而不是全量命中。否则一个实体只要沾边就会进栈，栈里全是弱相关对象。
func (p *Pipeline) pushEntities(ctx context.Context, req Request, cands []*candidate) []string {
	ids := make([]string, 0, len(cands)+len(req.Entities))
	for _, c := range cands[:minInt(len(cands), 5)] {
		if c.bucket == bucketAtom {
			ids = append(ids, c.EntityID)
		}
	}
	ids = append(ids, req.Entities...)
	return p.pushEntityIDs(ctx, req, ids)
}

// pushEntityIDs 把一批实体推进活跃实体栈。
//
// 单独成方法，是为了让**缓存命中**那一条路也能推进栈：
// 栈是**会话状态**，不是召回结果的一部分——
// 命中缓存省掉的是「取数 + 排序」，没省掉「这一轮谈到了谁」。
// 漏掉这一步的症状是：开了缓存之后代词消解变差（「那个」指不到东西），
// 而原因藏在缓存那一行里，从召回结果上看不出来。
func (p *Pipeline) pushEntityIDs(ctx context.Context, req Request, ids []string) []string {
	if req.ThreadID == "" {
		return nil
	}
	now := p.now()
	seen := map[string]bool{}
	var pushed []string

	push := func(id, name string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		pushed = append(pushed, id)
		_ = p.store.UpsertActiveEntity(ctx, req.Namespace, &types.ActiveEntity{
			ThreadID: req.ThreadID, EntityID: id, CanonicalName: name, LastSeen: now,
		})
	}

	// 名字留空：召回片段本身不携带实体名（那是另一张表的事），硬塞 RefID
	// 会把「事实卡 ID」当成实体名存进栈里。存储层的空名按「沿用旧值」处理。
	for _, id := range ids {
		push(id, "")
	}

	// 栈有上限，超了按 LRU 淘汰（钉住项不参与，见 store 契约）。
	if _, err := p.store.EvictActiveEntities(ctx, req.Namespace, req.ThreadID, 8); err != nil {
		return pushed
	}
	return pushed
}

// allocate 分桶配额：按优先级逐条接收，装不下就丢弃并计数。
//
// 配额按桶各算：事实桶满了**不占用**画像桶的额度。这样画像这一档是**保留**的，
// 而不是「事实够多就永远挤掉画像」——后者会让页面重生成这个功能形同虚设。
//
// # 配额与丢弃计数走 ports.Budget 那件
//
// 这不是「为了抽象而抽象」：桶配额与丢弃原因计数这套契约与「怎么取数、
// 怎么排序」无关，宿主自造召回时也要它（见 ports/budget.go）。
// 本仓复用同一件，于是两边的行为口径不会分叉。
//
// # 已知边界：桶容量小于该桶最小单元时，该桶恒为空
//
// 分桶是**上限**，不是保底。默认口径下画像一句话通常远小于 10% × 1500 = 150
// token，故不成问题；但若宿主把 TotalBudget 压到很小、或画像写得极长，
// 画像会**静默地一次都不出现**。症状是「页面重生成白做了」而不是报错。
// 宿主调小预算时要用 Result.Dropped 盯一眼，别只看它非零。
func (p *Pipeline) allocate(cands []*candidate) ([]Snippet, *ports.Budget) {
	b := ports.NewBudget(p.cfg.TotalBudget, map[ports.BucketID]float64{
		bucketAtom:     p.cfg.BucketAtom,
		bucketQuote:    p.cfg.BucketQuote,
		bucketHeadline: p.cfg.BucketHeadline,
	})
	var kept []Snippet
	for _, c := range cands {
		if !b.Admit(c.bucket, c.Tokens) {
			continue // 不 break：后面可能有小片段塞得进去
		}
		kept = append(kept, c.Snippet)
	}
	return kept, b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// mergeEntities 合并两处实体来源（保序去重）。
//
// 宿主给的排前面：宿主的判断可能来自它自己的上下文，比句面解析准。
func mergeEntities(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, e := range list {
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}
