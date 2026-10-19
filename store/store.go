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
	ListRaw(ctx context.Context, ns Namespace, before time.Time, p Page) ([]*types.RawEvent, error)
	// SearchRaw 词法检索。
	SearchRaw(ctx context.Context, ns Namespace, query string, limit int) ([]*types.RawEvent, error)
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
	SaveAtom(ctx context.Context, ns Namespace, a *types.AtomCard) error
	GetAtom(ctx context.Context, ns Namespace, id string) (*types.AtomCard, error)
	ListAtoms(ctx context.Context, ns Namespace, f AtomFilter) ([]*types.AtomCard, error)
	// FindAtomBySignature 按签名查事实（签名定义见 types.AtomCard.Signature）。
	FindAtomBySignature(ctx context.Context, ns Namespace, signature string) (*types.AtomCard, error)
	// SearchAtoms 词法检索。
	SearchAtoms(ctx context.Context, ns Namespace, query string, limit int) ([]*types.AtomCard, error)
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
	Nodes       int
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
type PageStore interface {
	UpsertEntityPage(ctx context.Context, ns Namespace, p *types.EntityPage) error
	GetEntityPage(ctx context.Context, ns Namespace, entityID string) (*types.EntityPage, error)
	ListDirtyEntityPages(ctx context.Context, ns Namespace, limit int) ([]*types.EntityPage, error)
	// ListEntityPages 列出全部页面（导包要完整，不能只导脏的）。
	ListEntityPages(ctx context.Context, ns Namespace, limit int) ([]*types.EntityPage, error)
	// MarkEntityPageDirty 标脏（晋升侧调用，不触发生成）。
	MarkEntityPageDirty(ctx context.Context, ns Namespace, entityID string, when time.Time) error
	// ApplyEntityPageRegen 生成成功：落正文、清脏、记录尝试时刻与清失败。
	ApplyEntityPageRegen(ctx context.Context, ns Namespace, entityID string, p *types.EntityPage, at time.Time) error
	// RecordEntityPageRegenFailure 生成失败：记原因、清脏、**保持脏标记**。
	RecordEntityPageRegenFailure(ctx context.Context, ns Namespace, entityID, reason string, at time.Time) error
}

// TreeStore 树投影存取。
type TreeStore interface {
	SaveNode(ctx context.Context, ns Namespace, n *types.MemoryNode) error
	GetNode(ctx context.Context, ns Namespace, id string) (*types.MemoryNode, error)
	// GetNodeByAtomID 查某事实挂在哪个叶子上（供「事实→树」反向定位）。
	GetNodeByAtomID(ctx context.Context, ns Namespace, atomID string) (*types.MemoryNode, error)
	// GetChildren parentID 为空表示取根。
	GetChildren(ctx context.Context, ns Namespace, parentID string) ([]*types.MemoryNode, error)
	GetTree(ctx context.Context, ns Namespace) ([]*types.MemoryNode, error)
	DeleteNode(ctx context.Context, ns Namespace, id string, cascade bool) error
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
	TreeStore
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

// AtomReader 是 AtomStore 的读取面小接口（召回阶段只需要这一片）。
type AtomReader interface {
	ListAtoms(ctx context.Context, ns Namespace, f AtomFilter) ([]*types.AtomCard, error)
	SearchAtoms(ctx context.Context, ns Namespace, query string, limit int) ([]*types.AtomCard, error)
}
