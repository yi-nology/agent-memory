// Package bridge 跨语言宿主的入口：JSON-RPC 2.0 over stdio。
//
// # 为什么要有它
//
// 记忆内核是 Go 库，而宿主可能是任何语言。跨语言调用有两条路：进程内调用
// （要求同语言）与协议调用（本包）。协议这条路的代价是序列化与进程边界，
// 收益是**宿主不必是 Go**——而这正是「记忆组件可被任意宿主使用」的前提。
//
// # 协议
//
// 一行一个 JSON-RPC 2.0 请求，响应也是一行。方法名带 `memory.` 前缀，
// 避免与宿主自己的命名空间撞车。
//
//	memory.capture   捕获一条事件
//	memory.recall    召回（返回可注入的内容）
//	memory.search    检索事实卡
//	memory.get       取一条事实卡
//	memory.stats     计数
//
// # 两条边界纪律
//
//  1. **白名单方法**：宿主能调什么由本包列出，不提供「执行任意 SQL」式的
//     逃逸口。逃逸口一旦存在，它就迟早被用。
//  2. **错误不吞**：JSON-RPC 的 error 里带 code 与 message，宿主据此能分辨
//     「没这回事」与「出错了」。把两者混成一个字符串，宿主就只能重试到天荒地老。
package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/pipeline/capture"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// JSON-RPC 错误码。
const (
	// CodeParseError 请求不是合法 JSON。
	CodeParseError = -32700
	// CodeInvalidRequest 请求结构不对。
	CodeInvalidRequest = -32600
	// CodeMethodNotFound 方法不在白名单里。
	CodeMethodNotFound = -32601
	// CodeInvalidParams 参数不对。
	CodeInvalidParams = -32602
	// CodeInternal 内部错误。
	CodeInternal = -32603
	// CodeNotFound 目标不存在。
	CodeNotFound = -32004
	// CodeUnavailable 能力未装配（没模型等）。
	CodeUnavailable = -32005
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error 让 rpcError 能当 error 传（它同时是协议帧与 Go 错误值）。
func (e *rpcError) Error() string { return e.Message }

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Server JSON-RPC 服务端。
type Server struct {
	svc *application.Service
}

// New 造服务端。
func New(svc *application.Service) (*Server, error) {
	if svc == nil {
		return nil, errors.New("bridge: 服务未装配")
	}
	return &Server{svc: svc}, nil
}

// Serve 读 stdin 直到 EOF，逐行处理，响应写 stdout。
//
// **每条响应一行**：宿主按行读协议，跨行会读到半个响应。
// 通知（无 id）不产生响应——JSON-RPC 规范如此，且对宿主更省事。
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	rd := bufio.NewReaderSize(in, 1<<20)
	wr := bufio.NewWriter(out)
	defer func() { _ = wr.Flush() }()

	for {
		line, err := rd.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			resp, rerr := s.handle(ctx, line)
			if rerr != nil {
				return rerr
			}
			if resp != nil {
				b, merr := json.Marshal(resp)
				if merr != nil {
					return merr
				}
				if _, werr := wr.Write(append(b, '\n')); werr != nil {
					return werr
				}
				if ferr := wr.Flush(); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// Handle 处理一条请求（导出供测试与 in-process 宿主用）。
func (s *Server) Handle(ctx context.Context, line string) *rpcResponse {
	resp, _ := s.handle(ctx, line)
	return resp
}

func (s *Server) handle(ctx context.Context, line string) (*rpcResponse, error) {
	var req rpcRequest
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", Error: &rpcError{
			Code: CodeParseError, Message: "请求不是合法 JSON: " + err.Error()}}, nil
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{
			Code: CodeInvalidRequest, Message: "jsonrpc 版本只支持 2.0"}}, nil
	}

	result, rerr := s.dispatch(ctx, req.Method, req.Params)
	if req.ID == nil {
		// 通知：出错了也不回（规范如此），但错误要看得见——写 stderr。
		if rerr != nil {
			return nil, fmt.Errorf("bridge: %s: %w", req.Method, rerr)
		}
		return nil, nil
	}
	if rerr != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: toRPCError(rerr)}, nil
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}, nil
}

func toRPCError(err error) *rpcError {
	// 已经带好协议码的错误直接透传。
	//
	// 漏了这一步的后果是「方法不存在」与「参数不合法」都被降级成 internal：
	// 宿主于是分不清「调错了」与「服务端坏了」，只能重试到天荒地老。
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &rpcError{Code: CodeNotFound, Message: err.Error()}
	case errors.Is(err, capture.ErrNilRedactor), errors.Is(err, capture.ErrNilStore):
		return &rpcError{Code: CodeUnavailable, Message: err.Error()}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &rpcError{Code: CodeInternal, Message: err.Error()}
	default:
		return &rpcError{Code: CodeInternal, Message: err.Error()}
	}
}

// dispatch 方法分派。**白名单**：只认下面这几个。
func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "memory.capture":
		var p struct {
			Namespace  string           `json:"namespace"`
			EventID    string           `json:"event_id"`
			Host       string           `json:"host"`
			SessionID  string           `json:"session_id"`
			ThreadID   string           `json:"thread_id"`
			User       string           `json:"user"`
			Type       string           `json:"type"`
			Content    string           `json:"content"`
			Payload    map[string]any   `json:"payload"`
			Provenance types.Provenance `json:"provenance"`
			Explicit   bool             `json:"explicit"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		out, err := s.svc.Capture(ctx, capture.Input{
			Namespace: store.Namespace(p.Namespace), EventID: p.EventID, Host: p.Host,
			SessionID: p.SessionID, ThreadID: p.ThreadID, User: p.User,
			Type: types.RawEventType(p.Type), Content: p.Content, Payload: p.Payload,
			Provenance: p.Provenance, Explicit: p.Explicit,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"accepted": out.Accepted, "skip_reason": out.SkipReason, "redacted": out.Redacted,
		}, nil

	case "memory.recall":
		var p struct {
			Namespace string   `json:"namespace"`
			Query     string   `json:"query"`
			SessionID string   `json:"session_id"`
			ThreadID  string   `json:"thread_id"`
			Entities  []string `json:"entities"`
			Limit     int      `json:"limit"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		res, err := s.svc.Recall(ctx, recall.Request{
			Namespace: store.Namespace(p.Namespace), Query: p.Query,
			SessionID: p.SessionID, ThreadID: p.ThreadID,
			Entities: p.Entities, Limit: p.Limit,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"rendered": res.Render(), "dropped": res.Dropped,
			"dropped_by_budget":    res.DroppedByBudget,
			"dropped_by_diversity": res.DroppedByDiversity,
			"dropped_by_duplicate": res.DroppedByDuplicate,
			"entities":             res.Entities, "count": len(res.Snippets),
		}, nil

	case "memory.search":
		var p struct {
			Namespace string `json:"namespace"`
			Query     string `json:"query"`
			Limit     int    `json:"limit"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		atoms, err := s.svc.Search(ctx, store.Namespace(p.Namespace), p.Query, p.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"atoms": atoms}, nil

	case "memory.get":
		var p struct {
			Namespace string `json:"namespace"`
			ID        string `json:"id"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		a, err := s.svc.Get(ctx, store.Namespace(p.Namespace), p.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"atom": a}, nil

	case "memory.stats":
		var p struct {
			Namespace string `json:"namespace"`
		}
		if err := decodeParams(params, &p); err != nil {
			return nil, err
		}
		st, err := s.svc.Stats(ctx, store.Namespace(p.Namespace))
		if err != nil {
			return nil, err
		}
		return map[string]any{"stats": st}, nil
	}
	return nil, &rpcError{Code: CodeMethodNotFound, Message: "未知方法: " + method}
}

func decodeParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &rpcError{Code: CodeInvalidParams, Message: "参数不合法: " + err.Error()}
	}
	return nil
}
