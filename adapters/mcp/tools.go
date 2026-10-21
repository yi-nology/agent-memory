package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// toolSpec 一个工具的定义与执行体。
//
// 定义（name/description/inputSchema）是给**模型**看的：模型据此决定
// 调不调、怎么传参。执行体是给**内核**看的：它只认白名单里的那几张脸。
type toolSpec struct {
	// Name 工具名（tools/list 与 tools/call 都用它）。
	Name string `json:"name"`
	// Description 一句话说明，给模型看的。
	Description string `json:"description"`
	// InputSchema 参数的 JSON Schema。
	InputSchema map[string]any `json:"inputSchema"`
	// run 执行体。返回给模型看的文本。
	run func(ctx context.Context, s *Server, args json.RawMessage) (string, error)
}

// toolSpecs 工具白名单。
//
// **只有这四个，且刻意不提供「执行任意动作」式的口子**：
// 逃逸口一旦存在迟早被用——先是「临时排障用一下」，然后成了默认路径，
// 于是「宿主只能碰到它自己想干的事」这条白名单形同虚设。
//
// 同样不提供提炼与晋升：那两个要模型，且属于**宿主的编排面**
// （什么时候提炼、什么时候晋升是宿主自己的决定，不是「调个工具」能表达的）。
func toolSpecs() []toolSpec {
	return []toolSpec{
		{
			Name:        "memory.capture",
			Description: "记一条事件到长期记忆库。落库前已脱敏，返回是否受理。",
			InputSchema: objectSchema(map[string]any{
				"event_id":   stringProp("事件标识。宿主分配，须能回溯到来源。"),
				"content":    stringProp("事件正文。空白正文不受理。"),
				"type":       stringProp("事件类型：turn_end | user_message | agent_message | tool_result | external | manual。"),
				"session_id": stringProp("会话标识。"),
				"thread_id":  stringProp("线程标识。"),
				"user":       stringProp("用户标识。"),
				"scope":      stringProp("记录级归属（空=全局可见）。"),
			}, "event_id", "content", "type"),
			run: runCapture,
		},
		{
			Name:        "memory.search",
			Description: "检索事实卡。按关键词返回匹配的事实卡。",
			InputSchema: objectSchema(map[string]any{
				"query": stringProp("检索词。"),
				"limit": intProp("返回条数上限（0=后端默认）。"),
				"scope": stringProp("读者归属（空=不隔离）。"),
			}, "query"),
			run: runSearch,
		},
		{
			Name:        "memory.recall",
			Description: "召回与当前话题相关的记忆，返回可直接放进提示词的内容。",
			InputSchema: objectSchema(map[string]any{
				"query":      stringProp("召回查询（用户当轮的提问或主题）。"),
				"session_id": stringProp("当前会话标识。用于防回声：本会话的原始事件不进提示词。"),
				"thread_id":  stringProp("当前线程标识。"),
				"entities":   arrayProp("宿主已解析出的实体标识。"),
				"scope":      stringProp("读者归属（空=不隔离）。"),
				"limit":      intProp("条数上限（0=用配置）。"),
			}, "query"),
			run: runRecall,
		},
		{
			Name:        "memory.stats",
			Description: "取记忆库的计数（事件、候选、事实卡、实体、页面等）。",
			InputSchema: objectSchema(nil),
			run:         runStats,
		},
	}
}

// lookupTool 按名字找工具。
func lookupTool(name string) (toolSpec, bool) {
	for _, spec := range toolSpecs() {
		if spec.Name == name {
			return spec, true
		}
	}
	return toolSpec{}, false
}

// runCapture 记一条事件。
func runCapture(ctx context.Context, s *Server, args json.RawMessage) (string, error) {
	var p struct {
		EventID   string `json:"event_id"`
		Content   string `json:"content"`
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
		User      string `json:"user"`
		Scope     string `json:"scope"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return "", err
	}
	if p.EventID == "" {
		return "", &rpcError{Code: CodeInvalidParams, Message: "event_id 不能为空：事件标识是回溯的凭据"}
	}
	if strings.TrimSpace(p.Content) == "" {
		return "", &rpcError{Code: CodeInvalidParams, Message: "content 不能为空：空白正文的事件既提炼不出候选，也召不回片段"}
	}
	if !knownEventType(p.Type) {
		return "", &rpcError{Code: CodeInvalidParams,
			Message: "type 不认识，只能是 turn_end | user_message | agent_message | tool_result | external | manual，实得 " + p.Type}
	}
	out, err := s.svc.Capture(ctx, application.CaptureInput{
		Namespace: s.svc.Namespace(),
		EventID:   p.EventID,
		SessionID: p.SessionID,
		ThreadID:  p.ThreadID,
		User:      p.User,
		Scope:     types.Scope(p.Scope),
		Type:      types.RawEventType(p.Type),
		Content:   p.Content,
	})
	if err != nil {
		return "", err
	}
	return jsonText(map[string]any{
		"accepted":    out.Accepted,
		"redacted":    out.Redacted,
		"skip_reason": out.SkipReason,
	})
}

// runSearch 检索事实卡。
func runSearch(ctx context.Context, s *Server, args json.RawMessage) (string, error) {
	var p struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
		Scope string `json:"scope"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Query) == "" {
		return "", &rpcError{Code: CodeInvalidParams, Message: "query 不能为空"}
	}
	if p.Limit < 0 {
		return "", &rpcError{Code: CodeInvalidParams, Message: "limit 不能为负"}
	}
	atoms, err := s.svc.Search(ctx, s.svc.Namespace(), p.Query, store.AtomFilter{
		Limit: p.Limit,
		Scope: types.Scope(p.Scope),
		// 只要**现在还成立**的事实：一条被推翻或作废的卡，
		// 调用方从返回内容里分辨不出来（投影里没有「这条已退役」这一栏），
		// 于是它会被当成当前事实用——而那在结果里完全看不出异常。
		// 追问「当初为什么这么判」是审计面的事，不走这个工具。
		ActiveOnly: true,
	})
	if err != nil {
		return "", err
	}
	views := make([]atomView, 0, len(atoms))
	for _, a := range atoms {
		if a == nil {
			continue
		}
		views = append(views, atomView{
			ID:         a.ID,
			EntityID:   a.EntityID,
			Assertion:  a.Assertion,
			Scope:      string(a.Scope),
			Importance: string(a.Importance),
			Confidence: string(a.Confidence),
		})
	}
	return jsonText(views)
}

// runRecall 召回：返回可直接放进提示词的内容。
func runRecall(ctx context.Context, s *Server, args json.RawMessage) (string, error) {
	var p struct {
		Query     string   `json:"query"`
		SessionID string   `json:"session_id"`
		ThreadID  string   `json:"thread_id"`
		Entities  []string `json:"entities"`
		Scope     string   `json:"scope"`
		Limit     int      `json:"limit"`
	}
	if err := decodeArgs(args, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Query) == "" {
		return "", &rpcError{Code: CodeInvalidParams, Message: "query 不能为空"}
	}
	if p.Limit < 0 {
		return "", &rpcError{Code: CodeInvalidParams, Message: "limit 不能为负"}
	}
	res, err := s.svc.Recall(ctx, recall.Request{
		Namespace: s.svc.Namespace(),
		Query:     p.Query,
		SessionID: p.SessionID,
		ThreadID:  p.ThreadID,
		Entities:  p.Entities,
		Scope:     types.Scope(p.Scope),
		Limit:     p.Limit,
	})
	if err != nil {
		return "", err
	}
	rendered := res.Render()
	if rendered == "" {
		// 召回为空时**说明**它为空：空串让调用方分不清「召回失败」与「确实没有」。
		// （召回层自己返回空串是给「注入提示词」用的——那里空串不占预算；
		// 这里是工具结果，调用方要的是「确实没有」这个事实。）
		return "（没有召回到内容）", nil
	}
	return rendered, nil
}

// runStats 取计数。
func runStats(ctx context.Context, s *Server, _ json.RawMessage) (string, error) {
	st, err := s.svc.Stats(ctx, s.svc.Namespace())
	if err != nil {
		return "", err
	}
	return jsonText(map[string]any{
		"raw_events":   st.RawEvents,
		"candidates":   st.Candidates,
		"pending":      st.Pending,
		"atoms":        st.Atoms,
		"active_atoms": st.ActiveAtoms,
		"entities":     st.Entities,
		"pages":        st.Pages,
		"dirty_pages":  st.DirtyPages,
		"episodes":     st.Episodes,
	})
}

// atomView 事实卡给模型看的投影。
//
// 刻意**不**给整张卡：卡上还有出处、检索词、签名那些字段，
// 对「模型判断这条事实说了什么」没有帮助，却把上下文塞满。
type atomView struct {
	ID         string `json:"id"`
	EntityID   string `json:"entity_id"`
	Assertion  string `json:"assertion"`
	Scope      string `json:"scope"`
	Importance string `json:"importance"`
	Confidence string `json:"confidence"`
}

// knownEventType 事件类型是不是本仓认识的那几个。
//
// 类型是**闭集**：写错的话，存储层会拒收并抛一句「unknown type」，
// 而那句里没有「你该写什么」。在工具边界就报清楚，调用方才知道怎么改。
func knownEventType(t string) bool {
	switch types.RawEventType(t) {
	case types.EventTurnEnd, types.EventUserMessage, types.EventAgentMessage,
		types.EventToolResult, types.EventExternal, types.EventManual:
		return true
	}
	return false
}

// decodeArgs 解工具参数。
//
// 未知字段**不**报错：MCP 客户端可能自带 "_meta" 之类的附加字段，
// 报错了就是「客户端加个字段就调不通」。类型错才报——那是真调错了。
func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &rpcError{Code: CodeInvalidParams, Message: "参数不合法: " + err.Error()}
	}
	return nil
}

// jsonText 把一段结构渲染成给模型看的 JSON 文本。
func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// objectSchema 造一个对象型参数的 JSON Schema。
func objectSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object"}
	if len(props) > 0 {
		schema["properties"] = props
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func arrayProp(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}
