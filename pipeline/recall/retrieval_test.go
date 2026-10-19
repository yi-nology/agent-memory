package recall_test

import (
	"context"
	"testing"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件钉住召回的**取数面**：查询词必须参与「捞哪几条」。
//
// 这条曾经被违反：取数只按「重要度 + 新鲜度」取前 N 条，查询词只参与**重排**。
// 于是候选池与「用户问了什么」无关——与查询强相关的事实只要排在池子之外，
// 就永远进不了 prompt，而重排再准也无济于事。
//
// 现在是两路：**查询命中 ∪ 高价值兜底**，合并后统一重排。

// 与查询强相关的事实即便排在「重要度+新鲜度」窗口之外，也必须被召回。
func TestQueryHitOutsideValueWindowIsStillRecalled(t *testing.T) {
	// 20 条高重要度的「常规记录」——它们会占满 ListAtoms 的窗口。
	var atoms []*types.AtomCard
	for i := 0; i < 20; i++ {
		atoms = append(atoms, atom("common", "e-common", "服务甲的常规记录", types.ImportanceHigh))
	}
	// 唯一与查询强相关的一条。
	atoms = append(atoms, atom("target", "e-target", "磁盘扩容流程必须先停机", types.ImportanceMed))

	f := &fakeReader{atoms: atoms}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘扩容", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range res.Snippets {
		if s.RefID == "target" {
			found = true
		}
	}
	if !found {
		t.Fatalf("与查询强相关的事实未被召回（召回 %d 条）", len(res.Snippets))
	}
}

// 无查询词时，兜底路（重要度/新鲜度）仍然生效。
func TestFallbackPathWorksWithoutQuery(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) == 0 {
		t.Fatal("无查询词时也要召回：最近/最重要的事实本身就是要召回的")
	}
}

// 退役事实不得借检索路进 prompt。
//
// 检索**故意**能搜到已退役的事实（「当初为什么这么判」要能追问），
// 所以召回这一侧必须显式滤掉——
// 否则一条被推翻的事实会照进 prompt，而它在数据上完全看不出异常。
func TestRetiredAtomMustNotEnterPromptViaSearch(t *testing.T) {
	live := atom("live", "e1", "磁盘长期偏小", types.ImportanceHigh)
	dead := atom("dead", "e1", "磁盘扩容流程", types.ImportanceHigh)
	dead.SupersededBy = "live" // 软退役：被继任者推翻
	f := &fakeReader{atoms: []*types.AtomCard{live, dead}}

	p := newPipe(t, f, recall.DefaultConfig())
	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘扩容", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Snippets {
		if s.RefID == "dead" {
			t.Fatal("已退役的事实不得进 prompt：它看着是对的，只是过时了")
		}
	}
}

// 两路合并不得产生重复片段（同一张卡被两条路各捞一次）。
func TestTwoPathsMustNotDuplicateSnippet(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "磁盘长期偏小", types.ImportanceHigh),
	}}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range res.Snippets {
		if seen[s.RefID] {
			t.Fatalf("同一条事实出现两次: %s", s.RefID)
		}
		seen[s.RefID] = true
	}
}
