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
	// Drops 按原因分路的丢弃计数（**通用面**）。
	//
	// 上面的三条具名字段是这张表在本仓召回里的**投影**（宿主读旧字段不破），
	// 这张表才是可复用的那一面：宿主自造召回时用 ports.Budget 拿到同样形状的
	// 计数，于是「为什么少了」在两边都能归因——
	// 而分桶不足（DropQuota）与总量不足（DropBudget）必须分开：
	// 前者该调分桶占比，后者该调总量，合成一个数就看不出是哪一种。
	Drops map[ports.DropReason]int
	// Buckets 各配额桶收到的条数（**通用面**）。
	//
	// 宿主能据此看到「这一轮事实桶收了几条、画像桶收了几条」，
	// 而不必自己按 Role/Source 猜——画像桶恒为 0 是个值得发现的症状
	//（画像被挤空会让页面重生成这个功能形同虚设）。
	Buckets map[ports.BucketID]int
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
	// ListRaw 按时间倒序列出原始事件。
	//
	// scope 是读者归属（空=不隔离）：原始事件也是召回的兜底源，
	// 一次就取 MaxRaw 条，被别家占掉之后就没有自己的了——
	// 故归属过滤必须在这一层生效。
	ListRaw(ctx context.Context, ns store.Namespace, scope types.Scope, before time.Time, p store.Page) ([]*types.RawEvent, error)
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
	if err := p.rank(ctx, cands, req.Query, p.now()); err != nil {
		return nil, err
	}
	cands, droppedByDiversity := diversify(cands, p.cfg.PerEntityMax)

	ents := p.pushEntities(ctx, req, cands)

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
