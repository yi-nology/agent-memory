// Package entity 归一面：把同一个对象的不同叫法收敛到一张实体卡上。
//
// # 为什么归一必须在记忆内核这一层
//
// 没有归一，「10.44.56.107」「那台研发机」「web-03」会各自长出一份事实。
// 召回时它们互相稀释——每个名字都命中一点，合起来谁也不显著；而用户
// 完全不知道自己「换了个说法」就没被认出来。
//
// 宿主当然可以做归一，但**它做不了**：宿主认识自己那套对象标识，记忆内核
// 认识的是「实体」这个抽象。让内核持有宿主的对象词表，等于把领域词抄进来
// （门禁第一条就拦）。故本包提供的是**机制**：按名字找实体、把新叫法登记成
// 别名、必要时把两个实体合并。词表与判定标准归宿主。
package entity

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Store 归一面用到的存储。
type Store interface {
	FindEntityByName(ctx context.Context, ns store.Namespace, name string) (*types.Entity, error)
	GetEntity(ctx context.Context, ns store.Namespace, id string) (*types.Entity, error)
	ListEntities(ctx context.Context, ns store.Namespace, entityType types.EntityType, limit int) ([]*types.Entity, error)
	SaveEntity(ctx context.Context, ns store.Namespace, e *types.Entity) error
	SaveAlias(ctx context.Context, ns store.Namespace, a *types.Alias) error
	ListAliases(ctx context.Context, ns store.Namespace, entityID string) ([]*types.Alias, error)
	BumpEntityAtomCount(ctx context.Context, ns store.Namespace, entityID string, delta int) error
	// ListAtomsByEntity 取该实体下的事实卡（合并时要迁移）。
	ListAtomsByEntity(ctx context.Context, ns store.Namespace, entityID string) ([]*types.AtomCard, error)
	// SaveAtom 写回事实卡（合并时要改归属）。
	SaveAtom(ctx context.Context, ns store.Namespace, a *types.AtomCard) error
}

// ErrNotFound 目标不存在。
var ErrNotFound = store.ErrNotFound

// Resolver 归一器。
type Resolver struct {
	store Store
	now   func() time.Time
}

// New 造归一器。
func New(s Store) (*Resolver, error) {
	if s == nil {
		return nil, errors.New("entity: 存储未装配")
	}
	return &Resolver{store: s, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Resolve 按名字查实体；查不到时**不新建**。
//
// 不新建是刻意的：新建意味着「我认为这个叫法指向一个新对象」，而内核手里
// 只有一条断言，没有上下文。新建错了的后果是同一对象裂成两个（更糟：
// 两个都像对的，事实被劈成两半）。
// 要新建由宿主显式调 Register。
func (r *Resolver) Resolve(ctx context.Context, ns store.Namespace, name string) (*types.Entity, error) {
	n := types.NormalizeAlias(name)
	if n == "" {
		return nil, ErrNotFound
	}
	return r.store.FindEntityByName(ctx, ns, n)
}

// Register 登记实体与它的别名。
//
// 幂等：同名同实体重复登记不报错（同一条事实被提炼两次是常态）。
// 同名**不同**实体则返回冲突——那说明两个对象抢同一个名字，
// 内核判不出谁对，交给宿主。
func (r *Resolver) Register(ctx context.Context, ns store.Namespace, name string,
	entityType types.EntityType, alias string, by types.DecidedBy) (*types.Entity, error) {

	norm := types.NormalizeAlias(name)
	if norm == "" {
		return nil, errors.New("entity: 实体名不能为空")
	}
	existing, err := r.store.FindEntityByName(ctx, ns, norm)
	if err == nil && existing != nil {
		if alias != "" {
			if err := r.registerAlias(ctx, ns, existing, alias, by); err != nil {
				return nil, err
			}
		}
		return existing, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	now := r.now()
	ent := &types.Entity{
		ID:            "ent-" + norm,
		EntityType:    entityType,
		CanonicalName: norm,
		CreatedAt:     now,
	}
	if err := r.store.SaveEntity(ctx, ns, ent); err != nil {
		return nil, err
	}
	if alias != "" {
		if err := r.registerAlias(ctx, ns, ent, alias, by); err != nil {
			return nil, err
		}
	}
	return ent, nil
}

// registerAlias 登记别名。
//
// 别名归一后若与**别的**实体的规范名撞上，那是真冲突：两个对象共用一个名字，
// 查谁都会被检索到。返回冲突而不是默默覆盖——默默覆盖会让其中一个对象
// 从此「查不到」，而它并没有消失。
func (r *Resolver) registerAlias(ctx context.Context, ns store.Namespace,
	e *types.Entity, alias string, by types.DecidedBy) error {

	norm := types.NormalizeAlias(alias)
	if norm == "" || norm == types.NormalizeAlias(e.CanonicalName) {
		return nil // 与规范名同义，登记了也是多余
	}
	other, err := r.store.FindEntityByName(ctx, ns, norm)
	if err == nil && other != nil && other.ID != e.ID {
		return &ConflictError{Alias: norm, EntityID: e.ID, OtherID: other.ID}
	}
	return r.store.SaveAlias(ctx, ns, &types.Alias{
		Alias: norm, EntityID: e.ID, EntityType: e.EntityType,
		CreatedBy: by, CreatedAt: r.now(),
	})
}

// ConflictError 别名撞上别的实体。
type ConflictError struct {
	Alias    string
	EntityID string
	OtherID  string
}

func (e *ConflictError) Error() string {
	return "entity: 别名 " + e.Alias + " 已属于另一个实体 " + e.OtherID + "（本次想登记到 " + e.EntityID + "）"
}

// MergeResult 合并结果。
type MergeResult struct {
	// Target 保留下来的实体。
	Target *types.Entity
	// Merged 被并进来的实体。
	Merged *types.Entity
	// MovedAtoms 迁移过去的事实卡数。
	MovedAtoms int
	// MovedAliases 迁移过去的别名数。
	MovedAliases int
}

// Merge 把 from 并进 to。
//
// 方向是**显式**的：内核不判断「谁才是真的那个」——那是宿主的领域知识。
// 内核只做机械迁移：事实卡改归属、别名改指向、计数相加、from 退役。
//
// 退役而非删除：被并进来的实体曾是真的，删掉它就无从查证「当初为什么分成两个」。
func (r *Resolver) Merge(ctx context.Context, ns store.Namespace, toID, fromID string) (*MergeResult, error) {
	if toID == fromID {
		return nil, errors.New("entity: 合并双方不能相同")
	}
	to, err := r.store.GetEntity(ctx, ns, toID)
	if err != nil {
		return nil, err
	}
	from, err := r.store.GetEntity(ctx, ns, fromID)
	if err != nil {
		return nil, err
	}

	res := &MergeResult{Target: to, Merged: from}

	atoms, err := r.store.ListAtomsByEntity(ctx, ns, fromID)
	if err != nil {
		return nil, err
	}
	for _, a := range atoms {
		a.EntityID = toID
		a.UpdatedAt = r.now()
		if err := r.store.SaveAtom(ctx, ns, a); err != nil {
			return nil, err
		}
	}
	res.MovedAtoms = len(atoms)

	aliases, err := r.store.ListAliases(ctx, ns, fromID)
	if err != nil {
		return nil, err
	}
	for _, al := range aliases {
		al.EntityID = toID
		if err := r.store.SaveAlias(ctx, ns, al); err != nil {
			return nil, err
		}
	}
	res.MovedAliases = len(aliases)

	// 计数相加（两边各自可能有偏差，取相加而不是重算——重算要遍历全部事实，
	// 而事实的归属可能还有正在迁移的，边加边算会算出中间的数）。
	if err := r.store.BumpEntityAtomCount(ctx, ns, toID, len(atoms)); err != nil {
		return nil, err
	}
	if err := r.store.BumpEntityAtomCount(ctx, ns, fromID, -len(atoms)); err != nil {
		return nil, err
	}

	// from 退役：清空计数、留一条痕迹。
	from.AtomCount = 0
	from.LastPromotedAt = time.Time{}
	if err := r.store.SaveEntity(ctx, ns, from); err != nil {
		return nil, err
	}
	return res, nil
}

// NameOf 取实体的展示名：规范名优先，其次第一个别名。
func NameOf(e *types.Entity) string {
	if e == nil {
		return ""
	}
	if n := strings.TrimSpace(e.CanonicalName); n != "" {
		return n
	}
	if len(e.Aliases) > 0 {
		return e.Aliases[0]
	}
	return e.ID
}
