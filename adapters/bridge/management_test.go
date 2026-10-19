package bridge_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/adapters/bridge"
	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/pipeline/episode"
	"github.com/yi-nology/agent-memory/pipeline/page"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件钉住 bridge 的**库管理面**（页面重生成、情节抽取、周期摘要）。
//
// 这三个动作**都需要模型**，而 application 的服务面刻意不装它们
// （见该包的包注释：它们是库管理动作，由 CLI 与宿主自己触发）。
// 它们挂在 bridge 上，是因为 bridge 才是「宿主不必是 Go」时唯一能触达的口子——
// 能力实现了却没有入口，既不会被用也不会被验证。

// fakeLLM 按用途返回固定内容。
type fakeLLM struct{}

func (fakeLLM) Complete(_ context.Context, req ports.LLMRequest) (string, error) {
	switch req.Purpose {
	case "memory.page.regen":
		return `{"headline":"机器甲的磁盘长期偏小","body":"机器甲的磁盘长期偏小。"}`, nil
	case "memory.episode.extract":
		return `{"title":"一次磁盘排查","summary":"定位到磁盘偏小","decisions":["清理旧文件"]}`, nil
	case "memory.digest":
		return "本周：定位并清理了磁盘。", nil
	default:
		return "[]", nil
	}
}

// newServerWithMgmt 造一个带库管理面的服务端。
func newServerWithMgmt(t *testing.T) (*bridge.Server, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.OpenMemory("bridge-mgmt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := application.New(application.Config{
		Namespace: store.Namespace("b"), Host: "test",
		Recall: recall.DefaultConfig(),
	}, application.Deps{
		Store: st, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{},
		LLM: fakeLLM{},
	})
	if err != nil {
		t.Fatal(err)
	}
	regen, err := page.New(st, fakeLLM{}, page.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ex, err := episode.New(st, fakeLLM{}, episode.DefaultPrompt)
	if err != nil {
		t.Fatal(err)
	}
	dig, err := episode.NewDigester(st, fakeLLM{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := bridge.NewWithManagement(svc, &bridge.Management{
		Page: regen, Episode: ex, Digester: dig,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

// 未装配库管理面时，那三个方法必须报「能力未装配」。
//
// 关键是**不能静默返回空结果**——空结果会被宿主读成「没有脏页面 / 没有情节」，
// 而真相是「你没装这个能力」。
func TestManagementMethodsReportUnavailable(t *testing.T) {
	s := newServer(t)
	for _, m := range []string{"memory.page.regen", "memory.episode.extract", "memory.episode.digest"} {
		raw, _ := json.Marshal(map[string]any{"namespace": "b"})
		resp := s.Handle(context.Background(),
			`{"jsonrpc":"2.0","id":1,"method":"`+m+`","params":`+string(raw)+`}`)
		if resp == nil {
			t.Fatal("无响应")
		}
		if resp.Error == nil {
			t.Fatalf("%s 未装配时必须报错（能力未装配），实得结果 %+v", m, resp.Result)
		}
		if resp.Error.Code != bridge.CodeUnavailable {
			t.Fatalf("%s 的错误码应是 CodeUnavailable（能力未装配），实得 %d", m, resp.Error.Code)
		}
	}
}

// 页面重生成：标脏 → 重生成 → 清脏。
func TestPageRegenThroughBridge(t *testing.T) {
	s, st := newServerWithMgmt(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.SaveEntity(ctx, store.Namespace("b"), &types.Entity{
		ID: "e1", EntityType: types.EntityType("service"), CanonicalName: "机器甲", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 实体得**有事实卡**才有页面可写：没有事实时重生成会跳过
	// （「没有事实就没有页面可写」，事实将来会有）。
	if err := st.SaveAtom(ctx, store.Namespace("b"), &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "机器甲的磁盘长期偏小", QuoteEventID: "ev1",
		Importance: types.ImportanceHigh, Confidence: types.ConfidenceMed,
		OccurredAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 标脏时刻要**早于冷却期**：页面刚被标脏时不算「到期」——
	// 冷却的用处是「宿主连续晋升十条事实时只生成一次页面」。
	if err := st.MarkEntityPageDirty(ctx, store.Namespace("b"), "e1",
		now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	out := call(t, s, "memory.page.regen", map[string]any{"namespace": "b"})
	if got, _ := out["regenerated"].(float64); got != 1 {
		t.Fatalf("应重生成 1 个页面，实得 %+v", out)
	}
	pg, err := st.GetEntityPage(ctx, store.Namespace("b"), "e1")
	if err != nil {
		t.Fatal(err)
	}
	if pg.Dirty {
		t.Fatal("重生成成功后必须清脏")
	}
	if pg.Headline == "" {
		t.Fatal("重生成后必须有画像")
	}
}

// 情节抽取：宿主给出本会话的事件标识，内核据此取素材并抽情节。
func TestEpisodeExtractThroughBridge(t *testing.T) {
	s, st := newServerWithMgmt(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.SaveRaw(ctx, store.Namespace("b"), &types.RawEvent{
		ID: "ev1", Content: "机器甲的磁盘长期偏小", Timestamp: now, Type: types.EventManual,
	}); err != nil {
		t.Fatal(err)
	}

	out := call(t, s, "memory.episode.extract", map[string]any{
		"namespace": "b", "session_id": "s1", "event_ids": []string{"ev1"},
	})
	if out["episode"] == nil {
		t.Fatalf("应抽出一条情节: %+v", out)
	}
	eps, err := st.ListEpisodes(ctx, store.Namespace("b"), "s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 {
		t.Fatalf("情节应落库，实得 %d 条", len(eps))
	}
}

// 情节抽取必须给素材：没有 event_ids 就该报参数错，而不是抽出一个空情节。
func TestEpisodeExtractRequiresEvents(t *testing.T) {
	s, _ := newServerWithMgmt(t)
	raw, _ := json.Marshal(map[string]any{"namespace": "b", "session_id": "s1"})
	resp := s.Handle(context.Background(),
		`{"jsonrpc":"2.0","id":1,"method":"memory.episode.extract","params":`+string(raw)+`}`)
	if resp == nil || resp.Error == nil {
		t.Fatal("没有素材时必须报参数错")
	}
	if resp.Error.Code != bridge.CodeInvalidParams {
		t.Fatalf("错误码应是 CodeInvalidParams，实得 %d", resp.Error.Code)
	}
}

// 周期摘要：周期枚举错要报参数错（宿主不该拿到一个空摘要还以为「这周没事」）。
func TestEpisodeDigestValidatesPeriod(t *testing.T) {
	s, _ := newServerWithMgmt(t)
	raw, _ := json.Marshal(map[string]any{"namespace": "b", "period": "fortnight"})
	resp := s.Handle(context.Background(),
		`{"jsonrpc":"2.0","id":1,"method":"memory.episode.digest","params":`+string(raw)+`}`)
	if resp == nil || resp.Error == nil {
		t.Fatal("非法 period 必须报参数错")
	}
	if resp.Error.Code != bridge.CodeInvalidParams {
		t.Fatalf("错误码应是 CodeInvalidParams，实得 %d", resp.Error.Code)
	}

	out := call(t, s, "memory.episode.digest", map[string]any{"namespace": "b", "period": "week"})
	if out["digest"] == nil {
		t.Fatalf("应产出周期摘要: %+v", out)
	}
}
