package page_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/page"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("pg")

// pgKey 页面夹具的键：(归属, 实体)。
//
// 页面按归属视角分页，故夹具也必须按 (归属, 实体) 分键——
// 用实体 ID 单键的夹具会让「取到别家画像」那条用例验不到东西。
func pgKey(scope types.Scope, entityID string) string {
	return string(scope) + "\x00" + entityID
}

type fakeStore struct {
	pages   map[string]*types.EntityPage
	dirty   []string
	atoms   []*types.AtomCard
	applied int
	failed  int
	// dirtyScopes 记下每次标脏用的归属（供「标脏落在哪一视角」那条用例断言）。
	dirtyScopes []types.Scope
}

func newFake() *fakeStore {
	return &fakeStore{pages: map[string]*types.EntityPage{}}
}

func (f *fakeStore) GetEntityPage(_ context.Context, _ store.Namespace, id string,
	scope types.Scope) (*types.EntityPage, error) {
	if p, ok := f.pages[pgKey(scope, id)]; ok {
		return p, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListDirtyEntityPages(_ context.Context, _ store.Namespace, scope types.Scope,
	_ int) ([]*types.EntityPage, error) {
	var out []*types.EntityPage
	for _, k := range f.dirty {
		if p, ok := f.pages[k]; ok && p.Scope == scope {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkEntityPageDirty(_ context.Context, _ store.Namespace, id string,
	scope types.Scope, _ time.Time) error {
	k := pgKey(scope, id)
	if _, ok := f.pages[k]; !ok {
		f.pages[k] = &types.EntityPage{EntityID: id, Scope: scope, Dirty: true}
	}
	f.pages[k].Dirty = true
	f.dirtyScopes = append(f.dirtyScopes, scope)
	if len(f.dirty) == 0 || f.dirty[len(f.dirty)-1] != k {
		f.dirty = append(f.dirty, k)
	}
	return nil
}

func (f *fakeStore) ApplyEntityPageRegen(_ context.Context, _ store.Namespace, id string,
	scope types.Scope, p *types.EntityPage, at time.Time) error {
	p.EntityID = id
	p.Scope = scope
	p.Dirty = false
	p.RegenFailure = ""
	p.LastRegenAt = at
	f.pages[pgKey(scope, id)] = p
	f.applied++
	return nil
}

func (f *fakeStore) RecordEntityPageRegenFailure(_ context.Context, _ store.Namespace, id string,
	scope types.Scope, reason string, at time.Time) error {
	k := pgKey(scope, id)
	p, ok := f.pages[k]
	if !ok {
		p = &types.EntityPage{EntityID: id, Scope: scope}
		f.pages[k] = p
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
	// prompt 记下最近一次收到的提示词（用来验「哪些事实进了模型」）。
	prompt string
}

func (f *fakeLLM) Complete(_ context.Context, req ports.LLMRequest) (string, error) {
	f.prompt = req.Prompt
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

func ctx2() context.Context { return context.Background() }

// 生成成功：清脏、落正文。
func TestRegenSuccessClearsDirty(t *testing.T) {
	f := newFake()
	f.dirty = []string{pgKey("", "e1")}
	f.pages[pgKey("", "e1")] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小"}}

	r, err := page.New(f, &fakeLLM{out: `{"headline":"磁盘偏小的机器","body":"长期不足"}`}, page.Config{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.RegenerateDirty(ctx2(), ns, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Regenerated != 1 {
		t.Fatalf("应生成 1 页，实得 %+v", res)
	}
	got := f.pages[pgKey("", "e1")]
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
	f.dirty = []string{pgKey("", "e1")}
	f.pages[pgKey("", "e1")] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "x"}}

	r, _ := page.New(f, &fakeLLM{err: errors.New("模型超时")}, page.Config{})
	res, err := r.RegenerateDirty(ctx2(), ns, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("应记 1 次失败: %+v", res)
	}
	got := f.pages[pgKey("", "e1")]
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
	f.dirty = []string{pgKey("", "e1")}
	f.pages[pgKey("", "e1")] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "x"}}

	r, _ := page.New(f, nil, page.Config{})
	res, err := r.RegenerateDirty(ctx2(), ns, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 {
		t.Fatalf("无模型应记失败: %+v", res)
	}
	if !f.pages[pgKey("", "e1")].Dirty {
		t.Fatal("无模型时页面必须保持脏")
	}
}

// 模型输出不是合法 JSON → 记失败，不产出空页面。
func TestBadOutputIsAFailure(t *testing.T) {
	f := newFake()
	f.dirty = []string{pgKey("", "e1")}
	f.pages[pgKey("", "e1")] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "x"}}

	r, _ := page.New(f, &fakeLLM{out: "我看了一下，没发现什么特别的问题"}, page.Config{})
	res, _ := r.RegenerateDirty(ctx2(), ns, "")
	if res.Failed != 1 {
		t.Fatalf("非法输出应记失败: %+v", res)
	}
	if f.applied != 0 {
		t.Fatal("解析失败不得落任何正文")
	}
}

// 模型带 ``` 围栏输出时必须照样解析成功。
//
// 这条不是「宽松」：被要求输出 JSON 时，模型经常连着 ```json 围栏一起给。
// 不剥围栏的后果是**每次重生成都失败一次**，而失败保持脏标记——
// 于是这个实体的画像永远重生成不出来，且失败原因（解析错误）指向的是
// 格式噪声而不是真问题。
//
// 「字段缺失即失败」那条不变：剥围栏只解决格式噪声，不放宽内容校验。
func TestFencedOutputIsParsed(t *testing.T) {
	f := newFake()
	f.dirty = []string{pgKey("", "e1")}
	f.pages[pgKey("", "e1")] = &types.EntityPage{EntityID: "e1", Dirty: true}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Assertion: "x"}}

	out := "```json\n{\"headline\": \"一句话画像\", \"body\": \"正文\"}\n```"
	r, _ := page.New(f, &fakeLLM{out: out}, page.Config{})
	res, _ := r.RegenerateDirty(ctx2(), ns, "")
	if res.Failed != 0 {
		t.Fatalf("带围栏的合法输出不该判失败: %+v", res)
	}
	if res.Regenerated != 1 {
		t.Fatalf("带围栏的合法输出应重生成成功: %+v", res)
	}
}

// 没有事实 → 跳过，不算失败。
func TestNoAtomsSkips(t *testing.T) {
	f := newFake()
	f.dirty = []string{pgKey("", "e1")}
	f.pages[pgKey("", "e1")] = &types.EntityPage{EntityID: "e1", Dirty: true}

	r, _ := page.New(f, &fakeLLM{out: "{}"}, page.Config{})
	res, _ := r.RegenerateDirty(ctx2(), ns, "")
	if res.Failed != 0 || res.Skipped != 1 {
		t.Fatalf("无事实应跳过: %+v", res)
	}
}

// ── 归属隔离 ──

// 画像只汇总**对这一视角可见**的事实：全局 + 同归属。
//
// 别家的事实一条都不许进提示词——
// 提示词是要发给模型的那一份，混进去就是把内容送出去了。
//
// 这条是本仓最贵的一类失效：症状是「画像里出现了没提过的东西」，
// 而它在数据上完全看不出来（页面就是一段正常的文字）。
func TestRegenOnlyUsesFactsVisibleToScope(t *testing.T) {
	const scopeA = types.Scope("team-a")
	const scopeB = types.Scope("team-b")

	newScoped := func() *fakeStore {
		f := newFake()
		f.atoms = []*types.AtomCard{
			{ID: "g1", EntityID: "e1", Scope: "", Assertion: "全局事实"},
			{ID: "a1", EntityID: "e1", Scope: scopeA, Assertion: "甲组事实"},
			{ID: "b1", EntityID: "e1", Scope: scopeB, Assertion: "乙组事实"},
		}
		return f
	}

	// 甲组视角：全局 + 甲组，不含乙组
	f := newScoped()
	f.dirty = []string{pgKey(scopeA, "e1")}
	f.pages[pgKey(scopeA, "e1")] = &types.EntityPage{EntityID: "e1", Scope: scopeA, Dirty: true}
	llm := &fakeLLM{out: `{"headline":"h","body":"b"}`}
	r, _ := page.New(f, llm, page.Config{})
	if _, err := r.RegenerateDirty(ctx2(), ns, scopeA); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(llm.prompt, "乙组事实") {
		t.Fatalf("别家的事实进了提示词: %s", llm.prompt)
	}
	if !strings.Contains(llm.prompt, "甲组事实") || !strings.Contains(llm.prompt, "全局事实") {
		t.Fatalf("本视角与全局的事实都该进提示词: %s", llm.prompt)
	}
	got := f.pages[pgKey(scopeA, "e1")]
	if len(got.AtomIDs) != 2 {
		t.Fatalf("页面应只覆盖两条可见事实: %+v", got.AtomIDs)
	}
	if got.Scope != scopeA {
		t.Fatalf("页面归属应落成这一视角: %q", got.Scope)
	}

	// 全局视角：只汇总全局事实
	f2 := newScoped()
	f2.dirty = []string{pgKey("", "e1")}
	f2.pages[pgKey("", "e1")] = &types.EntityPage{EntityID: "e1", Dirty: true}
	llm2 := &fakeLLM{out: `{"headline":"h","body":"b"}`}
	r2, _ := page.New(f2, llm2, page.Config{})
	if _, err := r2.RegenerateDirty(ctx2(), ns, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(llm2.prompt, "甲组事实") || strings.Contains(llm2.prompt, "乙组事实") {
		t.Fatalf("全局页混进了带归属的事实: %s", llm2.prompt)
	}
	if !strings.Contains(llm2.prompt, "全局事实") {
		t.Fatalf("全局事实该进全局页: %s", llm2.prompt)
	}
}

// 一轮的脏页面重生成只动**本视角**的：别家的页面不该被这一轮的调用改掉。
func TestRegenDirtyOnlyTouchesOwnScope(t *testing.T) {
	const scopeA = types.Scope("team-a")
	const scopeB = types.Scope("team-b")
	f := newFake()
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Scope: scopeA, Assertion: "甲组事实"}}
	f.dirty = []string{pgKey(scopeA, "e1"), pgKey(scopeB, "e1")}
	f.pages[pgKey(scopeA, "e1")] = &types.EntityPage{EntityID: "e1", Scope: scopeA, Dirty: true}
	f.pages[pgKey(scopeB, "e1")] = &types.EntityPage{EntityID: "e1", Scope: scopeB, Dirty: true}

	r, _ := page.New(f, &fakeLLM{out: `{"headline":"h","body":"b"}`}, page.Config{})
	res, err := r.RegenerateDirty(ctx2(), ns, scopeA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Regenerated != 1 {
		t.Fatalf("只应重生成本页: %+v", res)
	}
	if !f.pages[pgKey(scopeB, "e1")].Dirty {
		t.Fatal("别家视角的页面不该被这一轮改掉")
	}
	if f.pages[pgKey(scopeA, "e1")].Dirty {
		t.Fatal("本页应已清脏")
	}
}

// 标记归属：重生成失败要记在**本视角**那一页上，
// 别让「甲组的失败」写到「乙组的页面」上——那会让两边的看板都失真。
func TestRegenFailureRecordsUnderOwnScope(t *testing.T) {
	const scopeA = types.Scope("team-a")
	f := newFake()
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e1", Scope: scopeA, Assertion: "甲组事实"}}
	f.dirty = []string{pgKey(scopeA, "e1")}
	f.pages[pgKey(scopeA, "e1")] = &types.EntityPage{EntityID: "e1", Scope: scopeA, Dirty: true}

	r, _ := page.New(f, &fakeLLM{err: errors.New("模型超时")}, page.Config{})
	if _, err := r.RegenerateDirty(ctx2(), ns, scopeA); err != nil {
		t.Fatal(err)
	}
	got := f.pages[pgKey(scopeA, "e1")]
	if !got.Dirty || got.RegenFailure == "" {
		t.Fatalf("失败必须留在本视角这一页上: %+v", got)
	}
	if _, ok := f.pages[pgKey("", "e1")]; ok {
		t.Fatal("失败不许写到别的视角那一页上")
	}
}
