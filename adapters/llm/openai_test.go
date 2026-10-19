package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/ports"
)

// fakeServer 起一个 OpenAI 兼容端点。
func fakeServer(t *testing.T, status int, body string, capture *chatRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		if capture != nil {
			_ = json.NewDecoder(r.Body).Decode(capture)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewRejectsMissingConfig(t *testing.T) {
	if _, err := New(Config{Model: "m"}); err != ErrNoBaseURL {
		t.Fatalf("缺 BaseURL 应返回 ErrNoBaseURL，实际: %v", err)
	}
	if _, err := New(Config{BaseURL: "http://x"}); err != ErrNoModel {
		t.Fatalf("缺 Model 应返回 ErrNoModel，实际: %v", err)
	}
}

func TestCompleteReturnsContent(t *testing.T) {
	srv := fakeServer(t, 200, `{"choices":[{"message":{"role":"assistant","content":"  你好  "}}]}`, nil)
	c, err := New(Config{BaseURL: srv.URL + "/v1", Model: "m", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "hi"})
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if out != "你好" {
		t.Fatalf("正文应去空白，实际: %q", out)
	}
}

func TestCompleteSendsBearerAndModel(t *testing.T) {
	var got chatRequest
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m", APIKey: "secret", PurposeModels: map[string]string{"extract": "m-fast"}})
	if _, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "p", Purpose: "extract"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret" {
		t.Fatalf("鉴权头不对: %q", auth)
	}
	if got.Model != "m-fast" {
		t.Fatalf("按用途分派模型不对: %q", got.Model)
	}
}

func TestCompleteJSONModeSendsResponseFormat(t *testing.T) {
	var got chatRequest
	srv := fakeServer(t, 200, `{"choices":[{"message":{"content":"{\"a\":1}"}}]}`, &got)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	if _, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "p", JSON: true}); err != nil {
		t.Fatal(err)
	}
	if got.ResponseFormat == nil || got.ResponseFormat.Type != "json_object" {
		t.Fatalf("JSON 模式应下发 response_format，实际: %+v", got.ResponseFormat)
	}
	// JSON 模式必须同时给 system 消息说明形态，否则部分端点会拒。
	if len(got.Messages) == 0 || got.Messages[0].Role != "system" {
		t.Fatalf("JSON 模式应带 system 消息，实际: %+v", got.Messages)
	}
}

func TestCompleteEmptyContentIsErrorNotEmptyString(t *testing.T) {
	// 这是本包最关键的不变量：空正文必须报错，不能返回空串。
	// 空串会被提炼面当成「模型说了没什么」，从而静默丢弃整批候选——
	// 基础设施故障被伪装成一次正常但空洞的提炼。
	srv := fakeServer(t, 200, `{"choices":[{"message":{"content":"   "}}]}`, nil)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	out, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "p"})
	if err == nil {
		t.Fatalf("空正文必须报错，实际返回 %q", out)
	}
	if err != ErrEmptyResponse {
		t.Fatalf("应为 ErrEmptyResponse，实际: %v", err)
	}
}

func TestCompleteNoChoicesIsError(t *testing.T) {
	srv := fakeServer(t, 200, `{"choices":[]}`, nil)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	if _, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "p"}); err == nil {
		t.Fatal("无 choices 必须报错")
	}
}

func TestCompleteHTTPErrCarriesStatusAndBody(t *testing.T) {
	srv := fakeServer(t, 401, `{"error":{"message":"invalid api key"}}`, nil)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	_, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "p"})
	if err == nil {
		t.Fatal("401 必须报错")
	}
	// 状态码与服务端消息都要在错误里——只报状态码等于让宿主去猜。
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("错误应含状态码与服务端消息，实际: %v", err)
	}
}

func TestCompleteServerErrorFieldIsSurfaced(t *testing.T) {
	// 有些端点在 HTTP 200 里带 error 字段，这种形态不能被当成成功。
	srv := fakeServer(t, 200, `{"error":{"message":"model overloaded"},"choices":[]}`, nil)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	_, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("200 里的 error 字段必须被报出，实际: %v", err)
	}
}

func TestCompleteRejectsEmptyPrompt(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://127.0.0.1:1/v1", Model: "m"})
	if _, err := c.Complete(context.Background(), ports.LLMRequest{}); err == nil {
		t.Fatal("空提示必须报错")
	}
}

func TestHealthSurfacesEndpointFailure(t *testing.T) {
	srv := fakeServer(t, 500, `boom`, nil)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m", Timeout: 2 * time.Second})
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("端点故障时 Health 必须报错")
	}
}

// 变异验证：把 ErrEmptyResponse 的判定去掉（返回空串假装成功），
// TestCompleteEmptyContentIsErrorNotEmptyString 必红。
func TestMutantEmptyBecomesSuccess(t *testing.T) {
	srv := fakeServer(t, 200, `{"choices":[{"message":{"content":""}}]}`, nil)
	c, _ := New(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	out, err := c.Complete(context.Background(), ports.LLMRequest{Prompt: "p"})
	if err == nil && out == "" {
		t.Fatal("变异体（空串假装成功）已复现：说明本用例的断言确实钉住了这条不变量")
	}
}
