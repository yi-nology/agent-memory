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
type Store interface {
	GetRaw(ctx context.Context, ns store.Namespace, id string) (*types.RawEvent, error)
	FindAtomBySignature(ctx context.Context, ns store.Namespace, signature string) (*types.AtomCard, error)
	ListAtoms(ctx context.Context, ns store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error)
	FindEntityByName(ctx context.Context, ns store.Namespace, name string) (*types.Entity, error)
	SaveEntity(ctx context.Context, ns store.Namespace, e *types.Entity) error
	SaveAlias(ctx context.Context, ns store.Namespace, a *types.Alias) error
	SaveAtom(ctx context.Context, ns store.Namespace, a *types.AtomCard) error
	BumpEntityAtomCount(ctx context.Context, ns store.Namespace, entityID string, delta int) error
	UpdateCandidateStatus(ctx context.Context, ns store.Namespace, id string,
		expected, next types.CandidateStatus, actor types.DecidedBy, reason string) error
	AppendJournal(ctx context.Context, ns store.Namespace, e *types.JournalEntry) error
}

// Escalation 判不了的升级通道。
//
// 存在是因为**有些判定不该由内核猜**：实体归一（"它"指哪个对象）需要上下文，
// 而内核手里只有一条断言。宿主有全量上下文，故由它答。
type Escalation interface {
	// ResolveEntity 返回该断言归属的实体 ID。
	//
	// 返回空串 = 判不了（走送人工），不是「没有实体」。
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
	// Check 停在第几道检查。
	Check string
	// Atom 晋升/并入的事实卡（Decision 为 promote/merge 时有值）。
	Atom *types.AtomCard
	// EntityID 归一后的实体。
	EntityID string
}

// Promote 判定一条候选。
func (p *Promoter) Promote(ctx context.Context, ns store.Namespace, c *types.Candidate) (*Verdict, error) {
	// 1 价值
	if v := p.checkValue(c); v != nil {
		return v, nil
	}
	// 2 证据
	if v, err := p.checkEvidence(ctx, ns, c); err != nil || v != nil {
		return v, err
	}

	entityID, v := p.resolveEntity(ctx, ns, c)
	if v != nil {
		return v, nil
	}

	// 4 重复：同签名即并入，不新增
	sig := atomSignature(entityID, c.Assertion)
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

	return &Verdict{Decision: DecisionPromote, EntityID: entityID,
		Check: "dup", Reason: "通过全部检查"}, nil
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
func (p *Promoter) checkEvidence(ctx context.Context, ns store.Namespace, c *types.Candidate) (*Verdict, error) {
	if c.QuoteEventID == "" {
		return &Verdict{Decision: DecisionDrop, Check: "evidence", Reason: "无出处引用"}, nil
	}
	ev, err := p.store.GetRaw(ctx, ns, c.QuoteEventID)
	if errors.Is(err, store.ErrNotFound) {
		return &Verdict{Decision: DecisionDrop, Check: "evidence",
			Reason: "出处事件已不存在（可能被保留期清理了）"}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(c.RawEventIDs) == 0 {
		c.RawEventIDs = []string{ev.ID}
	}
	return nil, nil
}

// resolveEntity 实体归一。
//
// 三条路径：主体名直接命中（含别名）→ 升级通道 → 送人工。
// **新建实体不是选项**：归一错了会把两条不相干的事实挂到同一张卡上，
// 而那种错误在召回时表现为「总是答非所问」，极难归因到归一步。
func (p *Promoter) resolveEntity(ctx context.Context, ns store.Namespace, c *types.Candidate) (string, *Verdict) {
	if c.SubjectName == "" {
		return "", &Verdict{Decision: DecisionReview, Check: "entity", Reason: "无主体名，归一无从下手"}
	}
	e, err := p.store.FindEntityByName(ctx, ns, c.SubjectName)
	if err == nil && e != nil {
		return e.ID, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// 存储故障不是「判不了」，往上抛——不能把一次数据库抖动
		// 记成「需人工判断」，那会让待办队列被噪音淹没。
		return "", nil
	}
	if p.escalation == nil {
		return "", &Verdict{Decision: DecisionReview, Check: "entity",
			Reason: "主体名未命中既有实体，且未装配升级通道"}
	}
	id, err := p.escalation.ResolveEntity(ctx, ns, c, nil)
	if err != nil || id == "" {
		return "", &Verdict{Decision: DecisionReview, Check: "entity",
			Reason: "升级通道判不出归属（宿主上下文不足）"}
	}
	return id, nil
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
	return &Verdict{Decision: DecisionMerge, Check: "dup", EntityID: existing.EntityID,
		Reason: "与既有事实同签名，证据已合并", Atom: existing}, nil
}

// checkConflict 矛盾检测。
//
// 判据：**同一实体下**、词面高度相似、但否定词相反。
// 三条同时成立才算冲突——只看相似度会把「A 和 B 都是磁盘相关」判成矛盾，
// 只看否定词会把两件不相干的事判成矛盾。
//
// 判出冲突**不自动裁决**：模型和规则都分不清「已修复」与「未修复」哪个是
// 新的。这种情况送人工，由看过现场的人决定。
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

// atomSignature 事实签名（与 types.AtomCard.Signature 同规则）。
//
// 在此重算而不调 a.Signature()：候选还没有 FactCard，签名是晋升时才产生的。
func atomSignature(entityID, assertion string) string {
	return entityID + "\x00" + types.NormalizeAssertion(assertion)
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

func dedupStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
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
