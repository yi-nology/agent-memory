// Package lifecycle 记忆库的库管理动作：回收、合并、保留期、压缩。
//
// # 一条贯穿全包的纪律：**只软退役，不硬删事实**
//
// 事实卡一旦入库就代表「这件事发生过」。被推翻的事实**曾经是真的**，
// 硬删掉它就无从查证「当初为什么这么判」——而那是记忆系统必须能回答的问题。
// 故：退役是标记，清理只发生在**有明确保留期**的素材面（原始事件）上。
//
// # 四个动作的触发面
//
//	回收 GC        原始事件按龄清理 + 孤儿索引清理
//	合并 Consolidate 同签名事实合并（事实面是去重，不是硬删）
//	保留期 Retention 运行期错误来源的事实批量退役
//	压缩 Vacuum    后端专属的物理整理（SQLite 的 VACUUM）
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oekit/pkg/container"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Store 生命周期用到的存储。
type Store interface {
	DeleteRawBefore(ctx context.Context, ns store.Namespace, before time.Time) (int, error)
	GetRaw(ctx context.Context, ns store.Namespace, id string) (*types.RawEvent, error)
	ListAtoms(ctx context.Context, ns store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error)
	SaveAtom(ctx context.Context, ns store.Namespace, a *types.AtomCard) error
	SupersedeAtom(ctx context.Context, ns store.Namespace, id, supersededBy string) error
	DeprecateAtom(ctx context.Context, ns store.Namespace, id string, at time.Time) error
	ListEntities(ctx context.Context, ns store.Namespace, entityType types.EntityType, limit int) ([]*types.Entity, error)
	AppendJournal(ctx context.Context, ns store.Namespace, e *types.JournalEntry) error
}

// Option 生命周期动作的可选项。
type Option func(*opts)

type opts struct {
	now func() time.Time
}

// WithClock 注入时钟（测试用）。
//
// 为什么要注入：退役时刻与审计条目的时刻是**可断言的事实**——
// 「这条事实是什么时候被退役的」是审计链上的一环。
// 时钟写死成 time.Now() 时，测试只能断言「大概刚刚」，
// 而「大概」在断言里等于「没断言」。
//
// 形参是 variadic，故既有调用点一行不动。
func WithClock(now func() time.Time) Option {
	return func(o *opts) {
		if now != nil {
			o.now = now
		}
	}
}

func newOpts(opt []Option) opts {
	o := opts{now: func() time.Time { return time.Now().UTC() }}
	for _, f := range opt {
		if f != nil {
			f(&o)
		}
	}
	return o
}

// ── 回收 ──

// GCResult 回收结果。
type GCResult struct {
	// RawDeleted 删除的原始事件数。
	RawDeleted int
	// AtomsRetired 顺带退役的事实数（证据全部被删的）。
	AtomsRetired int
}

// GC 回收原始事件。
//
// # 顺带退役「证据全没了」的事实
//
// 一条事实的全部证据都被保留期清掉之后，它就成了一条**无法回溯**的断言——
// 而无法回溯的记忆既不能审计也不能推翻（types.AtomCard 的不变量 1）。
// 故 GC 顺带把这类事实退役：不是删，是标记为「证据已过期」。
func GC(ctx context.Context, s Store, ns store.Namespace, before time.Time, opt ...Option) (*GCResult, error) {
	o := newOpts(opt)
	if s == nil {
		return nil, errors.New("lifecycle: 存储未装配")
	}
	n, err := s.DeleteRawBefore(ctx, ns, before)
	if err != nil {
		return nil, err
	}
	res := &GCResult{RawDeleted: n}

	// 找出证据全被清掉的事实。
	atoms, err := s.ListAtoms(ctx, ns, store.AtomFilter{ActiveOnly: true, Limit: 100000})
	if err != nil {
		return res, err
	}
	for _, a := range atoms {
		if len(a.RawEventIDs) == 0 {
			continue
		}
		// 逐条查证据是否还在。代价高，但保留期本就是低频动作，
		// 而这里查错的后果是「把还能查证的事实误判成查不了」——
		// 那比多查几次糟糕得多。
		allGone := true
		for _, id := range a.RawEventIDs {
			if _, err := s.GetRaw(ctx, ns, id); err == nil {
				allGone = false
				break
			} else if !errors.Is(err, store.ErrNotFound) {
				// 查不动 ≠ 查不到。存储故障时**不动**这条事实。
				allGone = false
				break
			}
		}
		if !allGone {
			continue
		}
		if err := s.DeprecateAtom(ctx, ns, a.ID, o.now()); err != nil {
			return res, err
		}
		res.AtomsRetired++
		_ = s.AppendJournal(ctx, ns, &types.JournalEntry{
			ID: "journal-gc-" + a.ID, At: o.now(),
			Kind: types.JournalRetire, Subject: a.ID, Actor: types.DecidedRule,
			Summary: "证据已过保留期，事实退役",
		})
	}
	return res, nil
}

// ── 合并 ──

// ConsolidateResult 合并结果。
type ConsolidateResult struct {
	// Merged 合并掉的事实数（被继任者取代的那些）。
	Merged int
	// EvidenceMoved 迁移的证据条数。
	EvidenceMoved int
}

// Consolidate 合并同签名的事实。
//
// # 合并而不是删
//
// 同一件事被反复记下时，事实面上会有多张同签名的卡。合并把它们归到一张，
// 其余的**软退役**（SupersededBy 指向保留下来的那张），证据全部迁过去。
// 退役而非删除的理由同包注释：被合并掉的那张卡曾经是真的。
func Consolidate(ctx context.Context, s Store, ns store.Namespace, opt ...Option) (*ConsolidateResult, error) {
	o := newOpts(opt)
	if s == nil {
		return nil, errors.New("lifecycle: 存储未装配")
	}
	atoms, err := s.ListAtoms(ctx, ns, store.AtomFilter{ActiveOnly: true, Limit: 100000})
	if err != nil {
		return nil, err
	}
	bySig := map[string][]*types.AtomCard{}
	for _, a := range atoms {
		bySig[a.Signature()] = append(bySig[a.Signature()], a)
	}

	res := &ConsolidateResult{}
	for sig, group := range bySig {
		if len(group) < 2 {
			continue
		}
		// 保留「最早建的那张」：它的证据通常最全，且被引用过（页面、树），
		// 换掉它会让那些引用指向一个不存在的卡。
		keep := group[0]
		for _, a := range group[1:] {
			// 创建时刻相同时按 ID 决胜：**必须有一个确定的决胜条件**。
			// 「谁更早」在两张卡同一刻创建时是并列的，而并列时若不决胜，
			// 合并结果就会随遍历顺序变——同一批数据合并两次得到不同结果。
			if a.CreatedAt.Before(keep.CreatedAt) ||
				(a.CreatedAt.Equal(keep.CreatedAt) && a.ID < keep.ID) {
				keep = a
			}
		}
		for _, a := range group {
			if a.ID == keep.ID {
				continue
			}
			merged := append(append([]string{}, keep.RawEventIDs...), a.RawEventIDs...)
			keep.RawEventIDs = dedup(merged)
			keep.UpdatedAt = o.now()
			if err := s.SaveAtom(ctx, ns, keep); err != nil {
				return res, err
			}
			if err := s.SupersedeAtom(ctx, ns, a.ID, keep.ID); err != nil {
				return res, err
			}
			res.Merged++
			res.EvidenceMoved += len(a.RawEventIDs)
			_ = s.AppendJournal(ctx, ns, &types.JournalEntry{
				ID: "journal-consolidate-" + a.ID, At: o.now(),
				Kind: types.JournalSupersede, Subject: a.ID, Actor: types.DecidedRule,
				Summary: "同签名事实合并到 " + keep.ID + "（签名 " + short(sig) + "）",
			})
		}
	}
	return res, nil
}

// ── 保留期 ──

// RetentionResult 保留期处理结果。
type RetentionResult struct {
	// Retired 退役的事实数。
	Retired int
	// CandidatesRetired 退役的候选数。
	CandidatesRetired int
}

// Retention 退役「证据全部来自运行期错误」的事实。
//
// # 判据是**结构化的来源**，不是关键词
//
// 上游 octop-memory 的做法是用 `payload.octop_runtime_error=true` 标运行期错误；
// 本仓把它做成了**类型**（types.Provenance.RuntimeError），所以这里读的是字段，
// 不解析字符串。
//
// # 为什么只退役「证据全部」的
//
// 一条事实若混着正常证据与运行期错误证据，**不动它**：正常那部分证据是真的，
// 整条退役会误伤。只在「全部证据都标了运行期错误」时才退役——
// 这与上游的措辞一致（「retire only automatic atoms whose evidence is entirely
// marked as runtime failures」）。
func Retention(ctx context.Context, s Store, ns store.Namespace, opt ...Option) (*RetentionResult, error) {
	o := newOpts(opt)
	if s == nil {
		return nil, errors.New("lifecycle: 存储未装配")
	}
	atoms, err := s.ListAtoms(ctx, ns, store.AtomFilter{ActiveOnly: true, Limit: 100000})
	if err != nil {
		return nil, err
	}
	res := &RetentionResult{}
	for _, a := range atoms {
		if len(a.RawEventIDs) == 0 {
			continue
		}
		// 判据是「**全部**证据都来自运行期错误」，不是事实卡自己的那个标记。
		//
		// 这两者不等价：事实卡的来源凭证取自它的证据事件（见 promotion 落库），
		// 但证据**后来被换掉/补上**时，事实卡自己的标记就过时了。
		// 按文档说的「证据全部」判，才与上游的口径一致
		// （「retire only automatic atoms whose evidence is entirely marked
		// as runtime failures」）。
		allRuntime := true
		for _, id := range a.RawEventIDs {
			ev, err := s.GetRaw(ctx, ns, id)
			if err != nil {
				// 查不动 ≠ 判定：存储故障时**不动**这条事实。
				allRuntime = false
				break
			}
			if !ev.Provenance.RuntimeError {
				allRuntime = false
				break
			}
		}
		if !allRuntime {
			continue
		}
		if err := s.DeprecateAtom(ctx, ns, a.ID, o.now()); err != nil {
			return res, err
		}
		res.Retired++
		_ = s.AppendJournal(ctx, ns, &types.JournalEntry{
			ID: "journal-retention-" + a.ID, At: o.now(),
			Kind: types.JournalRetire, Subject: a.ID, Actor: types.DecidedRule,
			Summary: "证据全部为运行期错误，事实退役",
		})
	}
	return res, nil
}

// ── 压缩 ──

// Vacuumer 后端专属的物理整理能力（可选）。
//
// 它**不**在 Store 接口里：VACUUM 是 SQLite 的概念，PostgreSQL 没有对应物。
// 把它做成可选接口而不是塞进 Store，是为了让「本仓支持多个后端」这件事
// 保持为真——每个后端实现自己该实现的那部分。
type Vacuumer interface {
	Vacuum(ctx context.Context) error
}

// Vacuum 执行物理整理；后端不支持时返回明确的「不支持」而不是静默跳过。
//
// 参数是接口而不是 any：类型断言的责任应该在调用方，
// 而调用方本来就知道自己拿的是哪个后端。
func Vacuum(ctx context.Context, v Vacuumer) error {
	if v == nil {
		return fmt.Errorf("lifecycle: 该后端不支持物理整理")
	}
	return v.Vacuum(ctx)
}

// dedup 保序去重。
//
// 与 pipeline/promotion 的同名件是两份实现而非一处共享：两者分处平级包
// （lifecycle 与 pipeline/promotion 之间不互相依赖），而合并它们需要先给
// 这 10 行找一个「家」——而每个候选的家（types 是零依赖的地基、store 是
// 持久化契约）都比这 10 行本身更不搭。故各留一份，语义保持一致。
//
// 保序是刻意的：证据列表的顺序承载「先发生的在前」，丢掉顺序就等于
// 让审计读不出因果。
func dedup(in []string) []string {
	seen := container.NewSet[string]()
	out := in[:0:0]
	for _, s := range in {
		if seen.Contains(s) {
			continue
		}
		seen.Add(s)
		out = append(out, s)
	}
	return out
}

func short(s string) string {
	if len(s) <= 16 {
		return s
	}
	return strings.ReplaceAll(s[:16], "\x00", "|") + "…"
}
