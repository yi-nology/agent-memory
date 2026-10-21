// Package application 装配面：把存储、端口与流水线装成一个宿主可调用的服务。
//
// # 位置
//
// 它是**唯一**知道「谁跟谁怎么装」的地方：adapters（bridge/cli）只跟它说话，
// 它跟 pipeline/store/ports 说话，pipeline 不知道 adapters 的存在。
//
// # 服务面只做五件事
//
//	捕获（capture）   事件 → 落库（落库前已打码）
//	提炼（extract）   事件 → 候选（要模型）
//	晋升（promote）   候选 → 事实 / 并入 / 丢弃 / 送人工
//	召回（recall）     查询 → 该进 prompt 的内容
//	检索（search）     查询 → 事实卡
//
// 这五件之外的事（导出、页面重生成、周期摘要、生命周期）不进服务面：
// 它们是**库管理动作**，由 CLI 与宿主自己触发，不该占服务面的位置。
package application

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/capture"
	"github.com/yi-nology/agent-memory/pipeline/extractor"
	"github.com/yi-nology/agent-memory/pipeline/promotion"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Config 装配配置。
type Config struct {
	// Namespace 默认记忆库。
	Namespace store.Namespace
	// Host 宿主标识（写进事件的来源字段）。
	Host string
	// Recall 召回配置。
	Recall recall.Config
}

// Service 记忆服务。
type Service struct {
	cfg   Config
	store store.Store
	cap   *capture.Capturer
	ext   *extractor.Extractor
	promo *promotion.Promoter
	rec   *recall.Pipeline
	// embedder / vecIdx 向量轨的两件（nil = 不启用）。
	embedder ports.Embedder
	vecIdx   ports.VectorIndex
	now      func() time.Time
}

// Deps 装配依赖。
type Deps struct {
	// Store 存储。
	Store store.Store
	// Redactor 脱敏器（**必需**）。
	Redactor ports.Redactor
	// LLM 模型能力（可 nil：提炼与页面重生成会显式失败，其余功能照常）。
	LLM ports.LLM
	// Bander 注入防伪（**必需**）。
	Bander ports.Bander
	// Escalation 实体归一升级通道（可 nil）。
	Escalation promotion.Escalation
	// Promotion 晋升配置（可 nil：nil = 用缺省配置）。
	//
	// 为什么要透出：事实更新判定（同一个东西换了值）需要模型，
	// 而模型是由宿主配的（端点、密钥、模型名都是宿主的事）。
	// 不透出的话，宿主只能「永远拿不到模型」——
	// 于是「智能事实更新」这一支在真实宿主里永远是死的。
	Promotion *promotion.Config
	// Estimator token 计量（可 nil：退到静态代理）。
	Estimator ports.TokenEstimator
	// Embedder 向量化器（可 nil：nil = 不启用向量面）。
	//
	// 给了它，向量轨才真正通电：召回多一路语义召回，
	// 晋升成事实后自动把向量写进索引。
	//
	// 不给 = 召回**逐条**回到纯词面（零漂移），
	// 且「没有模型也能手工写入 + 词法召回」那条降级路径不变。
	Embedder ports.Embedder
	// VectorIndex 向量索引（可 nil：nil = 不启用向量面）。
	//
	// 两者**都**给了才启用——只给一个装配不出语义路。
	VectorIndex ports.VectorIndex
}

// New 装配服务。
func New(cfg Config, deps Deps) (*Service, error) {
	if deps.Store == nil {
		return nil, errors.New("application: 存储未装配")
	}
	if deps.Bander == nil {
		return nil, errors.New("application: 必须装配 Bander：召回内容是不可信内容")
	}
	cap, err := capture.New(deps.Store, deps.Redactor)
	if err != nil {
		return nil, err
	}
	promoCfg := promotion.Config{}
	if deps.Promotion != nil {
		promoCfg = *deps.Promotion
	}
	// 存储包一层：事实退役时同步删掉它的向量。
	//
	// 放装配面而不是生命周期面：生命周期面只声明自己需要的窄接口，
	// 为了一次向量清理把索引塞进它的依赖是本末倒置；
	// 而**装饰器**让所有走装配面的退役路径都自动带上清理
	// （保留期、运行期错误批量退役、合并……）——
	// 不必指望每条路径都记得去删。
	st := deps.Store
	promo, err := promotion.New(promoStore{st}, deps.Escalation, promoCfg)
	if err != nil {
		return nil, err
	}
	// 向量索引：宿主显式给了就用宿主的；
	// 没给而**存储自己实现了向量索引**（存储层实现 ports.VectorIndex）就用存储的——
	// 那样向量轨跟着记忆库一起活（进程一退，内存索引就没了，
	// 语义召回随之失效，等于这条轨白装）。
	vecIdx := deps.VectorIndex
	if vecIdx == nil {
		if vi, ok := st.(ports.VectorIndex); ok {
			vecIdx = vi
		}
	}
	// 存储自己就是向量索引时，包一层：事实退役时同步删掉它的向量。
	if vecIdx != nil {
		if _, already := st.(vectorStore); !already {
			st = vectorStore{Store: st, idx: vecIdx}
		}
	}
	// 语义召回面：宿主显式配了 Semantic 就以宿主为准；
	// 否则在给了向量化器与索引时自动装上「内核自带的向量轨」。
	recallCfg := cfg.Recall
	if recallCfg.Semantic == nil && deps.Embedder != nil && vecIdx != nil {
		recallCfg.Semantic = ports.VectorSemanticReader{
			Embedder: deps.Embedder,
			Index:    vecIdx,
		}
	}
	rec, err := recall.New(recallCfg, st, deps.Estimator, deps.Bander)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg:      cfg,
		store:    st,
		cap:      cap,
		ext:      extractor.New(deps.LLM),
		promo:    promo,
		rec:      rec,
		embedder: deps.Embedder,
		vecIdx:   vecIdx,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// CaptureInput 捕获输入。
type CaptureInput = capture.Input

// Capture 捕获一条事件。
func (s *Service) Capture(ctx context.Context, in CaptureInput) (*capture.Outcome, error) {
	if in.Namespace == "" {
		in.Namespace = s.cfg.Namespace
	}
	if in.Host == "" {
		in.Host = s.cfg.Host
	}
	return s.cap.Capture(ctx, in)
}

// Extract 从事件里抽候选。
func (s *Service) Extract(ctx context.Context, ns store.Namespace, sessionID string,
	events []*types.RawEvent) ([]*types.Candidate, error) {
	if ns == "" {
		ns = s.cfg.Namespace
	}
	return s.ext.Extract(ctx, ns, sessionID, events)
}

// Promote 判定一条候选。
//
// 晋升成功后**顺带把该实体的画像标脏**：事实卡是**真源**，画像是它的摘要，
// 事实变了摘要就该重算。少了这一步，画像永远停在「晋升之前」的那一版，
// 而宿主侧的 `memory.page.regen` 只重生成**脏**页面——于是它会安静地什么都不做。
func (s *Service) Promote(ctx context.Context, ns store.Namespace, c *types.Candidate) (*promotion.Verdict, error) {
	if ns == "" {
		ns = s.cfg.Namespace
	}
	v, err := s.promo.Promote(ctx, ns, c)
	if err != nil {
		return nil, err
	}
	s.markPageDirty(ctx, ns, v)
	// 向量轨：新事实入索引，被取代的旧事实出索引。
	//
	// 顺序要紧：先写新的、再删旧的——反过来会有一瞬间「两条都不在索引里」
	// （召回少一条），而先写后删最多是「两条都在」（多一条会被取回时过滤掉）。
	if v.Decision == promotion.DecisionPromote || v.Decision == promotion.DecisionUpdate {
		if v.Atom != nil {
			s.indexVector(ctx, ns, v.Atom)
		}
	}
	if v.Superseded != nil {
		s.unindexVector(ctx, ns, v.Superseded.ID)
	}
	return v, nil
}

// markPageDirty 晋升成事实后标脏该实体的画像。
//
// # 为什么在装配面做，而不是在晋升面
//
// 晋升面只声明自己需要的窄接口（阶段只声明自己需要的子接口）。
// 为了一次标脏把整张页面表塞进晋升面的依赖，是**为了一个派生标记**扩大依赖面。
// 装配面本来就看得见全量存储，故这一步落在它这里。
//
// # 只在「晋升成新事实」时标
//
// 并入（merge）只补证据不改断言，画像的正文由断言决定，故并入不必标脏——
// 标了就是白花一次模型调用去重算一份一模一样的摘要。
//
// # 标脏失败**不阻断晋升**
//
// 事实卡是真源，画像是派生物。让一次派生标记的失败把一条真源记忆挡在门外，
// 是本末倒置。但失败必须**留痕**：画像会停在旧版本，
// 而「页面存在且是旧的」与「页面正常」在看板上长得一模一样——
// 没有留痕就没人知道该去重算它。
func (s *Service) markPageDirty(ctx context.Context, ns store.Namespace, v *promotion.Verdict) {
	if v == nil || v.Decision != promotion.DecisionPromote || v.Atom == nil || v.EntityID == "" {
		return
	}
	now := s.now()
	if err := s.store.MarkEntityPageDirty(ctx, ns, v.EntityID, v.Atom.Scope, now); err != nil {
		// 留痕失败本身也只记一次：审计条目写不进去时，存储已经不可靠了，
		// 这时再抛错会把「标脏失败」变成「晋升失败」，那是更糟的失真。
		_ = s.store.AppendJournal(ctx, ns, &types.JournalEntry{
			ID: "journal-page-dirty-" + v.Atom.ID, At: now,
			Kind: types.JournalPageDirty, Subject: v.Atom.ID, Actor: v.Actor,
			Summary: "事实已晋升，但实体画像标脏失败：" + err.Error(),
		})
	}
}

// Recall 召回。
func (s *Service) Recall(ctx context.Context, req recall.Request) (*recall.Result, error) {
	if req.Namespace == "" {
		req.Namespace = s.cfg.Namespace
	}
	return s.rec.Recall(ctx, req)
}

// Search 检索事实卡。
//
// 带过滤条件（而不是只带 limit）：归属过滤必须在**取数层**生效，
// 交给调用方后置过滤的话，被别家占掉的 TopK 配额已经花掉了。
// 条件里最要紧的是 AtomFilter.Scope（读者归属，空=不隔离）。
func (s *Service) Search(ctx context.Context, ns store.Namespace, query string,
	f store.AtomFilter) ([]*types.AtomCard, error) {
	if ns == "" {
		ns = s.cfg.Namespace
	}
	return s.store.SearchAtoms(ctx, ns, query, f)
}

// Get 取一条事实卡。
func (s *Service) Get(ctx context.Context, ns store.Namespace, id string) (*types.AtomCard, error) {
	if ns == "" {
		ns = s.cfg.Namespace
	}
	return s.store.GetAtom(ctx, ns, id)
}

// Stats 取计数。
func (s *Service) Stats(ctx context.Context, ns store.Namespace) (store.Stats, error) {
	if ns == "" {
		ns = s.cfg.Namespace
	}
	return s.store.CountStats(ctx, ns)
}

// Store 暴露存储（供库管理动作使用：导出、页面重生成、生命周期）。
//
// 返回的是**全量接口**：这条口子是给同仓的库管理动作用的，不是给外部调用方
// 用的。外部调用方走 bridge/CLI 的白名单方法。
func (s *Service) Store() store.Store { return s.store }

// Namespace 取默认记忆库。
func (s *Service) Namespace() store.Namespace { return s.cfg.Namespace }
