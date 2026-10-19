package ports

import (
	"context"
	"errors"
)

// ErrNoEmbedder 未注入向量化能力时的哨兵（语义同 ErrNoLLM）。
var ErrNoEmbedder = errors.New("ports: no embedder injected")

// ErrNoIndex 未注入向量索引时的哨兵。
var ErrNoIndex = errors.New("ports: no vector index injected")

// Embedder 向量化端口。
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
// # 默认关，且**不是**关系后端
//
// 索引丢了可以从关系表重建，关系表丢了索引救不回来——两者的地位不对称。
// 因此索引接口只提供「增强检索」，不提供任何写入真源的入口：
// 事实卡的生杀大权始终在 Store 上。
//
// # 当前不接的理由（见方案 §5）
//
// 上游 octop-memory 自己也把向量面标为「默认关闭、测试全为 mock、真实集成
// 未验证」。本仓的知识检索面另有 RAG 平面，两者不是一回事。在本地面
// （词法 + 结构化过滤）稳定之前接向量，是给不确定的收益付确定的复杂度。
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
