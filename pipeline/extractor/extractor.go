// Package extractor 提炼面：从捕获事件里抽���「可能值得记住」的断言。
//
// # 分层理由
//
// 抽取要调模型（贵、慢、**会幻觉**），判定要可复现（便宜、能回归）。两者混在
// 一起时，一次模型抖动就把幻觉写进长期记忆，而且没人察觉——数据看着完全正常。
// 故本包只负责「抽」，抽出来的东西一律是**待判定候选**（types.Candidate），
// 判定在 pipeline/promotion。
//
// # 没有模型时必须报错，不许返回空结果
//
// 这是 M0 立下的纪律：返回空串会被下游读成「模型认为没什么可记」，
// 于是整批事件被静默丢弃，而运行期一个错都没有。ports.ErrNoLLM 是显式失败。
package extractor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// ErrNoLLM 未注入模型能力。
var ErrNoLLM = ports.ErrNoLLM

// Version 抽取器版本。写进每条候选，故抽取口径变化时能分辨历史数据。
const Version = "extractor/1"

// PromptFunc 构造提示词。抽出来是因为提示词是**产品面**：宿主可能要用
// 自己的措辞与语言风格，而本包不该持有那些。
type PromptFunc func(events []*types.RawEvent, maxCandidates int) string

// DefaultPrompt 缺省提示词。
//
// 极简：只给「要什么形状的输出」，不教模型怎么判断价值——那是晋升层的事。
// 教了会得到一个「既在抽取又在判定」的黑箱，出错时无法归因到某一层。
func DefaultPrompt(events []*types.RawEvent, maxCandidates int) string {
	var b strings.Builder
	b.WriteString("从下面的对话事件里抽取可能值得长期记住的稳定事实。\n")
	b.WriteString("只输出 JSON 数组，每个元素包含：\n")
	b.WriteString(`  assertion      断言正文（这条记忆说了什么）` + "\n")
	b.WriteString(`  quote          逐字引用（必须原样出现在事件文本里，不得改写）` + "\n")
	b.WriteString(`  event_id       引用所在事件的 id` + "\n")
	b.WriteString(`  type           fact | decision | preference | procedure` + "\n")
	b.WriteString(`  subject_name   断言的主体名` + "\n")
	b.WriteString(`  importance     high | medium | low` + "\n")
	b.WriteString(`  confidence     high | medium | low` + "\n")
	b.WriteString(fmt.Sprintf("最多 %d 条；没有值得记的就输出空数组。\n", maxCandidates))
	b.WriteString("事件：\n")
	for _, e := range events {
		b.WriteString(fmt.Sprintf("- [%s] %s\n", e.ID, e.Content))
	}
	return b.String()
}

// Extractor 提炼器。
type Extractor struct {
	llm     ports.LLM
	prompt  PromptFunc
	max     int
	now     func() time.Time
	version string
	// strictQuote 逐字引用校验。
	//
	// 缺省为 true。关掉它等于放弃唯一一道防幻觉闸：模型只要「引用」一段
	// 素材里没有的话，这条断言就无法回溯，而无法回溯的记忆既不能审计
	// 也不能推翻（见 types.AtomCard 的不变量 1）。
	strictQuote bool
}

// Option 构造选项。
type Option func(*Extractor)

// WithPrompt 自定义提示词。
func WithPrompt(f PromptFunc) Option { return func(e *Extractor) { e.prompt = f } }

// WithMaxCandidates 限制单次抽取条数。
func WithMaxCandidates(n int) Option { return func(e *Extractor) { e.max = n } }

// WithStrictQuote 开关逐字引用校验（**没有理由关**，仅供复现历史数据时用）。
func WithStrictQuote(on bool) Option { return func(e *Extractor) { e.strictQuote = on } }

// New 造提炼器。
func New(llm ports.LLM, opts ...Option) *Extractor {
	e := &Extractor{
		llm:         llm,
		prompt:      DefaultPrompt,
		max:         20,
		now:         func() time.Time { return time.Now().UTC() },
		version:     Version,
		strictQuote: true,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// rawCandidate 模型输出的单条形态。
type rawCandidate struct {
	Assertion   string `json:"assertion"`
	Quote       string `json:"quote"`
	EventID     string `json:"event_id"`
	Type        string `json:"type"`
	SubjectName string `json:"subject_name"`
	Importance  string `json:"importance"`
	Confidence  string `json:"confidence"`
}

// ParseError 解析失败。
type ParseError struct{ Reason string }

func (e *ParseError) Error() string { return "extractor: 解析失败: " + e.Reason }

// Extract 从事件里抽候选。**不落库**——落库由调用方在看过结果后决定。
func (e *Extractor) Extract(ctx context.Context, _ store.Namespace, sessionID string,
	events []*types.RawEvent) ([]*types.Candidate, error) {
	if e.llm == nil {
		return nil, ErrNoLLM
	}
	if len(events) == 0 {
		return nil, nil
	}

	out, err := e.llm.Complete(ctx, ports.LLMRequest{
		Prompt: e.prompt(events, e.max),
		// Purpose 交给宿主选模型档位：抽取是批量作业，与交互路径差着数量级。
		Purpose:   "memory.extract",
		JSON:      true,
		MaxTokens: e.max * 160,
	})
	if err != nil {
		return nil, err
	}

	raw, err := Parse(out)
	if err != nil {
		return nil, err
	}

	byID := map[string]*types.RawEvent{}
	for _, ev := range events {
		byID[ev.ID] = ev
	}
	now := e.now()
	cands := make([]*types.Candidate, 0, len(raw))
	for i, r := range raw {
		c, ok := e.toCandidate(r, i, sessionID, byID, now)
		if !ok {
			continue
		}
		cands = append(cands, c)
	}
	return cands, nil
}

// toCandidate 校验并组装一条候选。
//
// 校验顺序是刻意的：**先查引用出处，再谈其余**。出处不成立时这条断言
// 就没有存在的资格，后面几项检查的结论都建立在一段可能不存在的话上。
func (e *Extractor) toCandidate(r rawCandidate, idx int, sessionID string,
	byID map[string]*types.RawEvent, now time.Time) (*types.Candidate, bool) {

	if strings.TrimSpace(r.Assertion) == "" {
		return nil, false
	}
	ev, ok := byID[r.EventID]
	if !ok {
		// 引用了一个不存在的事件：整条候选作废。**不修补**——猜它指哪条
		// 等于替模型编造证据，而没有证据的记忆既不能审计也不能推翻。
		return nil, false
	}
	if e.strictQuote && !QuoteAppears(ev.Content, r.Quote) {
		return nil, false
	}

	c := &types.Candidate{
		ID:                fmt.Sprintf("cand-%d-%s", now.UnixNano(), ev.ID),
		RawEventIDs:       []string{ev.ID},
		Type:              parseType(r.Type),
		Status:            types.CandidatePending,
		Assertion:         strings.TrimSpace(r.Assertion),
		VerbatimQuote:     strings.TrimSpace(r.Quote),
		QuoteEventID:      ev.ID,
		SubjectName:       strings.TrimSpace(r.SubjectName),
		Confidence:        parseConfidence(r.Confidence),
		Importance:        parseImportance(r.Importance),
		RecommendedAction: types.ActionStore,
		ExtractorVersion:  e.version,
		SessionID:         sessionID,
		CreatedAt:         now,
	}
	if c.VerbatimQuote == "" {
		c.RecommendedAction = types.ActionUnsure
	}
	return c, true
}

// QuoteAppears 逐字引用是否真出现在素材里。
//
// 归一化只做空白折叠与大小写无关：过度归一会让「改了一个标点」的引用通过，
// 而逐字引用的全部价值就在于**可核对**。判据是「读者能在原文里找到它」。
func QuoteAppears(content, quote string) bool {
	q := normalizeForMatch(quote)
	if q == "" {
		return false
	}
	return strings.Contains(normalizeForMatch(content), q)
}

func normalizeForMatch(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Parse 解析模型输出。
//
// 刻意**严格**：JSON 里的枚举值只认写得出的大小写，非法枚举一律拒绝而不是
// 悄悄取缺省——静默取缺省会让「模型开始输出 confidence: Very High」这件事
// 在数据上完全看不出来，而那正是口径开始漂移的第一个信号。
func Parse(raw string) ([]rawCandidate, error) {
	body := extractJSONArray(raw)
	if body == "" {
		return nil, &ParseError{Reason: "输出里找不到 JSON 数组"}
	}
	var list []rawCandidate
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		return nil, &ParseError{Reason: err.Error()}
	}
	for _, r := range list {
		if err := validate(r); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func validate(r rawCandidate) error {
	switch parseTypeRaw(r.Type) {
	case string(types.CandidateFact), string(types.CandidateDecision),
		string(types.CandidatePreference), string(types.CandidateProcedure):
	default:
		return &ParseError{Reason: "未知 type: " + r.Type}
	}
	for name, v := range map[string]string{"importance": r.Importance, "confidence": r.Confidence} {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "high", "medium", "low", "":
		default:
			return &ParseError{Reason: fmt.Sprintf("未知 %s: %q", name, v)}
		}
	}
	return nil
}

func parseTypeRaw(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func parseType(s string) types.CandidateType {
	switch parseTypeRaw(s) {
	case string(types.CandidateDecision):
		return types.CandidateDecision
	case string(types.CandidatePreference):
		return types.CandidatePreference
	case string(types.CandidateProcedure):
		return types.CandidateProcedure
	default:
		return types.CandidateFact
	}
}

// parseConfidence 置信档位；缺失或非法取「中」。
//
// 缺省取**中**而不是「高」：模型说"不确定"时，缺省若给高，晋升层就少了
// 一个该有的复核信号。非法值在 Parse 阶段已被 validate 拦掉，所以这里的
// default 分支只服务于「模型压根没给」。
func parseConfidence(s string) types.Confidence {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return types.ConfidenceHigh
	case "low":
		return types.ConfidenceLow
	default:
		return types.ConfidenceMed
	}
}

// parseImportance 重要度档位；缺失或非法取「中」，理由同 parseConfidence。
func parseImportance(s string) types.Importance {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return types.ImportanceHigh
	case "low":
		return types.ImportanceLow
	default:
		return types.ImportanceMed
	}
}

// extractJSONArray 从模型输出里抠出 JSON 数组。
//
// 容忍首尾的散文与 ``` 围栏：模型总会多说两句，那是格式噪声不是内容问题。
// 但**不做**「找到第一个 { 就截到最后一个 }」这种宽松切法——那会把两段
// 并列 JSON 粘成一段，报一个和真实病因无关的语法错。
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		if j := strings.Index(s[i+3:], "```"); j >= 0 {
			block := strings.TrimSpace(s[i+3 : i+3+j])
			block = strings.TrimPrefix(block, "json") // 围栏的语言标记
			s = strings.TrimSpace(block)
		}
	}
	if i := strings.Index(s, "["); i >= 0 {
		if j := strings.LastIndex(s, "]"); j > i {
			return s[i : j+1]
		}
	}
	return ""
}
