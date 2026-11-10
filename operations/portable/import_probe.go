package portable

import (
	"context"
	"errors"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：导入幂等的「已存在」探测（可选能力）与事务包裹。
// 从 portable.go 拆出（文件行数门禁）：导入的**读面**独立成件——
// 写面（Target）保持冻结，读面单独探测，这条边界本身就该单独一个文件。

// ExistingProbe 导入幂等的「已存在」探测面（可选能力）。
//
// Target 是已发布的写面，冻结不加方法（与 store 的契约演进规约同一条
// 纪律，见 store/optional.go）；读面单独成接口、运行时探测：
// store.Store 天然满足它，装配面（adapters/cli）传进来的就是全量存储，
// 于是默认路径自动获得「已存在 → 跳过」的幂等。
//
// 只实现了写面的薄适配（测试桩、宿主转发层）退回「直接写」：
// Save* 本身是 upsert，重导不产生重复行；journal 是纯 INSERT，
// 重导会在审计面上撞唯一键——退化路径的边界在此写明。
type ExistingProbe interface {
	GetRaw(ctx context.Context, ns store.Namespace, id string) (*types.RawEvent, error)
	GetCandidate(ctx context.Context, ns store.Namespace, id string) (*types.Candidate, error)
	GetAtom(ctx context.Context, ns store.Namespace, id string) (*types.AtomCard, error)
	GetEntity(ctx context.Context, ns store.Namespace, id string) (*types.Entity, error)
	GetEntityPage(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope) (*types.EntityPage, error)
	ListAliases(ctx context.Context, ns store.Namespace, entityID string) ([]*types.Alias, error)
	ListEpisodes(ctx context.Context, ns store.Namespace, session string, limit int) ([]*types.Episode, error)
	GetDigest(ctx context.Context, ns store.Namespace, period types.DigestPeriod, key string) (*types.DigestRecord, error)
	ListJournal(ctx context.Context, ns store.Namespace, subject string, limit int) ([]*types.JournalEntry, error)
}

// transactor 事务能力（可选：store.Store 天然满足）。
//
// 有它，整包导入包在一个事务里——「宁可一句都不导」从注释变成行为；
// 没有它，逐类落库、失败即停在半程（与从前一致）。
type transactor interface {
	Transaction(ctx context.Context, fn func(ctx context.Context, s store.Store) error) error
}

// importProbe 存在性探测：把「ErrNotFound = 不存在」这条判定收在一处。
//
// 情节与审计表没有「按标识取一条」的口子（只有列表），一次性建集合并
// 缓存——逐行走列表查询会把导入变成 N 次全表扫；别名表只有「按实体列出」，
// 按实体惰性建集合。其余四类都有精确取键，逐行探测。
type importProbe struct {
	r ExistingProbe
	// episodes / journal 的标识集合（惰性建一次）。
	episodes map[string]bool
	journal  map[string]bool
	// aliasCache 每个实体的别名字面集合（entityID → 集合，惰性建）。
	aliasCache map[string]map[string]bool
}

// exists 通用判定：err 为 nil = 已存在；ErrNotFound = 不存在；其余上抛。
func (p *importProbe) exists(_ any, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (p *importProbe) raw(ctx context.Context, ns store.Namespace, id string) (bool, error) {
	return p.exists(p.r.GetRaw(ctx, ns, id))
}

func (p *importProbe) candidate(ctx context.Context, ns store.Namespace, id string) (bool, error) {
	return p.exists(p.r.GetCandidate(ctx, ns, id))
}

func (p *importProbe) atom(ctx context.Context, ns store.Namespace, id string) (bool, error) {
	return p.exists(p.r.GetAtom(ctx, ns, id))
}

func (p *importProbe) entity(ctx context.Context, ns store.Namespace, id string) (bool, error) {
	return p.exists(p.r.GetEntity(ctx, ns, id))
}

func (p *importProbe) page(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope) (bool, error) {
	return p.exists(p.r.GetEntityPage(ctx, ns, entityID, scope))
}

func (p *importProbe) digest(ctx context.Context, ns store.Namespace, period types.DigestPeriod, key string) (bool, error) {
	return p.exists(p.r.GetDigest(ctx, ns, period, key))
}

func (p *importProbe) episode(ctx context.Context, ns store.Namespace, id string) (bool, error) {
	if p.episodes == nil {
		eps, err := p.r.ListEpisodes(ctx, ns, "", 1000000)
		if err != nil {
			return false, err
		}
		set := make(map[string]bool, len(eps))
		for _, e := range eps {
			set[e.ID] = true
		}
		p.episodes = set
	}
	return p.episodes[id], nil
}

func (p *importProbe) journalID(ctx context.Context, ns store.Namespace, id string) (bool, error) {
	if p.journal == nil {
		js, err := p.r.ListJournal(ctx, ns, "", 1000000)
		if err != nil {
			return false, err
		}
		set := make(map[string]bool, len(js))
		for _, e := range js {
			set[e.ID] = true
		}
		p.journal = set
	}
	return p.journal[id], nil
}

// alias 别名表按 (namespace, alias) 唯一；探测按「该实体名下已有的别名字面」。
// 字面判据与写路口径一致（NormalizeAlias 在两处各走一遍，不会一边归一一边不归一）。
func (p *importProbe) alias(ctx context.Context, ns store.Namespace, entityID, alias string) (bool, error) {
	if p.aliasCache == nil {
		p.aliasCache = map[string]map[string]bool{}
	}
	set, ok := p.aliasCache[entityID]
	if !ok {
		as, err := p.r.ListAliases(ctx, ns, entityID)
		if err != nil {
			return false, err
		}
		set = make(map[string]bool, len(as))
		for _, a := range as {
			set[a.Alias] = true
		}
		p.aliasCache[entityID] = set
	}
	return set[types.NormalizeAlias(alias)], nil
}
