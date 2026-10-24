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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/pkg/cache"

	"github.com/yi-nology/agent-memory/pipeline/query"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

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
	// Scope 读者归属（空=不隔离）。
	//
	// 非空时只召回「全局事实 + 同归属事实」：归属为空的卡是全局的，
	// 人人都该看到；归属非空且不等于本值的卡**一条都不进 prompt**。
	//
	// # 过滤在**取数层**生效，这里是给它的那个值
	//
	// 归属条件进存储层的查询，与 TopK 配额同一次求值——
	// 交给调用方后置过滤的话，被滤掉的条目已经占掉了配额，
	// 于是「该给我的那条」根本没进候选池，而症状只是「召回变少了」。
	//
	// ⚠️ 空值 = **不隔离**（不是「只看全局」）：这条语义与
	// store.AtomFilter.Scope 一致，两边必须一致，否则「传了没用」。
	// 不隔离是**显式选择**：宿主在调用点写不写得出这一栏，代码里看得见。
	Scope types.Scope
	// Limit 条数上限（0=用配置）。
	Limit int
	// OccurredFrom / OccurredTo 发生时间窗（含 From，不含 To；零值=不限）。
	//
	// 问「上周那次问题」时要的就是这个：**按事情发生的时刻**筛，
	// 而不是按事实「现在还成不成立」筛（那是 At）。
	//
	// 宿主显式给了就以宿主为准；没给而装了查询理解时，
	// 由查询理解从问句里解析（「上周」→ 具体起止）。
	OccurredFrom time.Time
	OccurredTo   time.Time
	// At 判定时刻（零值=不按生效区间过滤）。
	//
	// 「那会儿的事实」与「现在的事实」是两个不同的问题：
	// 一条事实可以「现在还成立」却「在那个时刻还不成立」
	// （它是后来才被写下来的）。故它是**独立**于时间窗的一个维度。
	//
	// 零值=不按生效区间过滤（既有行为不变）。
	At time.Time
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
	// Ranker 打分器（可 nil：nil = 用 ports.LexicalRanker 的缺省权重）。
	//
	// 注入它即可换排序口径（如走语义轨），**不必绕开整条召回链自造一套**——
	// 两套排序口径并存，后续无论往哪边收敛都要付迁移成本。
	//
	// 契约见 ports.Ranker：分数与候选一一对应且同序，长度不符即报错；
	// 打分失败**上抛**，不许静默回退到缺省（静默回退的失效形态是
	// 「排序悄悄变回老样子」，而调用方看不出发生了什么）。
	Ranker ports.Ranker
	// Semantic 语义召回面（可 nil：nil = 只走词面）。
	//
	// 装上它就变成**混合召回**：词面一路 + 语义一路，
	// 两路用 RRF 融合（见 ports.RRF）——融合决定**候选池里有什么**，
	// 排序仍归打分器。
	//
	// 刻意是**可选**的：没装时召回行为与装之前**逐条一致**，
	// 那条降级路径（没有模型也能召回）必须一直能跑。
	Semantic ports.SemanticReader
	// Analyzer 查询理解器（可 nil：nil = 不做查询理解）。
	//
	// 装上它，「上周那个问题」会被翻成「上周的时间窗 + 那个实体」，
	// 于是取数能按时间与实体收窄，而不是全靠词面碰运气。
	//
	// 同样是**可选**：不装时行为与现在完全一致。
	Analyzer *query.Analyzer
	// EventWindow 事件类事实的**默认召回窗口**（零值=用 DefaultEventWindow）。
	//
	// 存在的理由：短期事件会过期，而「过期」在召回侧是**时间**：
	// 三周前的一次重启不该再进 prompt——它不但占预算，
	// 还会让模型把那次重启当成当前状态。
	//
	// 只作用于事件类事实（types.FactKind.IsEvent）：状态与长期经验不按时间筛
	// （状态的过期由取代链表达，经验不因时间失效）。
	//
	// ⚠️ **用户显式问了历史时，这条让位**：请求里带了
	// OccurredFrom/OccurredTo（问哪个时间段）时，窗口不生效——
	// 显式查询优先于默认策略。
	EventWindow time.Duration
	// Metrics 指标面（可 nil：nil = 不打点）。
	//
	// 打点口径由内核定（指标名见本包的指标名常量），
	// 宿主只负责把它送到自己的监控——口径分散在两处时，
	// 两边的数字不可比，而不可比的指标比没有指标更坏。
	Metrics ports.Metrics
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
	if c.EventWindow <= 0 {
		c.EventWindow = DefaultEventWindow
	}
	// 缺省打分器：词面 + 重要度 + 新鲜度（权重 0.6/0.25/0.15）。
	//
	// 填在这里（而不是在调用点现取）是为了让「默认」这件事只有一个地方：
	// Config 归一后拿到的一定是可用的打分器，链上不必判 nil。
	if c.Ranker == nil {
		c.Ranker = ports.LexicalRanker{}
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
// ⚠️ **它是冻结面**（仓外宿主会实现它）：加方法 = 隐式 breaking，
// 新能力走可选能力面（见下面 SearchAtoms 那段与 store/optional.go）。
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
	//
	// 带过滤条件：归属过滤必须在这条路上生效——
	// 只在 ListAtoms 上过滤是不够的，检索路会绕过滤面直接取 TopK，
	// 被别家的卡占掉配额（症状是「召回变少了」，看着像排序问题）。
	SearchAtoms(ctx context.Context, ns store.Namespace, query string, f store.AtomFilter) ([]*types.AtomCard, error)
	// ⚠️ **本接口已冻结**：不要往这里加方法。
	//
	// 它是宿主要实现的面（仓外适配层直接实现它），
	// 加一个方法 ⇒ 下游**编译期直接红**（上游发 tag 的当天下游就得改）。
	//
	// 宽松词面检索曾在本接口里（v0.8.0 的 SearchAtomsLoose），
	// 现已移出为 **store.AtomLooseSearcher**（可选能力面）：
	// 召回链在 New 里探测它有没有，没有就回落到严格路
	// （降级路径见 gather.collectLoose，结果里看 Result.LooseSearchUnavailable）。
	//
	// 移出即是对下游的**非破坏**修复：原本实现了它的下游继续实现即可，
	// 没实现的也能编译——两种宿主都不会被打红。
	GetEntityPage(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope) (*types.EntityPage, error)
	UpsertActiveEntity(ctx context.Context, ns store.Namespace, a *types.ActiveEntity) error
	// ListActiveEntities 读活跃实体栈（代词消解的来源）。
	//
	// 刻意在**召回面**读而不是让宿主喂：代词消解依赖的是
	// 「这一轮之前聊过谁」，而那是**会话状态**，
	// 宿主每次自己喂一遍既啰嗦又容易喂错（喂了别的线程的栈）。
	ListActiveEntities(ctx context.Context, ns store.Namespace, threadID string, limit int) ([]*types.ActiveEntity, error)
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
	// ListRaw 按时间倒序列出原始事件。
	//
	// scope 是读者归属（空=不隔离）：原始事件也是召回的兜底源，
	// 一次就取 MaxRaw 条，被别家占掉之后就没有自己的了——
	// 故归属过滤必须在这一层生效。
	ListRaw(ctx context.Context, ns store.Namespace, scope types.Scope, before time.Time, p store.Page) ([]*types.RawEvent, error)
}

// Pipeline 召回流水线。
type Pipeline struct {
	cfg   Config
	store Reader
	raw   RawReader // nil = 宿主没有捕获面
	// loose 宽松词面检索能力（nil = 宿主没实现，召回回落到严格路）。
	//
	// 探测在 New 里做一次：能力不会中途长出来，而每次召回都断言一次
	// 是白花的开销（还会在指标里灌重复的计数）。
	loose  store.AtomLooseSearcher
	est    ports.TokenEstimator
	bander ports.Bander
	cache  *cache.LocalCache[Result]
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
	// 宽松词面检索是**可选能力**：有就用，没有就回落到严格路。
	//
	// ⚠️ 这里**不报错**：宽松路是增强（多话题查询的召回率），不是前提。
	// 报错等于把「可选」写成了「必需」——宿主装不起就崩。
	// 但降级必须可观测：Result.LooseSearchUnavailable + 指标
	// （否则症状只是「多话题查询打不中」，看着像内容不够）。
	// 接线与降级见 loose.go。
	loose := probeLooseSearcher(s)
	p := &Pipeline{
		cfg: norm, store: s, raw: raw, loose: loose, est: est, bander: bander,
		now: func() time.Time { return time.Now().UTC() },
	}
	// 缓存用流水线自己的时钟：两处时间不一致时，「缓存过期」这条行为
	// 就没法在测试里复现。
	p.cache = newRecallCache(norm.CacheTTL, p.now)
	return p, nil
}

// Config 取归一后的配置（供装配层自检与测试）。
func (p *Pipeline) Config() Config { return p.cfg }

// candidate 内部候选。
type candidate struct {
	Snippet
	score float64
	// fusion 多路融合分（零值=没走融合）。
	//
	// 它**不直接决定次序**（次序归打分器），
	// 而是作为一路信号交给打分器（ports.RankCandidate.FusionScore）。
	fusion float64
	// bucket 该片段占哪个配额桶。
	bucket ports.BucketID
	// dedupKey 去重键。
	dedupKey string
	// plain 去包裹的原文。词面匹配用它——用带标签的 Body 会让「记忆/画像」
	// 这两个每次都出现的标签词把所有片段的得分抬平。
	plain string
	// rawSession / rawRuntimeError 只对 SourceRaw 有意义，是防回声两闸的判据。
	rawSession      string
	rawRuntimeError bool
}

// 配额桶。
//
// 身份用 ports.BucketID（字符串）而不是本仓私有的枚举：
// 桶的**契约**（标识 + 配额 + 计数）在 ports.Budget 那件里，
// 宿主自造召回时用同一套；本仓的三个桶只是「本仓的三种来源分类」，
// 不该是内核替所有宿主定下的枚举。
const (
	bucketAtom     = ports.BucketID("atom")
	bucketQuote    = ports.BucketID("quote")
	bucketHeadline = ports.BucketID("headline")
)

// Recalle 单次召回。
func (p *Pipeline) Recall(ctx context.Context, req Request) (*Result, error) {
	started := p.now()
	if p.cfg.RecallTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.cfg.RecallTimeout)
		defer cancel()
	}

	// 查询理解**在查缓存之前**跑：解析结果依赖活跃实体栈，
	// 而栈会随对话变（「那个」指的实体变了）——
	// 不把解析结果算进键，缓存就会把上一次的解析结果端上来。
	//
	// 代价是缓存命中也要多一次栈查询；那只在装了查询理解时才有。
	eff, plan := p.analyze(ctx, req)

	if p.cache != nil {
		if hit, ok, _ := p.cache.GetEx(ctx, p.cacheKey(eff, plan)); ok && hit != nil {
			// 命中这件事必须对调用方可见，否则无法观测缓存是否生效。
			// GetEx 返回的是 CloneOnRead 副本，改它不污染缓存里那份。
			hit.FromCache = true
			// 缓存命中也要推活跃实体栈：栈是会话状态，不是召回结果的一部分。
			// 漏掉这一步的症状是「开了缓存之后代词消解变差」，
			// 而原因藏在缓存那一行里，从结果上看不出来。
			p.pushEntityIDs(ctx, req, hit.Entities)
			p.metrics().Incr(MetricCacheHit, 1)
			// 缓存命中的那次召回**也是**降级的（端出来的就是降级结果），
			// 故降级计数在这条路径上也要记——口径是「降级过的召回次数」。
			p.markDegraded()
			p.metrics().Observe(MetricRecallDuration, p.now().Sub(started))
			return hit, nil
		}
		p.metrics().Incr(MetricCacheMiss, 1)
	}

	limit := req.Limit
	if limit <= 0 {
		limit = p.cfg.TopK
	}

	cands, err := p.gather(ctx, eff, limit, plan)
	if err != nil {
		return nil, err
	}
	// 宽松路：命中任一词的事实（严格路是隐式 AND，多话题查询打不中）。
	looseAtoms, err := p.collectLoose(ctx, eff, limit)
	if err != nil {
		return nil, err
	}
	// 语义路（装了才有）：与词面路融合，决定**候选池里有什么**。
	sem, err := p.collectSemantic(ctx, eff, limit)
	if err != nil {
		return nil, err
	}
	loose, err := p.looseCandidates(ctx, eff, looseAtoms)
	if err != nil {
		return nil, err
	}
	cands = p.fuse(ctx, cands, loose, sem)
	p.metrics().Gauge(MetricSemanticHits, float64(len(sem)))

	cands, droppedByDuplicate := p.suppress(cands, eff)
	if err := p.rank(ctx, cands, eff.Query, p.now()); err != nil {
		return nil, err
	}
	cands, droppedByDiversity := diversify(cands, p.cfg.PerEntityMax)

	ents := p.pushEntities(ctx, eff, cands)

	kept, budget := p.allocate(cands)
	// 重复与多样性两条丢弃是召回器自己的判定（不是配额判定的结果），
	// 按原因上报进同一张表——「为什么少了」要在**一处**看全。
	budget.Drop(ports.DropDiversity, droppedByDiversity)
	budget.Drop(ports.DropDuplicate, droppedByDuplicate)
	drops := budget.Dropped()
	res := &Result{
		Snippets: kept,
		Entities: ents,
		// 旧的三条具名字段是通用表的投影：DroppedByBudget 仍是「预算装不下」
		// 的总数（桶不足 + 总量不足），处置不变。
		DroppedByBudget:    drops[ports.DropBudget] + drops[ports.DropQuota],
		DroppedByDiversity: droppedByDiversity,
		DroppedByDuplicate: droppedByDuplicate,
		Dropped:            budget.DroppedTotal(),
		Drops:              drops,
		Buckets:            budget.Kept(),
		// 能力缺失也要说清：降级不可见 ⇒ 宿主以为自己在用宽松路。
		LooseSearchUnavailable: p.loose == nil,
	}
	p.observe(res, budget, p.now().Sub(started))
	p.markDegraded()
	if p.cache != nil {
		// 写入时也存副本：调用方随后改写 res 不得污染缓存里那份
		// （旧实现是 put 时手工拷贝一份）。
		_ = p.cache.Set(ctx, p.cacheKey(eff, plan), cloneResult(res))
	}
	return res, nil
}

// analyze 跑查询理解，把结果并进请求。
//
// 返回（生效的请求，解析计划）。
//
// # 谁说了算
//
// **宿主优先**：宿主显式给了时间窗或实体，就以宿主的为准
// （宿主的判断可能来自它自己的上下文，比句面解析准）。
// 只有宿主没给时，才用解析结果补上。
//
// # 解析出的实体只用来补画像，不用来划事实范围
//
// Request.Entities 同时承载两种语义相反的用法，
// 而事实范围那一支必须由宿主用 RestrictAtomsToEntities **显式**声明。
// 让一句代词去划事实范围，等于让「那个」清空整轮事实。
func (p *Pipeline) analyze(ctx context.Context, req Request) (Request, types.QueryPlan) {
	plan := types.QueryPlan{Raw: req.Query}
	if p.cfg.Analyzer == nil {
		return req, plan
	}
	// 活跃实体栈：代词消解的来源。
	var recent []string
	if req.ThreadID != "" {
		if ents, err := p.store.ListActiveEntities(ctx, req.Namespace, req.ThreadID, 5); err == nil {
			for _, e := range ents {
				recent = append(recent, e.EntityID)
			}
		}
	}
	plan = p.cfg.Analyzer.Analyze(ctx, query.Input{
		Query:          req.Query,
		HostEntities:   req.Entities,
		RecentEntities: recent,
	})
	if req.OccurredFrom.IsZero() {
		req.OccurredFrom = plan.Time.From
	}
	if req.OccurredTo.IsZero() {
		req.OccurredTo = plan.Time.To
	}
	return req, plan
}

// metrics 取指标面（没装时给一个空的，调用点不用判 nil）。
func (p *Pipeline) metrics() ports.Metrics {
	if p.cfg.Metrics == nil {
		return ports.NoopMetrics{}
	}
	return p.cfg.Metrics
}

// observe 打点：结果规模、丢弃原因、耗时。
//
// 口径在这里定死（指标名见本包的常量），宿主只负责搬运。
func (p *Pipeline) observe(res *Result, budget *ports.Budget, dur time.Duration) {
	m := p.metrics()
	m.Observe(MetricRecallDuration, dur)
	m.Gauge(MetricSnippets, float64(len(res.Snippets)))
	m.Gauge(MetricTokens, float64(budget.UsedTotal()))
	m.Gauge(MetricEntitiesPushed, float64(len(res.Entities)))
	for reason, n := range res.Drops {
		m.Incr("recall.dropped."+string(reason), n)
	}
}

// DefaultEventWindow 事件类事实的缺省召回窗口（30 天）。
//
// 与 lifecycle 的缺省事件保留期**一致**：一条事件的「过期」在两处是同一件事，
// 两处口径不同就会出现「生命周期说它过期了、召回却还端上来」的错位
// （症状是「模型把三周前的一次重启当成当前状态」）。
//
// ⚠️ 它**不影响既有事实**：它们的 kind 是空串（= 长期经验），
// 而事件窗口只作用于事件类——所以开启它不改变任何老数据的行为。
const DefaultEventWindow = 30 * 24 * time.Hour

// 指标名。
//
// 刻意集中在这里：口径分散在两处时，两边的数字不可比，
// 而不可比的指标比没有指标更坏（它会让人得出「这个版本变慢了」的假结论）。
const (
	// MetricRecallDuration 单次召回耗时。
	MetricRecallDuration = "recall.duration"
	// MetricCacheHit 缓存命中次数。
	MetricCacheHit = "recall.cache.hit"
	// MetricCacheMiss 缓存未命中次数。
	MetricCacheMiss = "recall.cache.miss"
	// MetricSnippets 本次召回带出的片段数。
	MetricSnippets = "recall.snippets"
	// MetricTokens 本次召回占用的 token。
	MetricTokens = "recall.tokens"
	// MetricEntitiesPushed 推进活跃实体栈的实体数。
	MetricEntitiesPushed = "recall.entities.pushed"
	// MetricSemanticHits 语义路命中的条数。
	MetricSemanticHits = "recall.semantic.hits"
	// MetricLooseSearchUnavailable 宽松词面检索不可用（宿主没实现那个能力）的召回次数。
	//
	// 口径：每次**发生降级**的召回记一次（不是每次探测）。
	MetricLooseSearchUnavailable = "recall.loose.unavailable"
)

// cacheKey 缓存键。
//
// # 键必须覆盖**每一个会改变结果的请求维度**
//
// 这是本仓的一条通用纪律：缓存键漏一个维度，那一维上「不同的两次请求」
// 就会命中同一条缓存——而症状是**结果看起来完全正常**，只是内容串了。
//
// 本仓的请求维度共六个，少一个就串一路：
//
//	Namespace 记忆库
//	SessionID 防回声：本会话的原始事件不进 prompt
//	ThreadID  活跃实体栈
//	Query     检索词
//	Scope     读者归属 ← 漏了它就是**跨租户内容泄漏**
//	Entities  宿主解析出的实体（补画像源；显式限定时还划定事实边界）
//	Limit     条数上限
//
// ⚠️ 初版只键了前四个，漏了 Scope / Entities / Limit：
// 同一会话同一线程问同一句话、但**归属不同**的两次请求会命中同一条缓存，
// 于是第二次请求拿到了第一次（别的归属）的召回结果——
// 那不是「缓存不准确」，是**把别家的记忆塞进了当前 prompt**。
// 缓存默认关闭（CacheTTL=0）把风险压住了，但那是**配置在替代码兜底**，
// 一旦有人开了缓存，泄漏立刻发生。故键先修对，再谈开不开缓存。
//
// # 为什么要含 SessionID 与 ThreadID
//
// 这两样参与防回声与活跃实体栈，结果本就随会话变化：
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
func (p *Pipeline) cacheKey(req Request, plan types.QueryPlan) string {
	var b strings.Builder
	sep := func() { b.WriteByte(0) }
	b.WriteString(string(req.Namespace))
	sep()
	b.WriteString(req.SessionID)
	sep()
	b.WriteString(req.ThreadID)
	sep()
	b.WriteString(req.Query)
	sep()
	b.WriteString(string(req.Scope))
	sep()
	// 解析出来的实体**排序后**再进键：代词消解依赖活跃实体栈，
	// 而栈会随对话变（「那个」指的实体变了）——
	// 不进键的话，缓存会把上一次的解析结果端上来，
	// 而症状是「同一句话两次问，答案不一样」——看着像随机。
	planEnts := append([]string(nil), plan.Entities...)
	sort.Strings(planEnts)
	b.WriteString(strings.Join(planEnts, "\x01"))
	sep()
	// 实体集合**排序后**再进键：同一组实体换个书写顺序是同一次请求，
	// 不排序的话键会随调用点的遍历顺序变，命中率凭空掉一截
	//（而那会让人误以为「缓存好像不太行」，去查一个本来没问题的方向）。
	ents := append([]string(nil), req.Entities...)
	sort.Strings(ents)
	b.WriteString(strings.Join(ents, "\x01"))
	sep()
	b.WriteString(strconv.Itoa(req.Limit))
	sep()
	// 时间维度同样进键：时间窗或判定时刻一变，召回结果就变。
	// 这条纪律对自己也成立——新加的维度若不进键，
	// 就会用「上周的窗口」去命中「今天的窗口」的缓存。
	b.WriteString(strconv.FormatInt(req.OccurredFrom.UnixNano(), 10))
	sep()
	b.WriteString(strconv.FormatInt(req.OccurredTo.UnixNano(), 10))
	sep()
	b.WriteString(strconv.FormatInt(req.At.UnixNano(), 10))
	return b.String()
}
