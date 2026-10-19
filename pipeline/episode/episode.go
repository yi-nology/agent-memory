// Package episode 情节面：把一次会话/一件事的经过摘要成情节，并按周期聚合成摘要。
//
// # 它与事实卡是两条独立的路
//
// 事实卡回答「**是什么**」（这个对象的磁盘长期就偏小），
// 情节回答「**发生了什么**」（上周三那次故障我们怎么定位的）。
// 两者都从 RawEvent 出发，但一条走晋升、一条走摘要，互不依赖——
// 没有情节一样能记住事实，反之亦然。
//
// # 时区口径
//
// 周期边界一律按 **UTC** 算，不用宿主本地时区。
//
// 理由：记忆库可能被多个宿主共写，而「周三」在 UTC-8 与 UTC+9 的宿主那里
// 指的是不同的日子。用本地时区算周期，同一批情节在两个宿主上会落进不同的
// 日报——而「上周三的日报」这种事一旦对不上，人是查不出来的。
// 真要本地时区，宿主在自己的调用侧按本地时间圈定窗口再传进来。
package episode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Store 情节面用到的存储。
type Store interface {
	SaveEpisode(ctx context.Context, ns store.Namespace, e *types.Episode) error
	ListEpisodes(ctx context.Context, ns store.Namespace, session string, limit int) ([]*types.Episode, error)
	ListEpisodesInWindow(ctx context.Context, ns store.Namespace, from, to time.Time) ([]*types.Episode, error)
	SaveDigest(ctx context.Context, ns store.Namespace, d *types.DigestRecord) error
	GetDigest(ctx context.Context, ns store.Namespace, period types.DigestPeriod, key string) (*types.DigestRecord, error)
}

// ErrNoLLM 未注入模型能力。
var ErrNoLLM = ports.ErrNoLLM

// PromptFunc 构造提示词。
type PromptFunc func(events []*types.RawEvent) string

// DefaultPrompt 缺省提示词。
func DefaultPrompt(events []*types.RawEvent) string {
	var b strings.Builder
	b.WriteString("把下面的事件总结成一段情节：发生了什么、定了什么。\n")
	b.WriteString("只输出 JSON：{\"title\": \"\", \"summary\": \"\", \"decisions\": [\"\"]}\n")
	b.WriteString("不得添加事件之外的信息。\n事件：\n")
	for _, e := range events {
		b.WriteString(fmt.Sprintf("- [%s] %s\n", e.ID, e.Content))
	}
	return b.String()
}

// Extractor 情节提炼器。
type Extractor struct {
	store  Store
	llm    ports.LLM
	prompt PromptFunc
	now    func() time.Time
}

// New 造提炼器。
func New(s Store, llm ports.LLM, prompt PromptFunc) (*Extractor, error) {
	if s == nil {
		return nil, errors.New("episode: 存储未装配")
	}
	if prompt == nil {
		prompt = DefaultPrompt
	}
	return &Extractor{store: s, llm: llm, prompt: prompt,
		now: func() time.Time { return time.Now().UTC() }}, nil
}

// ExtractFromSession 从一次会话的事件里抽情节并落库。
func (e *Extractor) ExtractFromSession(ctx context.Context, ns store.Namespace,
	sessionID string, events []*types.RawEvent) (*types.Episode, error) {

	if e.llm == nil {
		return nil, ErrNoLLM
	}
	if len(events) == 0 {
		return nil, nil
	}
	out, err := e.llm.Complete(ctx, ports.LLMRequest{
		Prompt: e.prompt(events), Purpose: "memory.episode.extract",
		JSON: true, MaxTokens: 1200,
	})
	if err != nil {
		return nil, err
	}
	ep, err := parseEpisode(out, sessionID, events, e.now())
	if err != nil {
		return nil, err
	}
	if err := e.store.SaveEpisode(ctx, ns, ep); err != nil {
		return nil, err
	}
	return ep, nil
}

func parseEpisode(raw, sessionID string, events []*types.RawEvent, now time.Time) (*types.Episode, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var obj struct {
		Title     string   `json:"title"`
		Summary   string   `json:"summary"`
		Decisions []string `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return nil, fmt.Errorf("episode: 解析失败: %w", err)
	}
	if strings.TrimSpace(obj.Summary) == "" {
		return nil, errors.New("episode: 模型输出没有 summary")
	}
	ids := make([]string, 0, len(events))
	occurred := now
	for _, ev := range events {
		ids = append(ids, ev.ID)
		if ev.Timestamp.Before(occurred) {
			occurred = ev.Timestamp
		}
	}
	return &types.Episode{
		ID:          "ep-" + sessionID + "-" + fmt.Sprint(now.UnixNano()),
		SessionID:   sessionID,
		Title:       strings.TrimSpace(obj.Title),
		Summary:     strings.TrimSpace(obj.Summary),
		Decisions:   obj.Decisions,
		OccurredAt:  occurred,
		CreatedAt:   now,
		RawEventIDs: ids,
	}, nil
}

// ── 周期摘要 ──

// Period 周期面（与 types.DigestPeriod 同值，便于参数传递）。
type Period = types.DigestPeriod

const (
	PeriodDay   = types.PeriodDay
	PeriodWeek  = types.PeriodWeek
	PeriodMonth = types.PeriodMonth
)

// Window 取某周期在给定时刻的窗口。
//
// 用**自然周**（周一起始）而不是 ISO 周：ISO 周的边界规则（哪一周算第一周）
// 每年会变一次，而人记忆里的「上周」是「七天前那七天」。
// 宁可与 ISO 差一点，也不要在跨年那几天算出反直觉的窗口。
func Window(p Period, at time.Time) (from, to time.Time, key string) {
	at = at.UTC()
	switch p {
	case PeriodWeek:
		wd := (int(at.Weekday()) + 6) % 7 // 周一=0
		from = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -wd)
		to = from.AddDate(0, 0, 7)
		return from, to, fmt.Sprintf("%04d-W%02d", from.Year(), (from.YearDay()-1)/7+1)
	case PeriodMonth:
		from = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		to = from.AddDate(0, 1, 0)
		return from, to, from.Format("2006-01")
	default:
		from = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		to = from.AddDate(0, 0, 1)
		return from, to, from.Format("2006-01-02")
	}
}

// Digester 周期摘要器。
type Digester struct {
	store Store
	llm   ports.LLM
	now   func() time.Time
}

// NewDigester 造摘要器。
func NewDigester(s Store, llm ports.LLM) (*Digester, error) {
	if s == nil {
		return nil, errors.New("episode: 存储未装配")
	}
	return &Digester{store: s, llm: llm, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Digest 聚合某周期内的情节成一条周期摘要。
//
// 无模型时**不失败**，落一份**确定性回退摘要**（列出情节标题与摘要要点）。
// 理由：周期摘要的价值是「有个可查的索引」，而不是文采；没有模型时给一份
// 结构化的清单，比整份摘要缺失有用得多。
func (d *Digester) Digest(ctx context.Context, ns store.Namespace, p Period, at time.Time) (*types.DigestRecord, error) {
	from, to, key := Window(p, at)
	eps, err := d.store.ListEpisodesInWindow(ctx, ns, from, to)
	if err != nil {
		return nil, err
	}
	rec := &types.DigestRecord{
		Period: p, PeriodKey: key,
		WindowStart: from, WindowEnd: to, GeneratedAt: d.now(),
	}
	ids := make([]string, 0, len(eps))
	for _, e := range eps {
		ids = append(ids, e.ID)
	}
	rec.EpisodeIDs = ids
	if len(eps) == 0 {
		rec.Markdown = "" // 窗口内无情节：空摘要，不写「今天没发生什么」
		if err := d.store.SaveDigest(ctx, ns, rec); err != nil {
			return nil, err
		}
		return rec, nil
	}

	if d.llm != nil {
		out, err := d.llm.Complete(ctx, ports.LLMRequest{
			Prompt: digestPrompt(p, key, eps), Purpose: "memory.digest",
			JSON: false, MaxTokens: 2000,
		})
		if err == nil && strings.TrimSpace(out) != "" {
			rec.Markdown = strings.TrimSpace(out)
			if err := d.store.SaveDigest(ctx, ns, rec); err != nil {
				return nil, err
			}
			return rec, nil
		}
		// 模型失败：落回退摘要，而不是整条失败。
	}
	rec.Markdown = fallbackMarkdown(eps)
	if err := d.store.SaveDigest(ctx, ns, rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func digestPrompt(p Period, key string, eps []*types.Episode) string {
	var b strings.Builder
	b.WriteString("把下面这些情节汇总成一段")
	b.WriteString(string(p))
	b.WriteString("摘要（周期键 " + key + "）。\n")
	b.WriteString("只写这些情节里出现过的内容，不得添加。\n情节：\n")
	for _, e := range eps {
		b.WriteString(fmt.Sprintf("- [%s] %s\n", e.Title, e.Summary))
	}
	return b.String()
}

// fallbackMarkdown 无模型时的确定性回退摘要。
//
// 刻意是**清单**而不是散文：它不假装是「总结」，而是把原料按时间摆出来。
// 一份假装成总结的清单会误导读者以为漏掉了什么；而诚实的清单不会。
func fallbackMarkdown(eps []*types.Episode) string {
	var b strings.Builder
	for _, e := range eps {
		if e.Title != "" {
			b.WriteString("- **" + e.Title + "**：" + e.Summary + "\n")
		} else {
			b.WriteString("- " + e.Summary + "\n")
		}
		for _, dec := range e.Decisions {
			b.WriteString("  - 决策：" + dec + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
