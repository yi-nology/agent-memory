package recall

import (
	"context"
	"time"

	"github.com/oekit/pkg/cache"

	"github.com/yi-nology/agent-memory/ports"
)

// 召回结果 TTL 缓存。
//
// # 为什么值得有
//
// 同一会话里用户会反复问同一个问题（「那台呢」「再看看它」）。召回要打库、
// 要跑重排，而**结果在 TTL 内是稳定的**——记忆只在显式写入时变。
// 没有缓存时，每一次「再看看」都是一次全量扫描。
//
// # 但它必须可关
//
// Config.CacheTTL = 0 即关闭。理由：缓存引入的是**陈旧面**——记忆刚写进去，
// 同一问题在 TTL 内仍会拿到旧答案。写入路径要能立刻失效，见 CacheTTL 字段注释。
//
// # 实现：缓存本体委派 oekit/pkg/cache
//
// TTL 判定、过期条目的惰性删除、显式清扫与读命中拷贝都只有一份实现，
// 不在本包再手写一遍 map+TTL。
//
// 本包保留的是**领域判断**：结果副本的逐字段深拷贝（cloneResult）——
// 那是本仓对 Result 结构的纪律（新增字段必须一并复制），不是通用缓存语义，
// 换成通用深拷贝会把这个纪律弄丢。
//
// 语义与旧的手搓实现逐条对齐：
//   - TTL 从**写入**时起算，读命中不续期；
//   - 过期判定与旧实现同口径（now.After(expiresAt)）；
//   - 读命中返回副本、**写入时也存副本**（调用方改写返回值不污染缓存里那份）；
//   - 过期条目由读路径惰性删除 + 调用方显式 Purge 清扫——**不起后台协程**
//     （本仓纪律：不引入宿主看不见的常驻件）。

// newRecallCache 构造召回缓存。ttl<=0 返回 nil = 缓存关闭（调用点判 nil 即禁用）。
//
// now 时钟刻意注入而不是直接 time.Now()：流水线自己有可注入的时钟（p.now），
// 缓存却用真实时间，于是「缓存过期」这条行为在测试里不可控——
// 而它恰好是召回面最容易出「第二次问同一问题才显现」的那类问题。
func newRecallCache(ttl time.Duration, now func() time.Time) *cache.LocalCache[Result] {
	if ttl <= 0 {
		return nil
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return cache.NewLocalCache(cache.LocalConfig[Result]{
		TTL: ttl,
		Now: now,
		// 不起后台清扫协程：过期条目由惰性删除 + 调用方显式 Purge 回收。
		NoBackgroundSweep: true,
		// 读命中返回副本（cloneResult 是本仓的逐字段深拷贝纪律）。
		CloneOnRead: cloneResult,
	})
}

// Purge 清过期项。召回层在每次调用前调一次即可，不必再引一个后台 goroutine
// （本仓的纪律是「不引入宿主看不见的常驻件」）。
func (p *Pipeline) Purge() int {
	if p.cache == nil {
		return 0
	}
	return p.cache.Sweep(context.Background())
}

// cloneResult 复制一份结果再交出去。
//
// 必须复制：缓存里那份若被调用方改了（比如追加了来源标记），下次命中就是脏数据，
// 而这种 bug 只在「第二次问同一个问题」时才显现——极难复现。
//
// ⚠️ **新增字段必须一并复制**：漏掉一个 map，缓存命中就会返回 nil
// （「第一次有、第二次没了」——正好是这条注释说的那种难复现的形态）。
// 故这里用「逐字段手抄」是刻意的：抄一遍时新字段会**显眼地缺一块**，
// 而「遍历反射复制」不会——它把漏抄变成静默。
//
// ⚠️ 但「显眼」不等于「编译器会拦」：Go 的结构体字面量**不要求**写全字段，
// 漏抄一个字段编译照过（所以下面那句「加字段时编译器会指出来」是**错的**，
// 已改）。真正的兜底是 TestCloneResultCopiesEveryField：
// 它用反射逐字段比对「源」与「副本」，漏一个字段立刻红。
func cloneResult(r *Result) *Result {
	if r == nil {
		return nil
	}
	cp := &Result{
		Snippets:               append([]Snippet(nil), r.Snippets...),
		Dropped:                r.Dropped,
		DroppedByBudget:        r.DroppedByBudget,
		DroppedByDiversity:     r.DroppedByDiversity,
		DroppedByDuplicate:     r.DroppedByDuplicate,
		Drops:                  copyDropReasons(r.Drops),
		Buckets:                copyBuckets(r.Buckets),
		FromCache:              r.FromCache,
		Entities:               append([]string(nil), r.Entities...),
		LooseSearchUnavailable: r.LooseSearchUnavailable,
	}
	return cp
}

func copyDropReasons(m map[ports.DropReason]int) map[ports.DropReason]int {
	if m == nil {
		return nil
	}
	out := make(map[ports.DropReason]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyBuckets(m map[ports.BucketID]int) map[ports.BucketID]int {
	if m == nil {
		return nil
	}
	out := make(map[ports.BucketID]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
