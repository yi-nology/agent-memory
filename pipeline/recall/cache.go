package recall

import (
	"sync"
	"time"
)

// cache 召回结果 TTL 缓存。
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
type cache struct {
	ttl time.Duration
	// now 时钟。刻意注入而不是直接 time.Now()：
	// 流水线自己有可注入的时钟（p.now），缓存却用真实时间，
	// 于是「缓存过期」这条行为在测试里不可控——
	// 而它恰好是召回面最容易出「第二次问同一问题才显现」的那类问题。
	now func() time.Time
	mu  sync.Mutex
	m   map[string]entry
}

type entry struct {
	res       *Result
	expiresAt time.Time
}

func newCache(ttl time.Duration, now func() time.Time) *cache {
	if ttl <= 0 {
		return nil // nil 接收者的方法全部安全，这是 Go 的惯用法
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &cache{ttl: ttl, now: now, m: make(map[string]entry)}
}

func (c *cache) get(key string) (*Result, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if c.now().After(e.expiresAt) {
		delete(c.m, key)
		return nil, false
	}
	hit := cloneResult(e.res)
	hit.FromCache = true // 命中这件事必须对调用方可见，否则无法观测缓存是否生效
	return hit, true
}

func (c *cache) put(key string, res *Result) {
	if c == nil || res == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = entry{res: cloneResult(res), expiresAt: c.now().Add(c.ttl)}
}

// purge 清过期项。
//
// 单独导出是因为**没有它，缓存就是内存泄漏**：每次命中后 put 会覆盖同一个键，
// 但不同查询会不断新增键，只增不减。规模不大时它只是缓慢增长，
// 到了「长期运行 + 查询发散」就是真的涨。
func (c *cache) purge() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := 0
	for k, e := range c.m {
		if now.After(e.expiresAt) {
			delete(c.m, k)
			n++
		}
	}
	return n
}

// Purge 清过期项。召回层在每次调用前调一次即可，不必再引一个后台 goroutine
// （本仓的纪律是「不引入宿主看不见的常驻件」）。
func (p *Pipeline) Purge() int { return p.cache.purge() }

// cloneResult 复制一份结果再交出去。
//
// 必须复制：缓存里那份若被调用方改了（比如追加了来源标记），下次命中就是脏数据，
// 而这种 bug 只在「第二次问同一个问题」时才显现——极难复现。
func cloneResult(r *Result) *Result {
	if r == nil {
		return nil
	}
	cp := &Result{
		Snippets:           append([]Snippet(nil), r.Snippets...),
		Dropped:            r.Dropped,
		DroppedByBudget:    r.DroppedByBudget,
		DroppedByDiversity: r.DroppedByDiversity,
		DroppedByDuplicate: r.DroppedByDuplicate,
		FromCache:          r.FromCache,
		Entities:           append([]string(nil), r.Entities...),
	}
	return cp
}
