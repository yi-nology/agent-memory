// Package promotion 晋升面：把待判定候选落成事实、或并入、或丢弃、或送人工。
//
// # 五道检查，失败方向各不相同
//
//	1 价值    记不记       → 丢弃
//	2 证据    依据在哪     → 丢弃（没有出处的事实既不能审计也不能推翻）
//	3 实体    挂在谁名下   → 送人工（归一错了会把两条不相干的事实并成一条）
//	4 重复    是不是说过   → 并入已有事实
//	5 矛盾    与既有事实冲突 → 送人工（**不自动裁决**：自动判「哪个对」就是在猜）
//
// # 通过之后要**落库**
//
// 判定只是判决，事实得真的写进库里，否则整条记忆主链是断的。
// 落库是四步写：事实入库 + 实体计数 + 候选落定 + 审计条目，
// 四步在**同一个事务**里（见 Promote）。
//
// # 送人工是默认姿态，不是兜底
//
// 「判定不了就不判」——把可疑内容自动写进长期记忆，比漏记一条的代价高得多：
// 漏记的代价是「用户下次还得再说一遍」，误记的代价是「记忆库开始编造，
// 而且用户无从分辨哪条是真的」。
package promotion

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oekit/pkg/container"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Decision 判定结果。
type Decision string

const (
	// DecisionPromote 晋升为新事实。
	DecisionPromote Decision = "promote"
	// DecisionMerge 并入既有事实。
	DecisionMerge Decision = "merge"
	// DecisionDrop 丢弃。
	DecisionDrop Decision = "drop"
	// DecisionReview 送人工处置。
	DecisionReview Decision = "review"
)

// Store 晋升面用到的存储。
//
// 与 M2 召回那条纪律同源：只声明用得到的，不吃全量 store.Store。
// 好处在**测试**上：晋升逻辑是本仓最需要回归的部分，牵着的接口越少，
// 假实现越贴近真实语义，用例写得出来。
//
// 刻意**不含** SaveEntity / SaveAlias：实体归一不建新实体
// （见 resolveEntity 的注释「新建实体不是选项」），建实体是宿主投影的活。
type Store interface {
	GetRaw(ctx context.Context, ns store.Namespace, id string) (*types.RawEvent, error)
	FindAtomBySignature(ctx context.Context, ns store.Namespace, signature string) (*types.AtomCard, error)
	ListAtoms(ctx context.Context, ns store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error)
	FindEntityByName(ctx context.Context, ns store.Namespace, name string) (*types.Entity, error)
	// ListEntities 列出既有实体，供升级通道消歧。
	ListEntities(ctx context.Context, ns store.Namespace, entityType types.EntityType, limit int) ([]*types.Entity, error)
	SaveAtom(ctx context.Context, ns store.Namespace, a *types.AtomCard) error
	BumpEntityAtomCount(ctx context.Context, ns store.Namespace, entityID string, delta int) error
	UpdateCandidateStatus(ctx context.Context, ns store.Namespace, id string,
		expected, next types.CandidateStatus, actor types.DecidedBy, reason string) error
	AppendJournal(ctx context.Context, ns store.Namespace, e *types.JournalEntry) error

	// Transaction 事务边界。
	//
	// 晋升要写四样东西（候选落定 + 事实入库 + 实体计数 + 审计条目），
	// 中途失败若不回滚，记忆库会出现「候选已判但事实没进」的空洞——
	// 而那类不一致在读取侧完全看不出来（只是那条事实凭空消失）。
	//
	// 回调收到的那份存储必须是**事务内**的那一份：
	// 用外层那份写会落到事务外（原子性没了且不报任何错），
	// 连接池只有一条连接时还会直接挂起。
	//
	// 窄接口的事务：回调的存储类型是本接口（晋升面只看得见自己这一小片），
	// 而不是一个全量 store.Store——全量接口在测试里要实现 70 多个方法。
	// 装配面（application）负责把全量存储适配成这一窄接口。
	Transaction(ctx context.Context, fn func(ctx context.Context, s Store) error) error
}

// Escalation 判不了的升级通道。
//
// 存在是因为**有些判定不该由内核猜**：实体归一（"它"指哪个对象）需要上下文，
// 而内核手里只有一条断言。宿主有全量上下文，故由它答。
type Escalation interface {
	// ResolveEntity 返回该断言归属的实体 ID。
	//
	// **空串 = 判不了**（走送人工），不是「没有实体」。
	// error = **通道故障**（宿主超时、内部错误）——调用方会把它上抛，
	// 不当作「判不了」。把故障记成「上下文不足」会让送审理由指向错误的原因，
	// 而送审队列的积压恰恰会掩盖真正的故障。
	//
	// existing 是该库里已登记的实体，供消歧（「它」指哪个对象）。
	// 调用方保证它有内容：传空列表等于让升级通道盲判。
	ResolveEntity(ctx context.Context, ns store.Namespace, c *types.Candidate,
		existing []types.Entity) (entityID string, err error)
}

// Config 晋升配置。
type Config struct {
	// MinAssertionRunes 断言最短长度（0=6）。
	//
	// 短于它的多半是「是的」「好」这类应答，记下来只会在召回时占预算。
	MinAssertionRunes int
	// MaxAssertionRunes 断言最长长度（0=500）。
	//
	// 超长的多半是整段被误当成断言——它更像情节能取材，不是一条事实。
	MaxAssertionRunes int
	// ConflictSimilarity 相似度阈值（0=0.6）：超过且互相否定即送人工。
	ConflictSimilarity float64
	// NewEntitySuffix 新建实体的 ID 前缀。
	NewEntitySuffix string
	// Actor 判定者署名（0=rule）。
	Actor types.DecidedBy
}

func (c Config) normalize() Config {
	if c.MinAssertionRunes <= 0 {
		c.MinAssertionRunes = 6
	}
	if c.MaxAssertionRunes <= 0 {
		c.MaxAssertionRunes = 500
	}
	if c.ConflictSimilarity <= 0 {
		c.ConflictSimilarity = 0.6
	}
	if c.NewEntitySuffix == "" {
		c.NewEntitySuffix = "ent-"
	}
	if c.Actor == "" {
		c.Actor = types.DecidedRule
	}
	return c
}

// Promoter 晋升器。
type Promoter struct {
	store      Store
	escalation Escalation
	cfg        Config
	now        func() time.Time
}

// New 造晋升器。
func New(s Store, esc Escalation, cfg Config) (*Promoter, error) {
	if s == nil {
		return nil, errors.New("promotion: 存储未装配")
	}
	return &Promoter{store: s, escalation: esc, cfg: cfg.normalize(),
		now: func() time.Time { return time.Now().UTC() }}, nil
}

// Verdict 一条候选的判定结论。
type Verdict struct {
	Decision Decision
	// Reason 判定理由（留痕面：驳回也要有说法）。
	Reason string
	// Check 停在第几道检查（drop/review 时有意义；promote 时为 "pass"）。
	Check string
	// Atom 晋升/并入的事实卡（Decision 为 promote/merge 时有值）。
	Atom *types.AtomCard
	// EntityID 归一后的实体。
	EntityID string
}

// Promote 判定一条候选，**并在通过时落库**。
//
// # 前置：候选必须已在库里
//
// 「候选落定」那一步要改候选的状态，候选不在库里时那一步会失败，
// 整笔晋升随即回滚（库里不留下半成品）。提炼器**不落库**
// （「落库由调用方在看过结果后决定」），故调用方要先存候选再晋升。
//
// # 落库在事务里，且用回调收到的那份存储
//
// 晋升要写四样东西：候选落定、事实入库、实体计数、审计条目。四步必须原子——
// 中途失败若不回滚，记忆库会出现「候选已判但事实没进」的空洞，
// 而那类不一致在读取侧完全看不出来（只是那条事实凭空消失）。
// 这正是 store.Store 把 Transaction 当作一等公民的原因。
//
// 事务回调收到的那份存储是**事务内**的那一份。若用外层那份（p.store），
// 写会落到事务外——原子性没了**且不报任何错**；
// 连接池只有一条连接时还会直接挂起（事务持着唯一连接，外层语句永远等不到）。
// 故这里复制一份 Promoter，把存储换成回调给的那份。
func (p *Promoter) Promote(ctx context.Context, ns store.Namespace, c *types.Candidate) (*Verdict, error) {
	var v *Verdict
	err := p.store.Transaction(ctx, func(ctx context.Context, s Store) error {
		tx := *p
		tx.store = s
		var err error
		v, err = tx.promote(ctx, ns, c)
		return err
	})
	if err != nil {
		return nil, err
	}
	return v, nil
}

// promote 判定一条候选（五道检查），并在通过时落库。
//
// 存储一律取自 p.store——Promote 已把它换成事务内的那一份。
func (p *Promoter) promote(ctx context.Context, ns store.Namespace, c *types.Candidate) (*Verdict, error) {
	// 1 价值
	if v := p.checkValue(c); v != nil {
		return v, nil
	}
	// 2 证据
	ev, v, err := p.checkEvidence(ctx, ns, c)
	if err != nil || v != nil {
		return v, err
	}

	entityID, v, err := p.resolveEntity(ctx, ns, c)
	if err != nil {
		// 存储故障上抛：继续走会产出一条无归属的事实。
		return nil, err
	}
	if v != nil {
		return v, nil
	}

	// 4 重复：同签名即并入，不新增
	//
	// 签名含归属（见 types.AtomSignature）：同一条断言在两个归属下是
	// **两张不同的卡**（各自环境里的事实）。签名不含归属的话，
	// 这里的并入会把一组的卡片并进另一组的卡片——那是内容泄漏，不是去重。
	sig := types.AtomSignature(entityID, c.Scope, c.Assertion)
	existing, err := p.store.FindAtomBySignature(ctx, ns, sig)
	switch {
	case err == nil && existing != nil:
		return p.merge(ctx, ns, c, existing)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	// 5 矛盾：与该实体下既有事实高度相似却互相否定 → 送人工，不自动裁决
	if v, err := p.checkConflict(ctx, ns, entityID, c); err != nil || v != nil {
		return v, err
	}

	// 全部通过 → 落库
	return p.promoteToAtom(ctx, ns, c, entityID, ev)
}

// promoteToAtom 落库一条新事实（晋升路径的最后一步）。
//
// # 四步写，顺序有意义
//
//	事实入库 → 实体计数 → 候选落定 → 审计条目
//
// **先写事实**是刻意的：若第二步失败，库里会多一条事实
// （事实本身是对的，只是计数没跟上，重跑一次能补上）；
// 而反过来（先落定候选）失败时，那条事实就**永远丢了**——
// 候选已判为「已晋升」但库里没有这条事实，读取侧完全看不出来。
//
// 审计条目放最后：它失败不该回滚已经正确的落库。
func (p *Promoter) promoteToAtom(ctx context.Context, ns store.Namespace, c *types.Candidate,
	entityID string, ev *types.RawEvent) (*Verdict, error) {

	now := p.now()
	atom := &types.AtomCard{
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
	}
	if err := p.store.SaveAtom(ctx, ns, atom); err != nil {
		return nil, err
	}
	if err := p.store.BumpEntityAtomCount(ctx, ns, entityID, 1); err != nil {
		return nil, err
	}
	if err := p.store.UpdateCandidateStatus(ctx, ns, c.ID,
		types.CandidatePending, types.CandidatePromoted, p.cfg.Actor,
		"晋升为事实 "+atom.ID); err != nil {
		return nil, err
	}
	if err := p.store.AppendJournal(ctx, ns, &types.JournalEntry{
		ID: "journal-promote-" + atom.ID, At: now,
		Kind: types.JournalPromote, Subject: atom.ID, Actor: p.cfg.Actor,
		Summary: "候选 " + c.ID + " 晋升为事实 " + atom.ID,
	}); err != nil {
		return nil, err
	}
	return &Verdict{Decision: DecisionPromote, EntityID: entityID,
		Check: "pass", Reason: "通过全部检查", Atom: atom}, nil
}

// searchTerms 取检索词面。
//
// 用 store.LexicalTokens（契约层那套切词）：汉字逐字成词元、英文按空白切词，
// 与存储层建索引时用的是同一套规则——两处切法不一致会让检索漏命中。
func searchTerms(assertion string) []string {
	tokens := store.LexicalTokens(assertion)
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// occurredAt 事实**发生**的时刻。
//
// 取证据事件的时刻：三天前说「磁盘只剩 2G」是**昨天发生**的事实，
// 两者混同会让新鲜度排序把「刚写进来的旧事实」排到最前。
// 证据事件没有时刻时退回候选的创建时刻。
func occurredAt(c *types.Candidate, ev *types.RawEvent) time.Time {
	if ev != nil && !ev.Timestamp.IsZero() {
		return ev.Timestamp
	}
	if !c.CreatedAt.IsZero() {
		return c.CreatedAt
	}
	return time.Now().UTC()
}

// checkValue 长期价值。
//
// 判据只有长度与「是不是纯应答」两类确定性的，不含任何语义判断——语义价值
// 由抽取层负责（它看过全文），内核这一层要的是**可复现**。
func (p *Promoter) checkValue(c *types.Candidate) *Verdict {
	n := len([]rune(strings.TrimSpace(c.Assertion)))
	switch {
	case n == 0:
		return &Verdict{Decision: DecisionDrop, Check: "value", Reason: "断言为空"}
	case n < p.cfg.MinAssertionRunes:
		return &Verdict{Decision: DecisionDrop, Check: "value",
			Reason: fmt.Sprintf("断言仅 %d 字，短于下限 %d，多半是应答而非事实", n, p.cfg.MinAssertionRunes)}
	case n > p.cfg.MaxAssertionRunes:
		return &Verdict{Decision: DecisionDrop, Check: "value",
			Reason: fmt.Sprintf("断言 %d 字，超过上限 %d，多半是整段被当成了断言", n, p.cfg.MaxAssertionRunes)}
	}
	return nil
}

// checkEvidence 证据。
//
// 出处必须能解析到一条**真实的**捕获事件。查不到就丢弃——不是降级成
// 「无出处但先记着」：没有出处的事实既不能审计也不能推翻，
// 而那正是长期记忆最核心的两项能力。
//
// 返回那条事件：晋升要用它的时刻作事实的发生时刻、用它的来源凭证作事实的来源。
func (p *Promoter) checkEvidence(ctx context.Context, ns store.Namespace, c *types.Candidate) (*types.RawEvent, *Verdict, error) {
	if c.QuoteEventID == "" {
		return nil, &Verdict{Decision: DecisionDrop, Check: "evidence", Reason: "无出处引用"}, nil
	}
	ev, err := p.store.GetRaw(ctx, ns, c.QuoteEventID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &Verdict{Decision: DecisionDrop, Check: "evidence",
			Reason: "出处事件已不存在（可能被保留期清理了）"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(c.RawEventIDs) == 0 {
		c.RawEventIDs = []string{ev.ID}
	}
	return ev, nil, nil
}

// resolveEntity 实体归一。
//
// 三条路径：主体名直接命中（含别名）→ 升级通道 → 送人工。
// **新建实体不是选项**：归一错了会把两条不相干的事实挂到同一张卡上，
// 而那种错误在召回时表现为「总是答非所问」，极难归因到归一步。
//
// # 「查不动」与「查不到」是两件事
//
// 存储故障（连接重置、超时）**不是**「判不了」：前者是基础设施出问题了，
// 后者是一个需要人来看的判定结论。混在一起的后果比「队列被噪音淹没」更重：
// 存储故障会让归一「成功」但实体为空，于是产出一条**谁都不属于的事实**——
// 空 EntityID 一路流到签名、矛盾检测、召回配额，最后作为一条无归属事实
// 进 prompt，而数据上完全看不出异常。
//
// 故这里给 error 返回位：**存储故障上抛**，只有「查不到」才走升级通道。
func (p *Promoter) resolveEntity(ctx context.Context, ns store.Namespace, c *types.Candidate) (string, *Verdict, error) {
	if c.SubjectName == "" {
		return "", &Verdict{Decision: DecisionReview, Check: "entity",
			Reason: "无主体名，归一无从下手"}, nil
	}
	e, err := p.store.FindEntityByName(ctx, ns, c.SubjectName)
	switch {
	case err == nil && e != nil:
		return e.ID, nil, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		// 存储故障不是「判不了」，往上抛——不能把一次数据库抖动
		// 记成「需人工判断」，那会让待办队列被噪音淹没；
		// 更不能让它继续走成一条无归属的事实。
		return "", nil, fmt.Errorf("按名查实体失败（存储故障）: %w", err)
	}
	// 查不到（ErrNotFound）或后端返回 (nil, nil)：交给升级通道。
	if p.escalation == nil {
		return "", &Verdict{Decision: DecisionReview, Check: "entity",
			Reason: "主体名未命中既有实体，且未装配升级通道"}, nil
	}
	// 升级通道要判「它指谁」就得看见既有实体——传空列表等于让它盲判。
	existingPtrs, err := p.store.ListEntities(ctx, ns, "", 50)
	if err != nil {
		return "", nil, fmt.Errorf("列既有实体失败（存储故障）: %w", err)
	}
	existing := make([]types.Entity, 0, len(existingPtrs))
	for _, e := range existingPtrs {
		if e != nil {
			existing = append(existing, *e)
		}
	}
	id, err := p.escalation.ResolveEntity(ctx, ns, c, existing)
	if err != nil {
		// 接口约定：**空串才是「判不了」**，error 是通道故障。
		return "", nil, fmt.Errorf("实体归一升级通道失败: %w", err)
	}
	if id == "" {
		return "", &Verdict{Decision: DecisionReview, Check: "entity",
			Reason: "升级通道判不出归属（宿主上下文不足）"}, nil
	}
	return id, nil, nil
}

// merge 并入既有事实。
func (p *Promoter) merge(ctx context.Context, ns store.Namespace,
	c *types.Candidate, existing *types.AtomCard) (*Verdict, error) {

	// 并入时**补证据不补断言**：两条断言说的是同一件事（签名相同），
	// 它们的逐字引用各自有效，合并证据让「这条事实被哪些场合支持过」可查。
	merged := append(append([]string{}, existing.RawEventIDs...), c.RawEventIDs...)
	existing.RawEventIDs = dedupStrings(merged)
	existing.UpdatedAt = p.now()
	if err := p.store.SaveAtom(ctx, ns, existing); err != nil {
		return nil, err
	}
	if err := p.store.UpdateCandidateStatus(ctx, ns, c.ID,
		types.CandidatePending, types.CandidateMerged, p.cfg.Actor,
		"并入既有事实 "+existing.ID); err != nil {
		return nil, err
	}
	// 审计条目：并入也是一次「记忆改写自己」，同样要留痕。
	if err := p.store.AppendJournal(ctx, ns, &types.JournalEntry{
		ID: "journal-merge-" + c.ID, At: p.now(),
		Kind: types.JournalMerge, Subject: existing.ID, Actor: p.cfg.Actor,
		Summary: "候选 " + c.ID + " 并入既有事实 " + existing.ID,
	}); err != nil {
		return nil, err
	}
	return &Verdict{Decision: DecisionMerge, Check: "dup", EntityID: existing.EntityID,
		Reason: "与既有事实同签名，证据已合并", Atom: existing}, nil
}

// checkConflict 矛盾检测。
//
// 判据：**同一实体下**、词面高度相似、但否定词相反。
// 三条同时成立才算冲突——只看相似度会把「A 和 B 都是磁盘相关」判成矛盾，
// 只看否定词会把两件不相干的事判成矛盾。
//
// # 只与**同一归属**的事实比
//
// 归属不同的两张卡说的是**各自环境里的事实**：一组的内核版本与另一组的
// 内核版本本来就该不一样，拿它们当矛盾是把「环境差异」当成「有人搞错了」。
// 故归属必须**逐字相同**才算可比——
// 连「全局的」也不参与比较（全局事实是通则，与某组的事实不一致未必是错）。
//
// # 判出冲突**不自动裁决**
//
// 模型和规则都分不清「已修复」与「未修复」哪个是新的。
// 这种情况送人工，由看过现场的人决定。
func (p *Promoter) checkConflict(ctx context.Context, ns store.Namespace,
	entityID string, c *types.Candidate) (*Verdict, error) {

	atoms, err := p.store.ListAtoms(ctx, ns, store.AtomFilter{
		EntityIDs:  []string{entityID},
		ActiveOnly: true,
		Limit:      200,
	})
	if err != nil {
		return nil, err
	}
	for _, a := range atoms {
		if a.Scope != c.Scope {
			continue // 归属不同 = 不同环境，不是矛盾
		}
		sim := similarity(c.Assertion, a.Assertion)
		if sim < p.cfg.ConflictSimilarity {
			continue
		}
		if !negationDiffers(c.Assertion, a.Assertion) {
			continue
		}
		return &Verdict{Decision: DecisionReview, Check: "conflict", EntityID: entityID,
			Reason: fmt.Sprintf("与既有事实 %s 相似度 %.2f 且否定方向相反，不自动裁决", a.ID, sim)}, nil
	}
	return nil, nil
}

// negationTokens 否定词表。
//
// 与 extractor 的防稀释提示**共用同一份词表**是硬约定：抽取器说「引用丢了否定」
// 时，晋升器必须用同一个词族去检测「旧事实说 X，新候选说 非X」——两处词表
// 一旦漂移，一条被悄悄反过来的事实就既抽不出来也查不出来。
var negationTokens = []string{
	"不", "未", "没", "无", "非", "别", "勿", "尚未", "还没",
	"not", "non", "un", "never", "no",
}

func negationDiffers(a, b string) bool {
	return hasNegation(a) != hasNegation(b)
}

func hasNegation(s string) bool {
	lower := strings.ToLower(s)
	for _, t := range negationTokens {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// dedupStrings 保序去重。
//
// 与 lifecycle 的同名件是两份实现而非一处共享：两者分处平级包
// （pipeline/promotion 与 lifecycle 之间不互相依赖），而合并它们需要先给
// 这 10 行找一个「家」——而每个候选的家（types 是零依赖的地基、store 是
// 持久化契约）都比这 10 行本身更不搭。故各留一份，语义保持一致。
func dedupStrings(in []string) []string {
	seen := container.NewSet[string]()
	out := in[:0:0]
	for _, s := range in {
		if seen.Contains(s) {
			continue
		}
		seen.Add(s)
		out = append(out, s)
	}
	return out
}

// similarity 词面 Jaccard 相似度。
//
// 用词集合而非最长公共子串：子串会被「新增限定词」骗过去——
// 「已修复」与「未修复」子串重合度很高，而它们恰恰是最需要被识别为冲突的一对。
func similarity(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	set := map[string]int{}
	for _, t := range ta {
		set[t]++
	}
	inter := 0
	for _, t := range tb {
		if set[t] > 0 {
			set[t]--
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// tokens 粗分词：中文按 rune、拉丁按空格。
//
// 中文逐字切对否定检测是够的（「已修复」/「未修复」共享「修复」两字），
// 而引入分词器会让本包依赖一个第三方库——核心层不许有第三方依赖。
func tokens(s string) []string {
	var out []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			out = append(out, strings.ToLower(word.String()))
			word.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r >= 0x4E00 && r <= 0x9FFF:
			flush()
			out = append(out, string(r))
		case r == ' ' || r == '\t' || r == '\n' || r == ',' || r == '，' || r == '.' || r == '。':
			flush()
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return out
}
