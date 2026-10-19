// Package recall 召回流水线：从记忆库里挑出该进 prompt 的内容。
//
// # 八段串行，各自可单测
//
//	router → 多源取数 → suppress（防回声）→ rerank（重排）
//	      → diversify（去同质）→ budget（分桶配额）→ thread_state（活跃实体栈）
//	      → cache（TTL 缓存）
//
// # 取数是两路：查询命中 ∪ 高价值兜底
//
// 事实卡这一路的取数走两条线（见 gather.collectAtoms）：
// 按**查询词**检索的那一路，以及按**重要度/新鲜度**取的那一路。
// 查询词只参与重排是不够的——那样候选池与「用户问了什么」无关，
// 再相关的事实只要排在池子之外就永远进不了 prompt。
//
// # 两条不可协商的纪律
//
//  1. **召回内容是不可信内容。** 它来自别的会话、别的用户、别的宿主。
//     每一段进 prompt 的正文都必须经 ports.Bander 包成数据区——流水线构造时
//     就要求提供 Bander，不接受 nil（理由见 ports/band.go）。
//  2. **脱敏是上游的事。** 本层不负责把明文变密文：内容在**落库前**就该被打码
//     （pipeline/capture），落库之后再打等于已泄漏。本层只负责不把已退役、
//     已作废、本轮刚说过的东西重复塞进去。
package recall

import (
	"context"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Source 召回来源。
type Source string

const (
	// SourceAtom 事实卡。召回的**主力**——它是判断过的稳定事实。
	SourceAtom Source = "atom"
	// SourceRaw 原始捕获事件。只作兜底：持久命中足够时就不该塞原始对话，
	// 原始事件体量大、噪声高，进 prompt 的性价比远低于事实卡。
	SourceRaw Source = "raw"
	// SourcePageHeadline 实体页面的一句话画像。
	//
	// 只在宿主已解析出实体时才加：它是一个固定成本（每个实体一句），
	// 而它换来的上下文收益只在话题确实锁定时才兑现。
	SourcePageHeadline Source = "page_headline"
)

// Snippet 一段可注入的内容。
type Snippet struct {
	// Source 来源。
	Source Source
	// RefID 溯源标识（事实卡 ID / 事件 ID / 实体 ID）。
	RefID string
	// EntityID 归属实体（raw 可能为空）。
	EntityID string
	// Role 渲染角色（"记忆" / "历史" / "画像"）。
	Role string
	// Body 正文，**已经过 Bander 包裹**。
	Body string
	// Importance 重要度（raw 无此概念，取零值）。
	Importance types.Importance
	// OccurredAt 发生时刻（排序用）。
	OccurredAt time.Time
	// Tokens 该段占用的估算 token（进预算用）。
	Tokens int
}

// Result 召回结果。
type Result struct {
	// Snippets 按优先级排序，可直接渲染。
	Snippets []Snippet
	// Dropped 被丢弃的总条数（预算 + 多样性两路之和）。
	//
	// 刻意暴露：装不下就是装不下，而「用户以为带上了记忆、实际被静默丢掉」
	// 是最难被发现的失效。分两路给出，是因为处置不同——预算不足说明该调大
	// TotalBudget 或改进提炼，多样性超限说明该调大 PerEntityMax。
	Dropped int
	// DroppedByBudget 预算装不下而丢弃的条数。
	DroppedByBudget int
	// DroppedByDiversity 单一实体超额而丢弃的条数。
	DroppedByDiversity int
	// DroppedByDuplicate 内容重复而丢弃的条数。
	//
	// 三路分开给是因为处置不同：重复说明提炼层在制造同一件事（改 extractor），
	// 多样性说明实体偏斜（改 PerEntityMax 或改分桶），预算说明装不下（改总量）。
	DroppedByDuplicate int
	// FromCache 是否命中缓存。
	FromCache bool
	// Entities 本次推入活跃实体栈的实体（按栈顶在前）。
	Entities []string
}

// Empty 结果是否为空。
func (r *Result) Empty() bool { return r == nil || len(r.Snippets) == 0 }

// Render 渲染成可注入的文本块。
//
// 空结果返回空串而不是一句「没有记忆」——后者会占预算且诱导模型去想记忆这件事。
func (r *Result) Render() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder
	for _, s := range r.Snippets {
		b.WriteString("- [")
		b.WriteString(s.Role)
		b.WriteString("] ")
		b.WriteString(s.Body)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// Request 一次召回请求。
type Request struct {
	// Namespace 记忆库。
	Namespace store.Namespace
	// Query 召回查询（用户当轮的提问或主题）。
	Query string
	// SessionID 当前会话。用于防回声：本会话的原始事件不进 prompt。
	SessionID string
	// ThreadID 当前线程。用于活跃实体栈。
	ThreadID string
	// Entities 宿主已解析出的实体 ID；非空时补 page_headline 源。
	Entities []string
	// Limit 条数上限（0=用配置）。
	Limit int
}

// Config 召回配置。
type Config struct {
	// Sources 来源面。空=默认 {atom, raw}。
	Sources []Source
	// TopK 单源取数上限（重排前的候选池大小，0=50）。
	TopK int
	// TotalBudget 总量预算（token，0=1500）。
	TotalBudget int
	// BucketAtom 事实桶占比（0=0.70）。
	BucketAtom float64
	// BucketQuote 引用桶占比（0=0.20）。
	BucketQuote float64
	// BucketHeadline 画像桶占比（0=0.10）。
	//
	// 这三桶之和必须为 1；Start 时校验并归一。
	BucketHeadline float64
	// PerEntityMax 单个实体最多贡献几条（0=不限）。
	//
	// 存在的理由：同一实体挂十条事实时，它们说的是同一件事的十个侧面，
	// 全塞进去会让其他实体的事实被预算挤掉，而用户往往同时关心多个对象。
	PerEntityMax int
	// CacheTTL 缓存有效期（0=不缓存）。
	CacheTTL time.Duration
	// RecallTimeout 单次召回的总时限（0=不限）。
	RecallTimeout time.Duration
	// MaxRaw 原始事件取数上限（0=5）。
	MaxRaw int
	// RestrictAtomsToEntities 是否把事实卡**限定**在 Request.Entities 之内。
	//
	// # 为什么默认 false
	//
	// Request.Entities 这个字段同时承载两种**语义相反**的用法：
	//
	//   - 「本轮**提到**的实体」——用来补画像页（页面桶）；
	//   - 「本会话的**锚点**实体」——用来给事实卡划范围。
	//
	// 内核从字段本身**区分不了**这两者。故默认不过滤，
	// 由宿主显式声明「这一轮的 Entities 就是事实边界」——
	// 接错了的后果是「本轮随口提到的一个对象，把整个会话的事实集清空」。
	//
	// # 置真时的语义
	//
	//   - Request.Entities 非空 → 事实卡只取这些实体的；
	//   - Request.Entities 为空 → **返回空集**（不是全库）。
	//
	// 最后一条是要点：「显式声明了范围但范围为空」与「没声明范围」
	// 是两种不同的意图——前者是「这一轮不该带任何事实」，
	// 后者是「不限」。退化成全库会把「限定」变成「装饰」。
	RestrictAtomsToEntities bool
	// NoCaptureFace 宿主**没有捕获面**（只蒸馏终稿，不采集原始事件）。
	//
	// 缺省 false = 有捕获面。置 true 时：
	//
	//   - raw 源**不出现在路由里**（不取、不注入、也不占预算桶）；
	//   - 存储**不需要**实现 recall.RawReader。
	//
	// # 为什么需要它
	//
	// 只有采集原始事件的宿主才有捕获面；只蒸馏终稿的宿主没有。
	// 让那种宿主实现 ListRaw 只能给一个「永远返回空集」的桩——
	// 那是**契约洗白**（声称支持捕获面，实际不支持），
	// 而内核会据此在 raw 源上做出「有原始事件」的假设。
	//
	// 这条开关优先于 Sources：显式说「没有捕获面」时，
	// 即使 Sources 里还留着 SourceRaw，也会被摘掉。
	NoCaptureFace bool
}

// DefaultConfig 缺省配置。
//
// 数字沿用上游 octop-memory 的缺省口径（1500 总预算 / 70-20-10 分桶）——
// 它已经过一轮实测调参，而本仓的调用场景属于同一类对话负载（单会话、上下文竞争、
// 记忆与对话共处一个 prompt）。**默认值不是圣旨**：真要调，先看 Result 的
// 三路丢弃计数，它们比任何一次主观感受都更能说明该往哪儿加。
func DefaultConfig() Config {
	return Config{
		Sources:        []Source{SourceAtom, SourceRaw},
		TopK:           50,
		TotalBudget:    1500,
		BucketAtom:     0.70,
		BucketQuote:    0.20,
		BucketHeadline: 0.10,
		PerEntityMax:   3,
		CacheTTL:       0,
		RecallTimeout:  2 * time.Second,
		MaxRaw:         5,
	}
}

// normalize 归一配置并校验分桶。
func (c Config) normalize() (Config, error) {
	if len(c.Sources) == 0 {
		c.Sources = []Source{SourceAtom, SourceRaw}
	}
	if c.TopK <= 0 {
		c.TopK = 50
	}
	if c.TotalBudget <= 0 {
		c.TotalBudget = 1500
	}
	if c.MaxRaw <= 0 {
		c.MaxRaw = 5
	}
	// 无捕获面：raw 源**不出现在路由里**。
	//
	// 这一步放在默认来源之后：Sources 为空时先补上默认（atom + raw），
	// 再按「没有捕获面」把 raw 摘掉——否则显式开关会被默认值吃掉。
	//
	// 它优先于 Sources：显式说「没有捕获面」时，
	// 即使 Sources 里还留着 SourceRaw，也以开关为准。
	if c.NoCaptureFace {
		c.Sources = withoutSource(c.Sources, SourceRaw)
	}
	sum := c.BucketAtom + c.BucketQuote + c.BucketHeadline
	if sum <= 0 {
		c.BucketAtom, c.BucketQuote, c.BucketHeadline = 0.70, 0.20, 0.10
		return c, nil
	}
	// 归一到 1 而不是报错：宿主常按「80/15/5」这类整数配，比例对就行，
	// 因为三桶的绝对 token 数由总量再乘出来的。
	c.BucketAtom /= sum
	c.BucketQuote /= sum
	c.BucketHeadline /= sum
	return c, nil
}

// Reader 召回链用到的存储面。
//
// 刻意**不直接吃 store.Store**：召回只用得到 6 个方法，声明全量接口会让
// 单元测试被迫实现 40 多个方法，也让「召回依赖了存储的哪些面」在类型上看不出来。
// store/store.go 的包注释里写了这条纪律（「阶段只声明自己需要的子接口」），
// 这里是对它自己的遵守。
//
// 叫 Reader 而不是 Source：Source 已经是「召回来源」那个枚举了（atom/raw/
// page_headline），而这里说的是「数据从哪读」，两个概念挨太近反而容易混谈。
type Reader interface {
	ListAtoms(ctx context.Context, ns store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error)
	// SearchAtoms 按查询词检索事实卡。
	//
	// 它是**取数**的一面（不是重排）：查询词必须参与「捞哪几条」，
	// 否则召回对「用户问了什么」只有排序上的响应、没有取数上的响应——
	// 候选池之外再相关的事实也进不来。
	// 故这一条不能省，尽管它让接口从 5 个方法变成 6 个。
	SearchAtoms(ctx context.Context, ns store.Namespace, query string, limit int) ([]*types.AtomCard, error)
	GetEntityPage(ctx context.Context, ns store.Namespace, entityID string) (*types.EntityPage, error)
	UpsertActiveEntity(ctx context.Context, ns store.Namespace, a *types.ActiveEntity) error
	EvictActiveEntities(ctx context.Context, ns store.Namespace, threadID string, keep int) ([]types.ActiveEntity, error)
}

// RawReader 召回链用到的**原始事件**面（可选）。
//
// 刻意与 Reader 分开：只有**采集原始事件**的宿主才有捕获面，
// 只蒸馏终稿的宿主没有。让那种宿主实现 ListRaw 只能给一个
// 「永远返回空集」的桩——那是**契约洗白**（声称支持捕获面，实际不支持），
// 而内核会据此在 raw 源上做出「有原始事件」的假设。
//
// 故它是**可选实现**：存储实现了它，raw 源才进路由；
// 没实现而配置又要 raw 源，New 会**响亮失败**（见 New）。
type RawReader interface {
	ListRaw(ctx context.Context, ns store.Namespace, before time.Time, p store.Page) ([]*types.RawEvent, error)
}

// Pipeline 召回流水线。
type Pipeline struct {
	cfg    Config
	store  Reader
	raw    RawReader // nil = 宿主没有捕获面
	est    ports.TokenEstimator
	bander ports.Bander
	cache  *cache
	now    func() time.Time
}

// New 造召回流水线。
//
// bander 为 nil 时**直接报错**：记忆是不可信内容，不裹数据区就进 prompt 等于
// 把控制权交给记忆里的一行字。装配层漏配时在这里炸，比在生产里被注入强。
//
// 存储**没有捕获面**（未实现 RawReader）而配置又要 raw 源时，同样**直接报错**：
// 让那种宿主实现 ListRaw 只能给一个「永远返回空集」的桩——
// 那是契约洗白（声称支持捕获面，实际不支持），
// 而内核会据此在 raw 源上做出「有原始事件」的假设。
// 两条出路由错误信息给出：实现 RawReader，或设 Config.NoCaptureFace = true。
func New(cfg Config, s Reader, est ports.TokenEstimator, bander ports.Bander) (*Pipeline, error) {
	if s == nil {
		return nil, errNilStore
	}
	if est == nil {
		// 未注入计量器不报错，退到静态代理——但静态代理高估（见 ports/band.go），
		// 高估的代价是少塞几条内容而不是撑爆 prompt，取可接受的那侧。
		est = ports.StaticEstimator{}
	}
	if bander == nil {
		return nil, errNilBander
	}
	norm, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	var raw RawReader
	if hasSource(norm.Sources, SourceRaw) {
		r, ok := s.(RawReader)
		if !ok {
			return nil, errNoRawSource
		}
		raw = r
	}
	p := &Pipeline{
		cfg: norm, store: s, raw: raw, est: est, bander: bander,
		now: func() time.Time { return time.Now().UTC() },
	}
	// 缓存用流水线自己的时钟：两处时间不一致时，「缓存过期」这条行为
	// 就没法在测试里复现。
	p.cache = newCache(norm.CacheTTL, p.now)
	return p, nil
}

// Config 取归一后的配置（供装配层自检与测试）。
func (p *Pipeline) Config() Config { return p.cfg }

// recall 取候选片段。
type candidate struct {
	Snippet
	score float64
	// bucket 该片段占哪个配额桶。
	bucket bucket
	// dedupKey 去重键。
	dedupKey string
	// plain 去包裹的原文。词面匹配用它——用带标签的 Body 会让「记忆/画像」
	// 这两个每次都出现的标签词把所有片段的得分抬平。
	plain string
	// rawSession / rawRuntimeError 只对 SourceRaw 有意义，是防回声两闸的判据。
	rawSession      string
	rawRuntimeError bool
}

// bucket 配额桶。
type bucket int

const (
	bucketAtom bucket = iota
	bucketQuote
	bucketHeadline
)

// Recalle 单次召回。
func (p *Pipeline) Recall(ctx context.Context, req Request) (*Result, error) {
	if p.cfg.RecallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.RecallTimeout)
		defer cancel()
	}

	if p.cache != nil {
		if hit, ok := p.cache.get(p.cacheKey(req)); ok {
			return hit, nil
		}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = p.cfg.TopK
	}

	cands, err := p.gather(ctx, req, limit)
	if err != nil {
		return nil, err
	}
	cands, droppedByDuplicate := p.suppress(cands, req)
	rank(cands, req.Query, p.now())
	cands, droppedByDiversity := diversify(cands, p.cfg.PerEntityMax)

	ents := p.pushEntities(ctx, req, cands)

	kept, droppedByBudget := p.allocate(cands)
	res := &Result{
		Snippets:           kept,
		Entities:           ents,
		DroppedByBudget:    droppedByBudget,
		DroppedByDiversity: droppedByDiversity,
		DroppedByDuplicate: droppedByDuplicate,
		Dropped:            droppedByBudget + droppedByDiversity + droppedByDuplicate,
	}
	if p.cache != nil {
		p.cache.put(p.cacheKey(req), res)
	}
	return res, nil
}

// cacheKey 缓存键。
//
// **含** SessionID 与 ThreadID。这两样参与防回声与活跃实体栈，结果本就随会话变化：
// 不把它们算进键，第二个会话问同一个问题就会拿到第一个会话的召回结果——
// 那等于把上一次会话提到过的内容原样搬进这一次，连同它已经被本轮说过的那部分。
//
// 代价是缓存命中率下降（同一问题跨会话不再复用）。这个方向的取舍是显然的：
// 多查一次的代价是一次数据库扫描，串错会话的代价是用户看见自己刚说过的话
// 又被「记忆」重复陈述一遍。
//
// ⚠️ **本条实现时首版恰好写反过一次**：注释里写的是「刻意不含」，理由却是
// 「不含才不会串会话」——那正是把它含进去的理由。方向写反的注释比没有注释更
// 危险，因为它读起来像是想清楚了。用例 TestCacheKeyIgnoresSession 钉住了行为，
// 但那条用例的断言文案当时也跟着写反了，一并改了。
func (p *Pipeline) cacheKey(req Request) string {
	return string(req.Namespace) + "\x00" + req.SessionID + "\x00" + req.ThreadID + "\x00" + req.Query
}
