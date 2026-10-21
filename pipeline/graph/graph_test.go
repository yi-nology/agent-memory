package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// fakeEdgeStore 内存假存储：按 ID 去重落边，按过滤条件取边。
//
// 刻意**把归属/时间/关系三道过滤都做成可关掉的**（leak* 开关）：
// 用来模拟「后端漏了过滤条件」——那种后端的失效形态是串户，
// 而它在结果里完全看不出来，所以本包必须自己再判一次。
type fakeEdgeStore struct {
	edges     map[string]*types.EntityEdge
	order     []string
	calls     int
	froms     []string
	leakScope bool
	leakAt    bool
	leakRel   bool
}

func newFake() *fakeEdgeStore {
	return &fakeEdgeStore{edges: map[string]*types.EntityEdge{}}
}

func (f *fakeEdgeStore) UpsertEdge(_ context.Context, _ store.Namespace, e *types.EntityEdge) error {
	if _, ok := f.edges[e.ID]; !ok {
		f.order = append(f.order, e.ID)
	}
	cp := *e
	f.edges[e.ID] = &cp
	return nil
}

func (f *fakeEdgeStore) GetEdge(_ context.Context, _ store.Namespace, id string) (*types.EntityEdge, error) {
	e, ok := f.edges[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *e
	return &cp, nil
}

func (f *fakeEdgeStore) ListEdges(_ context.Context, _ store.Namespace, flt store.EdgeFilter) ([]*types.EntityEdge, error) {
	f.calls++
	f.froms = append(f.froms, flt.FromEntityIDs...)
	out := make([]*types.EntityEdge, 0, len(f.order))
	for _, id := range f.order {
		e := f.edges[id]
		if len(flt.FromEntityIDs) > 0 && !contains(flt.FromEntityIDs, e.FromEntityID) {
			continue
		}
		if len(flt.ToEntityIDs) > 0 && !contains(flt.ToEntityIDs, e.ToEntityID) {
			continue
		}
		if !f.leakRel && len(flt.Relations) > 0 && !contains(flt.Relations, e.Relation) {
			continue
		}
		if !f.leakScope && flt.Scope != "" && e.Scope != "" && e.Scope != flt.Scope {
			continue
		}
		if !f.leakAt && !flt.At.IsZero() && !e.EffectiveAt(flt.At) {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	return out, nil
}

func (f *fakeEdgeStore) DeleteEdge(_ context.Context, _ store.Namespace, id string) error {
	if _, ok := f.edges[id]; ok {
		delete(f.edges, id)
		for i, o := range f.order {
			if o == id {
				f.order = append(f.order[:i], f.order[i+1:]...)
				break
			}
		}
	}
	return nil
}

func (f *fakeEdgeStore) DeleteEdgesBySource(_ context.Context, _ store.Namespace, sourceAtomID string) error {
	for _, id := range append([]string(nil), f.order...) {
		if f.edges[id].SourceAtomID == sourceAtomID {
			_ = f.DeleteEdge(context.Background(), "", id)
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func newWriter(t *testing.T, s store.EdgeStore) *Writer {
	t.Helper()
	w, err := NewWriter(s, WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }))
	if err != nil {
		t.Fatalf("造落边器失败: %v", err)
	}
	return w
}

func newTraverser(t *testing.T, s store.EdgeStore) *Traverser {
	t.Helper()
	tr, err := NewTraverser(s)
	if err != nil {
		t.Fatalf("造多跳检索器失败: %v", err)
	}
	return tr
}

func edge(id, from, to, relation string, weight float64) *types.EntityEdge {
	return &types.EntityEdge{
		ID:           id,
		FromEntityID: from,
		ToEntityID:   to,
		Relation:     relation,
		Weight:       weight,
		SourceAtomID: "atom-" + id,
	}
}

// TestPutIsIdempotent 同一 ID 落两次只有一条边。
//
// 幂等是这条写入面的基本承诺：同一张事实卡被重投影（重试、重跑）时，
// 库里不该出现两条一样的边——多出来的那条会让多跳路径重复，
// 而重复在结果里几乎看不出来。
func TestPutIsIdempotent(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	ctx := context.Background()

	e := edge("e1", "a1", "a2", "rel-1", 0)
	if err := w.Put(ctx, "ns", e); err != nil {
		t.Fatalf("第一次落边失败: %v", err)
	}
	e2 := edge("e1", "a1", "a2", "rel-1", 0)
	if err := w.Put(ctx, "ns", e2); err != nil {
		t.Fatalf("第二次落边失败: %v", err)
	}

	got, err := f.ListEdges(ctx, "ns", store.EdgeFilter{})
	if err != nil {
		t.Fatalf("取边失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("同一 ID 落了 %d 条边，期望 1 条", len(got))
	}
}

// TestPutRejectsEdgeWithoutSource 没有溯源的边落不下去。
//
// 没有溯源的边是「没人敢删的垃圾」：事实卡被推翻后清不掉它，
// 图就开始说谎——而这种失效**不报错**，只是让图悄悄说反话。
func TestPutRejectsEdgeWithoutSource(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)

	e := edge("e1", "a1", "a2", "rel-1", 0)
	e.SourceAtomID = ""
	err := w.Put(context.Background(), "ns", e)
	if err == nil {
		t.Fatal("没有溯源的边落了下去，期望报错")
	}
	if !errors.Is(err, ErrMissingSource) {
		t.Fatalf("错误=%v，期望 ErrMissingSource", err)
	}
	got, _ := f.ListEdges(context.Background(), "ns", store.EdgeFilter{})
	if len(got) != 0 {
		t.Fatalf("落库里有 %d 条边，期望 0 条", len(got))
	}
}

// TestPutFillsCreatedAt 建边时刻由注入的时钟补齐。
//
// 调用方不该负责编造建边时刻：它是派生事实。
func TestPutFillsCreatedAt(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)

	e := edge("e1", "a1", "a2", "rel-1", 0)
	if err := w.Put(context.Background(), "ns", e); err != nil {
		t.Fatalf("落边失败: %v", err)
	}
	if e.CreatedAt.IsZero() {
		t.Fatal("建边时刻没被补齐")
	}
	if want := time.Unix(1700000000, 0).UTC(); !e.CreatedAt.Equal(want) {
		t.Fatalf("建边时刻=%v，期望 %v", e.CreatedAt, want)
	}
}

// TestClearSourceRemovesProjectedEdges 事实卡被推翻后，投影出来的边要清干净。
//
// 留着旧边等于「关系还在」，而事实已经不成立了——那是一张说谎的图。
func TestClearSourceRemovesProjectedEdges(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	ctx := context.Background()

	e1 := edge("e1", "a1", "a2", "rel-1", 0)
	e1.SourceAtomID = "atom-x"
	e2 := edge("e2", "a1", "a3", "rel-1", 0)
	e2.SourceAtomID = "atom-y"
	if err := w.Put(ctx, "ns", e1); err != nil {
		t.Fatalf("落边失败: %v", err)
	}
	if err := w.Put(ctx, "ns", e2); err != nil {
		t.Fatalf("落边失败: %v", err)
	}

	if err := w.ClearSource(ctx, "ns", "atom-x"); err != nil {
		t.Fatalf("清投影失败: %v", err)
	}
	got, _ := f.ListEdges(ctx, "ns", store.EdgeFilter{})
	if len(got) != 1 {
		t.Fatalf("清完还有 %d 条边，期望 1 条", len(got))
	}
	if got[0].ID != "e2" {
		t.Fatalf("剩下的边是 %q，期望 e2", got[0].ID)
	}
}

// TestTraverseReachesTwoHops 两跳能到第三层，MaxDepth=1 到不了。
func TestTraverseReachesTwoHops(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	for _, e := range []*types.EntityEdge{
		edge("e1", "a1", "a2", "rel-1", 0),
		edge("e2", "a2", "a3", "rel-1", 0),
	} {
		if err := w.Put(ctx, "ns", e); err != nil {
			t.Fatalf("落边失败: %v", err)
		}
	}

	got, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 2})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("两跳取到 %d 个实体，期望 2 个: %+v", len(got), got)
	}
	byID := map[string]Traversal{}
	for _, g := range got {
		byID[g.EntityID] = g
	}
	if _, ok := byID["a3"]; !ok {
		t.Fatal("两跳没取到第三层实体")
	}
	if d := byID["a3"].Depth; d != 2 {
		t.Fatalf("第三层深度=%d，期望 2", d)
	}
	if p := byID["a3"].Path; len(p) != 3 || p[0] != "a1" || p[2] != "a3" {
		t.Fatalf("第三层路径=%v，期望 [a1 a2 a3]", p)
	}
	if _, ok := byID["a1"]; ok {
		t.Fatal("起点自己出现在结果里，期望不出现")
	}

	one, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	for _, g := range one {
		if g.EntityID == "a3" {
			t.Fatal("MaxDepth=1 却取到了第三层实体")
		}
	}
	if len(one) != 1 {
		t.Fatalf("MaxDepth=1 取到 %d 个实体，期望 1 个: %+v", len(one), one)
	}
}

// TestTraverseCycleTerminates 有环的图不会无限展开。
//
// A→B→A：多跳在有环的图上不去重就是无限展开，而且它**不报错**，
// 只是越跑越慢直到把预算耗光。
func TestTraverseCycleTerminates(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	for _, e := range []*types.EntityEdge{
		edge("e1", "a1", "a2", "rel-1", 0),
		edge("e2", "a2", "a1", "rel-1", 0),
	} {
		if err := w.Put(ctx, "ns", e); err != nil {
			t.Fatalf("落边失败: %v", err)
		}
	}

	got, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 3})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("环上取到 %d 个实体，期望 1 个: %+v", len(got), got)
	}
	if got[0].EntityID != "a2" {
		t.Fatalf("取到 %q，期望 a2", got[0].EntityID)
	}
	// 起点只被展开过一次：这是「环不无限展开」的直接证据。
	if n := countOccurrences(f.froms, "a1"); n != 1 {
		t.Fatalf("起点被展开 %d 次，期望 1 次", n)
	}
}

func countOccurrences(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

// TestTraverseScopeIsolation 读者看不到别归属的边。
func TestTraverseScopeIsolation(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	global := edge("e1", "a1", "a2", "rel-1", 0)
	mine := edge("e2", "a1", "a3", "rel-1", 0)
	mine.Scope = "scope-a"
	other := edge("e3", "a1", "a4", "rel-1", 0)
	other.Scope = "scope-b"
	for _, e := range []*types.EntityEdge{global, mine, other} {
		if err := w.Put(ctx, "ns", e); err != nil {
			t.Fatalf("落边失败: %v", err)
		}
	}

	got, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 2, Scope: "scope-a"})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	seen := map[string]bool{}
	for _, g := range got {
		seen[g.EntityID] = true
		for _, p := range g.Path {
			if p == "a4" {
				t.Fatalf("路径里出现了别归属的实体: %v", g.Path)
			}
		}
	}
	if !seen["a2"] {
		t.Fatal("全局边的终点没取到")
	}
	if !seen["a3"] {
		t.Fatal("同归属边的终点没取到")
	}
	if seen["a4"] {
		t.Fatal("别归属边的终点取到了")
	}
}

// TestTraverseScopeIsolationSurvivesLeakyBackend 后端漏了归属过滤时，本包仍要挡住。
//
// 纵深防御：后端漏过滤的失效形态是**串户**（把别家的关系接进当前链路），
// 而那在结果里完全看不出来——路径看着仍然自洽。
func TestTraverseScopeIsolationSurvivesLeakyBackend(t *testing.T) {
	f := newFake()
	f.leakScope = true
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	other := edge("e3", "a1", "a4", "rel-1", 0)
	other.Scope = "scope-b"
	if err := w.Put(ctx, "ns", other); err != nil {
		t.Fatalf("落边失败: %v", err)
	}

	got, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 2, Scope: "scope-a"})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	for _, g := range got {
		for _, p := range g.Path {
			if p == "a4" {
				t.Fatalf("后端漏过滤时路径里出现了别归属的实体: %v", g.Path)
			}
		}
	}
}

// TestTraverseTimeFilter 边失效之后就走不到。
func TestTraverseTimeFilter(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	t0 := time.Unix(1700000000, 0).UTC()
	e := edge("e1", "a1", "a2", "rel-1", 0)
	e.ValidFrom = t0
	e.ValidTo = t0.Add(time.Hour)
	if err := w.Put(ctx, "ns", e); err != nil {
		t.Fatalf("落边失败: %v", err)
	}

	before, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 2, At: t0.Add(30 * time.Minute)})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	if len(before) != 1 || before[0].EntityID != "a2" {
		t.Fatalf("生效期内取到 %+v，期望取到 a2", before)
	}

	after, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 2, At: t0.Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("失效后取到 %+v，期望取不到", after)
	}
}

// TestTraverseOrdersByStrength 两条路径到同一实体时，强的在前。
//
// 路径强度是各段强度的**乘积**：一段弱关系会把整条链路拉弱——
// 最弱的一环决定这条链有多可信。
func TestTraverseOrdersByStrength(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	// 两条路径都到 a4：经 a2 的强，经 a3 的弱。
	// 另有一条到 a5 的路径，用来验证排序不是「恰好只有一条」。
	for _, e := range []*types.EntityEdge{
		edge("e1", "a1", "a2", "rel-1", 0.5),
		edge("e2", "a2", "a4", "rel-1", 0.9),
		edge("e3", "a1", "a3", "rel-1", 0.5),
		edge("e4", "a3", "a4", "rel-1", 0.1),
		edge("e5", "a1", "a5", "rel-1", 0.3),
	} {
		if err := w.Put(ctx, "ns", e); err != nil {
			t.Fatalf("落边失败: %v", err)
		}
	}

	got, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 3})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	byID := map[string]Traversal{}
	for _, g := range got {
		byID[g.EntityID] = g
	}
	a4, ok := byID["a4"]
	if !ok {
		t.Fatalf("没取到 a4: %+v", got)
	}
	// a4 只有一条记录（实体只到达一次），走的是强的那条路径。
	if want := 0.5 * 0.9; a4.Weight != want {
		t.Fatalf("a4 强度=%v，期望 %v", a4.Weight, want)
	}
	if p := a4.Path; len(p) != 3 || p[1] != "a2" {
		t.Fatalf("a4 路径=%v，期望走强的那条", p)
	}
	if n := countByEntity(got, "a4"); n != 1 {
		t.Fatalf("a4 出现 %d 次，期望 1 次", n)
	}
	if len(got) < 3 {
		t.Fatalf("结果只有 %d 条，排序无从验证: %+v", len(got), got)
	}
	// 强度高的在前：a4（0.45）在 a5（0.3）之前。
	if indexOf(got, "a4") > indexOf(got, "a5") {
		t.Fatalf("强度排序不对: %+v", got)
	}
	// 同强度按到达顺序稳定排序：a2 与 a3 都是 0.5，a2 先到。
	if indexOf(got, "a2") > indexOf(got, "a3") {
		t.Fatalf("同强度的到达顺序没保持: %+v", got)
	}
}

func countByEntity(list []Traversal, entityID string) int {
	n := 0
	for _, g := range list {
		if g.EntityID == entityID {
			n++
		}
	}
	return n
}

func indexOf(list []Traversal, entityID string) int {
	for i, g := range list {
		if g.EntityID == entityID {
			return i
		}
	}
	return -1
}

// TestTraverseDepthLimitBeyondFour 跳数超过上限即报错。
//
// 多跳再深就不可解释（人说不出「它经由什么到达」），也不可预算。
func TestTraverseDepthLimitBeyondFour(t *testing.T) {
	f := newFake()
	tr := newTraverser(t, f)

	_, err := tr.Traverse(context.Background(), "ns", []string{"a1"}, TraverseOptions{MaxDepth: 5})
	if err == nil {
		t.Fatal("MaxDepth=5 没报错，期望报错")
	}
	if !errors.Is(err, ErrInvalidDepth) {
		t.Fatalf("错误=%v，期望 ErrInvalidDepth", err)
	}
}

// TestTraverseRelationFilter 只走指定的关系。
func TestTraverseRelationFilter(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	for _, e := range []*types.EntityEdge{
		edge("e1", "a1", "a2", "rel-1", 0),
		edge("e2", "a1", "a3", "rel-2", 0),
	} {
		if err := w.Put(ctx, "ns", e); err != nil {
			t.Fatalf("落边失败: %v", err)
		}
	}

	got, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 2, Relations: []string{"rel-2"}})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	if len(got) != 1 || got[0].EntityID != "a3" {
		t.Fatalf("取到 %+v，期望只取到 a3", got)
	}
}

// TestTraverseMaxResults 结果条数受上限约束。
func TestTraverseMaxResults(t *testing.T) {
	f := newFake()
	w := newWriter(t, f)
	tr := newTraverser(t, f)
	ctx := context.Background()

	for _, e := range []*types.EntityEdge{
		edge("e1", "a1", "a2", "rel-1", 0),
		edge("e2", "a1", "a3", "rel-1", 0),
		edge("e3", "a1", "a4", "rel-1", 0),
	} {
		if err := w.Put(ctx, "ns", e); err != nil {
			t.Fatalf("落边失败: %v", err)
		}
	}

	got, err := tr.Traverse(ctx, "ns", []string{"a1"}, TraverseOptions{MaxDepth: 2, MaxResults: 2})
	if err != nil {
		t.Fatalf("多跳失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("取到 %d 条，期望 2 条: %+v", len(got), got)
	}
}
