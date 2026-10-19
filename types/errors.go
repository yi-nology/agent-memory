package types

import (
	"errors"
	"strings"
)

// 本包的形状校验错误。
//
// 全部是**哨兵**（可 errors.Is），不用 fmt.Errorf 现场造字符串——
// 造出来的错误值消费方判不出来，最后又退化成字符串匹配。
var (
	// ErrAtomNoEvidence 事实卡无出处（QuoteEventID 为空）。
	ErrAtomNoEvidence = errors.New("atom: missing evidence event reference")
	// ErrAtomBadConfidence 置信档位非法。
	ErrAtomBadConfidence = errors.New("atom: invalid confidence level")
	// ErrAtomBadImportance 重要度档位非法。
	ErrAtomBadImportance = errors.New("atom: invalid importance level")
	// ErrAtomDoubleRetired 同时被推翻且被作废。
	ErrAtomDoubleRetired = errors.New("atom: superseded and deprecated at once")
	// ErrAtomEmptyAssertion 事实卡断言为空白。
	//
	// 断言是事实卡**唯一的内容**——没有断言的事实卡既无法召回（召回片段
	// 就是断言），也无法审计（审计看的是「它说了什么」）。
	// 空白断言卡曾能落库，进而让召回链在读它时崩溃。
	ErrAtomEmptyAssertion = errors.New("atom: empty assertion")
	// ErrRawEmptyContent 捕获事件正文为空白。
	//
	// 正文是事件在记忆流程里的唯一可检索载体：提炼的提示词只带正文、
	// 全文索引只索引正文、召回片段也只有正文。空白正文的事件落库后
	// 一路可走却处处无物——它既提炼不出候选，也召不回片段。
	ErrRawEmptyContent = errors.New("raw event: empty content")
	// ErrCandidateNoSource 候选无素材来源。
	ErrCandidateNoSource = errors.New("candidate: no raw event source")
	// ErrCandidateUnknownStatus 候选状态未知。
	ErrCandidateUnknownStatus = errors.New("candidate: unknown status")
)

// Valid 事实卡形状校验。
//
// 逐条对应 atom.go 的不变量声明。此处只校验**形状**（必填/枚举/互斥），
// 不校验内容真伪——那属于晋升层的判定，不是存储层的职责。
func (a *AtomCard) Valid() error {
	if a.ID == "" {
		return errors.New("atom: missing id")
	}
	if a.QuoteEventID == "" {
		return ErrAtomNoEvidence
	}
	switch a.Confidence {
	case ConfidenceHigh, ConfidenceMed, ConfidenceLow:
	default:
		return ErrAtomBadConfidence
	}
	switch a.Importance {
	case ImportanceHigh, ImportanceMed, ImportanceLow:
	default:
		return ErrAtomBadImportance
	}
	if a.SupersededBy != "" && !a.DeprecatedAt.IsZero() {
		return ErrAtomDoubleRetired
	}
	// 空白断言的检查放在**最后**：它是内容面的瑕疵，而上面几条是状态面的
	// 硬错误。让状态错误先报，调用方拿到的就是更可操作的那一个。
	if strings.TrimSpace(a.Assertion) == "" {
		return ErrAtomEmptyAssertion
	}
	return nil
}

// Valid 候选形状校验。
func (c *Candidate) Valid() error {
	if c.ID == "" {
		return errors.New("candidate: missing id")
	}
	if len(c.RawEventIDs) == 0 {
		return ErrCandidateNoSource
	}
	switch c.Status {
	case CandidatePending, CandidatePromoted, CandidateMerged,
		CandidateDropped, CandidateReview:
	default:
		return ErrCandidateUnknownStatus
	}
	return nil
}

// Valid 捕获事件形状校验。
func (r *RawEvent) Valid() error {
	if r.ID == "" {
		return errors.New("raw event: missing id")
	}
	switch r.Type {
	case EventTurnEnd, EventUserMessage, EventAgentMessage,
		EventToolResult, EventExternal, EventManual:
	default:
		return errors.New("raw event: unknown type")
	}
	// 空白正文的检查放在**最后**，理由同 AtomCard.Valid()：类型错是状态面的
	// 硬错误，空白正文是内容面的瑕疵，前者更可操作。
	if strings.TrimSpace(r.Content) == "" {
		return ErrRawEmptyContent
	}
	return nil
}

// Capturable 该事件是否可进入自动捕获。
//
// 判据只有一条：**是不是合成/运行期错误**。刻意不做长度/意图关键词过滤——
// 「像不像值得记的话」不是判据，来源才是。真正的取舍发生在晋升层。
func (r *RawEvent) Capturable() bool {
	return !r.Provenance.RuntimeError
}
