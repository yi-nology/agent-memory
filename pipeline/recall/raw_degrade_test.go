package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：GlobalRawReader 降级路径的**可观测性**（issue #63）。
//
// # 降级是什么
//
// 存储没实现 store.GlobalRawReader（可选能力）时，全局读者的 raw 取数
// 降级为 ListRaw(空 scope=不隔离) + 后置 VisibleTo 滤除：正确性由滤除
// 兜着（绝不进别家的事件），但**先取后滤**——被滤掉的条目已经花掉了
// MaxRaw 配额。
//
// # 要钉住的是什么
//
// 那段配额损失必须**可归因**：Result.Drops[ports.DropDegradedScope]
// 等于被后置滤掉的条数。没有它，症状只是「raw 源召回变少」——
// 看着像内容不够，而病因是「宿主没实现那个可选能力」，两者处置不同。
// 与 optional 能力「降级可观测」纪律同一条（宽松路、语义全局路同款）。
//
// 两条路径都走同一个 fakeReader 的**数据**，只差存储有没有实现那个
// 可选能力——差异只可能来自降级机制本身（判别特征干净）。

// rawEvent 造一条混合归属的事件。
func rawEvent(id string, scope types.Scope) *types.RawEvent {
	return &types.RawEvent{
		ID: id, Content: "事件正文-" + id,
		Timestamp: base(), Type: types.EventManual,
		Scope: scope,
	}
}

// mixedRawStore 造一个带 raw 源的假存储：2 条全局 + 3 条别归属（team-b）。
func mixedRawStore() *fakeReader {
	return &fakeReader{raws: []*types.RawEvent{
		rawEvent("g1", ""),
		rawEvent("g2", ""),
		rawEvent("f1", "team-b"),
		rawEvent("f2", "team-b"),
		rawEvent("f3", "team-b"),
	}}
}

// globalRawStore 在同一份数据上实现 store.GlobalRawReader：
// 只回全局事件（与 in-repo 后端的 ListRawGlobal 同判据）。
type globalRawStore struct {
	*fakeReader
}

func (g *globalRawStore) ListRawGlobal(_ context.Context, _ store.Namespace,
	_ time.Time, _ store.Page) ([]*types.RawEvent, error) {
	var out []*types.RawEvent
	for _, e := range g.raws {
		if e.Scope == "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// recallEmptyScope 全局读者的一次召回（带指标面）。
func recallEmptyScope(t *testing.T, f recall.Reader) (*recall.Result, *ports.MemMetrics) {
	t.Helper()
	cfg := recall.DefaultConfig()
	met := ports.NewMemMetrics()
	cfg.Metrics = met
	p := newPipe(t, f, cfg)
	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "", Scope: ""})
	if err != nil {
		t.Fatal(err)
	}
	return res, met
}

// TestRawDegradedScopeAttributed 降级宿主下，被后置滤掉的条数归因到
// DropDegradedScope：统计能区分「真 TopK」与「降级后置滤」。
func TestRawDegradedScopeAttributed(t *testing.T) {
	f := mixedRawStore() // 没实现 store.GlobalRawReader ⇒ 降级
	res, met := recallEmptyScope(t, f)

	// fakeReader.ListRaw(空 scope) 不隔离，5 条全取回；后置滤掉 3 条别归属的
	// ——那 3 条就是被占掉的配额，必须看得到。
	if got := res.Drops[ports.DropDegradedScope]; got != 3 {
		t.Fatalf("降级后置滤的配额损失应归因 3 条，实得 %v（drops=%v）", got, res.Drops)
	}
	if res.Dropped < 3 {
		t.Fatalf("丢弃总数应含降级滤除，实得 %d", res.Dropped)
	}
	// 同一损失也进了指标面（observe 对 Drops 逐项打点）。
	if got := met.Snapshot().Counts["recall.dropped.degraded_scope"]; got != 3 {
		t.Fatalf("指标 recall.dropped.degraded_scope 应为 3，实得 %v", got)
	}
	// 正确性照旧兜着：别归属的事件一条都不进 prompt（本库没有事实卡，
	// 任何片段都是 raw 源的）。
	for _, s := range res.Snippets {
		if s.RefID == "f1" || s.RefID == "f2" || s.RefID == "f3" {
			t.Fatalf("别归属的事件漏进了 prompt: %+v", res.Snippets)
		}
	}
}

// TestRawGlobalCapabilityNoDegradedDrops 实现了可选能力的宿主：
// 取数层只取全局，没有降级滤除 ⇒ 表里**没有** degraded_scope 一项。
func TestRawGlobalCapabilityNoDegradedDrops(t *testing.T) {
	f := &globalRawStore{fakeReader: mixedRawStore()}
	res, met := recallEmptyScope(t, f)

	if got, ok := res.Drops[ports.DropDegradedScope]; ok && got != 0 {
		t.Fatalf("实现了 GlobalRawReader 的宿主不该有降级滤除，实得 %v", got)
	}
	if got := met.Snapshot().Counts["recall.dropped.degraded_scope"]; got != 0 {
		t.Fatalf("降级指标应为 0，实得 %v", got)
	}
	// 全局事件照常进（能力没有让召回变少）。
	found := false
	for _, s := range res.Snippets {
		if s.Source == recall.SourceRaw && s.RefID == "g1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("全局事件应照常进 raw 源: %+v", res.Snippets)
	}
}

// TestRawDegradedScopeOnlyForEmptyReader 非空读者**没有**降级一说：
// ListRaw(非空 scope) 的「全局 + 同归属」本来就是读者语义，不计数。
func TestRawDegradedScopeOnlyForEmptyReader(t *testing.T) {
	f := mixedRawStore()
	cfg := recall.DefaultConfig()
	p := newPipe(t, f, cfg)
	res, err := p.Recall(context.Background(), recall.Request{Namespace: ns, Query: "", Scope: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := res.Drops[ports.DropDegradedScope]; ok && got != 0 {
		t.Fatalf("非空读者不该有降级归因，实得 %v", got)
	}
}
