package ports

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"sort"
	"sync"

	"github.com/oagentkit/textx/textutil"
)

// 本文件：向量轨的**可运行**实现（纯标准库 + 上游切词）。
//
// # 三件东西的分工
//
//   - Embedder：文本 → 向量（接口在 index.go；本文件给一个确定性实现，
//     宿主也可注入自己的真模型）。
//   - VectorIndex：向量的存取与检索（接口在 index.go；本文件给一个内存实现）。
//   - VectorSemanticReader：把上面两件组合成**语义召回面**（召回链只认它）。
//
// # 诚实声明：哈希嵌入**不是语义嵌入**
//
// HashEmbedder 把词元哈希到固定维度，它捕捉的是**词面**重叠——
// 与词面召回比，它多给的是「软匹配」（部分词元命中），
// 而**不是**「意思相近」。
// 要真正的语义相似度，宿主注入自己的 Embedder（接口形状见 index.go）。
//
// 那为什么还要它：向量轨要**跑得起来**才谈得上「接通」。
// 一个只有接口、没有实现、也没有消费点的向量面，
// 读起来像能力，实际不是（这正是 index.go 此前那版注释说的话）。
// 本仓的判断是：**接口 + 一个诚实的可跑实现 + 真的消费点**，
// 而「语义强度」交给宿主换实现。
//
// # 内存索引的适用面（别把它当生产方案）
//
// MemIndex 是**单进程**的：多副本部署时各副本的索引互不同步，
// 而事实卡是共享的——于是「按 A 副本的索引召回」可能拿到 B 副本没写的向量，
// 或漏掉 A 副本刚写的。
// 故它只适合单进程宿主与测试；多副本部署要注入宿主自己的索引
// （Chroma / Qdrant / PG 的向量扩展……）。

// DefaultEmbedDim 缺省向量维度。
//
// 256 是经验值：够大时哈希碰撞少（词元少的情况下 128 也够），
// 又小到「暴力扫描整库」在单进程里不肉疼。
const DefaultEmbedDim = 256

// ErrDimensionMismatch 向量维度不一致。
//
// 维度不一致时**必须报错**而不是静默返回空集：
// 静默空集的失效形态是「语义召回永远空」，而调用方看不出是维度错了。
var ErrDimensionMismatch = errors.New("ports: 向量维度不一致")

// HashEmbedder 词面哈希嵌入（确定性）。
//
// # 确定性是它的卖点
//
// 同一段文本永远得到同一个向量：没有随机初始化、没有模型版本漂移。
// 故同一批记忆在不同进程、不同机器上算出的向量一致——
// 而这正是「召回结果可复现」的前提（评测面靠它成立）。
//
// # 切词走上游，与检索面同一套
//
// 用 textutil.Tokens（与 store.LexicalTokens 同一个上游）——
// 两处切法不一致的话，向量轨与词面轨看到的就不是同一段文本，
// 融合出来的结果也就没法复现。
type HashEmbedder struct {
	// Dim 向量维度（零=DefaultEmbedDim）。
	Dim int
	// Weight 词元权重（零=1）。
	Weight float64
}

// Embed 把文本算成向量（单位长度）。
func (e HashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	dim := e.Dim
	if dim <= 0 {
		dim = DefaultEmbedDim
	}
	weight := e.Weight
	if weight == 0 {
		weight = 1
	}
	vec := make([]float32, dim)
	for _, tok := range textutil.Tokens(text) {
		h := fnv32a(tok)
		// 双哈希：低位定维、高位定符号。
		//
		// 带符号是为了压碰撞的**系统性偏差**：不带符号时，
		// 落在同一维的两个词元会互相抵消（一个加一个减），
		// 于是「撞了词的文本」得分偏低——那是个与内容无关的偏差。
		idx := int(h % uint32(dim))
		sign := float32(1)
		if h&0x80000000 != 0 {
			sign = -1
		}
		vec[idx] += float32(weight) * sign
	}
	normalize(vec)
	return vec, nil
}

// fnv32a FNV-1a 哈希（32 位）。
//
// 用标准库而不是手写：手写的位运算版本**不保证跨 Go 版本稳定**
// （哈希算法一旦变了，同一批记忆的向量就全变了，而那在数据上看不出来）。
//
// 不用 math/rand 的随机源：那会把「同一段文本」与「同一个向量」解耦，
// 而可复现是本仓的一条硬承诺。
func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// normalize 把向量归一成单位长度（就地）。
//
// 归一后余弦相似度就是点积，于是检索侧不必每次再算一次模长。
func normalize(vec []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		return
	}
	norm := float32(1 / math.Sqrt(sum))
	for i := range vec {
		vec[i] *= norm
	}
}

// MemIndex 内存向量索引（单进程）。
//
// # 它是**派生**的
//
// 索引可以从事实卡重算（向量是事实的函数），
// 而事实卡丢了索引救不回来。故它不提供任何改写事实的入口。
type MemIndex struct {
	mu   sync.RWMutex
	vecs map[string]map[string]memVec
	// dims 记下每个命名空间的维度（首次写入时定下）。
	dims map[string]int
}

// memVec 一条带归属的向量。
type memVec struct {
	vec   []float32
	scope string
}

// NewMemIndex 造内存索引。
func NewMemIndex() *MemIndex {
	return &MemIndex{
		vecs: map[string]map[string]memVec{},
		dims: map[string]int{},
	}
}

// Upsert 写入或更新一条向量（scope 是这条向量的归属）。
func (m *MemIndex) Upsert(_ context.Context, namespace, id, scope string, vec []float32) error {
	if id == "" {
		return errors.New("ports: 向量标识不能为空")
	}
	if len(vec) == 0 {
		return errors.New("ports: 向量不能为空")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.dims[namespace]; ok && d != len(vec) {
		return ErrDimensionMismatch
	}
	m.dims[namespace] = len(vec)
	cp := append([]float32(nil), vec...)
	normalize(cp)
	if m.vecs[namespace] == nil {
		m.vecs[namespace] = map[string]memVec{}
	}
	m.vecs[namespace][id] = memVec{vec: cp, scope: scope}
	return nil
}

// Search 返回最相似的若干条。
//
// # 维度必须对得上
//
// 对不上时报错（ErrDimensionMismatch）而不是返回空集：
// 「空集」的失效形态是「语义召回永远空」，而调用方看不出是维度错了。
func (m *MemIndex) Search(_ context.Context, namespace, scope string, vec []float32, topK int) ([]VectorHit, error) {
	if len(vec) == 0 {
		return nil, errors.New("ports: 查询向量不能为空")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if d, ok := m.dims[namespace]; ok && d != len(vec) {
		return nil, ErrDimensionMismatch
	}
	var qnorm float64
	for _, v := range vec {
		qnorm += float64(v) * float64(v)
	}
	if qnorm == 0 {
		return nil, nil
	}
	qnorm = math.Sqrt(qnorm)
	hits := make([]VectorHit, 0, len(m.vecs[namespace]))
	for id, item := range m.vecs[namespace] {
		// 归属过滤在**索引这一层**（不是调用方后置）：
		// 后置的话被别家占掉的 topK 槽位已经花掉了。
		if scope != "" && item.scope != "" && item.scope != scope {
			continue
		}
		stored := item.vec
		var dot float64
		for i, v := range stored {
			if i >= len(vec) {
				break
			}
			dot += float64(v) * float64(vec[i])
		}
		score := dot / qnorm
		hits = append(hits, VectorHit{ID: id, Score: float32(score)})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// Delete 删一条向量。
func (m *MemIndex) Delete(_ context.Context, namespace, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vecs[namespace] == nil {
		return nil
	}
	delete(m.vecs[namespace], id)
	return nil
}

// Count 该命名空间里的向量条数（自检用）。
func (m *MemIndex) Count(namespace string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.vecs[namespace])
}

// DefaultSemanticOverFetch 语义路的**过取倍数**。
//
// # 为什么还要过取（归属已经在索引里过滤了）
//
// 索引里**没有时间维度**，而退役事实的向量可能还在（退役时没删干净的那批）。
// 它们会被「取回事实卡」那一步滤掉——而那一步在 topK 之后，
// 于是被死事实占掉的槽位已经花掉了。过取就是补这个。
//
// 它的代价是**多几次取数**（不是多几次模型调用——向量化只算查询那一次）✓。
const DefaultSemanticOverFetch = 3

// VectorSemanticReader 用「向量化器 + 向量索引」实现语义召回面。
//
// # 它把两件事收在一个件里
//
// 召回链只认 SemanticReader（一条窄缝），
// 而向量化与索引是两件宿主的事——本件把它们组合起来，
// 于是宿主只需要装配两个端口，不必自己写那一层胶水。
type VectorSemanticReader struct {
	// Embedder 向量化器（必需）。
	Embedder Embedder
	// Index 向量索引（必需）。
	Index VectorIndex
	// OverFetch 过取倍数（零=DefaultSemanticOverFetch）。
	OverFetch int
}

// Search 检索最像的若干条。
//
// # 归属过滤不在这一层
//
// 这一层只管「像不像」；归属与时序的过滤在召回链的**取回事实卡**那一步做
// （那一步是真过滤，在存储层）。代价是本路的取数要过取（见 DefaultSemanticOverFetch）。
func (r VectorSemanticReader) Search(ctx context.Context, namespace, query, scope string,
	limit int) ([]SemanticHit, error) {
	if r.Embedder == nil || r.Index == nil {
		// 没装配向量面 = 不启用语义路（不是错误）。
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	vec, err := r.Embedder.Embed(ctx, query)
	if err != nil {
		return nil, err
	}
	// 过取只为了补上**失效事实**占掉的槽位（索引里没有时间维度，
	// 退役事实的向量可能还在）——归属已经在索引里过滤掉了，不该过取。
	fetch := limit
	over := r.OverFetch
	if over <= 0 {
		over = DefaultSemanticOverFetch
	}
	fetch = limit * over
	hits, err := r.Index.Search(ctx, namespace, scope, vec, fetch)
	if err != nil {
		return nil, err
	}
	out := make([]SemanticHit, 0, len(hits))
	for _, h := range hits {
		out = append(out, SemanticHit{ID: h.ID, Score: float64(h.Score)})
	}
	return out, nil
}
