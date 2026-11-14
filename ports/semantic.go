package ports

import "context"

// 本文件：语义召回（混合检索的另一半）的端口。
//
// # 为什么是**窄**的一条缝
//
// 宿主走语义轨时手里有的是「向量化器 + 向量索引」两件，
// 而召回需要的是「给我 topK 条最像的」——于是内核只声明后者。
//
// 差别很实际：内核若自己持有 Embedder 与 VectorIndex，
// 就得负责「什么时候向量化、谁维护索引、索引丢了怎么办」，
// 而那三件事**都是宿主的事**（宿主知道自己的模型与自己的索引）。
// 何况本仓的向量轨仍标为「默认关、真实集成未验证」——
// 内核替宿主把向量面跑起来，等于替宿主背一个自己没验证过的依赖。
//
// # 不注入也能跑
//
// 语义面是**可选增强**：不注入时召回走纯词面，
// 那条「没有模型时手工写入 + 词法召回必须能跑」的降级路径必须一直能跑。
// 注入时它是**一路**，与词面路并列，最后由融合器合并（见 RRF）。
//
// # 分数的量纲由实现自定，面板负责可比
//
// 与 Ranker 同一条纪律：跨路的分数**不能直接相加**
// （bm25 的分数无上界，向量相似度在 [-1,1]），
// 故融合走名次（RRF）而不是分数。

// SemanticHit 一条语义召回命中。
type SemanticHit struct {
	// ID 命中的事实卡标识。
	ID string
	// Score 相似度（越大越近；量纲由实现自定）。
	Score float64
}

// SemanticReader 语义召回面。
//
// # 契约
//
//  1. 同一个 ID **不许出现两次**：重复会让融合分数翻倍，
//     而那等价于「这一条被硬塞进结果」；
//  2. **归属过滤在这一层生效**：scope 非空时，别归属的事实一条都不许返回。
//     理由与词面路相同：这里也要参与 TopK 配额，
//     后置过滤的话被别家占掉的配额已经花掉了。
type SemanticReader interface {
	// Search 返回与查询最像的若干条。
	//
	// namespace 与 scope 分别是记忆库与读者归属（空=不隔离）。
	// limit 是本路的取数上限（融合前的候选池大小）。
	Search(ctx context.Context, namespace, query, scope string, limit int) ([]SemanticHit, error)
}

// GlobalSemanticReader 全局读者语义检索能力（issue #62）。
//
// # 它是什么
//
// 以**全局读者**身份检索：只返回**归属为空**（全局）的事实卡的命中，
// 归属过滤与 TopK 同一次求值（取数层生效，不许后置滤）。
//
// # 为什么是可选能力
//
// Search 的 scope 空值语义已在冻结面上发布为「**不隔离**」（宿主实现着
// 这个接口，改空值语义或加参数 = 隐式 breaking）——而**全局读者**按
// types.Scope.VisibleTo 只看得见全局卡。同一个空值承载不了两种相反的
// 语义（这正是 #62 的病根：空读者的语义 TopK 被别家的向量占掉，
// 只靠过取 + 后置滤兜着，配额损失不修自愈）。
// 故按可选能力三要件单独立面：支持「只取全局」的语义后端实现本面；
// 召回链运行时探测（GlobalSemanticReaderOf），没实现时维持原状
// （空 scope 透传——命中集合不变，代价只是配额可能被占，
// 降级可观测：Result.SemanticGlobalUnavailable 与
// recall.semantic.global.unavailable 指标）。
type GlobalSemanticReader interface {
	// SearchGlobal 返回与查询最像的若干条**全局**事实卡。
	//
	// namespace 与 limit 语义同 Search。实现方承诺：返回的每一条的
	// 归属都为空（全局卡人人都看得见，故无需读者归属做参数），
	// 且过滤发生在取数层（与 TopK 同一次求值）。
	SearchGlobal(ctx context.Context, namespace, query string, limit int) ([]SemanticHit, error)
}

// GlobalSemanticReaderOf 探测一个语义面有没有全局读者检索能力
// （语义同 store 的探测约定：探测语义只有一个地方定义）。
//
// 返回 ok=false 时调用方走**声明的降级路径**（空 scope 透传），
// 不许当它不存在地继续——也不许把缺失当致命错误（语义面是可选增强）。
func GlobalSemanticReaderOf(v any) (GlobalSemanticReader, bool) {
	g, ok := v.(GlobalSemanticReader)
	return g, ok
}

// NoopSemanticReader 未注入语义面时的占位实现。
type NoopSemanticReader struct{}

// Search 恒返回空集（不是错误）。
//
// 为什么不是错误：语义面是**可选增强**，
// 「没装语义面」是正常配置而不是故障。
// 真要语义召回却拿不到结果时，调用方能从「一路都没有」这件事上发现。
func (NoopSemanticReader) Search(context.Context, string, string, string, int) ([]SemanticHit, error) {
	return nil, nil
}
