package ports

import (
	"context"
	"errors"
)

// 本文件：向量化与向量索引端口。
//
// # 当前状态：**已接通**（消费点在 pipeline/recall 与 application）
//
// 这两个接口现在有真的消费方：
//
//   - 写路径：晋升成事实后把向量写进索引（application 的装配面）；
//   - 读路径：召回时算查询向量 → 搜索引 → 与词面路用 RRF 融合
//     （见 ports.VectorSemanticReader 与 pipeline/recall/hybrid.go）。
//
// 装配面只需给 Embedder 与 VectorIndex 两件；不给 = 不启用向量面，
// 召回**逐条**回到纯词面（零漂移，见 AGENTS.md「混合检索」那条）。
//
// # 为什么内核自己接，而不是继续留窄缝
//
// 此前只有一条窄缝（SemanticReader：宿主给「最像的 topK」），
// 宿主得自己写「算向量 + 搜索引 + 归属过取」那一层胶水。
// 胶水每家写一遍就会各写一遍：漏了归属过取的那家，
// 语义路的配额会被别家吃掉——症状是「语义召回像是没生效」，而查不出原因。
//
// 语义**强度**仍然归宿主（内核不引模型 SDK，见下）；
// 但**管线**归内核：这是本仓一贯的分工（排序口径归宿主、链路归内核）。
//
// # 语义强度：内核不引模型 SDK
//
// 上游 octop-memory 自己也把向量面标为「默认关闭、测试全为 mock、
// 真实集成未验证」。本仓的判断相同：核心一旦引了 SDK，
// 「没有模型时手工写入 + 词法召回必须能跑」那条降级路径就没了——
// 而它塌了之后不会立刻暴露，它塌在「用户配了模型、装了数据库」的生产环境里。
//
// 故本仓给的是**诚实的可跑实现**（ports.HashEmbedder：词面哈希嵌入，
// 捕捉词面软匹配，**不是语义嵌入**），
// 而真正接上模型的宿主注入自己的 Embedder 即可——接口形状已经定过，不必再改一轮类型。

// ErrNoEmbedder 未注入向量化能力时的哨兵（语义同 ErrNoLLM）。
var ErrNoEmbedder = errors.New("ports: no embedder injected")

// ErrNoIndex 未注入向量索引时的哨兵。
var ErrNoIndex = errors.New("ports: no vector index injected")

// Embedder 向量化端口。
//
// 本仓有一个可跑实现（HashEmbedder，词面哈希嵌入），
// 走真模型的宿主注入自己的实现即可。
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
// 本仓有一个可跑实现（MemIndex，**单进程**内存索引），
// 多副本部署的宿主注入自己的持久索引（Chroma / Qdrant / PG 向量扩展……）。
//
// # 默认关，且**不是**关系后端
//
// 索引丢了可以从关系表重建，关系表丢了索引救不回来——两者的地位不对称。
// 因此索引接口只提供「增强检索」，不提供任何写入真源的入口：
// 事实卡的生杀大权始终在 Store 上。
type VectorIndex interface {
	// Search 返回最相似的若干对象标识。
	//
	// scope 是读者归属（空=不隔离）：非空时只返回「全局 + 同归属」的向量，
	// 过滤**在这一层生效**（进 SQL，与 TopK 同一次求值）。
	//
	// # 为什么归属必须进索引
	//
	// 归属过滤若放在调用方后置，被别家占掉的槽位已经花掉了——
	// 于是「该给我的那条」根本没进候选池，而症状是「语义召回像是没生效」。
	// 过取（多取几倍）只是缓解，不是修法：取数上限仍被别家占着。
	//
	// 规则与 store.AtomFilter.Scope 同一条：空=不隔离，
	// 非空时取「全局 + 同归属」。
	Search(ctx context.Context, namespace, scope string, vec []float32, topK int) ([]VectorHit, error)
	// Upsert 写入或更新一条（scope 是这条向量的归属）。
	Upsert(ctx context.Context, namespace, id, scope string, vec []float32) error
	// Delete 删除一条（索引与关系表的一致性由调用方保证）。
	Delete(ctx context.Context, namespace, id string) error
}

// NoopIndex 未注入时的占位实现。
type NoopIndex struct{}

// Search 恒返回 ErrNoIndex。
func (NoopIndex) Search(context.Context, string, string, []float32, int) ([]VectorHit, error) {
	return nil, ErrNoIndex
}

// Upsert 恒返回 ErrNoIndex。
func (NoopIndex) Upsert(context.Context, string, string, string, []float32) error {
	return ErrNoIndex
}

// Delete 恒返回 ErrNoIndex。
func (NoopIndex) Delete(context.Context, string, string) error { return ErrNoIndex }
