package graph

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// pagedEdgeStore 给 fakeEdgeStore 补上 LIMIT/OFFSET 语义（与真后端一致）。
//
// 真后端的 ListEdges 是 LIMIT（0=缺省 100）：本假体按 flt.Limit/flt.Offset
// 切片返回，并记下每页请求，用来断言遍历器的翻页行为。
type pagedEdgeStore struct {
	*fakeEdgeStore
	pages []store.EdgeFilter
}

func (f *pagedEdgeStore) ListEdges(ctx context.Context, ns store.Namespace, flt store.EdgeFilter) ([]*types.EntityEdge, error) {
	f.pages = append(f.pages, flt)
	all, err := f.fakeEdgeStore.ListEdges(ctx, ns, flt)
	if err != nil {
		return nil, err
	}
	// 复刻后端语义：Limit 0 = 缺省 100；OFFSET 跳过前 n 条。
	limit := flt.Limit
	if limit <= 0 {
		limit = 100
	}
	offset := max(flt.Offset, 0)
	if offset >= len(all) {
		return nil, nil
	}
	end := min(offset+limit, len(all))
	return all[offset:end], nil
}

var edgeBase = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// 单实体出边超过后端缺省上限（100）时，遍历必须**取尽**而不是静默截断
// （issue #54）：截断按 created_at ASC 先丢最新的边——最新建的关系
// 永不参与多跳，「某些关系搜不到」就是这么来的且难归因。
//
// 钉两层：取边请求显式带分页 Limit（不带的话兜成缺省 100），
// 与结果集（150 条全到）。
func TestTraverseFetchesAllEdgesBeyondDefaultLimit(t *testing.T) {
	st := &pagedEdgeStore{fakeEdgeStore: newFake()}
	// 一个枢纽实体挂 150 条出边（> 缺省 100），指向 150 个不同实体
	// （同一目标会按实体去重，测不出条数）。
	for i := range 150 {
		if err := st.UpsertEdge(context.Background(), "ns", &types.EntityEdge{
			ID:           "edge-" + strconv.Itoa(i),
			FromEntityID: "hub", ToEntityID: "to-" + strconv.Itoa(i),
			Relation: "依赖", CreatedAt: edgeBase,
		}); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := NewTraverser(st)
	if err != nil {
		t.Fatal(err)
	}
	// MaxResults 显式给足：缺省 50 是**结果**上限（另一码事），别让它
	// 混淆本用例要钉的「取边取尽」。
	got, err := tr.Traverse(context.Background(), "ns", []string{"hub"}, TraverseOptions{MaxDepth: 1, MaxResults: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 {
		t.Fatalf("150 条出边应全部参与多跳（不得按缺省 100 截断），实得 %d", len(got))
	}
	if len(st.pages) == 0 {
		t.Fatal("遍历没取过边")
	}
	for _, pg := range st.pages {
		if pg.Limit != edgeScanPage {
			t.Fatalf("取边应显式带分页 Limit %d（0 会兜成缺省 100），实得 %d", edgeScanPage, pg.Limit)
		}
	}
}

// 整页返回时必须继续翻页：首页恰好满一页（500 条）时，第二页带着
// Offset=edgeScanPage 来，取尽即停（不许多发空页查询）。
func TestTraversePaginatesUntilExhausted(t *testing.T) {
	st := &pagedEdgeStore{fakeEdgeStore: newFake()}
	for i := range 600 {
		if err := st.UpsertEdge(context.Background(), "ns", &types.EntityEdge{
			ID:           "e" + strconv.Itoa(i),
			FromEntityID: "hub", ToEntityID: "t" + strconv.Itoa(i),
			Relation: "依赖", CreatedAt: edgeBase,
		}); err != nil {
			t.Fatal(err)
		}
	}
	tr, err := NewTraverser(st)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tr.Traverse(context.Background(), "ns", []string{"hub"}, TraverseOptions{MaxDepth: 1, MaxResults: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 600 {
		t.Fatalf("600 条出边（两页）应全部参与多跳，实得 %d", len(got))
	}
	// 翻页序列：Offset 0 → 500，两页后取尽。
	if len(st.pages) != 2 {
		t.Fatalf("600 条出边应恰好翻两页，实得 %d 页", len(st.pages))
	}
	if st.pages[0].Offset != 0 || st.pages[1].Offset != edgeScanPage {
		t.Fatalf("翻页 Offset 应为 0 → %d，实得 %d → %d",
			edgeScanPage, st.pages[0].Offset, st.pages[1].Offset)
	}
}
