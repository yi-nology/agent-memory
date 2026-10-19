// Package capture 捕获面：把宿主的事件写成记忆库的素材，**落库前完成脱敏**。
//
// # 脱敏为什么在这层，而不是注入面
//
// 注入面（M2 的 recall）打码已经没有意义了——那一刻明文已经躺在库里，
// 而且能被导出成包带走。**处置点必须早于持久化**，这是唯一一处还来得及的时刻。
//
// 真实现状（本仓发现的宿主缺陷，见方案 §7.1）：宿主把会话终稿蒸馏出的断言与
// 逐字引用原样落库，那条路径上没有任何脱敏接线。本包把「必须表态」做成
// 构造期硬约束：不给 Redactor 就不让建捕获器。
package capture

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var (
	// ErrNilRedactor 未装配脱敏器。
	//
	// 缺失的默认值不是宽容，是事故：记忆是留存资产，一旦落库即可被复制出去，
	// 而「忘了配脱敏」这件事在运行期没有任何报错。
	ErrNilRedactor = errors.New("capture: 必须装配 Redactor——记忆是留存资产，脱敏必须在落库前发生")
	// ErrNilStore 未装配存储。
	ErrNilStore = errors.New("capture: 存储未装配")
)

// Input 一次捕获的输入。
type Input struct {
	// Namespace 记忆库。
	Namespace store.Namespace
	// EventID 事件标识（宿主分配，须能回溯）。
	EventID string
	// Host 产出方标识。
	Host string
	// SessionID 会话标识。
	SessionID string
	// ThreadID 线程标识。
	ThreadID string
	// User 用户标识。
	User string
	// Type 事件类型。
	Type types.RawEventType
	// Content 正文。
	Content string
	// Payload 附加载荷。
	Payload map[string]any
	// Provenance 来源凭证。
	Provenance types.Provenance
	// At 发生时刻（零值=现在）。
	At time.Time
	// Explicit 人工显式写入。
	//
	// 显式写入**不受自动捕获的判据约束**：宿主（或人）明确说「记下这条」时，
	// 即便内容来源是工具产出、即便它长得像错误，也照收——那时它表达的是
	// 「这条有价值」，而不是「这条是自动捞到的」。
	Explicit bool
}

// Capturer 捕获器。
type Capturer struct {
	store    store.RawWriter
	redactor ports.Redactor
	now      func() time.Time
}

// New 造捕获器。
func New(s store.RawWriter, r ports.Redactor) (*Capturer, error) {
	if s == nil {
		return nil, ErrNilStore
	}
	if r == nil {
		return nil, ErrNilRedactor
	}
	return &Capturer{store: s, redactor: r, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Outcome 一次捕获的结果。
type Outcome struct {
	// Event 落库的事件（被拒收时为 nil）。
	Event *types.RawEvent
	// Accepted 是否落库。
	Accepted bool
	// SkipReason 未落库的原因（Accepted 为真时为空）。
	SkipReason string
	// Redacted 是否发生了脱敏处置。
	Redacted bool
}

// Capture 捕获一条事件。
//
// 顺序是**不可交换**的：先判收不收，再打码，最后落库。
// 反过来（先落库后打码）等于把明文先写出去再擦——库里的 WAL、备份、
// 以及那一刻的任何读操作都已经看见了它。
func (c *Capturer) Capture(ctx context.Context, in Input) (*Outcome, error) {
	ev := &types.RawEvent{
		ID:         in.EventID,
		Host:       in.Host,
		SessionID:  in.SessionID,
		ThreadID:   in.ThreadID,
		User:       in.User,
		Timestamp:  in.At,
		Type:       in.Type,
		Content:    in.Content,
		Payload:    in.Payload,
		Provenance: in.Provenance,
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = c.now()
	}

	// 闸：自动捕获不收运行期错误。显式写入收——
	// 人明确说「记这条」时，他的判断优先于自动判据。
	if !in.Explicit && !ev.Capturable() {
		return &Outcome{SkipReason: "运行期错误内容不进入自动捕获"}, nil
	}

	// 正文与载荷分别处置：载荷里的敏感串常在工具回显的字段值里，
	// 只打码正文会漏掉「正文干净、元数据里有连接串」这一类。
	body, err := c.redactor.Redact(ctx, ports.RedactRequest{
		Surface: ports.SurfaceBody, Text: ev.Content,
		Namespace: string(in.Namespace), EventID: ev.ID,
	})
	if err != nil {
		return nil, err
	}
	ev.Content = body

	payload, err := redactPayload(ctx, c.redactor, ev.Payload, string(in.Namespace), ev.ID)
	if err != nil {
		return nil, err
	}
	ev.Payload = payload

	redacted := ports.RedactionApplied(in.Content, ev.Content)
	if !redacted && !mapsEqual(in.Payload, ev.Payload) {
		redacted = true
	}
	// Provenance.Redacted 是审计位：导出、对外共享时按它决定能否带出去。
	ev.Provenance.Redacted = redacted

	if err := c.store.SaveRaw(ctx, in.Namespace, ev); err != nil {
		return nil, err
	}
	return &Outcome{Event: ev, Accepted: true, Redacted: redacted}, nil
}

// CaptureBatch 批量捕获。
//
// **逐条独立**：一条被打码失败或被判拒收，不影响其余落库。批量是一个
// 便利入口，不是一个事务——把「捕获 N 条」做成原子操作，等于让最后一条的
// 故障把前面已经成功脱敏并落库的内容也回滚掉，而那些内容本来就该落库。
func (c *Capturer) CaptureBatch(ctx context.Context, in []Input) ([]*Outcome, error) {
	out := make([]*Outcome, 0, len(in))
	for _, one := range in {
		o, err := c.Capture(ctx, one)
		if err != nil {
			return out, err
		}
		out = append(out, o)
	}
	return out, nil
}

// redactPayload 逐条处置载荷里的字符串值。
//
// 只下探一层 map + 一层 slice：更深的结构（如数组套数组套对象）在本仓的
// 载荷面里不存在，而为一个假想结构写递归遍历，等于给「无界深度」开门。
// 真需要更深时，宿主可以在进捕获之前先拍平。
func redactPayload(ctx context.Context, r ports.Redactor, p map[string]any, ns, evID string) (map[string]any, error) {
	if p == nil {
		return nil, nil
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		switch tv := v.(type) {
		case string:
			s, err := r.Redact(ctx, ports.RedactRequest{
				Surface: ports.SurfacePayload, Text: tv, Namespace: ns, EventID: evID,
			})
			if err != nil {
				return nil, err
			}
			out[k] = s
		case map[string]any:
			sub, err := redactPayload(ctx, r, tv, ns, evID)
			if err != nil {
				return nil, err
			}
			out[k] = sub
		case []string:
			arr := make([]string, len(tv))
			for i, s := range tv {
				rs, err := r.Redact(ctx, ports.RedactRequest{
					Surface: ports.SurfacePayload, Text: s, Namespace: ns, EventID: evID,
				})
				if err != nil {
					return nil, err
				}
				arr[i] = rs
			}
			out[k] = arr
		default:
			out[k] = v
		}
	}
	return out, nil
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		as, aok := av.(string)
		bs, bok := bv.(string)
		if aok && bok {
			if as != bs {
				return false
			}
			continue
		}
	}
	return true
}
