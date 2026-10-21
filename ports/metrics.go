package ports

import (
	"sort"
	"sync"
	"time"
)

// 本文件：可观测性端口。
//
// # 为什么是端口而不是内核自己打点
//
// 「P50/P95、token 消耗、缓存命中、丢弃原因」这些数字的**消费方是宿主**
// （它要接自己的监控、日志、看板），而**采集方是内核**。
// 内核若自己实现一套统计再暴露出去，宿主就得再写一遍「把内核的统计转成我的监控」，
// 而那份转换正是最容易出错的一层（口径对不上，数字看起来却合理）。
//
// 故这里只声明**采集面**：内核负责**按统一口径**打点，
// 宿主负责把它送到自己的系统。
//
// # 口径由内核定，不由宿主定
//
// 同一个指标在两处口径不同（比如「耗时」一处含队列等待、一处不含），
// 那两个数字就不可比——而不可比的指标比没有指标更坏：
// 它会让人得出「这个版本变慢了」的结论，而那是假的。
// 故指标名与含义钉在端口的注释里，宿主只负责搬运。

// Metrics 指标采集面。
type Metrics interface {
	// Observe 记一次耗时观测（同名指标聚合在一起）。
	Observe(op string, d time.Duration)
	// Incr 计数加 n（n<=0 忽略）。
	Incr(op string, n int)
	// Gauge 记一个瞬时值（覆盖式，不是累加）。
	Gauge(op string, v float64)
}

// NoopMetrics 未装配指标面时的空实现。
//
// 与 NoopSemanticReader 同一判断：指标是**可选**的，
// 「没装看板」是正常配置而不是故障，故不报错。
type NoopMetrics struct{}

func (NoopMetrics) Observe(string, time.Duration) {}
func (NoopMetrics) Incr(string, int)              {}
func (NoopMetrics) Gauge(string, float64)         {}

// MemMetrics 内存实现（纯标准库）。
//
// # 它为什么自带 P50/P95
//
// 百分位要**保留样本**（分桶只能给近似值），
// 而保留样本意味着内存随调用次数增长。
// 故它只适合**短窗口**（进程内自用的看板、测试里的断言）；
// 生产环境的长窗口指标该由宿主注入自己的实现（Prometheus 客户端之类）。
//
// # 并发安全
//
// 召回是并发的（多个会话同时召回），而打点也在并发路径上，
// 故内部加锁。锁的范围只包住 map 操作，别包住别的。
type MemMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
	gauges map[string]float64
	durs   map[string][]time.Duration
}

// NewMemMetrics 造一个内存指标面。
func NewMemMetrics() *MemMetrics {
	return &MemMetrics{
		counts: map[string]int64{},
		gauges: map[string]float64{},
		durs:   map[string][]time.Duration{},
	}
}

// Observe 记一次耗时观测。
func (m *MemMetrics) Observe(op string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durs[op] = append(m.durs[op], d)
}

// Incr 计数加 n。
func (m *MemMetrics) Incr(op string, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[op] += int64(n)
}

// Gauge 记一个瞬时值。
func (m *MemMetrics) Gauge(op string, v float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges[op] = v
}

// MetricsSnapshot 指标快照。
type MetricsSnapshot struct {
	// Counts 计数（累计）。
	Counts map[string]int64
	// Gauges 瞬时值。
	Gauges map[string]float64
	// Durations 各指标的耗时统计。
	Durations map[string]DurationStats
}

// DurationStats 一组耗时的统计。
type DurationStats struct {
	// Count 样本数。
	Count int
	// P50 中位数。
	P50 time.Duration
	// P95 95 分位。
	P95 time.Duration
	// Max 最大值。
	Max time.Duration
	// Total 总耗时。
	Total time.Duration
}

// Snapshot 取快照（副本，调用方改了不影响原表）。
func (m *MemMetrics) Snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := MetricsSnapshot{
		Counts:    make(map[string]int64, len(m.counts)),
		Gauges:    make(map[string]float64, len(m.gauges)),
		Durations: make(map[string]DurationStats, len(m.durs)),
	}
	for k, v := range m.counts {
		out.Counts[k] = v
	}
	for k, v := range m.gauges {
		out.Gauges[k] = v
	}
	for k, samples := range m.durs {
		out.Durations[k] = summarize(samples)
	}
	return out
}

// summarize 算一组耗时的统计。
//
// 百分位用**最近秩**法（取第 ceil(p×n) 个样本）而不是插值：
// 插值出来的 p95 是一个**不存在的**时长（样本里没有那个值），
// 而看板上的数字看着像实测值——故取实测样本，别造数。
func summarize(samples []time.Duration) DurationStats {
	if len(samples) == 0 {
		return DurationStats{}
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return DurationStats{
		Count: len(sorted),
		P50:   percentile(sorted, 0.50),
		P95:   percentile(sorted, 0.95),
		Max:   sorted[len(sorted)-1],
		Total: total,
	}
}

// percentile 最近秩分位。
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}
