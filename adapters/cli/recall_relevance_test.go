// 本文件钉住召回的**取数面**：查询词必须参与「捞哪几条」。
//
// 这条曾经被违反：召回的取数只按「重要度 + 新鲜度」取前 N 条，
// 查询词只参与**重排**。于是候选池与「用户问了什么」无关——
// 与查询强相关的事实只要排在池子之外，就永远进不了 prompt，
// 而重排再准也无济于事。库越大，召回越不准。

package cli_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 同一个库里，检索能找到的事实，召回也必须能找到。

func TestRecallMustBeQuerySensitive(t *testing.T) {
	ctx := context.Background()
	s, err := sqlite.OpenMemory("recall-relevance")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	ns := store.Namespace("t")
	now := time.Now().UTC()

	// 59 条「常规记录」：重要度与发生时刻**全部相同**，只让 id 排在目标前面。
	// 这样存储排序里唯一可区分的变量是 id，候选池的裁切就与重要度无关——
	// 隔离出「取数不消费查询词」这一条。
	for i := 0; i < 59; i++ {
		a := &types.AtomCard{
			ID:           fmt.Sprintf("a%02d", i),
			EntityID:     "e-common",
			Assertion:    fmt.Sprintf("服务甲的常规记录第 %d 条", i),
			QuoteEventID: fmt.Sprintf("ev-a%02d", i),
			Importance:   types.ImportanceHigh,
			Confidence:   types.ConfidenceMed,
			OccurredAt:   now,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		if err := s.SaveAtom(ctx, ns, a); err != nil {
			t.Fatal(err)
		}
	}
	// 1 条**目标**事实：断言与查询强相关，但 id 排在最后 → 被 LIMIT 裁掉。
	target := &types.AtomCard{
		ID:           "z-target",
		EntityID:     "e-target",
		Assertion:    "磁盘扩容流程必须先停机再操作",
		QuoteEventID: "ev-target",
		Importance:   types.ImportanceHigh,
		Confidence:   types.ConfidenceMed,
		OccurredAt:   now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.SaveAtom(ctx, ns, target); err != nil {
		t.Fatal(err)
	}

	// 佐证一：索引在维护——用**完整词组**能查到目标。
	hits, err := s.SearchAtoms(ctx, ns, "磁盘扩容流程必须先停机再操作", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.ID == target.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("前提不成立：FTS 索引应能按完整词组找到目标事实，实得 %d 条命中", len(hits))
	}

	// 佐证二：召回链找不到——它取的是「重要度+新鲜度」前 50 条。
	p, err := recall.New(recall.DefaultConfig(), s, ports.StaticEstimator{}, ports.MemoryBand{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Recall(ctx, recall.Request{
		Namespace: ns, Query: "磁盘扩容", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sn := range res.Snippets {
		if sn.RefID == target.ID {
			return // 目标被召回，本用例通过
		}
	}
	var got []string
	for _, sn := range res.Snippets {
		got = append(got, sn.RefID)
	}
	t.Fatalf("与查询强相关的事实未被召回：目标 %s 在库且可被 FTS 搜到，但召回结果里没有它（召回 %d 条: %s）",
		target.ID, len(res.Snippets), strings.Join(got, ","))
}
