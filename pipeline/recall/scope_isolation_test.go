package recall_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：召回面的归属隔离用例。
//
// # 两条防线
//
//  1. **存储层过滤**：归属条件进查询，与 TopK 配额同一次求值
//     （storage/parity 那边钉跨后端行为）。
//  2. **召回层复查**：万一某个后端漏了过滤条件，别组的卡也不会进 prompt。
//
// 第 2 条不是多余的：它是**纵深防御**，且它的失效形态是泄漏——
// 内容看着完全正常（就是一条说得通的事实），调用方分辨不出来。

// scopeReader 记下存储收到的过滤条件。
type scopeReader struct {
	*fakeReader
	listFilters   []store.AtomFilter
	searchFilters []store.AtomFilter
}

func (s *scopeReader) ListAtoms(ctx context.Context, ns store.Namespace,
	flt store.AtomFilter) ([]*types.AtomCard, error) {
	s.listFilters = append(s.listFilters, flt)
	return s.fakeReader.ListAtoms(ctx, ns, flt)
}

func (s *scopeReader) SearchAtoms(ctx context.Context, ns store.Namespace, q string,
	flt store.AtomFilter) ([]*types.AtomCard, error) {
	s.searchFilters = append(s.searchFilters, flt)
	return s.fakeReader.SearchAtoms(ctx, ns, q, flt)
}

// leakyReader **无视**归属过滤的存储（模拟「后端漏了过滤条件」）。
type leakyReader struct {
	*fakeReader
}

func (l leakyReader) ListAtoms(ctx context.Context, ns store.Namespace,
	flt store.AtomFilter) ([]*types.AtomCard, error) {
	return l.fakeReader.ListAtoms(ctx, ns, store.AtomFilter{ActiveOnly: flt.ActiveOnly, Limit: flt.Limit})
}

func (l leakyReader) SearchAtoms(ctx context.Context, ns store.Namespace, q string,
	flt store.AtomFilter) ([]*types.AtomCard, error) {
	return l.fakeReader.SearchAtoms(ctx, ns, q, store.AtomFilter{ActiveOnly: flt.ActiveOnly, Limit: flt.Limit})
}

func (l leakyReader) ListRaw(ctx context.Context, ns store.Namespace, _ types.Scope,
	_ time.Time, p store.Page) ([]*types.RawEvent, error) {
	return l.fakeReader.ListRaw(ctx, ns, "", time.Time{}, p)
}

// 归属必须**下推到存储层**：过滤在取数层才生效。
//
// 后置过滤的话，被别家占掉的配额已经花掉了——
// 症状只是「召回变少了」，看着像排序问题，根因却在取数。
func TestRecallPassesScopeDownToStore(t *testing.T) {
	f := &scopeReader{fakeReader: &fakeReader{}}
	f.atoms = []*types.AtomCard{scopedAtom("a1", "scope-a", "磁盘长期偏小")}
	p := newPipe(t, f, recall.DefaultConfig())

	if _, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", Scope: "scope-a",
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.listFilters) == 0 {
		t.Fatal("召回没取过事实卡")
	}
	for _, flt := range f.listFilters {
		if flt.Scope != "scope-a" {
			t.Fatalf("列表路的过滤条件没带归属: %+v", flt)
		}
	}
	if len(f.searchFilters) == 0 {
		t.Fatal("召回没走过检索路")
	}
	for _, flt := range f.searchFilters {
		if flt.Scope != "scope-a" {
			t.Fatalf("检索路的过滤条件没带归属: %+v", flt)
		}
	}
}

// 纵深防御：存储层漏了归属过滤时，别组的卡也不许进 prompt。
//
// 这是**泄漏**与「少取」的分界：漏了它，别人环境里的事实会以
// 「一条说得通的事实」的形态出现在 prompt 里，而调用方分辨不出来。
func TestRecallDropsForeignScopeCards(t *testing.T) {
	mine := scopedAtom("a1", "scope-a", "磁盘长期偏小")
	other := scopedAtom("a2", "scope-b", "内存占用偏高")
	f := leakyReader{fakeReader: &fakeReader{}}
	f.atoms = []*types.AtomCard{mine, other}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", Scope: "scope-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Snippets {
		if s.RefID == "a2" {
			t.Fatalf("别组的卡进了结果: %+v", s)
		}
	}
	if len(res.Snippets) == 0 {
		t.Fatal("本组的卡也该在：过滤不能把可见的内容一起滤掉")
	}
}

// 原始事件面同样要按归属隔离：它一次就取 MaxRaw 条。
func TestRecallDropsForeignScopeRawEvents(t *testing.T) {
	f := leakyReader{fakeReader: &fakeReader{}}
	f.raws = []*types.RawEvent{
		{ID: "r1", Content: "磁盘长期偏小", Timestamp: base()},
		{ID: "r2", Content: "内存占用偏高", Timestamp: base(), Scope: "scope-b"},
	}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", Scope: "scope-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Snippets {
		if s.RefID == "r2" {
			t.Fatalf("别组的事件进了结果: %+v", s)
		}
	}
}

// scopedAtom 造一条带归属的事实卡。
func scopedAtom(id string, scope types.Scope, text string) *types.AtomCard {
	a := atom(id, "e1", text, types.ImportanceHigh)
	a.Scope = scope
	return a
}
