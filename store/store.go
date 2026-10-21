// Package store 记忆持久化契约：捕获事件、候选、事实卡、实体、页面、树、情节与审计。
//
// # 为什么按消费面拆成小接口，而不是一个大接口
//
// 上游 octop-memory 用一个 60 方法的 MemoryBackend Protocol，因为实现方恰好两个。
// 本仓的消费者是流水线阶段（捕获 / 晋升 / 召回 / 页面 / 生命周期），每个阶段
// 只需要其中一小片。拆开后：
//
//   - 单测能用小假实现覆盖一段流水线，不必实现全量接口；
//   - 「阶段只允许碰哪些表」在**类型上**可见，漏写一条越权调用即编译不过；
//   - 新增后端只需补齐被用到的片段。
//
// # namespace 是显式参数
//
// 不做「构造 Store 时绑定一个 namespace」（那是上游的做法）：记忆库要多租户时，
// 绑定式接口的每个方法都要么塞进参数、要么多造一个实例。显式传参让「写错了
// namespace」这件事在调用点就看得到。
package store

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/agent-memory/types"
)

// Namespace 记忆命名空间：同一后端内相互隔离的一组记忆。
type Namespace string

// 存储层哨兵错误（消费方一律 errors.Is 判定，不做字符串匹配）。
var (
	// ErrNotFound 目标不存在。
	ErrNotFound = errors.New("store: not found")
	// ErrConflict 唯一性冲突（重复标识）。
	ErrConflict = errors.New("store: conflict")
	// ErrStaleWrite 陈旧覆盖：写入方持有的版本比已落库版本旧。
	//
	// 幂等挡不住陈旧覆盖——见 checkpoint 的教训：进程 A 写 seq=5、
	// 进程 B 拿更旧视图写 seq=3，两次键不同、都「幂等」，B 的慢写照样盖掉 A。
	ErrStaleWrite = errors.New("store: stale write rejected")
	// ErrClosed 后端已关闭。
	ErrClosed = errors.New("store: closed")
)

// 共享查询参数。
type (
	// Page 分页参数。
	Page struct {
		Offset int
		Limit  int
	}
	// AtomFilter 事实卡过滤条件（零值=不过滤）。
	AtomFilter struct {
		// EntityIDs 限定实体集合（空=全部）。
		EntityIDs []string
		// Scope 读者归属（空=不隔离）。
		//
		// 非空时取「**全局事实 + 同归属事实**」：归属为空的卡是全局的，
		// 人人都该看到；归属非空且不等于读者的卡**一条都不给**。
		// 判据见 types.Scope.VisibleTo。
		//
		// # 为什么空值是「不隔离」而不是「只看全局」
		//
		// 两种语义都自洽，但**失效方向相反**：
		//
		//   - 空=不隔离：宿主忘了传归属 → 看到全部（含别人的卡）→ **泄漏**；
		//   - 空=只看全局：宿主忘了传归属 → 只看到全局卡 → **少取**（看起来正常）。
		//
		// 本仓取「空=不隔离」，理由是**与既有过滤面一致**
		// （EntityIDs 空=不限、Importance 空=不限）：
		// 同一份过滤条件里两种空值语义会让调用点读错。
		// 泄漏的风险不靠这条语义兜，而靠**归属是显式参数**：
		// 宿主在调用点写不写得出这一栏，代码里看得见；
		// 且读取侧还有一道兜底（召回层复查可见性，见 pipeline/recall）。
		//
		// ⚠️ 隔离**必须在取数层生效**，不能交给调用方后置过滤：
		// 后置过滤时，被滤掉的条目已经占掉了取数配额（TopK），
		// 于是「该给我的那条」根本没进候选池——而症状是「召回变少了」，
		// 看着像排序问题，根因却在取数。
		Scope types.Scope
		// ActiveOnly 只要活跃事实（软退役排除）。
		//
		// 召回路径**必须**置位：默认值 false 意味着「忘了过滤就捞到已作废事实」，
		// 而那种 bug 在数据上完全看不出来。
		ActiveOnly bool
		// Importance 限定重要度档（空=全部）。
		Importance []types.Importance
		// OccurredFrom / OccurredTo 发生时间窗（含 From，不含 To）。
		OccurredFrom time.Time
		OccurredTo   time.Time
		// Limit 条数上限（0=后端默认）。
		Limit int
	}
	// CandidateFilter 候选过滤条件。
	CandidateFilter struct {
		Statuses []types.CandidateStatus
		Session  string
		Limit    int
	}
)

// RawStore 捕获事件存取。
type RawStore interface {
	SaveRaw(ctx context.Context, ns Namespace, ev *types.RawEvent) error
	SaveRawBatch(ctx context.Context, ns Namespace, evs []*types.RawEvent) error
	GetRaw(ctx context.Context, ns Namespace, id string) (*types.RawEvent, error)
	GetRawByIDs(ctx context.Context, ns Namespace, ids []string) ([]*types.RawEvent, error)
	// ListRaw 按时间倒序列出。before 非零时只取更早的（分页游标）。
	//
	// scope 是读者归属（空=不隔离），语义同 AtomFilter.Scope：
	// 非空时只取「全局 + 同归属」的事件。
	//
	// 它必须在这里生效而不能后置过滤：原始事件是召回的兜底源，
	// 一次就取 MaxRaw 条——被别家占掉之后就没有自己的了。
	ListRaw(ctx context.Context, ns Namespace, scope types.Scope, before time.Time, p Page) ([]*types.RawEvent, error)
	// SearchRaw 词法检索。
	SearchRaw(ctx context.Context, ns Namespace, scope types.Scope, query string, limit int) ([]*types.RawEvent, error)
	// DeleteRawBefore 清理早于 before 的事件，返回删除条数。
	DeleteRawBefore(ctx context.Context, ns Namespace, before time.Time) (int, error)
}

// CandidateStore 候选存取。
type CandidateStore interface {
	// FindDuplicateCandidate 查同签名候选。
	//
	// 签名比对而非内容全等：内容全等会漏掉「同义不同字」的重复，
	// 而记忆库里重复事实的代价是召回互相稀释。
	FindDuplicateCandidate(ctx context.Context, ns Namespace, signature string) (*types.Candidate, error)
	SaveCandidate(ctx context.Context, ns Namespace, c *types.Candidate) error
	GetCandidate(ctx context.Context, ns Namespace, id string) (*types.Candidate, error)
	ListCandidates(ctx context.Context, ns Namespace, f CandidateFilter) ([]*types.Candidate, error)
	// UpdateCandidateStatus 落定状态；带 expected 乐观锁，防并发双判。
	//
	// expected 非空时与库内现值不符即 ErrStaleWrite。
	UpdateCandidateStatus(ctx context.Context, ns Namespace, id string, expected, next types.CandidateStatus, actor types.DecidedBy, reason string) error
	SearchCandidates(ctx context.Context, ns Namespace, query string, limit int) ([]*types.Candidate, error)
}

// AtomStore 事实卡存取。
type AtomStore interface {
	// SaveAtom 落库一条事实卡。
	//
	// **向量不在落库范围内**：本仓的定案是「向量轨不接」，
	// 两个后端都不存向量（见 types.AtomCard.Vector）。
	// 故卡上带着非空向量时**必须报错**——
	// 静默丢掉的话，宿主的症状是「相似度检索永远空」，
	// 而它与「存错了地方」毫无相似之处。
	SaveAtom(ctx context.Context, ns Namespace, a *types.AtomCard) error
	GetAtom(ctx context.Context, ns Namespace, id string) (*types.AtomCard, error)
	ListAtoms(ctx context.Context, ns Namespace, f AtomFilter) ([]*types.AtomCard, error)
	// ListAtomsByEntity 取某个实体名下的全部事实卡。
	//
	// **不按退役态过滤**：实体合并要把该实体名下**所有**卡（含已退役的）
	// 一起迁到新归属——漏掉已退役的会让那些卡永远指向一个已经不存在的实体。
	// 要「活跃的那部分」由调用方自己判（页面重生成就要）。
	//
	// 之所以单列一个方法而不让调用方用 ListAtoms + AtomFilter.EntityIDs：
	// 后者的语义是「按归属过滤」，而「某个实体的全部卡」是页面与归一面
	// 反复要用的一种**取数形态**——把它写进契约，各后端就不会各自实现一遍。
	ListAtomsByEntity(ctx context.Context, ns Namespace, entityID string) ([]*types.AtomCard, error)
	// FindAtomBySignature 按签名查事实（签名定义见 types.AtomCard.Signature）。
	FindAtomBySignature(ctx context.Context, ns Namespace, signature string) (*types.AtomCard, error)
	// SearchAtoms 词法检索。
	//
	// 带过滤条件（而不是只带 limit）是**刻意的**：检索是取数面的一条路，
	// 归属过滤必须在这条路上生效——只在 ListAtoms 上过滤是不够的，
	// 因为检索路会绕过滤面直接取 TopK，被别家的卡占掉配额。
	//
	// 过滤条件的其余各项语义与 ListAtoms 相同（零值=不过滤）：
	// ActiveOnly 默认 false，故「退役事实仍可搜到」这条约定保持不变
	// （追问「当初为什么这么判」要能搜到）。
	SearchAtoms(ctx context.Context, ns Namespace, query string, f AtomFilter) ([]*types.AtomCard, error)
	// SupersedeAtom 软退役：被继任者推翻。旧卡保留可查。
	SupersedeAtom(ctx context.Context, ns Namespace, id, supersededBy string) error
	// DeprecateAtom 软退役：人工作废。
	DeprecateAtom(ctx context.Context, ns Namespace, id string, at time.Time) error
	// CountStats 各类计数（看板与体检用）。
	CountStats(ctx context.Context, ns Namespace) (Stats, error)
}

// Stats 记忆库计数。
type Stats struct {
	RawEvents   int
	Candidates  int
	Pending     int
	Atoms       int
	ActiveAtoms int
	Entities    int
	Pages       int
	DirtyPages  int
	Episodes    int
}

// EntityStore 实体与别名存取。
type EntityStore interface {
	SaveEntity(ctx context.Context, ns Namespace, e *types.Entity) error
	GetEntity(ctx context.Context, ns Namespace, id string) (*types.Entity, error)
	// FindEntityByName 按规范名或别名查实体（两条路径都要走，缺一条就会漏归一）。
	FindEntityByName(ctx context.Context, ns Namespace, name string) (*types.Entity, error)
	ListEntities(ctx context.Context, ns Namespace, entityType types.EntityType, limit int) ([]*types.Entity, error)
	// BumpEntityAtomCount 事实卡增减时同步计数。
	BumpEntityAtomCount(ctx context.Context, ns Namespace, entityID string, delta int) error
	SaveAlias(ctx context.Context, ns Namespace, a *types.Alias) error
	ListAliases(ctx context.Context, ns Namespace, entityID string) ([]*types.Alias, error)
	// UpsertActiveEntity 活跃实体栈写入（幂等）。
	UpsertActiveEntity(ctx context.Context, ns Namespace, a *types.ActiveEntity) error
	// ListActiveEntities 按最近提及倒序列出。
	ListActiveEntities(ctx context.Context, ns Namespace, threadID string, limit int) ([]*types.ActiveEntity, error)
	// EvictActiveEntities 淘汰最久未提及者，返回被淘汰的实体。
	//
	// 钉住项（pinned 且未过钉住窗口）**不参与淘汰**——用户刚确认过的对象
	// 滑出栈会让「那个」立刻指错人。
	EvictActiveEntities(ctx context.Context, ns Namespace, threadID string, keep int) ([]types.ActiveEntity, error)
}

// PageStore 实体页面存取。
//
// # 页面按「归属视角」分页
//
// 页面的逻辑主键是 (namespace, entity_id, scope)：同一实体在不同归属下各有一页。
// 事实卡已经按归属隔离，页面是事实的摘要——摘要若不分视角，
// 等于把别家的事实揉进一张所有人都能看的画像，而读者分不清哪句属于自己。
//
// 下面各方法里的 scope 是**键的一部分**（页面视角），不是过滤面：
// 空 = 全局视角（这一页只汇总全局事实）。
// 它与 AtomFilter.Scope 的空值语义（空=不隔离）刻意不同，理由见 types.EntityPage.Scope。
type PageStore interface {
	// UpsertEntityPage 写入页面。归属取 p.Scope（页面自带的视角）。
	UpsertEntityPage(ctx context.Context, ns Namespace, p *types.EntityPage) error
	// GetEntityPage 取 (namespace, entity_id, scope) 这一页。
	//
	// **精确取键**：别的视角的页面取不到（取不到就是取不到，不回退）。
	// 回退策略归召回层管——它知道读者是谁。
	GetEntityPage(ctx context.Context, ns Namespace, entityID string, scope types.Scope) (*types.EntityPage, error)
	// ListDirtyEntityPages 列出该视角下的脏页面。
	ListDirtyEntityPages(ctx context.Context, ns Namespace, scope types.Scope, limit int) ([]*types.EntityPage, error)
	// ListEntityPages 列出全部页面（导包要完整，不能只导脏的）。
	//
	// **跨视角全列**：导包要的是这个命名空间的全部记忆，
	// 只导一个视角等于导出时把别家的画像悄悄丢了（而导入方看不出来）。
	ListEntityPages(ctx context.Context, ns Namespace, limit int) ([]*types.EntityPage, error)
	// MarkEntityPageDirty 标脏（晋升侧调用，不触发生成）。
	//
	// scope 是**这条事实的归属**：
	//
	//   - scope 非空 → 只标该视角那一页；
	//   - scope 为空（全局事实）→ **该实体的所有视角都标脏**。
	//
	// 第二条的理由：全局事实出现在每个视角的画像里（它对谁都可见），
	// 它一变，所有视角的摘要都过时了。只标全局那一页的话，
	// 各归属的画像会停在旧版本，而看板上「页面存在且是旧的」
	// 与「页面正常」长得一模一样。
	MarkEntityPageDirty(ctx context.Context, ns Namespace, entityID string, scope types.Scope, when time.Time) error
	// ApplyEntityPageRegen 生成成功：落正文、清脏、记录尝试时刻与清失败。
	ApplyEntityPageRegen(ctx context.Context, ns Namespace, entityID string, scope types.Scope,
		p *types.EntityPage, at time.Time) error
	// RecordEntityPageRegenFailure 生成失败：记原因、清脏、**保持脏标记**。
	RecordEntityPageRegenFailure(ctx context.Context, ns Namespace, entityID string, scope types.Scope,
		reason string, at time.Time) error
}

// EpisodeStore 情节与周期摘要存取。
type EpisodeStore interface {
	SaveEpisode(ctx context.Context, ns Namespace, e *types.Episode) error
	ListEpisodes(ctx context.Context, ns Namespace, session string, limit int) ([]*types.Episode, error)
	// ListEpisodesInWindow 按发生时间窗取（周期摘要的素材）。
	ListEpisodesInWindow(ctx context.Context, ns Namespace, from, to time.Time) ([]*types.Episode, error)
	SaveDigest(ctx context.Context, ns Namespace, d *types.DigestRecord) error
	GetDigest(ctx context.Context, ns Namespace, period types.DigestPeriod, key string) (*types.DigestRecord, error)
	// ListDigests 列出全部周期摘要（导包要完整）。
	ListDigests(ctx context.Context, ns Namespace, limit int) ([]*types.DigestRecord, error)
}

// JournalStore 审计日志（追加面）。
type JournalStore interface {
	AppendJournal(ctx context.Context, ns Namespace, e *types.JournalEntry) error
	ListJournal(ctx context.Context, ns Namespace, subject string, limit int) ([]*types.JournalEntry, error)
}

// MetaStore 命名空间级元数据（schema 版本、抽取器版本、配置指纹）。
type MetaStore interface {
	GetMeta(ctx context.Context, ns Namespace, key string) (string, error)
	SetMeta(ctx context.Context, ns Namespace, key, value string) error
}

// Store 全量存储面。流水线阶段可声明自己需要的**子接口**，
// 测试时只实现那一小片。
type Store interface {
	RawStore
	CandidateStore
	AtomStore
	EntityStore
	PageStore
	EpisodeStore
	JournalStore
	MetaStore

	// Transaction 事务边界。
	//
	// 晋升是「候选落定 + 事实入库 + 实体计数 + 审计条目」四步，
	// 中途失败若不回滚，记忆库会出现「候选已判但事实没进」的空洞——
	// 而这类不一致在读取侧完全看不出来（只是那条事实凭空消失）。
	Transaction(ctx context.Context, fn func(ctx context.Context, s Store) error) error

	// Close 释放底层资源。
	Close() error
}

// RawWriter 是 RawStore 的写入面小接口（捕获阶段只需要这一片）。
type RawWriter interface {
	SaveRaw(ctx context.Context, ns Namespace, ev *types.RawEvent) error
	SaveRawBatch(ctx context.Context, ns Namespace, evs []*types.RawEvent) error
}
