package parity

import (
	"context"
	"testing"
)

// 本文件：向量索引（持久化那一面）的跨后端共享行为断言。
//
// # 为什么这一组断言不挂在 store.Store 上
//
// 向量索引不是关系表的一翼，它是**另一个端口**（ports.VectorIndex）：
// 事实卡是它的真源，向量是派生数据（丢了可以从事实卡重算）。
// store.Store 是「关系表的存取契约」，把向量塞进去等于把派生面写成真源面。
//
// # 为什么本包不复用 ports.VectorIndex
//
// 本包（storage-parity）的依赖方向只许 types 与 store（见 architecture-policy.yaml），
// 而端口契约住在 ports（它零内部依赖，是全仓地基）。
// 复述一份最小面（VectorIndex / VectorHit）换来的是：两个后端仍跑**同一份**断言，
// 而这份断言不被依赖方向挡掉。
//
// 代价是这层形状必须与 ports.VectorIndex 对齐——对齐由 Adapt 钉住：
// 各后端把「命中类型待定」的那一面交给 Adapt（命中类型由调用方用类型参数填），
// 端口那边改了形状，调用方那一行就**编译不过**。
// 于是「共享断言断言的不是真接口」这件事不会悄悄发生。
//
// # 这一组断言守的是什么
//
// ① 归属隔离：**跨租户泄漏**的防线（别归属的向量一条都不许返回）；
// ② 维度不一致：必须报错而不是静默返回空集（空集的失效形态是「语义召回永远空」）；
// ③ 覆盖写与删除：索引是派生面，写坏了不会自己好，得能改能删；
// ④ 命名空间隔离：两个记忆库之间互不可见。

// VectorHit 一条向量检索命中（**本包自己的形状**，理由见文件头注释）。
type VectorHit struct {
	// ID 命中的对象标识。
	ID string
	// Score 相似度（越大越近）。
	Score float32
}

// VectorIndex 共享断言要求的向量索引面。
//
// 形状与 ports.VectorIndex 一致（见文件头注释）。
type VectorIndex interface {
	// Search 返回最相似的若干对象标识。
	//
	// scope 是读者归属（空=不隔离）：非空时只返回「全局 + 同归属」的向量，
	// 过滤**在这一层生效**（进 SQL，与 TopK 同一次求值）。
	Search(ctx context.Context, namespace, scope string, vec []float32, topK int) ([]VectorHit, error)
	// Upsert 写入或更新一条（scope 是这条向量的归属）。
	Upsert(ctx context.Context, namespace, id, scope string, vec []float32) error
	// Delete 删除一条。
	Delete(ctx context.Context, namespace, id string) error
}

// searcher 命中类型待定的那一面（Adapt 的约束）。
//
// 刻意只声明方法、不提命中类型：命中类型由调用方用类型参数填，
// 于是本包不必认识 ports 的那一份（依赖方向不许）。
type searcher[H any] interface {
	Search(ctx context.Context, namespace, scope string, vec []float32, topK int) ([]H, error)
	Upsert(ctx context.Context, namespace, id, scope string, vec []float32) error
	Delete(ctx context.Context, namespace, id string) error
}

// Adapt 把「命中类型待定」的向量索引面翻成本包的面。
//
// 调用方（各后端的测试文件）传端口那一面与它的翻法：
//
//	parity.Adapt(idx, func(h ports.VectorHit) parity.VectorHit {
//		return parity.VectorHit{ID: h.ID, Score: h.Score}
//	})
//
// 这一行同时是**契约钉子**：端口那边改了命中类型或签名，这一行立刻编译不过。
func Adapt[H any, S searcher[H]](idx S, conv func(H) VectorHit) VectorIndex {
	return &adapter[H]{idx: idx, conv: conv}
}

// adapter Adapt 的实现（只做形状翻译，不加任何判断）。
type adapter[H any] struct {
	idx  searcher[H]
	conv func(H) VectorHit
}

// Search 检索并把命中翻成本包的形状。
func (a *adapter[H]) Search(ctx context.Context, namespace, scope string, vec []float32,
	topK int) ([]VectorHit, error) {
	hits, err := a.idx.Search(ctx, namespace, scope, vec, topK)
	if err != nil {
		return nil, err
	}
	out := make([]VectorHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, a.conv(h))
	}
	return out, nil
}

// Upsert 写入或更新一条。
func (a *adapter[H]) Upsert(ctx context.Context, namespace, id, scope string, vec []float32) error {
	return a.idx.Upsert(ctx, namespace, id, scope, vec)
}

// Delete 删除一条。
func (a *adapter[H]) Delete(ctx context.Context, namespace, id string) error {
	return a.idx.Delete(ctx, namespace, id)
}

// RunVector 跑向量索引这一组共享断言。
//
// # 为什么单独一个入口，而不是挂在 Run 里
//
// Run 收的是 store.Store，而向量索引**不是** store.Store 的一翼
// （它是另一个端口，见文件头注释）——Run 里没有它可取。
// 故这一组由各后端在自己的测试文件里调用本函数，
// 断言本体仍然只有**这一份**，两个后端跑的是同一组行为。
//
// newIndex 每个用例给一份**干净的**索引（各后端自己开一份新库）。
func RunVector(t *testing.T, newIndex func(t *testing.T) VectorIndex) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, VectorIndex)
	}{
		{"向量索引：写入后可搜到", tVectorRoundTrip},
		{"向量索引：归属隔离", tVectorScopeIsolation},
		{"向量索引：覆盖写与删除", tVectorUpsertAndDelete},
		{"向量索引：维度不一致报错", tVectorDimensionMismatch},
		{"向量索引：命名空间隔离", tVectorNamespaceIsolation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.fn(t, newIndex(t))
		})
	}
}

// tVectorRoundTrip 写入后可搜到：自己搜自己，分数接近 1。
//
// 「接近 1」而不是「等于 1」：分数是 float32，算出来的 1 也可能差一个 ulp。
func tVectorRoundTrip(t *testing.T, idx VectorIndex) {
	vec := []float32{1, 0, 0}
	if err := idx.Upsert(bg(), string(nsA), "v1", "", vec); err != nil {
		t.Fatalf("写向量失败: %v", err)
	}
	hits, err := idx.Search(bg(), string(nsA), "", vec, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "v1" {
		t.Fatalf("应恰好命中 v1，实得 %+v", hits)
	}
	if hits[0].Score < 0.99 {
		t.Fatalf("自己搜自己分数应接近 1，实得 %v", hits[0].Score)
	}
}

// tVectorScopeIsolation 归属隔离：读者只看得到「全局 + 同归属」。
//
// 这是**跨租户泄漏**的防线：别归属的向量一条都不许返回。
// 与事实卡、关系边同一判据（全局的 + 同归属的，别组的一条都不给）。
func tVectorScopeIsolation(t *testing.T, idx VectorIndex) {
	for _, v := range []struct {
		id    string
		scope string
	}{
		{"g", ""},        // 全局：人人都该看到
		{"a", "scope-a"}, // 本组
		{"b", "scope-b"}, // 别组
	} {
		if err := idx.Upsert(bg(), string(nsA), v.id, v.scope, []float32{1, 0}); err != nil {
			t.Fatalf("写向量失败(%s): %v", v.id, err)
		}
	}

	// 以 scope-a 搜：全局的 + 本组的，别组的一条都不许出现。
	hits, err := idx.Search(bg(), string(nsA), "scope-a", []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	for _, h := range hits {
		if h.ID == "b" {
			t.Fatalf("别归属的向量漏进来了（跨归属泄漏）: %+v", hits)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("scope-a 应看到 2 条（全局 + 本组），实得 %d: %+v", len(hits), hits)
	}

	// 归属为空 = **不隔离**：三条都在（既有语义不变）。
	hits, err = idx.Search(bg(), string(nsA), "", []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("不隔离时应得 3 条，实得 %d", len(hits))
	}
}

// tVectorUpsertAndDelete 覆盖写与删除。
//
// 索引是**派生面**：写错了不会自己好，故必须能覆盖、能删。
// 覆盖写后同一条**只有一条**（不是两条），且内容换成新的。
func tVectorUpsertAndDelete(t *testing.T, idx VectorIndex) {
	oldVec := []float32{1, 0, 0}
	newVec := []float32{0, 1, 0}
	if err := idx.Upsert(bg(), string(nsA), "v1", "", oldVec); err != nil {
		t.Fatalf("写向量失败: %v", err)
	}
	if err := idx.Upsert(bg(), string(nsA), "v1", "", newVec); err != nil {
		t.Fatalf("覆盖写失败: %v", err)
	}
	hits, err := idx.Search(bg(), string(nsA), "", newVec, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("覆盖写后应只有一条，实得 %d: %+v", len(hits), hits)
	}
	if hits[0].Score < 0.99 {
		t.Fatalf("覆盖后应搜到新内容，实得分数 %v", hits[0].Score)
	}
	// 旧向量不该还满分（两条正交，余弦是 0）。
	hits, err = idx.Search(bg(), string(nsA), "", oldVec, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 1 || hits[0].Score > 0.5 {
		t.Fatalf("覆盖后旧向量不该还高分，实得 %+v", hits)
	}
	// 删掉后搜不到。
	if err := idx.Delete(bg(), string(nsA), "v1"); err != nil {
		t.Fatalf("删向量失败: %v", err)
	}
	hits, err = idx.Search(bg(), string(nsA), "", newVec, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("删除后仍搜得到: %+v", hits)
	}
}

// tVectorDimensionMismatch 维度不一致必须**报错**，而不是返回空集。
//
// 空集的失效形态是「语义召回永远空」，而调用方看不出是维度错了
// ——那正是本仓明令要防的那种静默失效。
//
// 哨兵（ports.ErrDimensionMismatch）由各后端自己的用例钉：
// 本包不认识 ports，故这里只断言「报错 + 没有命中」。
func tVectorDimensionMismatch(t *testing.T, idx VectorIndex) {
	if err := idx.Upsert(bg(), string(nsA), "v1", "", []float32{1, 2, 3}); err != nil {
		t.Fatalf("写向量失败: %v", err)
	}
	hits, err := idx.Search(bg(), string(nsA), "", []float32{1, 2, 3, 4}, 10)
	if err == nil {
		t.Fatalf("维度不一致必须报错（实得空集 %+v）", hits)
	}
	if len(hits) != 0 {
		t.Fatalf("维度不一致时不该返回命中: %+v", hits)
	}
}

// tVectorNamespaceIsolation 命名空间之间互不可见。
func tVectorNamespaceIsolation(t *testing.T, idx VectorIndex) {
	if err := idx.Upsert(bg(), string(nsA), "v1", "", []float32{1, 0}); err != nil {
		t.Fatalf("写向量失败: %v", err)
	}
	hits, err := idx.Search(bg(), string(nsB), "", []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("另一个命名空间的向量不该被搜到: %+v", hits)
	}
}
