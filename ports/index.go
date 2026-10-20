package ports

import (
	"context"
	"errors"
)

// 本文件：向量化与向量索引端口。
//
// # 当前状态：**reserved（当前无消费点）**
//
// 这两个接口定义齐全，但**本仓没有任何代码调用它们**
// （全模块 grep Embedder / VectorIndex 只命中本文件自身）。
//
// 刻意把这件事写明，而不是让它们读起来像一项现成能力：
// **一个定义了却零消费者的导出接口，读起来像能力，实际不是**——
// 下游照着它接线会以为「装上就有语义检索」，而实际什么都不会发生。
//
// # 为什么留着
//
// 三个理由，合起来是「把缝留出来」：
//
//  1. 类型层已经有落脚点（types.AtomCard.Vector），
//     走语义轨的宿主不必另造一个平行结构；
//  2. 排序侧已经有注入缝（ports.Ranker），
//     宿主换排序口径不必绕开整条召回链自造一套；
//  3. 真要接向量轨时，接口形状已经定过，不必再改一轮类型。
//
// # 为什么现在不接
//
// 上游 octop-memory 自己也把向量面标为「默认关闭、测试全为 mock、
// 真实集成未验证」。本仓的判断相同：先把「该捞哪几层」「怎么排」
// 这件事做对，语义相似度是第二阶段的替换件，不是第一阶段的前提。
// 本仓的知识检索面另有 RAG 平面，两者不是一回事。

// ErrNoEmbedder 未注入向量化能力时的哨兵（语义同 ErrNoLLM）。
var ErrNoEmbedder = errors.New("ports: no embedder injected")

// ErrNoIndex 未注入向量索引时的哨兵。
var ErrNoIndex = errors.New("ports: no vector index injected")

// Embedder 向量化端口。
//
// # reserved：当前无消费点
//
// 本仓不调用它（见本文件的包级说明）。
type Embedder interface {
	// Embed 返回文本的向量。维度由实现固定，消费方从首次返回取。
	Embed(ctx context.Context, text string) ([]float32, error)
}

// NoopEmbedder 未注入时的占位实现。
type NoopEmbedder struct{}

// Embed 恒返回 ErrNoEmbedder。
func (NoopEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, ErrNoEmbedder
}

// VectorHit 一条向量检索命中。
type VectorHit struct {
	// ID 命中的对象标识（事实卡 ID）。
	ID string
	// Score 相似度（越大越近；实现自定度量，面板需保证可比）。
	Score float32
}

// VectorIndex 向量索引端口。
//
// # reserved：当前无消费点
//
// 本仓不调用它（见本文件的包级说明）。
//
// # 默认关，且**不是**关系后端
//
// 索引丢了可以从关系表重建，关系表丢了索引救不回来——两者的地位不对称。
// 因此索引接口只提供「增强检索」，不提供任何写入真源的入口：
// 事实卡的生杀大权始终在 Store 上。
type VectorIndex interface {
	// Search 返回最相似的若干对象标识。
	Search(ctx context.Context, namespace string, vec []float32, topK int) ([]VectorHit, error)
	// Upsert 写入或更新一条。
	Upsert(ctx context.Context, namespace, id string, vec []float32) error
	// Delete 删除一条（索引与关系表的一致性由调用方保证）。
	Delete(ctx context.Context, namespace, id string) error
}

// NoopIndex 未注入时的占位实现。
type NoopIndex struct{}

// Search 恒返回 ErrNoIndex。
func (NoopIndex) Search(context.Context, string, []float32, int) ([]VectorHit, error) {
	return nil, ErrNoIndex
}

// Upsert 恒返回 ErrNoIndex。
func (NoopIndex) Upsert(context.Context, string, string, []float32) error { return ErrNoIndex }

// Delete 恒返回 ErrNoIndex。
func (NoopIndex) Delete(context.Context, string, string) error { return ErrNoIndex }
