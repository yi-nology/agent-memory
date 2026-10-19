package bridge_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yi-nology/agent-memory/adapters/bridge"
	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
)

func newServer(t *testing.T) *bridge.Server {
	t.Helper()
	st, err := sqlite.OpenMemory("bridge")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := application.New(application.Config{
		Namespace: store.Namespace("b"), Host: "test",
		Recall: recall.DefaultConfig(),
	}, application.Deps{
		Store: st, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := bridge.New(svc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func call(t *testing.T, s *bridge.Server, method string, params any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	resp := s.Handle(context.Background(),
		`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+string(raw)+`}`)
	if resp == nil {
		t.Fatal("无响应")
	}
	b, _ := json.Marshal(resp.Result)
	var out map[string]any
	if resp.Error != nil {
		t.Fatalf("方法 %s 报错: %+v", method, resp.Error)
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// 捕获 → 召回，一条闭环走通。
func TestCaptureThenRecall(t *testing.T) {
	s := newServer(t)
	out := call(t, s, "memory.capture", map[string]any{
		"event_id": "e1", "type": "manual", "content": "磁盘长期偏小需要清理",
		"provenance": map[string]any{"origin": "operator"},
	})
	if out["accepted"] != true {
		t.Fatalf("捕获未受理: %v", out)
	}
	if out["redacted"] != false {
		t.Fatalf("普通内容不该被处置: %v", out)
	}

	rec := call(t, s, "memory.recall", map[string]any{"query": "磁盘"})
	if rec["count"].(float64) < 1 {
		t.Fatalf("召回应有内容: %v", rec)
	}
	rendered, _ := rec["rendered"].(string)
	if !strings.Contains(rendered, "磁盘") {
		t.Fatalf("召回内容不对: %q", rendered)
	}
}

// 白名单：未知方法必须被拒，且给出「方法不存在」这个可分辨的码。
func TestUnknownMethodRejected(t *testing.T) {
	s := newServer(t)
	resp := s.Handle(context.Background(), `{"jsonrpc":"2.0","id":1,"method":"memory.drop_everything"}`)
	if resp == nil || resp.Error == nil {
		t.Fatal("未知方法必须报错")
	}
	if resp.Error.Code != bridge.CodeMethodNotFound {
		t.Fatalf("应是「方法不存在」码，实得 %d", resp.Error.Code)
	}
}

// 非法 JSON 与非法参数要能被分辨。
func TestErrorCodesAreDistinguishable(t *testing.T) {
	s := newServer(t)

	bad := s.Handle(context.Background(), `not json at all`)
	if bad == nil || bad.Error == nil || bad.Error.Code != bridge.CodeParseError {
		t.Fatalf("非法 JSON 应判 parse error: %+v", bad)
	}

	badParams := s.Handle(context.Background(),
		`{"jsonrpc":"2.0","id":1,"method":"memory.recall","params":"not an object"}`)
	if badParams == nil || badParams.Error == nil || badParams.Error.Code != bridge.CodeInvalidParams {
		t.Fatalf("非法参数应判 invalid params: %+v", badParams)
	}

	notFound := s.Handle(context.Background(),
		`{"jsonrpc":"2.0","id":1,"method":"memory.get","params":{"id":"不存在"}}`)
	if notFound == nil || notFound.Error == nil || notFound.Error.Code != bridge.CodeNotFound {
		t.Fatalf("取不到应判 not found: %+v", notFound)
	}
}

// 通知（无 id）不产生响应。
func TestNotificationProducesNoResponse(t *testing.T) {
	s := newServer(t)
	resp := s.Handle(context.Background(),
		`{"jsonrpc":"2.0","method":"memory.stats","params":{}}`)
	if resp != nil {
		t.Fatalf("通知不该有响应: %+v", resp)
	}
}

// 逐行协议：每个响应一行。
func TestServeIsLineOriented(t *testing.T) {
	s := newServer(t)
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"memory.stats","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"memory.stats","params":{}}` + "\n")
	var out strings.Builder
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有 2 行响应，实得 %d: %q", len(lines), out.String())
	}
	for _, l := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("每行都该是独立 JSON: %q", l)
		}
	}
}

// 捕获面在 bridge 上也必须打码（协议不能绕过脱敏）。
func TestCaptureOverBridgeStillRedacts(t *testing.T) {
	s := newServer(t)
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := call(t, s, "memory.capture", map[string]any{
		"event_id": "e2", "type": "manual",
		"content":    "凭据是 " + secret,
		"provenance": map[string]any{"origin": "operator"},
	})
	if out["redacted"] != true {
		t.Fatalf("协议面同样必须脱敏: %v", out)
	}
}
