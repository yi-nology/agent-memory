package types

import "time"

// Candidate 一条提炼候选：从捕获事件里抽出来的「可能值得记住」的断言，
// 尚未获得事实地位。
//
// 候选是**可丢弃的中间态**。它存在的意义是把「抽出来」与「判定它值不值」
// 分成两步：抽取要调模型（贵、慢、会幻觉），判定要可复现（便宜、可回归）。
// 两步混在一起时，一次模型抖动就会把幻觉写进长期记忆且无人察觉。
type Candidate struct {
	// ID 候选标识。
	ID string
	// RawEventIDs 证据来源事件标识（非空：没有出处的断言不许成为候选）。
	RawEventIDs []string
	// Type 候选类型。
	Type CandidateType
	// Status 处置状态。
	Status CandidateStatus
	// Title 短标题（渲染与列表用，不进召回正文）。
	Title string
	// Assertion 断言正文——这条记忆「说了什么」。
	Assertion string
	// VerbatimQuote 逐字引用（可为空串，但 QuoteEventID 必须能指回一条事件）。
	VerbatimQuote string
	// QuoteEventID 引用出处的事件标识。**审计与推翻的唯一凭据**。
	QuoteEventID string
	// SubjectName 主体名（待归一，见 Entity）。
	SubjectName string
	// SubjectEntityType 主体类型（待归一）。
	SubjectEntityType EntityType
	// TargetEntityID 归一后的目标实体标识（晋升时回填）。
	TargetEntityID string
	// Confidence 置信档位。
	Confidence Confidence
	// Importance 重要度档位。
	Importance Importance
	// RecommendedAction 抽取侧给出的建议处置；**不参与最终判定**，
	// 只在人工处置时作为参考呈现。
	RecommendedAction RecommendedAction
	// PromotionReason 判定理由（规则或模型给出）。留痕面：驳回也要有说法。
	PromotionReason string
	// ExtractorVersion 抽取器版本。抽取结果的可复现性靠它区分。
	ExtractorVersion string
	// SessionID 来源会话。
	SessionID string
	// CreatedAt 创建时刻。
	CreatedAt time.Time
	// DecidedAt 判定时刻（未判定为零值）。
	DecidedAt time.Time
	// DecidedBy 判定者（未判定为空串）。
	DecidedBy DecidedBy
	// Payload 附加载荷。
	Payload map[string]any
}

// RecommendedAction 抽取侧的建议处置。
type RecommendedAction string

const (
	// ActionStore 建议记住。
	ActionStore RecommendedAction = "store"
	// ActionSkip 建议跳过。
	ActionSkip RecommendedAction = "skip"
	// ActionUnsure 抽取侧自己也没把握——升级为人工处置。
	ActionUnsure RecommendedAction = "unsure"
)

// Decided 判定层是否已表态。
//
// 转人工（review）算**已表态**——判定层的结论就是「我判不了，交给人」，
// 它不是一个悬而未决的状态，而是一个被显式选出的出口。
func (c *Candidate) Decided() bool {
	return c.Status != CandidatePending && c.Status != ""
}

// Settled 候选是否已落定事实地位（晋升 / 并入 / 丢弃）。
//
// 召回面与导出面只认落定候选：转人工的候选还没决定要不要成为事实，
// 把它当事实用等于替人做了决定。
func (c *Candidate) Settled() bool {
	switch c.Status {
	case CandidatePromoted, CandidateMerged, CandidateDropped:
		return true
	default:
		return false
	}
}
