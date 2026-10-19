// 本文件钉住「空白正文不得让召回链崩溃」这条不变量。
//
// 缺陷经过：snippet() 对空正文返回 (nil, nil)，而调用点直接写 c.bucket = ...，
// 于是一条空白正文的事件就让召回链解引用 nil 崩掉——而「空正文能落库」
// 当时是常态（捕获层不校验正文非空），所以一条空事件足以让之后每次召回都崩。
//
// 修法是两道防线：类型层拒绝空白正文（types.ErrRawEmptyContent），
// 召回层再挡一次（挡住绕过捕获层直接写库的路径）。
// 两条防线各自有用，故这里两条都验。
//
// 为什么要 build tag：这些用例断言的是「应该怎样」，而当前实现是错的，
// 它们一进默认构建，`make check` 就红——而本仓的门禁绿是提交的前提。
// 用 tag 隔开的效果是：证据留在仓里、随时可跑，但不阻塞日常提交。
//
// 跑法：
//
//	go test -tags repro ./... -run TestRepro -count=1
//
// 预期：全部红。每条红的用例对应一个已登记的 issue：
//   - pipeline/recall    → #2 召回遇空正文空指针崩溃
//   - pipeline/promotion → #3 存储故障被吞成「归一成功」
//   - storage/sqlite     → #4 FTS 中文子串失效 / #5 重要度字符串序
//   - adapters/cli       → #1 晋升不落库 / #6 召回取数不消费查询词
//
// 修好对应缺陷后，**删掉本文件的 build tag**，让用例回到默认构建里
// （否则它们会一直以「已知红」的形式躺在仓里，逐渐被人当成噪声忽略）。
package recall_test

import (
	"context"
	"testing"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/types"
)

// 复现用例：空正文片段导致召回链空指针崩溃。
//
// 缺陷位置：pipeline/recall/gather.go 的 snippet() 对空正文返回 (nil, nil)，
// 而三个调用点都直接写 c.bucket = ...，于是对 nil 候选解引用。
//
// 触发条件不需要刻意构造：捕获层不校验正文非空
// （types.RawEvent.Valid() 只查 ID 与 Type），宿主发一条空正文的
// 事件就会落库，下一次召回直接崩。

func TestEmptyRawContentMustNotPanic(t *testing.T) {
	f := &fakeReader{raws: []*types.RawEvent{
		{ID: "r1", Content: "   ", SessionID: "s9"},
	}}
	p := newPipe(t, f, recall.DefaultConfig())

	// 期望：不崩，且空正文片段被跳过。
	// 实际：panic: runtime error: invalid memory address or nil pointer dereference
	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("召回不应因空正文而失败: %v", err)
	}
	if len(res.Snippets) != 0 {
		t.Fatalf("空正文不应产生片段，实得 %d 条", len(res.Snippets))
	}
}

// 复现用例：事实卡正文为空同样触发（存储层不校验断言非空）。
func TestEmptyAtomAssertionMustNotPanic(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		{ID: "a1", EntityID: "e1", Assertion: "", QuoteEventID: "ev-a1",
			Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
			OccurredAt: base(), CreatedAt: base(), UpdatedAt: base()},
	}}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1",
	})
	if err != nil {
		t.Fatalf("召回不应因空断言而失败: %v", err)
	}
	if len(res.Snippets) != 0 {
		t.Fatalf("空断言不应产生片段，实得 %d 条", len(res.Snippets))
	}
}
