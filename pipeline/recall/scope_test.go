package recall_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件钉住召回面的三条**范围与形态**契约：
//
//  1. 事实卡能不能被限定在宿主给的实体集合内；
//  2. 宿主没有捕获面时，raw 源怎么处置；
//  3. 画像能不能被限定在读者自己的归属视角内。
//
// 第 1 条的坑在于：`Request.Entities` 同时承载两种语义相反的用法
// （「本轮提到的实体」用来补画像页 vs「会话锚点实体」用来划事实范围），
// 内核从字段本身区分不了。故必须由宿主**显式声明**，且缺省保持不过滤。
//
// 第 2 条的坑在于：让没有捕获面的宿主实现 ListRaw，只能给一个
// 「永远返回空集」的桩——那是契约洗白。
//
// 第 3 条的坑在于：画像是**事实的摘要**，摘要若不分视角，
// 别家的事实就揉进了这一份，而画像是人读的一整段话，
// 读者分不清哪句属于自己。

// 画像按读者归属取：读者只看得见**自己那一视角**的画像。
//
// 别家的画像进了 prompt 就是内容泄漏——
// 而且这类泄漏在结果里完全看不出来：画像就是一段正常的文字。
func TestPageHeadlineIsScopedToReader(t *testing.T) {
	const scopeA = types.Scope("team-a")
	const scopeB = types.Scope("team-b")
	f := &fakeReader{
		atoms: []*types.AtomCard{atom("a1", "e1", "某条事实", types.ImportanceHigh)},
		pages: map[string]*types.EntityPage{
			pageKey(scopeA, "e1"): {EntityID: "e1", Scope: scopeA, Headline: "甲组的画像", UpdatedAt: base()},
			pageKey(scopeB, "e1"): {EntityID: "e1", Scope: scopeB, Headline: "乙组的画像", UpdatedAt: base()},
		},
	}
	cfg := recall.DefaultConfig()
	cfg.Sources = []recall.Source{recall.SourceAtom, recall.SourcePageHeadline}
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "事实", Entities: []string{"e1"}, Scope: scopeA,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Render()
	if !strings.Contains(out, "甲组的画像") {
		t.Fatalf("应取到本视角的画像，实得 %s", out)
	}
	if strings.Contains(out, "乙组的画像") {
		t.Fatalf("别家视角的画像进了 prompt——内容泄漏: %s", out)
	}
}

// 自己那一视角的画像还没生成时，退到**全局页**。
//
// 退到全局页是安全的方向：全局页只汇总全局事实，
// 所以退过去是「少一点内容」，绝不是「别人的内容」。
// 反过来（把别家那一页退给读者）就是泄漏。
func TestPageHeadlineFallsBackToGlobalPage(t *testing.T) {
	const scopeA = types.Scope("team-a")
	f := &fakeReader{
		atoms: []*types.AtomCard{atom("a1", "e1", "某条事实", types.ImportanceHigh)},
		pages: map[string]*types.EntityPage{
			pageKey("", "e1"): {EntityID: "e1", Scope: "", Headline: "全局的画像", UpdatedAt: base()},
		},
	}
	cfg := recall.DefaultConfig()
	cfg.Sources = []recall.Source{recall.SourceAtom, recall.SourcePageHeadline}
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "事实", Entities: []string{"e1"}, Scope: scopeA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Render(), "全局的画像") {
		t.Fatalf("本视角没有页面时应退到全局页，实得 %s", res.Render())
	}
}

// 显式限定时，事实卡只取宿主给的那些实体。
func TestRestrictAtomsToEntitiesScopesFacts(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e-anchor", "磁盘长期偏小", types.ImportanceHigh),
		atom("a2", "e-other", "别的对象的事", types.ImportanceHigh),
	}}
	cfg := recall.DefaultConfig()
	cfg.RestrictAtomsToEntities = true
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1", Entities: []string{"e-anchor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Snippets {
		if s.EntityID == "e-other" {
			t.Fatal("限定后不得带出范围外实体的事实")
		}
	}
	found := false
	for _, s := range res.Snippets {
		if s.EntityID == "e-anchor" {
			found = true
		}
	}
	if !found {
		t.Fatal("限定范围内的实体的事实必须被召回")
	}
}

// 显式限定但范围为空 → **空集**，不是全库。
//
// 「显式声明了范围但范围为空」与「没声明范围」是两种不同的意图：
// 前者是「这一轮不该带任何事实」，后者是「不限」。
// 退化成全库会把「限定」变成「装饰」——而宿主正是靠它挡全库事实的。
func TestRestrictAtomsToEntitiesEmptyScopeYieldsEmptySet(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e-anchor", "磁盘长期偏小", types.ImportanceHigh),
	}}
	cfg := recall.DefaultConfig()
	cfg.RestrictAtomsToEntities = true
	p := newPipe(t, f, cfg)

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1", Entities: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) != 0 {
		t.Fatalf("显式限定为空时必须返回空集，实得 %d 条", len(res.Snippets))
	}
}

// 缺省不限定：既有调用方一行不改也不受影响。
func TestDefaultDoesNotRestrictByEntities(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e-anchor", "磁盘长期偏小", types.ImportanceHigh),
		atom("a2", "e-other", "别的对象的事", types.ImportanceHigh),
	}}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1", Entities: []string{"e-anchor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snippets) < 2 {
		t.Fatalf("缺省不应限定，实体集合只是「本轮提到的」，实得 %d 条", len(res.Snippets))
	}
}

// noRawReader 实现 recall.Reader，但**不**实现 recall.RawReader。
//
// 刻意不内嵌 fakeReader：内嵌会把 ListRaw 提升进方法集，
// 那它就「有捕获面」了——这正是本用例要验的反面。
type noRawReader struct{ f *fakeReader }

func (n noRawReader) ListAtoms(ctx context.Context, ns store.Namespace, flt store.AtomFilter) ([]*types.AtomCard, error) {
	return n.f.ListAtoms(ctx, ns, flt)
}

func (n noRawReader) SearchAtoms(ctx context.Context, ns store.Namespace, q string,
	flt store.AtomFilter) ([]*types.AtomCard, error) {
	return n.f.SearchAtoms(ctx, ns, q, flt)
}

func (n noRawReader) GetEntityPage(ctx context.Context, ns store.Namespace, id string,
	scope types.Scope) (*types.EntityPage, error) {
	return n.f.GetEntityPage(ctx, ns, id, scope)
}

func (n noRawReader) ListActiveEntities(ctx context.Context, ns store.Namespace, threadID string, limit int) ([]*types.ActiveEntity, error) {
	return n.f.ListActiveEntities(ctx, ns, threadID, limit)
}

func (n noRawReader) UpsertActiveEntity(ctx context.Context, ns store.Namespace, a *types.ActiveEntity) error {
	return n.f.UpsertActiveEntity(ctx, ns, a)
}

func (n noRawReader) EvictActiveEntities(ctx context.Context, ns store.Namespace, threadID string, keep int) ([]types.ActiveEntity, error) {
	return n.f.EvictActiveEntities(ctx, ns, threadID, keep)
}

// 配置要 raw 源而存储没有捕获面 → **构造期报错**，不静默空集。
//
// 这是本条纪律的核心：静默返回空集会让宿主以为「没有原始事件」，
// 而真相是「你没实现捕获面」——而内核会据此在 raw 源上做出错误假设。
func TestRawSourceWithoutCaptureFaceFailsLoudly(t *testing.T) {
	cfg := recall.DefaultConfig()
	_, err := recall.New(cfg, noRawReader{&fakeReader{}}, ports.StaticEstimator{}, ports.MemoryBand{})
	if err == nil {
		t.Fatal("配置要 raw 源而存储没有捕获面，必须在构造期报错")
	}
	if !strings.Contains(err.Error(), "RawReader") {
		t.Fatalf("报错要说清怎么改: %v", err)
	}
}

// 无捕获面形态：存储不必实现 RawReader，raw 源也不进路由。
func TestNoCaptureFaceFormNeedsNoRawReader(t *testing.T) {
	cfg := recall.DefaultConfig()
	cfg.NoCaptureFace = true
	p, err := recall.New(cfg, noRawReader{&fakeReader{}}, ports.StaticEstimator{}, ports.MemoryBand{})
	if err != nil {
		t.Fatalf("无捕获面形态不该要求实现 RawReader: %v", err)
	}
	// raw 源不在路由里：即使库里存着原始事件，也不该进召回。
	f := p // 仅为可读性
	_ = f
	p2 := p
	res, err := p2.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Snippets {
		if s.Source == recall.SourceRaw {
			t.Fatal("无捕获面时 raw 源不该出现在结果里")
		}
	}
}

// 无捕获面形态优先于 Sources：显式开关说了算。
func TestNoCaptureFaceWinsOverSources(t *testing.T) {
	cfg := recall.DefaultConfig()
	cfg.NoCaptureFace = true
	cfg.Sources = []recall.Source{recall.SourceAtom, recall.SourceRaw}
	p, err := recall.New(cfg, noRawReader{&fakeReader{}}, ports.StaticEstimator{}, ports.MemoryBand{})
	if err != nil {
		t.Fatalf("显式声明无捕获面时不该报错: %v", err)
	}
	for _, s := range p.Config().Sources {
		if s == recall.SourceRaw {
			t.Fatal("无捕获面优先于 Sources：raw 源必须被摘掉")
		}
	}
}

// 有捕获面时，raw 源照常进路由（既有行为不变）。
func TestRawSourceStillWorksWithCaptureFace(t *testing.T) {
	f := &fakeReader{raws: []*types.RawEvent{
		{ID: "r1", Content: "上一轮说过的话", SessionID: "s-old", Timestamp: time.Now()},
	}}
	p := newPipe(t, f, recall.DefaultConfig())

	res, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "磁盘", SessionID: "s1",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range res.Snippets {
		if s.Source == recall.SourceRaw {
			found = true
		}
	}
	if !found {
		t.Fatal("有捕获面时 raw 源应照常进路由")
	}
}
