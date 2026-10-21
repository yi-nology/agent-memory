package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// newTestServer 造一个**真**服务端：真 sqlite 后端 + 真 application.Service。
//
// 刻意不 mock 存储层：mock 掉的那一层正是最可能断的地方
// （协议拼装对了、参数映射错了、查询口径错了——mock 全都盖不住）。
func newTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := sqlite.OpenMemory("mcp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc, err := application.New(application.Config{
		Namespace: store.Namespace("mcp"),
		Host:      "test",
		Recall:    recall.DefaultConfig(),
	}, application.Deps{
		Store: st, Redactor: ports.PatternRedactor{}, Bander: ports.MemoryBand{},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(svc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// call 走一次协议往返，返回解码后的响应。
func call(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Serve(context.Background(), raw)
	if err != nil {
		t.Fatalf("处理失败: %v", err)
	}
	if out == nil {
		t.Fatal("该有响应却没有")
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, out)
	}
	return resp
}

// callErr 要求这一调报错，并返回错误体。
func callErr(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	resp := call(t, s, method, params)
	if _, ok := resp["result"]; ok {
		t.Fatalf("该报错却回了结果: %v", resp["result"])
	}
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("该有 error 却没了: %v", resp)
	}
	return e
}

// seedAtom 落一张事实卡（真后端）。
func seedAtom(t *testing.T, s *Server, id, assertion string) {
	t.Helper()
	now := time.Now().UTC()
	atom := &types.AtomCard{
		ID:           id,
		EntityID:     "ent-1",
		QuoteEventID: "ev-1",
		Assertion:    assertion,
		OccurredAt:   now,
		CreatedAt:    now,
		UpdatedAt:    now,
		Confidence:   types.ConfidenceMed,
		Importance:   types.ImportanceMed,
	}
	if err := s.svc.Store().SaveAtom(context.Background(), s.svc.Namespace(), atom); err != nil {
		t.Fatal(err)
	}
}

// toolText 取工具结果里的文本。
func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("没有 result: %v", resp)
	}
	if isErr, ok := result["isError"].(bool); ok && isErr {
		t.Fatalf("工具报了错: %v", result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("没有内容: %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// initialize 返回协议版本与本端能力。
func TestInitializeReturnsProtocolVersionAndCapabilities(t *testing.T) {
	s := newTestServer(t)
	resp := call(t, s, "initialize", map[string]any{})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("没有 result: %v", resp)
	}
	if result["protocolVersion"] != ProtocolVersion {
		t.Fatalf("协议版本不对: %v", result["protocolVersion"])
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("没有 capabilities: %v", result)
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("没声明 tools 能力: %v", caps)
	}
	info, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("没有 serverInfo: %v", result)
	}
	if info["name"] != ServerName || info["version"] == "" {
		t.Fatalf("serverInfo 不对: %v", info)
	}
}

// 工具白名单：恰好四个，每个都有 name/description/inputSchema。
func TestToolsListHasExactlyFourTools(t *testing.T) {
	s := newTestServer(t)
	resp := call(t, s, "tools/list", map[string]any{})
	result, _ := resp["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	if len(list) != 4 {
		t.Fatalf("应恰好四个工具，实得 %d 个: %v", len(list), list)
	}
	want := map[string]bool{
		"memory.capture": true, "memory.search": true,
		"memory.recall": true, "memory.stats": true,
	}
	for _, raw := range list {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if !want[name] {
			t.Fatalf("白名单外的工具: %s", name)
		}
		delete(want, name)
		if desc, _ := tool["description"].(string); desc == "" {
			t.Fatalf("工具 %s 没有说明", name)
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("工具 %s 的 inputSchema 不对: %v", name, tool["inputSchema"])
		}
	}
	if len(want) != 0 {
		t.Fatalf("少了工具: %v", want)
	}
}

// tools/call 调 memory.search：真后端、真服务，能拿到事实卡。
func TestToolsCallSearchFindsFactCard(t *testing.T) {
	s := newTestServer(t)
	seedAtom(t, s, "atom-1", "存储后端的全文索引按中文逐字建词元")
	resp := call(t, s, "tools/call", map[string]any{
		"name":      "memory.search",
		"arguments": map[string]any{"query": "全文索引", "limit": 5},
	})
	result, _ := resp["result"].(map[string]any)
	if isErr, ok := result["isError"].(bool); !ok || isErr {
		t.Fatalf("工具结果该带 isError=false: %v", result)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "全文索引") {
		t.Fatalf("检索结果里没有那张卡: %s", text)
	}
}

// tools/call 调 memory.capture：事件真的落库了。
func TestToolsCallCaptureStoresEvent(t *testing.T) {
	s := newTestServer(t)
	// 令牌本体要够长：脱敏兵底按**形态**收，已知前缀那一条要求 16 位以上，
	// 短串不算凭据（短十六进串在正常文本里太常见，误伤代价高于漏报代价）。
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	resp := call(t, s, "tools/call", map[string]any{
		"name": "memory.capture",
		"arguments": map[string]any{
			"event_id": "ev-9", "type": "manual", "content": "部署用的访问凭据是 " + secret,
		},
	})
	text := toolText(t, resp)
	if !strings.Contains(text, `"accepted":true`) {
		t.Fatalf("捕获未受理: %s", text)
	}
	ev, err := s.svc.Store().GetRaw(context.Background(), s.svc.Namespace(), "ev-9")
	if err != nil {
		t.Fatalf("事件没落库: %v", err)
	}
	// 脱敏在落库前：库里读出来也必须是打码后的。
	if strings.Contains(ev.Content, secret) {
		t.Fatalf("库里躺着明文凭据: %s", ev.Content)
	}
}

// tools/call 调 memory.recall：返回可放进提示词的内容。
func TestToolsCallRecallRendersContent(t *testing.T) {
	s := newTestServer(t)
	seedAtom(t, s, "atom-2", "记忆库的命名空间是整库级隔离维度")
	resp := call(t, s, "tools/call", map[string]any{
		"name":      "memory.recall",
		"arguments": map[string]any{"query": "命名空间"},
	})
	text := toolText(t, resp)
	if !strings.Contains(text, "命名空间") {
		t.Fatalf("召回内容不对: %s", text)
	}
}

// 未知工具：必须报错，**不许**当成成功返回空结果。
func TestToolsCallUnknownToolIsRejected(t *testing.T) {
	s := newTestServer(t)
	e := callErr(t, s, "tools/call", map[string]any{
		"name":      "memory.drop_everything",
		"arguments": map[string]any{},
	})
	if code, _ := e["code"].(float64); int(code) != CodeInvalidParams {
		t.Fatalf("应是「参数不合法」码，实得 %v", e["code"])
	}
}

// 未知方法：-32601。
func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	s := newTestServer(t)
	e := callErr(t, s, "memory.nope", map[string]any{})
	if code, _ := e["code"].(float64); int(code) != CodeMethodNotFound {
		t.Fatalf("应是「方法不存在」码，实得 %v", e["code"])
	}
}

// 参数不合法：-32602。
func TestInvalidParamsReturnsInvalidParams(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"缺 query", map[string]any{"limit": 3}},
		{"limit 类型不对", map[string]any{"query": "存储", "limit": "很多"}},
	}
	for _, c := range cases {
		e := callErr(t, s, "tools/call", map[string]any{
			"name": "memory.search", "arguments": c.args,
		})
		if code, _ := e["code"].(float64); int(code) != CodeInvalidParams {
			t.Fatalf("%s：应是「参数不合法」码，实得 %v", c.name, e["code"])
		}
	}
}

// 通知（无 id）：一个字节都不许回。
func TestNotificationProducesNoBytes(t *testing.T) {
	s := newTestServer(t)
	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"memory.stats"}`,
		`{"jsonrpc":"2.0","method":"memory.nope"}`,
	} {
		out, err := s.Serve(context.Background(), []byte(line))
		if err != nil {
			t.Fatalf("通知不该是错误: %v", err)
		}
		if out != nil {
			t.Fatalf("通知不许有响应: %s", out)
		}
	}
}

// 一行坏 JSON 之后，下一行好请求仍要正常处理。
func TestBadMessageDoesNotKillTheRound(t *testing.T) {
	s := newTestServer(t)
	in := "这不是 JSON\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	var out bytes.Buffer
	if err := s.Run(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("应回两条（坏消息一条 + 好请求一条），实得 %d 条: %q", len(lines), out.String())
	}
	var bad map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &bad); err != nil {
		t.Fatalf("坏消息的回包不是 JSON: %v", err)
	}
	if e, ok := bad["error"].(map[string]any); !ok {
		t.Fatalf("坏消息该回 error: %v", bad)
	} else if code, _ := e["code"].(float64); int(code) != CodeParseError {
		t.Fatalf("应是「解析失败」码，实得 %v", e["code"])
	}
	var good map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &good); err != nil {
		t.Fatalf("好请求的回包不是 JSON: %v", err)
	}
	if good["id"] != float64(2) {
		t.Fatalf("回包对不上请求: %v", good)
	}
	if _, ok := good["result"]; !ok {
		t.Fatalf("好请求该成功: %v", good)
	}
	if s.Failures() == 0 {
		t.Fatal("坏消息要留下痕迹，否则没人知道坏过")
	}
}

// Run 走完整条 stdin → stdout 的往返。
func TestRunRoundTrip(t *testing.T) {
	s := newTestServer(t)
	seedAtom(t, s, "atom-3", "实体画像是事实卡的派生摘要")
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory.search","arguments":{"query":"画像"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := s.Run(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// 通知不占一条回包：三条请求（其中一条是通知）→ 三条回包。
	if len(lines) != 3 {
		t.Fatalf("应回三条，实得 %d 条: %q", len(lines), out.String())
	}
	for i, line := range lines {
		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("第 %d 条回包不是 JSON: %v", i, err)
		}
		if resp["jsonrpc"] != "2.0" {
			t.Fatalf("第 %d 条回包缺 jsonrpc: %v", i, resp)
		}
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatal(err)
	}
	if last["id"] != float64(3) {
		t.Fatalf("回包顺序错了: %v", last)
	}
	result, _ := last["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("工具没回内容: %v", last)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, "画像") {
		t.Fatalf("工具结果不对: %s", text)
	}
}
