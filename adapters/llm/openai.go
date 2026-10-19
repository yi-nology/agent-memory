// Package llm 面向 OpenAI 兼容端点的模型适配器：让 ports.LLM 端口**开箱可用**。
//
// # 为什么有这一层
//
// ports 段只声明协议，不含实现——这是刻意的：核心不绑死在一个 SDK 上，
// 没有模型时必须能降级运行（手工写入 + 词法召回照常工作）。
//
// 但「不绑死 SDK」与「宿主必须自己写一遍 HTTP 客户端」是两回事。
// ports.LLM 只有一个 Complete 方法，形状极其简单，于是每个宿主都要**重写一遍**
// 同样的东西：拼请求体、解析响应、判错误码、算超时、处理 JSON 模式。
// 这正是「各仓都要重写一遍且边界易错」的那类轮子——
// 错误码判错会静默把失败当成功，JSON 模式漏传会让提炼面拿到带围栏的散文。
//
// 故本包提供**一个**开箱实现：纯标准库的 OpenAI 兼容客户端。
// 它不是「核心的一部分」——核心仍然只认 ports.LLM 接口，本包是**可替换的适配器**：
// 宿主若已有 SDK 客户端，照旧实现自己的 ports.LLM 即可，本包不用。
//
// # 为什么不引第三方 SDK
//
// 本仓的零第三方依赖纪律覆盖 adapters 段（见 architecture-policy.yaml 的
// memory-core-stdlib-only）。这条纪律不是为了洁癖：核心一旦引了 SDK，
// 「没有模型时手工写入 + 词法召回必须能跑」这条降级路径就没了——
// 而它塌了之后不会立刻暴露，它塌在「用户配了模型、装了数据库」的生产环境里。
//
// OpenAI 兼容协议本身足够简单，用 net/http + encoding/json 就能覆盖，
// 且比引 SDK 更可控：SDK 会带来自己的重试/超时/日志语义，
// 而本包的行为是**逐行可查**的。
//
// # 纯度说明
//
// 本包只依赖标准库与本仓的 ports 包。不引 oagentkit/llm：
// 那个包是 eino 重仓的，eino 会把「非 eino 生态的宿主」挡在门外，
// 而本仓的定位恰恰是**宿主无关**。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
)

// ErrNoBaseURL 未配置端点时的哨兵。
var ErrNoBaseURL = errors.New("llm: 未配置端点地址（BaseURL）")

// ErrNoModel 未配置模型名时的哨兵。
var ErrNoModel = errors.New("llm: 未配置模型名（Model）")

// ErrEmptyResponse 模型返回空正文的哨兵。
//
// 为什么要单独一个哨兵：提炼面把「空串」当成「模型说了没什么」，
// 于是**静默丢弃整批候选**——而「模型调不通」与「模型确实没内容」
// 在下游是同一种可观测结果（都是空）。分开报，才不会把基础设施故障
// 伪装成一次正常但空洞的提炼。
var ErrEmptyResponse = errors.New("llm: 模型返回空正文")

// Config OpenAI 兼容客户端配置。
type Config struct {
	// BaseURL 端点根地址，如 "http://127.0.0.1:8000/v1"。
	//
	// 末尾斜杠会被去掉；不带 "/v1" 后缀时**不会**自动补——
	// 自动补会把「宿主故意用 /compatible/v1 之类路径」的部署改坏，
	// 而这类部署在企业内部网关里并不少见。
	BaseURL string

	// APIKey 密钥。空串表示该端点不校验密钥（内网自建部署常见）。
	APIKey string

	// Model 模型名。
	Model string

	// Timeout 单次请求超时。零值取 60s。
	Timeout time.Duration

	// HTTPClient 自定义客户端。nil 时按 Timeout 构造一个。
	//
	// 给出它的理由：企业部署常要求走自定义 CA、走代理、走 mTLS，
	// 这些都只能经 http.Client 表达。
	HTTPClient *http.Client

	// PurposeModels 按用途分派模型。
	//
	// 键是 ports.LLMRequest.Purpose。缺失的用途回落到 Model。
	// 为什么需要它：不同用途的模型档位差着数量级
	// （摘要是批量作业，晋升升级是交互路径），用同一个模型既慢又贵。
	// 宿主据此选模型与预算——这个字段存在的意义就在这儿。
	PurposeModels map[string]string

	// ExtraHeaders 附加请求头（如租户标识、追踪 ID）。
	ExtraHeaders map[string]string
}

// Client OpenAI 兼容模型客户端，实现 ports.LLM。
type Client struct {
	cfg    Config
	client *http.Client
}

// New 构造客户端。
//
// 不做 I/O：端点通不通由第一次调用（或显式的 Health）去发现。
// 构造即联网会让「配置完先验一下」的用法被迫吃一次网络延迟，
// 而配置错误在第一次真调用时暴露得更早也更准。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, ErrNoBaseURL
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, ErrNoModel
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &Client{cfg: cfg, client: hc}, nil
}

// Model 返回默认模型名。
func (c *Client) Model() string { return c.cfg.Model }

// BaseURL 返回端点根地址。
func (c *Client) BaseURL() string { return c.cfg.BaseURL }

// Complete 实现 ports.LLM：把提示送出去，把正文拿回来。
//
// 错误一律**带类型**返回，绝不用空串假装成功——见 ErrEmptyResponse 的说明。
func (c *Client) Complete(ctx context.Context, req ports.LLMRequest) (string, error) {
	if req.Prompt == "" {
		return "", errors.New("llm: 提示正文为空")
	}
	model := c.cfg.Model
	if m, ok := c.cfg.PurposeModels[req.Purpose]; ok && m != "" {
		model = m
	}

	body := chatRequest{
		Model:    model,
		Messages: []chatMessage{{Role: "user", Content: req.Prompt}},
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	if req.JSON {
		// JSON 模式：部分服务端要求同时给一条 system 消息说明输出形态，
		// 否则该字段会被拒（OpenAI 兼容端点里这是常见形态）。
		body.ResponseFormat = &responseFormat{Type: "json_object"}
		body.Messages = append([]chatMessage{
			{Role: "system", Content: "只输出 JSON 对象，不要输出任何其他文字或代码围栏。"},
		}, body.Messages...)
	}

	raw, err := c.do(ctx, body)
	if err != nil {
		return "", err
	}
	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("llm: 解析响应失败: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return "", fmt.Errorf("llm: 服务端报错: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("llm: 响应无 choices")
	}
	text := strings.TrimSpace(resp.Choices[0].Message.Content)
	if text == "" {
		return "", ErrEmptyResponse
	}
	return text, nil
}

// Health 探活：发一次最小请求，确认端点与模型可用。
//
// 为什么需要它：宿主装好内核、配好端点后，第一个真实动作往往是
// 「跑一下提炼」。若端点不通，那次调用会以「提炼出 0 个候选」的形式
// 出现——**故障被伪装成了一次正常但空洞的业务结果**。
// 一个显式的探活把这类故障提前到装配期暴露。
func (c *Client) Health(ctx context.Context) error {
	_, err := c.Complete(ctx, ports.LLMRequest{
		Prompt:    "ping",
		MaxTokens: 1,
		Purpose:   "health",
	})
	return err
}

// do 发一次请求并返回原始响应体。
func (c *Client) do(ctx context.Context, body chatRequest) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: 编码请求失败: %w", err)
	}
	url := c.cfg.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: 构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	for k, v := range c.cfg.ExtraHeaders {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: 请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm: 读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 带上状态码与截断后的响应体：服务端的错误消息通常在 body 里，
		// 而只报状态码等于让宿主去猜（401？404？429？还是端点路径写错了）。
		return nil, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 512))
	}
	return raw, nil
}

// truncate 截断长文本。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ── 协议形状 ──

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}
