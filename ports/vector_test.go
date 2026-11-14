package ports_test

import (
	"context"
	"math"
	"testing"

	"github.com/yi-nology/agent-memory/ports"
)

// 本文件：向量轨的用例。
//
// # 三条要钉住的
//
//  1. **确定性**：同一段文本永远同一个向量（可复现是硬承诺）。
//  2. **维度一致**：维度对不上要**报错**，不���静默返回空集
//     （「空集」的失效形态是「语义召回永远空」，而调用方看不出是维度错了）。
//  3. **过取**：向量索引里没有归属维度，归属过滤在取回事实卡时做，
//     故本路的取数要多取几倍来补上被别家占掉的配额。

// 同一段文本两次嵌入必须完全一样。
func TestHashEmbedderIsDeterministic(t *testing.T) {
	e := ports.HashEmbedder{}
	a, err := e.Embed(context.Background(), "磁盘长期偏小")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(context.Background(), "磁盘长期偏小")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) {
		t.Fatalf("两次维度不同: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("同一段文本两次嵌入不同（第 %d 位）: %v vs %v", i, a[i], b[i])
		}
	}
}

// 维度固定；不同文本的向量不同。
func TestHashEmbedderShape(t *testing.T) {
	e := ports.HashEmbedder{Dim: 64}
	v, err := e.Embed(context.Background(), "磁盘长期偏小")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 64 {
		t.Fatalf("维度不对: %d", len(v))
	}
	other, err := e.Embed(context.Background(), "内存占用偏高")
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for i := range v {
		if v[i] != other[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("不同文本不该得到同一个向量")
	}
}

// 词面重叠越多，余弦相似度越高（它捕捉的是**词面**软匹配）。
//
// 这条钉的是**它到底是什么**：不是语义嵌入，是词面哈希。
// 用例名与注释都写清楚，免得后人把它当成「语义检索已通」。
func TestHashEmbedderCapturesLexicalOverlap(t *testing.T) {
	e := ports.HashEmbedder{}
	base, _ := e.Embed(context.Background(), "磁盘 长期 偏小")
	near, _ := e.Embed(context.Background(), "磁盘 长期 偏小 一些")
	far, _ := e.Embed(context.Background(), "内存 占用 偏高")
	simNear := cosine(base, near)
	simFar := cosine(base, far)
	if simNear <= simFar {
		t.Fatalf("词面更近的应更相似: near=%v far=%v", simNear, simFar)
	}
}

// 内存索引的增删查。
func TestMemIndexCRUD(t *testing.T) {
	ctx := context.Background()
	ix := ports.NewMemIndex()
	vec := []float32{1, 0, 0}
	if err := ix.Upsert(ctx, "ns", "a1", "", vec); err != nil {
		t.Fatal(err)
	}
	if ix.Count("ns") != 1 {
		t.Fatalf("应有 1 条: %d", ix.Count("ns"))
	}
	hits, err := ix.Search(ctx, "ns", "", []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "a1" {
		t.Fatalf("应命中 a1: %+v", hits)
	}
	if hits[0].Score < 0.99 {
		t.Fatalf("同向向量的相似度应接近 1: %v", hits[0].Score)
	}
	// 覆盖写
	if err := ix.Upsert(ctx, "ns", "a1", "", []float32{0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if ix.Count("ns") != 1 {
		t.Fatalf("同一 ID 覆盖写应仍只有一条: %d", ix.Count("ns"))
	}
	// 删
	if err := ix.Delete(ctx, "ns", "a1"); err != nil {
		t.Fatal(err)
	}
	if ix.Count("ns") != 0 {
		t.Fatalf("删后应为 0: %d", ix.Count("ns"))
	}
}

// 维度对不上要报错（不是静默空集）。
func TestMemIndexRejectsDimensionMismatch(t *testing.T) {
	ctx := context.Background()
	ix := ports.NewMemIndex()
	if err := ix.Upsert(ctx, "ns", "a1", "", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := ix.Upsert(ctx, "ns", "a2", "", []float32{1, 0, 0, 0}); err == nil {
		t.Fatal("维度不一致必须报错")
	}
	if _, err := ix.Search(ctx, "ns", "", []float32{1, 0, 0, 0}, 10); err == nil {
		t.Fatal("查询向量维度不一致必须报错")
	}
}

// recordingIndex 记下每次被要求取多少条的索引。
type recordingIndex struct {
	topKs []int
}

func (r *recordingIndex) Search(_ context.Context, _, _ string, _ []float32, topK int) ([]ports.VectorHit, error) {
	r.topKs = append(r.topKs, topK)
	return nil, nil
}
func (r *recordingIndex) Upsert(context.Context, string, string, string, []float32) error {
	return nil
}
func (r *recordingIndex) Delete(context.Context, string, string) error { return nil }

// 语义召回面：过取（补上被「失效事实」占掉的槽位）。
//
// # 过取的理由变了（2026-10-21）
//
// 此前过取是为了补**归属**过滤吃掉的槽位——那是个**后置过滤**，
// 正是本仓明令禁止的形态（被别家占掉的 topK 槽位已经花掉了）。
// 故归属进了**索引**（VectorIndex.Search 带 scope），过滤在取数层生效 ✓。
//
// 现在过取只为了补**失效事实**吃掉的槽位：索引里没有时间维度，
// 退役事实的向量可能还在（退役时没删干净的那批），
// 它们会被「取回事实卡」那一步滤掉——而那一步在 topK 之后。
//
// 判据：两次取数都是 limit 的过取倍数（与归属无关）。
func TestVectorSemanticReaderOverFetches(t *testing.T) {
	ctx := context.Background()
	ix := &recordingIndex{}
	r := ports.VectorSemanticReader{Embedder: ports.HashEmbedder{}, Index: ix}

	if _, err := r.Search(ctx, "ns", "磁盘", "", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Search(ctx, "ns", "磁盘", "team-a", 5); err != nil {
		t.Fatal(err)
	}
	if len(ix.topKs) != 2 {
		t.Fatalf("应取过两次: %v", ix.topKs)
	}
	for i, got := range ix.topKs {
		if got != 5*ports.DefaultSemanticOverFetch {
			t.Fatalf("第 %d 次取数应过取 %d 倍，实得 %d",
				i, ports.DefaultSemanticOverFetch, got)
		}
	}
}

// cosine 余弦相似度（夹具用）。
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// SearchGlobal 只返回**归属为空**（全局）的向量——全局读者不能共用
// Search 的空 scope（冻结语义是「不隔离」，issue #62）。
func TestMemIndexSearchGlobalOnly(t *testing.T) {
	ctx := context.Background()
	ix := ports.NewMemIndex()
	vec := []float32{1, 0}
	for _, v := range []struct {
		id, scope string
	}{
		{"g", ""},       // 全局
		{"a", "team-a"}, // 本归属
		{"b", "team-b"}, // 别归属
	} {
		if err := ix.Upsert(ctx, "ns", v.id, v.scope, vec); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ix.SearchGlobal(ctx, "ns", vec, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "g" {
		t.Fatalf("SearchGlobal 应只命中全局的 g: %+v", got)
	}
	// 对照：Search 的空 scope 是「不隔离」——三个全回来。
	// 两者不同判据正是 #62 的病根（空值承载不了两种相反的语义）。
	all, err := ix.Search(ctx, "ns", "", vec, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("空 scope（不隔离）应命中全部 3 条: %+v", all)
	}
}

// plainIndex 不实现 GlobalVectorSearcher 的最小组索引：只够跑 Search。
type plainIndex struct {
	hits []ports.VectorHit
}

func (p *plainIndex) Search(context.Context, string, string, []float32, int) ([]ports.VectorHit, error) {
	return p.hits, nil
}
func (p *plainIndex) Upsert(context.Context, string, string, string, []float32) error {
	return nil
}
func (p *plainIndex) Delete(context.Context, string, string) error { return nil }

// VectorSemanticReader.SearchGlobal：
//   - 索引实现了全局读者检索 ⇒ 透传，只回全局命中；
//   - 索引没实现 ⇒ 声明的降级路径：回落空 scope 检索（命中集合与
//     Search(ctx, ns, query, "", limit) 逐字同效——不变坏），不是报错。
func TestVectorSemanticReaderSearchGlobal(t *testing.T) {
	ctx := context.Background()
	emb := ports.HashEmbedder{}

	// ① 能力在索引层落实：MemIndex 实现了 SearchGlobal。
	ix := ports.NewMemIndex()
	vec, err := emb.Embed(ctx, "磁盘")
	if err != nil {
		t.Fatal(err)
	}
	_ = ix.Upsert(ctx, "ns", "g", "", vec)
	_ = ix.Upsert(ctx, "ns", "a", "team-a", vec)
	r := ports.VectorSemanticReader{Embedder: emb, Index: ix}
	got, err := r.SearchGlobal(ctx, "ns", "磁盘", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "g" {
		t.Fatalf("SearchGlobal 应只回全局的 g: %+v", got)
	}

	// ② 索引没实现 ⇒ 降级回落空 scope（命中集合不变坏）。
	plain := &plainIndex{hits: []ports.VectorHit{{ID: "x", Score: 0.9}}}
	r2 := ports.VectorSemanticReader{Embedder: emb, Index: plain}
	got2, err := r2.SearchGlobal(ctx, "ns", "磁盘", 10)
	if err != nil {
		t.Fatalf("降级不是错误（召回链把语义面错误当致命）: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != "x" {
		t.Fatalf("降级应与空 scope 检索同效: %+v", got2)
	}
}
