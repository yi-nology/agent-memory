// Package page 页面重生成面：把某个实体的长文画像按需重新生成。
//
// # 页面是派生物，不是真源
//
// 页面正文可以整段重写、可以随时丢弃重建，事实卡不受影响。
// 判断某条信息是否可信，永远回 AtomCard，不看页面。
//
// # 为什么标脏而不是同步重生成
//
// 每次晋升都重写页面，等于让「写一条记忆」的延迟里多一次模型调用——
// 而晋升发生在会话刚结束时，正是宿主最忙的时候。所以写入侧只标脏，
// 由运行时/CLI/宿主择机触发重生成。
//
// # 失败必须留痕且保持脏
//
// 一次失败的生成若把脏标记清掉，这条页面就永远不再进重生成队列，
// 而用户看到的是「页面存在、内容过时」——它与「页面正常」在看板上长得一模一样。
package page

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oagentkit/textx/textutil"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Store 页面面用到的存储。
//
// 页面按**归属视角**分页（逻辑主键 namespace + entity_id + scope），
// 故下面每个写/读口都带 scope。
type Store interface {
	GetEntityPage(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope) (*types.EntityPage, error)
	ListDirtyEntityPages(ctx context.Context, ns store.Namespace, scope types.Scope, limit int) ([]*types.EntityPage, error)
	MarkEntityPageDirty(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope, when time.Time) error
	ApplyEntityPageRegen(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope,
		p *types.EntityPage, at time.Time) error
	RecordEntityPageRegenFailure(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope,
		reason string, at time.Time) error
	ListAtomsByEntity(ctx context.Context, ns store.Namespace, entityID string) ([]*types.AtomCard, error)
}

// ErrNoLLM 未注入模型能力。
var ErrNoLLM = ports.ErrNoLLM

// PromptFunc 构造重生成提示词。抽出来是因为提示词是产品面。
type PromptFunc func(entityID string, atoms []*types.AtomCard) string

// DefaultPrompt 缺省提示词。
//
// 只给「要什么形状的输出」，不教模型怎么总结——那是模型的活。
// 提示里**不出现任何领域指令**，本包不认识任何领域。
func DefaultPrompt(entityID string, atoms []*types.AtomCard) string {
	var b strings.Builder
	b.WriteString("根据下面的事实，写一段这个对象的长期画像。\n")
	b.WriteString("只输出 JSON：{\"headline\": \"一句话画像\", \"body\": \"多段正文\"}\n")
	b.WriteString("不得添加事实之外的信息；不确定的不要写。\n")
	b.WriteString("事实：\n")
	for _, a := range atoms {
		b.WriteString(fmt.Sprintf("- [%s] %s\n", a.ID, a.Assertion))
	}
	return b.String()
}

// Config 页面面配置。
type Config struct {
	// BatchSize 单轮处理的脏页面数（0=20）。
	BatchSize int
	// StaleAfter 重生成冷却（0=1 分钟）。
	//
	// 冷却的用处：宿主连续晋升十条事实时，只该生成一次页面。
	StaleAfter time.Duration
	// MaxAtoms 单页纳入的事实数上限（0=50）。
	//
	// 封顶是必须的：一个对象的事实可能上百条，全塞进提示会让模型直接
	// 概括失真，而失真的画像比没有画像更糟（它会被当权威用）。
	MaxAtoms int
	// Actor 判定者署名。
	Actor types.DecidedBy
}

func (c Config) normalize() Config {
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = time.Minute
	}
	if c.MaxAtoms <= 0 {
		c.MaxAtoms = 50
	}
	if c.Actor == "" {
		c.Actor = types.DecidedRule
	}
	return c
}

// Regenerator 页面重生成器。
type Regenerator struct {
	store  Store
	llm    ports.LLM
	prompt PromptFunc
	cfg    Config
	now    func() time.Time
}

// New 造重生成器。
func New(s Store, llm ports.LLM, cfg Config, opts ...Option) (*Regenerator, error) {
	if s == nil {
		return nil, errors.New("page: 存储未装配")
	}
	r := &Regenerator{
		store: s, llm: llm, prompt: DefaultPrompt,
		cfg: cfg.normalize(), now: func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Option 构造选项。
type Option func(*Regenerator)

// WithPrompt 自定义提示词。
func WithPrompt(f PromptFunc) Option { return func(r *Regenerator) { r.prompt = f } }

// Result 一轮重生成的结果。
type Result struct {
	// Regenerated 成功重生成的页面数。
	Regenerated int
	// Failed 重生成失败的页面数。
	Failed int
	// Skipped 因冷却期被跳过的页面数。
	Skipped int
	// Failures 失败明细（实体 ID → 原因）。
	Failures map[string]string
}

// RegenerateDirty 重生成所有到期脏页面。
//
// scope 是**这一轮的视角**：只重生成该视角下的脏页面。
// 页面按归属分页，各视角的页面互不相干，
// 一轮里把别的视角也重生成掉，等于用这个视角的模型调用去改别人的页面。
func (r *Regenerator) RegenerateDirty(ctx context.Context, ns store.Namespace, scope types.Scope) (*Result, error) {
	pages, err := r.store.ListDirtyEntityPages(ctx, ns, scope, r.cfg.BatchSize)
	if err != nil {
		return nil, err
	}
	res := &Result{Failures: map[string]string{}}
	for _, p := range pages {
		now := r.now()
		if !p.RegenerationDue(now, r.cfg.StaleAfter) {
			res.Skipped++
			continue
		}
		if err := r.regenerateOne(ctx, ns, p.EntityID, scope, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// RegenerateOne 重生成指定实体的页面（指定视角）。
func (r *Regenerator) RegenerateOne(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope) error {
	return r.regenerateOne(ctx, ns, entityID, scope, &Result{Failures: map[string]string{}})
}

// regenerateOne 重生成一页。
//
// 失败**不**返回错误：一次生成失败是常态（模型超时、上下文超长），
// 它已经记进页面（失败原因 + 保持脏），不该让整轮重生成中断。
// 只有存储故障才上抛。
func (r *Regenerator) regenerateOne(ctx context.Context, ns store.Namespace, entityID string,
	scope types.Scope, res *Result) error {
	all, err := r.store.ListAtomsByEntity(ctx, ns, entityID)
	if err != nil {
		return err
	}
	// 摘要只用**活跃**的事实：一条被推翻的事实进了画像，
	// 画像就会同时说「磁盘长期偏小」和「磁盘已经扩容」——
	// 而画像是给人看的一整段话，两句矛盾的话没法由读者分辨哪句算数。
	//
	// 被推翻的事实并没有丢：它仍在事实卡里，仍可被搜到并看到「已被谁取代」
	// （见 types.AtomCard 的软退役约定）。摘要不必承担那份责任。
	//
	// 摘要还只用**对这一视角可见**的事实（全局 + 同归属）：
	// 画像是事实的摘要，摘要若揉进别家的事实，这一页就成了
	// 「一张所有人都能看到的、混着别人内容的画像」——内容泄漏。
	//
	// 过滤放在**进提示词之前**：提示词是要发给模型的那一份，
	// 过滤放在它后面等于把别家的事实先发出去再处理。
	//
	// 判据用 types.Scope.VisibleTo（与存储层 SQL 的 `(scope='' OR scope=?)`
	// 同一形状、与召回层的复查同一判据）：一处判据，三个后端不会分叉。
	// 之所以在内存里过滤而不在 SQL 里过滤：这里的取数是「某实体的全部事实」
	// （条数已被 MaxAtoms 封顶），没有 TopK 配额竞争，
	// 不存在「被别家占掉配额」那种失效形态；而放在一处判据上更不容易分叉。
	atoms := make([]*types.AtomCard, 0, len(all))
	for _, a := range all {
		if !a.Active() {
			continue
		}
		if !a.Scope.VisibleTo(scope) {
			continue
		}
		atoms = append(atoms, a)
	}
	if len(atoms) == 0 {
		// 没有活跃事实就没有页面可写。标脏保持不变（事实将来会有），
		// 但这轮不算失败。
		res.Skipped++
		return nil
	}
	if len(atoms) > r.cfg.MaxAtoms {
		atoms = atoms[:r.cfg.MaxAtoms]
	}
	if r.llm == nil {
		err := ErrNoLLM
		_ = r.store.RecordEntityPageRegenFailure(ctx, ns, entityID, scope, err.Error(), r.now())
		res.Failed++
		res.Failures[entityID] = err.Error()
		return nil
	}

	out, err := r.llm.Complete(ctx, ports.LLMRequest{
		Prompt:    r.prompt(entityID, atoms),
		Purpose:   "memory.page.regen",
		JSON:      true,
		MaxTokens: 1200,
	})
	if err != nil {
		_ = r.store.RecordEntityPageRegenFailure(ctx, ns, entityID, scope, err.Error(), r.now())
		res.Failed++
		res.Failures[entityID] = err.Error()
		return nil
	}

	headline, body, perr := parsePage(out)
	if perr != nil {
		_ = r.store.RecordEntityPageRegenFailure(ctx, ns, entityID, scope, perr.Error(), r.now())
		res.Failed++
		res.Failures[entityID] = perr.Error()
		return nil
	}

	ids := make([]string, 0, len(atoms))
	for _, a := range atoms {
		ids = append(ids, a.ID)
	}
	pg := &types.EntityPage{
		EntityID: entityID, Scope: scope, Headline: headline, Body: body, AtomIDs: ids,
	}
	if err := r.store.ApplyEntityPageRegen(ctx, ns, entityID, scope, pg, r.now()); err != nil {
		return err
	}
	res.Regenerated++
	return nil
}

// parsePage 解析模型输出。
//
// 严格：只认 JSON 对象，字段缺失即失败。**不许**「解析不出来就当空页面」——
// 那会把一次模型抖动变成「这个实体的画像没了」，而用户看不出区别。
//
// 围栏容忍不算「宽松」：模型被要求输出 JSON 时，经常连着 ```json 围栏一起
// 给。不剥围栏的结果是每次模型重生成都失败一次，而失败保持脏标记、
// 于是这个实体的画像永远重生成不出来。剥离交给 oekit/textx 的
// textutil.StripCodeFence；「字段缺失即失败」那条不变。
func parsePage(raw string) (headline, body string, err error) {
	s := textutil.StripCodeFence(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var obj struct {
		Headline string `json:"headline"`
		Body     string `json:"body"`
	}
	if uerr := json.Unmarshal([]byte(s), &obj); uerr != nil {
		return "", "", fmt.Errorf("page: 解析失败: %w", uerr)
	}
	obj.Headline = strings.TrimSpace(obj.Headline)
	obj.Body = strings.TrimSpace(obj.Body)
	if obj.Headline == "" && obj.Body == "" {
		return "", "", errors.New("page: 模型输出既无 headline 也无 body")
	}
	return obj.Headline, obj.Body, nil
}
