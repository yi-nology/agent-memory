package capture_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/capture"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("t")

// spyStore 抓写入内容的探针：断言落在存储层的那份文本长什么样。
type spyStore struct {
	saved []*types.RawEvent
}

func (s *spyStore) SaveRaw(_ context.Context, _ store.Namespace, ev *types.RawEvent) error {
	s.saved = append(s.saved, ev)
	return nil
}

func (s *spyStore) SaveRawBatch(context.Context, store.Namespace, []*types.RawEvent) error {
	return nil
}

func in(content string, prov types.Provenance) capture.Input {
	return capture.Input{
		Namespace: ns, EventID: "e1", Host: "h", SessionID: "s1", ThreadID: "t1",
		Type: types.EventExternal, Content: content, Provenance: prov,
		At: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
	}
}

// ── 装配闸 ──

// 脱敏器是必需依赖：记忆是留存资产，忘了配不会报任何错。
func TestNewRejectsMissingRedactor(t *testing.T) {
	_, err := capture.New(&spyStore{}, nil)
	if err == nil {
		t.Fatal("未装配脱敏器必须让构造失败：落库后再擦等于从未擦")
	}
	if !strings.Contains(err.Error(), "Redactor") {
		t.Fatalf("错误信息须点名 Redactor，实得 %v", err)
	}
	if _, err := capture.New(nil, ports.PatternRedactor{}); err == nil {
		t.Fatal("未装配存储必须让构造失败")
	}
}

// ── 本批的核心断言 ──

// **存储层拿不到明文。**
//
// 这条用例是整个 M3 存在的理由：宿主现有实现把断言与逐字引用原样落库，
// 而记忆是可导出、可复制出去的留存资产。处置点必须早于持久化。
func TestPlaintextNeverReachesStorage(t *testing.T) {
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	content := "部署用的访问凭据是 " + secret + "，请勿外传。"

	s := &spyStore{}
	c, err := capture.New(s, ports.PatternRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Capture(context.Background(), in(content, types.Provenance{Origin: types.OriginOperator}))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Accepted {
		t.Fatalf("本用例应落库，实得 %q", out.SkipReason)
	}
	if !out.Redacted {
		t.Fatal("发生处置时 Redacted 必须为真——那是导出时的审计位")
	}

	// 存储层拿到的那份（探针就是存储层）：
	got := s.saved[0].Content
	if strings.Contains(got, secret) {
		t.Fatalf("**存储层拿到了明文凭据**：%q", got)
	}
	if !strings.Contains(got, "凭据") {
		t.Fatalf("处置不该毁掉上下文（人名与语义要留下）：%q", got)
	}
	// 审计位也要落库，导出时才知道这份是处置过的。
	if !s.saved[0].Provenance.Redacted {
		t.Fatal("Provenance.Redacted 未落库：导出时无从判断能不能带出去")
	}
}

// 载荷里的敏感串同样不能漏：正文干净、元数据里有连接串是常见形态。
func TestPayloadSecretsAlsoRedacted(t *testing.T) {
	s := &spyStore{}
	c, _ := capture.New(s, ports.PatternRedactor{})
	ipt := in("部署完成", types.Provenance{Origin: types.OriginTool})
	ipt.Payload = map[string]any{
		"note":   "普通说明",
		"conn":   "postgres://admin:hunter2pass@10.0.0.1:5432/app",
		"nested": map[string]any{"token": "token=abcd1234efgh5678"},
	}
	if _, err := c.Capture(context.Background(), ipt); err != nil {
		t.Fatal(err)
	}
	raw := s.saved[0]
	if strings.Contains(raw.Payload["conn"].(string), "hunter2pass") {
		t.Fatalf("URL 里的凭据漏了：%q", raw.Payload["conn"])
	}
	nested := raw.Payload["nested"].(map[string]any)
	if strings.Contains(nested["token"].(string), "abcd1234efgh5678") {
		t.Fatalf("嵌套载荷里的凭据漏了：%q", nested["token"])
	}
	if raw.Payload["note"] != "普通说明" {
		t.Fatalf("无关字段不该被动：%q", raw.Payload["note"])
	}
}

// ── 自动捕获的闸 ──

// 合成的运行期错误不是「发生过的事」，不自动捕获。
func TestRuntimeErrorNotAutoCaptured(t *testing.T) {
	s := &spyStore{}
	c, _ := capture.New(s, ports.PatternRedactor{})
	out, err := c.Capture(context.Background(), in("执行失败：磁盘不足", types.Provenance{
		Origin: types.OriginModel, RuntimeError: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if out.Accepted {
		t.Fatal("运行期错误内容不得自动入库")
	}
	if len(s.saved) != 0 {
		t.Fatal("被拒收的事件不该有任何写入")
	}
	if out.SkipReason == "" {
		t.Fatal("拒收必须给理由：静默丢弃会让人以为记下来了")
	}
}

// 显式写入收：人明确说「记这条」时，他的判断优先于自动判据。
func TestExplicitCaptureAcceptsRuntimeError(t *testing.T) {
	s := &spyStore{}
	c, _ := capture.New(s, ports.PatternRedactor{})
	ipt := in("这条报错本身就是要留存的线索", types.Provenance{
		Origin: types.OriginModel, RuntimeError: true,
	})
	ipt.Explicit = true
	out, err := c.Capture(context.Background(), ipt)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Accepted {
		t.Fatalf("显式写入不该被自动判据拦下: %q", out.SkipReason)
	}
}

// ── 批量 ──

// 批量逐条独立：一条失败不该回滚已成功脱敏的内容。
func TestBatchIsIndependent(t *testing.T) {
	s := &spyStore{}
	c, _ := capture.New(s, ports.PatternRedactor{})
	batch := []capture.Input{
		in("第一条 token=abcdefghijklmnop", types.Provenance{Origin: types.OriginTool}),
		in("执行失败", types.Provenance{Origin: types.OriginModel, RuntimeError: true}),
		in("第三条", types.Provenance{Origin: types.OriginUser}),
	}
	outs, err := c.CaptureBatch(context.Background(), batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 {
		t.Fatalf("应逐条给出结论，实得 %d", len(outs))
	}
	if !outs[0].Accepted || outs[1].Accepted || !outs[2].Accepted {
		t.Fatalf("逐条结论应独立：%+v", outs)
	}
	if len(s.saved) != 2 {
		t.Fatalf("应落库 2 条，实得 %d", len(s.saved))
	}
}

// ── 脱敏器错误向上抛，不吞 ──

type boomRedactor struct{}

func (boomRedactor) Redact(context.Context, ports.RedactRequest) (string, error) {
	return "", errors.New("脱敏服务不可用")
}

func TestRedactorErrorIsNotSwallowed(t *testing.T) {
	s := &spyStore{}
	c, _ := capture.New(s, boomRedactor{})
	if _, err := c.Capture(context.Background(), in("任意内容", types.Provenance{})); err == nil {
		t.Fatal("脱敏失败必须让捕获失败：吞掉它等于明文入库")
	}
	if len(s.saved) != 0 {
		t.Fatal("脱敏失败时不得有任何写入")
	}
}
