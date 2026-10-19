package extractor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/extractor"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// fakeLLM 按提示词关键词回放预设输出。
type fakeLLM struct {
	out string
	err error
	// seen 记下收到的请求，用于断言 Purpose 这类契约。
	seen ports.LLMRequest
}

func (f *fakeLLM) Complete(_ context.Context, req ports.LLMRequest) (string, error) {
	f.seen = req
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

func events() []*types.RawEvent {
	return []*types.RawEvent{
		{ID: "e1", Content: "这台机器的磁盘长期偏小，低于 10%", Timestamp: time.Now()},
		{ID: "e2", Content: "我们决定每周清理一次旧文件", Timestamp: time.Now()},
	}
}

func ctx2() context.Context { return context.Background() }

// ── 无模型时必须显式失败 ──

// 空串会被下游读成「模型认为没什么可记」，于是整批事件被静默丢弃。
func TestExtractWithoutLLMFailsLoudly(t *testing.T) {
	e := extractor.New(nil)
	_, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if !errors.Is(err, ports.ErrNoLLM) {
		t.Fatalf("未注入模型必须报 ErrNoLLM，实得 %v", err)
	}
	if _, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events()); err == nil {
		t.Fatal("不得返回空结果 + nil 错误：那是静默丢数据")
	}
}

// NoopLLM 也一样——它是降级路径，不是「配了就等于有」。
func TestExtractWithNoopLLMFails(t *testing.T) {
	e := extractor.New(ports.NoopLLM{})
	if _, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events()); !errors.Is(err, ports.ErrNoLLM) {
		t.Fatalf("NoopLLM 必须透传 ErrNoLLM，实得 %v", err)
	}
}

// ── 正常路径 ──

func TestExtractProducesCandidates(t *testing.T) {
	llm := &fakeLLM{out: `[
	  {"assertion":"磁盘长期偏小","quote":"磁盘长期偏小","event_id":"e1",
	   "type":"fact","subject_name":"机器甲","importance":"high","confidence":"high"}
	]`}
	e := extractor.New(llm)
	cands, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("应得 1 条，实得 %d", len(cands))
	}
	c := cands[0]
	if c.Status != types.CandidatePending {
		t.Fatalf("抽出来的一律是待判定候选，实得 %s", c.Status)
	}
	if c.QuoteEventID != "e1" || len(c.RawEventIDs) != 1 || c.RawEventIDs[0] != "e1" {
		t.Fatalf("出处没挂上: %+v", c)
	}
	if c.Importance != types.ImportanceHigh || c.Confidence != types.ConfidenceHigh {
		t.Fatalf("档位解析错: %+v", c)
	}
	if c.ExtractorVersion == "" {
		t.Fatal("抽取器版本必须落候选：口径变了要能分辨历史数据")
	}
	// Purpose 是宿主选模型档位的依据，抽取是批量作业，与交互路径差着数量级。
	if llm.seen.Purpose != "memory.extract" || !llm.seen.JSON {
		t.Fatalf("请求契约不对: %+v", llm.seen)
	}
}

// ── 防稀释：引用必须真在素材里 ──

// 模型编造一段素材里没有的「引用」——这条断言将无法回溯，
// 而无法回溯的记忆既不能审计也不能推翻。
func TestFabricatedQuoteIsRejected(t *testing.T) {
	llm := &fakeLLM{out: `[
	  {"assertion":"磁盘长期偏小","quote":"磁盘长期偏小","event_id":"e1",
	   "type":"fact","subject_name":"机器甲","importance":"high","confidence":"high"},
	  {"assertion":"内存泄漏已修复","quote":"内存泄漏已修复","event_id":"e2",
	   "type":"fact","subject_name":"机器甲","importance":"high","confidence":"high"}
	]`}
	e := extractor.New(llm)
	cands, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].QuoteEventID != "e1" {
		var ids []string
		for _, c := range cands {
			ids = append(ids, c.QuoteEventID)
		}
		t.Fatalf("编造的引用必须整条作废（不修补、不猜它指哪条），实得 %v", ids)
	}
}

// 引用了不存在的事件 ID 同样作废。
func TestUnknownEventIDIsRejected(t *testing.T) {
	llm := &fakeLLM{out: `[
	  {"assertion":"磁盘长期偏小","quote":"磁盘长期偏小","event_id":"不存在的",
	   "type":"fact","subject_name":"机器甲","importance":"high","confidence":"high"}
	]`}
	e := extractor.New(llm)
	cands, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("出处不存在时整条候选作废，实得 %d 条", len(cands))
	}
}

// ── 解析严格性 ──

// 非法枚举必须报错而不是悄悄取缺省：静默取缺省会让「模型开始输出
// confidence: Very High」在数据上完全看不出来，而那是口径漂移的第一个信号。
func TestInvalidEnumIsRejected(t *testing.T) {
	for _, raw := range []string{
		`[{"assertion":"某条断言内容","quote":"某条断言内容","event_id":"e1","type":"mood","subject_name":"x","importance":"high","confidence":"high"}]`,
		`[{"assertion":"某条断言内容","quote":"某条断言内容","event_id":"e1","type":"fact","subject_name":"x","importance":"VERY HIGH","confidence":"high"}]`,
		`[{"assertion":"某条断言内容","quote":"某条断言内容","event_id":"e1","type":"fact","subject_name":"x","importance":"high","confidence":"probably"}]`,
	} {
		llm := &fakeLLM{out: raw}
		e := extractor.New(llm)
		if _, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events()); err == nil {
			t.Fatalf("非法枚举必须让抽取失败: %s", raw)
		}
	}
}

// 缺档位取「中」而不是「高」：模型说"不确定"时缺省若给高，
// 晋升层就少了一个该有的复核信号。
func TestMissingLevelDefaultsToMedium(t *testing.T) {
	llm := &fakeLLM{out: `[
	  {"assertion":"磁盘长期偏小","quote":"磁盘长期偏小","event_id":"e1",
	   "type":"fact","subject_name":"机器甲"}
	]`}
	e := extractor.New(llm)
	cands, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if err != nil {
		t.Fatal(err)
	}
	if cands[0].Confidence != types.ConfidenceMed || cands[0].Importance != types.ImportanceMed {
		t.Fatalf("缺档位应取中: %+v", cands[0])
	}
}

// ── 输出格式噪声 ──

// 模型多说两句、套个围栏是格式噪声不是内容问题。
func TestToleratesFencedJSON(t *testing.T) {
	llm := &fakeLLM{out: "好的，这是抽取结果：\n```json\n" +
		`[{"assertion":"磁盘长期偏小","quote":"磁盘长期偏小","event_id":"e1",` +
		`"type":"fact","subject_name":"机器甲","importance":"high","confidence":"high"}]` +
		"\n```\n希望有帮助。"}
	e := extractor.New(llm)
	cands, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("围栏与前后散文不该影响解析，实得 %d 条", len(cands))
	}
}

// 空数组是合法输出：确实没什么值得记。
func TestEmptyArrayIsValid(t *testing.T) {
	e := extractor.New(&fakeLLM{out: "[]"})
	cands, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("空数组应得到空候选，实得 %d", len(cands))
	}
}

// 没有 JSON 时必须报错，且要说清是什么错。
func TestNonJSONFails(t *testing.T) {
	e := extractor.New(&fakeLLM{out: "我觉得你们应该先看看日志"})
	_, err := e.Extract(ctx2(), store.Namespace("t"), "s1", events())
	if err == nil {
		t.Fatal("非 JSON 输出必须失败")
	}
	if !strings.Contains(err.Error(), "extractor") {
		t.Fatalf("错误信息应指明来自 extractor: %v", err)
	}
}

// 空素材不该白花一次模型调用。
func TestEmptyEventsSkipCall(t *testing.T) {
	llm := &fakeLLM{out: "[]"}
	e := extractor.New(llm)
	cands, err := e.Extract(ctx2(), store.Namespace("t"), "s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 || llm.seen.Prompt != "" {
		t.Fatal("空素材不该发起模型调用")
	}
}
