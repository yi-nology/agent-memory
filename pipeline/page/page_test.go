package page_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/page"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("pg")

func now() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

type fakeStore struct {
	pages   map[string]*types.EntityPage
	dirty   []string
	atoms   []*types.AtomCard
	applied int
	failed  int
}

func newFake() *fakeStore {
	return &fakeStore{pages: map[string]*types.EntityPage{}}
}

func (f *fakeStore) GetEntityPage(_ context.Context, _ store.Namespace, id string) (*types.EntityPage, error) {
	if p, ok := f.pages[id]; ok {
		return p, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListDirtyEntityPages(_ context.Context, _ store.Namespace, _ int) ([]*types.EntityPage, error) {
	var out []*types.EntityPage
	for _, id := range f.dirty {
		if p, ok := f.pages[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkEntityPageDirty(_ context.Context, _ store.Namespace, id string, _ time.Time) error {
	if _, ok := f.pages[id]; !ok {
		f.pages[id] = &types.EntityPage{EntityID: id, Dirty: true}
	}
	f.pages[id].Dirty = true
	if len(f.dirty) == 0 || f.dirty[len(f.dirty)-1] != id {
		f.dirty = append(f.dirty, id)
	}
	return nil
}

func (f *fakeStore) ApplyEntityPageRegen(_ context.Context, _ store.Namespace, id string,
	p *types.EntityPage, at time.Time) error {
	p.EntityID = id
	p.Dirty = false
	p.RegenFailure = ""
	p.LastRegenAt = at
	f.pages[id] = p
	f.applied++
	return nil
}

func (f *fakeStore) RecordEntityPageRegenFailure(_ context.Context, _ store.Namespace, id, reason string, at time.Time) error {
	p, ok := f.pages[id]
	if !ok {
		p = &types.EntityPage{EntityID: id}
		f.pages[id] = p
	}
	p.Dirty = true // 关键：失败必须保持脏
	p.RegenFailure = reason
	p.LastRegenAt = at
	f.failed++
	return nil
}

func (f *fakeStore) ListAtomsByEntity(_ context.Context, _ store.Namespace, entityID string) ([]*types.AtomCard, error) {
	var out []*types.AtomCard
	for _, a := range f.atoms {
		if a.EntityID == entityID {
			out = append(out, a)
		}
	}
	return out, nil
}

type fakeLLM struct {
	out string
	err error
}

func (f *fakeLLM) Complete(context.Context, ports.LLMRequest) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

func ctx2() context.Context { return context.Background() }

// 生成成功：清脏、落正文。
func TestRegenSuccessClearsDirty(t *testing.T) {
	f := newFake()
	f.dirty = []string{"e1"}
	f.pages["e1"] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小"}}

	r, err := page.New(f, &fakeLLM{out: `{"headline":"磁盘偏小的机器","body":"长期不足"}`}, page.Config{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.RegenerateDirty(ctx2(), ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Regenerated != 1 {
		t.Fatalf("应生成 1 页，实得 %+v", res)
	}
	got := f.pages["e1"]
	if got.Dirty {
		t.Fatal("成功后应清脏")
	}
	if got.Headline != "磁盘偏小的机器" {
		t.Fatalf("正文没落: %+v", got)
	}
	if len(got.AtomIDs) != 1 {
		t.Fatalf("页面应记录覆盖的事实: %+v", got.AtomIDs)
	}
}

// 生成失败：保持脏 + 记原因。
func TestRegenFailureKeepsDirtyAndRecords(t *testing.T) {
	f := newFake()
	f.dirty = []string{"e1"}
	f.pages["e1"] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "x"}}

	r, _ := page.New(f, &fakeLLM{err: errors.New("模型超时")}, page.Config{})
	res, err := r.RegenerateDirty(ctx2(), ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("应记 1 次失败: %+v", res)
	}
	got := f.pages["e1"]
	if !got.Dirty {
		t.Fatal("失败必须保持脏：清掉就永远不再进队列，而用户看到的是「页面存在、内容过时」")
	}
	if got.RegenFailure == "" {
		t.Fatal("失败原因必须留痕")
	}
}

// 没有模型 → 显式失败，不是静默产出空页面。
func TestNoLLMFailsExplicitly(t *testing.T) {
	f := newFake()
	f.dirty = []string{"e1"}
	f.pages["e1"] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "x"}}

	r, _ := page.New(f, nil, page.Config{})
	res, err := r.RegenerateDirty(ctx2(), ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("无模型应记失败: %+v", res)
	}
	if !f.pages["e1"].Dirty {
		t.Fatal("无模型时页面必须保持脏")
	}
}

// 模型输出不是合法 JSON → 记失败，不产出空页面。
func TestBadOutputIsAFailure(t *testing.T) {
	f := newFake()
	f.dirty = []string{"e1"}
	f.pages["e1"] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "x"}}

	r, _ := page.New(f, &fakeLLM{out: "我看了一下，没发现什么特别的问题"}, page.Config{})
	res, _ := r.RegenerateDirty(ctx2(), ns)
	if res.Failed != 1 {
		t.Fatalf("非法输出应记失败: %+v", res)
	}
	if f.applied != 0 {
		t.Fatal("解析失败不得落任何正文")
	}
}

// 没有事实 → 跳过，不算失败。
func TestNoAtomsSkips(t *testing.T) {
	f := newFake()
	f.dirty = []string{"e1"}
	f.pages["e1"] = &types.EntityPage{EntityID: "e1", Dirty: true}

	r, _ := page.New(f, &fakeLLM{out: "{}"}, page.Config{})
	res, _ := r.RegenerateDirty(ctx2(), ns)
	if res.Failed != 0 || res.Skipped != 1 {
		t.Fatalf("无事实应跳过: %+v", res)
	}
}
