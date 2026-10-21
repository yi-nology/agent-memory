package recall_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：召回面**缓存**的用例。
//
// 缓存是召回链上唯一一处「结果被复述」的地方，故它的用例比别处多一层：
// 除了「该命中时命中」，还要钉「不该命中时**不**命中」——
// 缓存键漏一个维度，症状是结果看起来完全正常，只是内容串了。

// ── 缓存 ──

func TestCacheHitAndDisabled(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{atom("a1", "e1", "某条事实", types.ImportanceHigh)}}

	cfg := recall.DefaultConfig()
	cfg.CacheTTL = time.Minute
	p := newPipe(t, f, cfg)
	req := recall.Request{Namespace: ns, Query: "事实"}
	first, err := p.Recall(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.FromCache {
		t.Fatal("首次不该命中缓存")
	}
	second, err := p.Recall(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.FromCache {
		t.Fatal("TTL 内重复查询必须命中缓存")
	}

	cfg2 := recall.DefaultConfig() // CacheTTL 缺省 0 = 关
	p2 := newPipe(t, f, cfg2)
	for i := 0; i < 2; i++ {
		r, err := p2.Recall(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if r.FromCache {
			t.Fatal("CacheTTL=0 必须完全禁用缓存")
		}
	}
}

// 缓存键**必须含**会话与线程：不含的话，第二个会话问同一个问题会拿到第一个
// 会话的召回结果——等于把上一次会话提到过的内容原样搬进这一次。
func TestCacheKeyIncludesSessionAndThread(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{atom("a1", "e1", "某条事实", types.ImportanceHigh)}}
	cfg := recall.DefaultConfig()
	cfg.CacheTTL = time.Minute
	p := newPipe(t, f, cfg)

	if _, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "事实", SessionID: "s1", ThreadID: "th1",
	}); err != nil {
		t.Fatal(err)
	}
	other, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "事实", SessionID: "s2", ThreadID: "th2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if other.FromCache {
		t.Fatal("缓存键必须含会话/线程：换个会话就命中上一个会话的结果")
	}
	// 同会话同线程才该复用
	same, err := p.Recall(context.Background(), recall.Request{
		Namespace: ns, Query: "事实", SessionID: "s1", ThreadID: "th1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !same.FromCache {
		t.Fatal("同会话同线程的重复查询必须命中缓存")
	}
}

// 缓存里的结果必须复制：调用方改了它，下次命中就是脏数据。
func TestCacheReturnsCopy(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{atom("a1", "e1", "某条事实", types.ImportanceHigh)}}
	cfg := recall.DefaultConfig()
	cfg.CacheTTL = time.Minute
	p := newPipe(t, f, cfg)
	req := recall.Request{Namespace: ns, Query: "事实"}

	first, _ := p.Recall(context.Background(), req)
	first.Snippets[0].Body = "被调用方改坏了"
	second, _ := p.Recall(context.Background(), req)
	if second.Snippets[0].Body == "被调用方改坏了" {
		t.Fatal("缓存交出的是同一份对象：调用方一改，缓存即被污染")
	}
}

// 缓存键必须含**归属**：同一会话、同一线程、同一查询词，
// 但归属不同的两次请求，不许命中同一条缓存。
//
// 这条不是「缓存不准」的问题，是**跨租户内容泄漏**：
// 乙组的请求拿到了甲组的召回结果，而那结果里全是甲组的事实。
//
// 默认 CacheTTL=0 把风险压住了，但那是配置在替代码兜底；
// 一旦有人开缓存，泄漏立刻发生。故键必须先修对。
func TestCacheKeyIsolatesScope(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("g1", "e1", "全局事实", types.ImportanceHigh),
		scopedAtom("a1", "team-a", "甲组事实"),
	}}
	cfg := recall.DefaultConfig()
	cfg.CacheTTL = time.Minute
	p := newPipe(t, f, cfg)

	base := recall.Request{Namespace: ns, Query: "事实", SessionID: "s1", ThreadID: "th1"}

	first, err := p.Recall(context.Background(), withScope(base, "team-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !hasRef(first, "a1") {
		t.Fatalf("甲组应看见本组事实，实得 %+v", first.Snippets)
	}

	// 乙组视角：其余请求维度全同，只有归属不同。
	other, err := p.Recall(context.Background(), withScope(base, "team-b"))
	if err != nil {
		t.Fatal(err)
	}
	if other.FromCache {
		t.Fatal("缓存键必须含归属：换个归属就命中上一个归属的结果")
	}
	if hasRef(other, "a1") {
		t.Fatalf("乙组看见了甲组的事实——跨租户泄漏: %+v", other.Snippets)
	}
	if !hasRef(other, "g1") {
		t.Fatalf("乙组应看见全局事实: %+v", other.Snippets)
	}
}

// 缓存键必须含**实体集合**：实体变了，画像源就变了，结果也就变了。
func TestCacheKeyIsolatesEntities(t *testing.T) {
	f := &fakeReader{
		atoms: []*types.AtomCard{atom("a1", "e1", "事实一", types.ImportanceHigh)},
		pages: map[string]*types.EntityPage{
			pageKey("", "e1"): {EntityID: "e1", Headline: "一号画像", UpdatedAt: base()},
			pageKey("", "e2"): {EntityID: "e2", Headline: "二号画像", UpdatedAt: base()},
		},
	}
	cfg := recall.DefaultConfig()
	cfg.CacheTTL = time.Minute
	cfg.Sources = []recall.Source{recall.SourceAtom, recall.SourcePageHeadline}
	p := newPipe(t, f, cfg)

	base := recall.Request{Namespace: ns, Query: "事实", SessionID: "s1", ThreadID: "th1"}
	if _, err := p.Recall(context.Background(), withEntities(base, "e1")); err != nil {
		t.Fatal(err)
	}
	other, err := p.Recall(context.Background(), withEntities(base, "e2"))
	if err != nil {
		t.Fatal(err)
	}
	if other.FromCache {
		t.Fatal("缓存键必须含实体集合：换个实体就命中上一个实体的结果")
	}
	if !strings.Contains(other.Render(), "二号画像") {
		t.Fatalf("应取到二号实体的画像，实得 %s", other.Render())
	}
}

// 缓存键必须含**条数上限**：上限变了结果条数就变了。
func TestCacheKeyIsolatesLimit(t *testing.T) {
	f := &fakeReader{atoms: []*types.AtomCard{
		atom("a1", "e1", "事实一", types.ImportanceHigh),
		atom("a2", "e2", "事实二", types.ImportanceHigh),
	}}
	cfg := recall.DefaultConfig()
	cfg.CacheTTL = time.Minute
	p := newPipe(t, f, cfg)

	base := recall.Request{Namespace: ns, Query: "事实", SessionID: "s1", ThreadID: "th1"}
	one, err := p.Recall(context.Background(), withLimit(base, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Snippets) != 1 {
		t.Fatalf("上限 1 应只回一条，实得 %d", len(one.Snippets))
	}
	two, err := p.Recall(context.Background(), withLimit(base, 2))
	if err != nil {
		t.Fatal(err)
	}
	if two.FromCache {
		t.Fatal("缓存键必须含条数上限：换个上限就命中上一个上限的结果")
	}
	if len(two.Snippets) != 2 {
		t.Fatalf("上限 2 应回两条，实得 %d", len(two.Snippets))
	}
}

// withScope 复制请求并带上归属。
func withScope(r recall.Request, scope types.Scope) recall.Request {
	r.Scope = scope
	return r
}

// withEntities 复制请求并带上实体集合。
func withEntities(r recall.Request, ids ...string) recall.Request {
	r.Entities = ids
	return r
}

// withLimit 复制请求并带上条数上限。
func withLimit(r recall.Request, limit int) recall.Request {
	r.Limit = limit
	return r
}

// hasRef 结果里有没有这一条。
func hasRef(r *recall.Result, refID string) bool {
	for _, s := range r.Snippets {
		if s.RefID == refID {
			return true
		}
	}
	return false
}

// 缓存命中必须**带着那两张通用表**回来，且是**副本**。
//
// 这条是被一个真实的漏抄抓出来的：Result 新增 Drops / Buckets 后，
// cloneResult 忘了抄它们——于是「第一次有、第二次没了」。
// 而缓存命中只在「第二次问同一个问题」时才显现，正是最难复现的那种形态。
func TestCacheHitCarriesDropTables(t *testing.T) {
	f := &fakeReader{}
	for i := 0; i < 40; i++ {
		f.atoms = append(f.atoms, atom(
			"a"+string(rune('a'+i%26))+string(rune('a'+i/26)), "e"+strconv.Itoa(i),
			"第 "+string(rune('A'+i/26))+"-"+string(rune('a'+i%26))+" 条很长的事实正文用来把预算吃满", types.ImportanceHigh))
	}
	cfg := recall.DefaultConfig()
	cfg.TotalBudget = 600
	cfg.CacheTTL = time.Minute
	cfg.Sources = []recall.Source{recall.SourceAtom}
	p := newPipe(t, f, cfg)
	req := recall.Request{Namespace: ns, Query: "事实"}

	first, err := p.Recall(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.Drops[ports.DropQuota] == 0 {
		t.Fatalf("前置条件：本用例要有桶不足的丢弃，实得 %v", first.Drops)
	}
	second, err := p.Recall(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.FromCache {
		t.Fatal("前置条件：第二次应命中缓存")
	}
	if second.Drops[ports.DropQuota] == 0 {
		t.Fatalf("缓存命中必须带着通用表回来，实得 %v", second.Drops)
	}
	if len(second.Buckets) == 0 {
		t.Fatalf("缓存命中必须带着桶计数回来，实得 %v", second.Buckets)
	}
	// 副本：调用方改了不该污染缓存。
	second.Drops[ports.DropQuota] = 999
	third, _ := p.Recall(context.Background(), req)
	if third.Drops[ports.DropQuota] == 999 {
		t.Fatal("通用表必须是副本：调用方改了不该污染缓存")
	}
}
