// Package mcp 是记忆内核的 MCP（Model Context Protocol）服务端。
//
// # 为什么要有它
//
// 记忆内核是 Go 库，而宿主可能是任何语言。adapters/bridge 已经是跨语言的那条口，
// 但它说的是**本仓自己的方言**（方法名 memory.*、结果形状自定）——
// 宿主要先理解本仓的概念才用得上。MCP 是当下宿主接工具的通用协议，
// 故这里给同一个内核再做一个门面：宿主把记忆当**工具**调，不必先学会本仓的概念。
//
// 两个入口**平级**，不互相取代：bridge 给「愿意照本仓契约写适配」的宿主，
// MCP 给「只懂 MCP」的宿主。同一套内核，两个门面。
//
// # 协议
//
// stdio 上的 JSON-RPC 2.0，**一行一条消息**（换行分隔）。
// 不用 JSON 数组：数组要求客户端把整条响应读完才能解析，
// 而工具调用要的是「来一条、回一条」的流式往返。
//
// # 白名单：只有四个工具
//
//	memory.capture   记一条事件
//	memory.search    检索事实卡
//	memory.recall    召回（给提示词用的内容）
//	memory.stats     计数
//
// **为什么不留「执行任意动作」式的口子**：逃逸口一旦存在迟早被用——
// 先是「临时排障用一下」，然后成了默认路径，于是「宿主只能碰到它自己想干的事」
// 这条白名单形同虚设。故本包**只**暴露上面四个。
//
// 同样**不**暴露提炼与晋升：那两个要模型，且属于**宿主的编排面**
// （什么时候提炼、什么时候晋升是宿主自己的决定，不是「调个工具」能表达的）。
//
// # 命名空间由装配决定
//
// 工具参数里没有 namespace：记忆库是服务端装配时定下的，
// 由参数打开等于允许调用方在别人的库里读写。
// 归属（scope）是另一回事——那是记录级的可见性，归宿主管，故它是工具参数。
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"

	"github.com/yi-nology/agent-memory/application"
)

// JSON-RPC 错误码。
const (
	// CodeParseError 请求不是合法 JSON。
	CodeParseError = -32700
	// CodeInvalidRequest 请求结构不对（jsonrpc 版本不是 2.0 之类）。
	CodeInvalidRequest = -32600
	// CodeMethodNotFound 方法不在白名单里。
	CodeMethodNotFound = -32601
	// CodeInvalidParams 参数不对。
	CodeInvalidParams = -32602
	// CodeInternal 内部错误（工具执行失败之类）。
	CodeInternal = -32603
)

// MCP 方法名。
const (
	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodPing        = "ping"
	methodToolsList   = "tools/list"
	methodToolsCall   = "tools/call"
)

// ProtocolVersion 本端实现的 MCP 协议版本。
//
// 2024-11-05 是当前广泛实现的那一版，本包只实现它用到的四个方法。
// 版本号写错的后果不是报错，而是客户端按另一版的握手规则来，
// 而症状是「连上了但什么工具都没有」。
const ProtocolVersion = "2024-11-05"

// ServerName 本端名（initialize 的 serverInfo.name）。
const ServerName = "agent-memory"

// ServerVersion 本端版本（initialize 的 serverInfo.version）。
//
// 本仓没有版本常量单源，故这里手写一处，跟随发布走。
const ServerVersion = "0.8.0"

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

// toolResult MCP 的工具调用结果。
type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

// toolContent 工具结果的一段内容。
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server MCP 服务端。
type Server struct {
	svc *application.Service
	// failed 处理失败的消息数。
	//
	// 为什么要有它：一条坏消息不许废掉整轮（见 Run），但「不许中断」不等于
	// 「可以悄悄消失」——没人知道坏过几条，排查就没有起点。
	failed atomic.Int64
}

// New 造服务端。
func New(svc *application.Service) (*Server, error) {
	if svc == nil {
		return nil, errors.New("mcp: 服务未装配")
	}
	return &Server{svc: svc}, nil
}

// Failures 处理失败的消息数。
//
// 协议级错误（方法不存在、参数不合法）与内部错误都算进去——
// 对调用方来说都是「这一条没成」。
func (s *Server) Failures() int { return int(s.failed.Load()) }

func (s *Server) countFailure() { s.failed.Add(1) }

// Serve 处理一条 JSON-RPC 请求，返回该请求的响应字节。
//
// **通知（无 id）返回 nil 且一个字节都不写**：JSON-RPC 规范如此。
//
// 抽成独立方法的理由是它让「一条消息的处理」**不依赖进程**——
// 于是协议面的每一条分支（未知方法、坏 JSON、通知）都能单测，
// 而不必起一条真 stdio 管道。
//
// 唯一的错误出口是**响应序列化失败**：那种情况下没有可回的字节，
// 只能把问题交回调用方（Run 会记账后继续，不废掉整轮）。
func (s *Server) Serve(ctx context.Context, line []byte) ([]byte, error) {
	resp := s.handle(ctx, line)
	if resp == nil {
		return nil, nil
	}
	return json.Marshal(resp)
}

// handle 分派一条消息，返回该回的响应（通知返回 nil）。
func (s *Server) handle(ctx context.Context, line []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		s.countFailure()
		// id 给 null：解析失败时无从知道原请求的 id 是什么，
		// 而 JSON-RPC 要求这种情况回一条 id 为 null 的错误。
		return &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: CodeParseError, Message: "请求不是合法 JSON: " + err.Error()}}
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		s.countFailure()
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: CodeInvalidRequest, Message: "jsonrpc 版本只支持 2.0"}}
	}

	result, rerr := s.dispatch(ctx, req.Method, req.Params)
	if rerr != nil {
		s.countFailure()
	}
	if req.ID == nil {
		// 通知：规范如此，**不回复**。
		//
		// 为什么通知不回复：客户端按 id 对齐请求与响应。
		// 多出一条没有 id 的响应，客户端要么当孤儿丢掉（那这行白写），
		// 要么错配到下一条请求上（那就串了）。
		return nil
	}
	if rerr != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: toRPCError(rerr)}
	}
	return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
}

// dispatch 方法分派。白名单：只认下面这几个。
func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case methodInitialize:
		return map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": ServerVersion},
		}, nil

	case methodInitialized:
		// 客户端说「我准备好了」。它**总是**以通知形式来（无 id），
		// 故这里返回空结果只是为了让「客户端误带 id 发了它」时不至于报错。
		return map[string]any{}, nil

	case methodPing:
		return map[string]any{}, nil

	case methodToolsList:
		return map[string]any{"tools": toolSpecs()}, nil

	case methodToolsCall:
		return s.callTool(ctx, params)
	}
	return nil, &rpcError{Code: CodeMethodNotFound, Message: "未知方法: " + method}
}

// callTool 执行一次工具调用。
func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, error) {
	if len(params) == 0 {
		return nil, &rpcError{Code: CodeInvalidParams, Message: "tools/call 缺参数"}
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: CodeInvalidParams, Message: "参数不合法: " + err.Error()}
	}
	if p.Name == "" {
		return nil, &rpcError{Code: CodeInvalidParams, Message: "tools/call 缺工具名"}
	}
	spec, ok := lookupTool(p.Name)
	if !ok {
		// 未知工具是**协议级**错误而不是执行失败：
		// 调用方调错了名字，重试多少次都一样。
		return nil, &rpcError{Code: CodeInvalidParams, Message: "未知工具: " + p.Name}
	}
	text, err := spec.run(ctx, s, p.Arguments)
	if err != nil {
		return nil, toRPCError(err)
	}
	return toolResult{Content: []toolContent{{Type: "text", Text: text}}}, nil
}

// Run 读 stdin 的行、逐条处理、写 stdout。
//
// **一条坏消息不许废掉整轮**：处理失败只记账（Failures），然后接着处理下一条。
// 否则一条格式错的消息就把整条会话废了——而会话里可能已经攒了不少东西，
// 丢的不是一条消息，是这轮的全部。
//
// 但**协议级错误**（不是 JSON、不是合法 JSON-RPC）要按 JSON-RPC 规范回 error：
// 那是「这条消息本身有问题」，与「服务端坏了」是两回事，回了客户端才知道。
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	rd := bufio.NewReaderSize(in, 1<<20)
	wr := bufio.NewWriter(out)
	defer func() { _ = wr.Flush() }()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := rd.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			resp, serr := s.Serve(ctx, []byte(trimmed))
			switch {
			case serr != nil:
				// 处理不了这条：记账后继续，不废掉整轮。
				s.countFailure()
			case resp != nil:
				if _, werr := wr.Write(resp); werr != nil {
					return werr
				}
				if werr := wr.WriteByte('\n'); werr != nil {
					return werr
				}
				// 逐条冲刷：客户端是「发一条等一条」，
				// 攒着不冲它就永远等不到这一条的回答。
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

// toRPCError 把 Go 错误映射成协议错误。
//
// 已经带好协议码的错误直接透传：漏了这一步，
// 「参数不合法」与「内部出错」会一起降级成 internal，
// 调用方就分不清「调错了」与「服务端坏了」，只能重试到天荒地老。
func toRPCError(err error) *rpcError {
	var re *rpcError
	if errors.As(err, &re) {
		return re
	}
	return &rpcError{Code: CodeInternal, Message: err.Error()}
}
