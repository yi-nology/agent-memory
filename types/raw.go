package types

import "time"

// RawEventType 捕获事件的类型面。
//
// 刻意**不收**「用户提问 / 模型回答 / 工具调用」这类宿主语义枚举——
// 那是宿主编排的东西。本包只保留「这条事件在记忆流程里扮演什么角色」。
type RawEventType string

const (
	// EventTurnEnd 一轮对话结束（宿主给的一次性素材，通常是终稿）。
	EventTurnEnd RawEventType = "turn_end"
	// EventUserMessage 用户消息。
	EventUserMessage RawEventType = "user_message"
	// EventAgentMessage 智能体消息。
	EventAgentMessage RawEventType = "agent_message"
	// EventToolResult 工具产出。**高危来源**：命令输出 / 抓取正文都在这里，
	// 落库前的脱敏面主要就针对这一类。
	EventToolResult RawEventType = "tool_result"
	// EventExternal 外部推送（webhook、第三方回调、人工补录）。
	EventExternal RawEventType = "external"
	// EventManual 人工显式写入。
	EventManual RawEventType = "manual"
)

// OriginKind 事件的来源性质。这是**结构化的来源声明**，不是关键词猜测。
type OriginKind string

const (
	// OriginUser 用户产生的输入。
	OriginUser OriginKind = "user"
	// OriginModel 模型产出。
	OriginModel OriginKind = "model"
	// OriginTool 工具产出（不可信内容的现实入口）。
	OriginTool OriginKind = "tool"
	// OriginExternal 外部系统推送。
	OriginExternal OriginKind = "external"
	// OriginOperator 人工显式写入（管理侧补录、裁决、修正）。
	OriginOperator OriginKind = "operator"
)

// Provenance 来源凭证。
//
// # 为什么做成结构而不是 payload 键
//
// 记忆一旦跨会话持久化并可导出，「这条内容从哪来」就成了**治理判据**而非元数据：
// 自动捕获要不要收录它、导出时要不要带上它、出问题时怎么回溯，全靠它。
// 用 `payload["octop_runtime_error"]` 这种字符串键承载，判读面就退化成
// 文本匹配——键名写错没有任何编译期信号。
//
// # RuntimeError 的用法
//
// 合成/运行期错误（宿主自己造出来的诊断文本、失败回执）**必须**标 true，
// 自动捕获据此排除，recency 与 importance 排序也据此降权。判读只读这个布尔位，
// **不做意图关键词黑名单**——「看起来像报错的话」不是判据，来源是。
type Provenance struct {
	// Origin 来源性质。
	Origin OriginKind
	// Host 产出这条内容的宿主标识（多宿主共用一个记忆库时区分用）。
	Host string
	// RuntimeError 是否为合成/运行期错误。true 时不得进入自动捕获与自动提炼。
	RuntimeError bool
	// Redacted 落库前是否已做过脱敏处置。
	Redacted bool
	// Tool 产出该内容时的工具标识（Origin=tool 时有值）。
	Tool string
}

// RawEvent 一条捕获事件：记忆流程的**素材**，尚未被判定是否值得记住。
//
// 素材可以任意多、任意杂；进不进长期记忆由晋升层决定，不在捕获层决定。
type RawEvent struct {
	// ID 事件标识（宿主分配，须全局唯一且可回溯）。
	ID string
	// Host 产出该事件的宿主标识。
	Host string
	// SessionID 会话标识（宿主资产，本包不认识其结构）。
	SessionID string
	// ThreadID 线程标识（宿主资产）。
	ThreadID string
	// User 用户标识。
	User string
	// Timestamp 发生时刻。
	Timestamp time.Time
	// Type 事件类型。
	Type RawEventType
	// Content 正文。落库前**必须**已过脱敏面（见 pipeline/capture）。
	Content string
	// Payload 结构化附加载荷。
	Payload map[string]any
	// Provenance 来源凭证（见其文件注释）。
	Provenance Provenance
}

// CandidateStatus 提炼候选的处置状态。
type CandidateStatus string

const (
	// CandidatePending 待判定。
	CandidatePending CandidateStatus = "pending"
	// CandidatePromoted 已晋升为事实卡。
	CandidatePromoted CandidateStatus = "promoted"
	// CandidateMerged 已并入既有事实卡。
	CandidateMerged CandidateStatus = "merged"
	// CandidateDropped 判定为不值得记（无价值 / 证据不足 / 纯重复）。
	CandidateDropped CandidateStatus = "dropped"
	// CandidateReview **留给人处置**：矛盾、实体归一存疑、需人工裁决。
	//
	// 「判定不了就不判」是晋升层的默认姿态——把可疑内容自动写进长期记忆，
	// 比漏记一条的代价高得多。
	CandidateReview CandidateStatus = "review"
)

// DecidedBy 判定者。
type DecidedBy string

const (
	// DecidedRule 规则判定（无模型时也可工作）。
	DecidedRule DecidedBy = "rule"
	// DecidedModel 模型判定。
	DecidedModel DecidedBy = "model"
	// DecidedUser 人工处置。
	DecidedUser DecidedBy = "user"
)

// CandidateType 候选的类型面。
type CandidateType string

const (
	// CandidateFact 稳定事实。
	CandidateFact CandidateType = "fact"
	// CandidateDecision 决策及其理由。
	CandidateDecision CandidateType = "decision"
	// CandidatePreference 偏好。
	CandidatePreference CandidateType = "preference"
	// CandidateProcedure 可复用方法。
	CandidateProcedure CandidateType = "procedure"
)

// Confidence 置信档位。刻意用**离散档**而非浮点：
// 浮点置信度会诱使人写 0.73 > 0.7 这类微调，而记忆的取舍是「够不够稳」，
// 离散档让阈值判定成为查表而不是算术。
type Confidence string

const (
	ConfidenceHigh Confidence = "high"
	ConfidenceMed  Confidence = "medium"
	ConfidenceLow  Confidence = "low"
)

// ConfidenceRank 档位排序用（越大越可信）。
func (c Confidence) Rank() int {
	switch c {
	case ConfidenceHigh:
		return 3
	case ConfidenceMed:
		return 2
	case ConfidenceLow:
		return 1
	default:
		return 0
	}
}

// Importance 重要度档位（离散，理由同 Confidence）。
type Importance string

const (
	ImportanceHigh Importance = "high"
	ImportanceMed  Importance = "medium"
	ImportanceLow  Importance = "low"
)

// ImportanceRank 档位排序用（越大越重要）。
func (i Importance) Rank() int {
	switch i {
	case ImportanceHigh:
		return 3
	case ImportanceMed:
		return 2
	case ImportanceLow:
		return 1
	default:
		return 0
	}
}
