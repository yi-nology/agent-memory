package types

import "time"

// Episode 一段情节：一次会话/一件事的经过摘要。
//
// # 它与事实卡是两条独立的路
//
// 事实卡回答「**是什么**」（这台机器的磁盘长期就偏小），
// 情节回答「**发生了什么**」（上周三那次故障我们怎么定位的）。
// 两者都从 RawEvent 出发，但一条走晋升、一条走摘要，互不依赖——
// 没有情节一样能记住事实，反之亦然。
type Episode struct {
	// ID 情节标识。
	ID string
	// SessionID 来源会话。
	SessionID string
	// Title 标题。
	Title string
	// Summary 经过摘要。
	Summary string
	// Decisions 过程中定下的决策（原文留存，便于复盘「为什么」）。
	Decisions []string
	// Emotions 情绪标注（宿主可选填；空表示宿主不采集）。
	Emotions []string
	// OccurredAt 发生时刻。
	OccurredAt time.Time
	// CreatedAt 入库时刻。
	CreatedAt time.Time
	// RawEventIDs 素材来源事件。
	RawEventIDs []string
}

// DigestPeriod 摘要周期面。
type DigestPeriod string

const (
	// PeriodDay 日摘要。
	PeriodDay DigestPeriod = "day"
	// PeriodWeek 周摘要（ISO 周）。
	PeriodWeek DigestPeriod = "week"
	// PeriodMonth 月摘要。
	PeriodMonth DigestPeriod = "month"
)

// DigestRecord 一条周期摘要：由若干情节聚合而成。
type DigestRecord struct {
	// Period 周期面。
	Period DigestPeriod
	// PeriodKey 周期键（日 `2006-01-02` / 周 `2006-W01` / 月 `2006-01`）。
	PeriodKey string
	// Markdown 摘要正文。
	Markdown string
	// EpisodeIDs 覆盖的情节。
	EpisodeIDs []string
	// WindowStart 覆盖窗口起（含）。
	WindowStart time.Time
	// WindowEnd 覆盖窗口止（不含）。
	WindowEnd time.Time
	// GeneratedAt 生成时刻。
	GeneratedAt time.Time
}
