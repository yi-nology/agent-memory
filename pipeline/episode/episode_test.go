package episode_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/episode"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("ep")

func now() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

type fakeStore struct {
	eps    []*types.Episode
	dig    []*types.DigestRecord
	savedE int
}

func (f *fakeStore) SaveEpisode(_ context.Context, _ store.Namespace, e *types.Episode) error {
	f.eps = append(f.eps, e)
	f.savedE++
	return nil
}

func (f *fakeStore) ListEpisodes(_ context.Context, _ store.Namespace, _ string, _ int) ([]*types.Episode, error) {
	return f.eps, nil
}

func (f *fakeStore) ListEpisodesInWindow(_ context.Context, _ store.Namespace, from, to time.Time) ([]*types.Episode, error) {
	var out []*types.Episode
	for _, e := range f.eps {
		if !e.OccurredAt.Before(from) && e.OccurredAt.Before(to) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeStore) SaveDigest(_ context.Context, _ store.Namespace, d *types.DigestRecord) error {
	f.dig = append(f.dig, d)
	return nil
}

func (f *fakeStore) GetDigest(_ context.Context, _ store.Namespace, p types.DigestPeriod, key string) (*types.DigestRecord, error) {
	for _, d := range f.dig {
		if d.Period == p && d.PeriodKey == key {
			return d, nil
		}
	}
	return nil, store.ErrNotFound
}

type fakeLLM struct {
	out string
	err error
}

func (f *fakeLLM) Complete(context.Context, ports.LLMRequest) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

func ctx2() context.Context { return context.Background() }

// 周期窗口边界：日/周/月。
func TestWindowBoundaries(t *testing.T) {
	at := time.Date(2026, 10, 19, 15, 30, 0, 0, time.UTC) // 周一

	from, to, key := episode.Window(episode.PeriodDay, at)
	if from.Format("2006-01-02") != "2026-10-19" || to.Format("2006-01-02") != "2026-10-20" {
		t.Fatalf("日窗口不对: %v → %v", from, to)
	}
	if key != "2026-10-19" {
		t.Fatalf("日键不对: %s", key)
	}

	from, to, key = episode.Window(episode.PeriodWeek, at)
	if from.Format("2006-01-02") != "2026-10-19" {
		t.Fatalf("周窗口应从周一起: %v", from)
	}
	if to.Sub(from) != 7*24*time.Hour {
		t.Fatalf("周窗口应是 7 天: %v", to.Sub(from))
	}
	if key == "" {
		t.Fatal("周键不该为空")
	}

	from, to, key = episode.Window(episode.PeriodMonth, at)
	if from.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("月窗口应从当月一号起: %v", from)
	}
	if key != "2026-10" {
		t.Fatalf("月键不对: %s", key)
	}
}

// 情节提炼：无模型显式失败，有模型落库。
func TestExtractEpisode(t *testing.T) {
	f := &fakeStore{}
	ex, err := episode.New(f, &fakeLLM{out: `{"title":"一次排查","summary":"定位到磁盘","decisions":["清理旧文件"]}`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := ex.ExtractFromSession(ctx2(), ns, "s1", []*types.RawEvent{
		{ID: "e1", Content: "排查过程", Timestamp: now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ep == nil || ep.Title != "一次排查" || len(ep.Decisions) != 1 {
		t.Fatalf("情节没落对: %+v", ep)
	}
	if f.savedE != 1 {
		t.Fatal("情节应落库")
	}

	// 没有模型 → 显式失败
	ex2, _ := episode.New(f, nil, nil)
	if _, err := ex2.ExtractFromSession(ctx2(), ns, "s1", []*types.RawEvent{{ID: "e1"}}); err == nil {
		t.Fatal("无模型必须显式失败")
	}
}

// 周期摘要：无模型时落**确定性回退清单**，不是整条失败。
func TestDigestFallsBackWithoutLLM(t *testing.T) {
	f := &fakeStore{}
	f.eps = []*types.Episode{{ID: "ep1", Title: "一次排查", Summary: "定位到磁盘", OccurredAt: now()}}

	d, err := episode.NewDigester(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := d.Digest(ctx2(), ns, episode.PeriodDay, now())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Markdown == "" {
		t.Fatal("无模型也该给出可查的清单")
	}
	if len(rec.EpisodeIDs) != 1 {
		t.Fatalf("应覆盖 1 个情节: %+v", rec.EpisodeIDs)
	}
	// 回退件是清单，不假装是总结
	if rec.Markdown == rec.EpisodeIDs[0] {
		t.Fatal("回退摘要不该只是 ID")
	}
}

// 窗口内无情节 → 空摘要，不写「今天没发生什么」。
func TestDigestEmptyWindow(t *testing.T) {
	f := &fakeStore{}
	d, _ := episode.NewDigester(f, nil)
	rec, err := d.Digest(ctx2(), ns, episode.PeriodDay, now())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Markdown != "" {
		t.Fatalf("空窗口该是空摘要，实得 %q", rec.Markdown)
	}
}
