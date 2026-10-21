package graph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// ErrInvalidMaxResults 结果上限非法。
var ErrInvalidMaxResults = errors.New("graph: 结果上限非法")

// Traverser 多跳检索器。
type Traverser struct {
	store store.EdgeStore
}

// NewTraverser 造多跳检索器。
func NewTraverser(s store.EdgeStore) (*Traverser, error) {
	if s == nil {
		return nil, errors.New("graph: 存储未装配")
	}
	return &Traverser{store: s}, nil
}

// Traversal 一条到达路径。
type Traversal struct {
	// EntityID 到达的实体。
	EntityID string
	// Depth 离起点的跳数。
	Depth int
	// Path 走过的实体（含起点）。
	Path []string
	// Relation 最后一段的关系名。
	Relation string
	// Weight 路径上的强度乘积。
	Weight float64
}

// TraverseOptions 多跳检索选项。
type TraverseOptions struct {
	// MaxDepth 跳数上限（0=1；超过 MaxDepthLimit 报错）。
	MaxDepth int
	// Scope 读者归属（空=不隔离）。
	Scope types.Scope
	// At 该时刻成立的边（零值=不按时间过滤）。
	At time.Time
	// Relations 只走这些关系（空=全走）。
	Relations []string
	// MaxResults 结果上限（0=缺省 50）。
	MaxResults int
}

// arrival 一个实体的一次到达（内部记账用）。
type arrival struct {
	depth    int
	path     []string
	relation string
	weight   float64
	// order 到达序号：同强度时的稳定排序依据。
	order int
}

// frontierItem 前沿上的一个实体（待展开的一层）。
type frontierItem struct {
	entityID string
	path     []string
	relation string
	weight   float64
}

// Traverse 从起点实体做广度优先的多跳展开。
//
// # 算法：按层推进 + 按实体去重
//
// 每层向存储层要一次「当前前沿的出边」，逐条判定后把新到的实体收进下一层前沿。
// 实体**只到达一次**（第一次到达即定深度），于是：
//
//   - **环不会无限展开**：A→B→A 里 A 早已到达过，第二圈不再展开——
//     不去重的话，多跳在有环的图上就是无限展开，而且它**不报错**，
//     只是越跑越慢直到把预算耗光。
//   - 深度即最短到达距离，不受路径顺序影响。
//
// # 归属过滤要判两次
//
// 存储层已经按归属过滤过一次，这里**再判一次** `Scope.VisibleTo`：
// 纵深防御。后端漏了过滤条件时，失效形态是**串户**（把别家的关系接进当前链路），
// 而那在结果里完全看不出来——路径看着仍然自洽。
// 漏一条的代价是「链路上多了一个不该出现的实体」，比少一条严重得多。
//
// # 强度为什么用乘积
//
// 路径强度 = 各段 Strength() 的**乘积**：一段弱关系会把整条链路拉弱，
// 这正是「链路」该有的性质——最弱的一环决定这条链有多可信。
// 求和会让「多经过几段无关关系」反而变强，取最大则等于「整条链只看最强的一段」，
// 两者都会把弱链路伪装成强链路。
//
// # 排序
//
// 强度高的在前；强度相同按**到达顺序**稳定排序——排序必须稳定，
// 否则同强度的条目顺序随实现漂移，调用方拿不到可复现的结果。
func (t *Traverser) Traverse(ctx context.Context, ns store.Namespace, from []string,
	opts TraverseOptions) ([]Traversal, error) {

	maxDepth, err := resolveDepth(opts.MaxDepth)
	if err != nil {
		return nil, err
	}
	maxResults, err := resolveMaxResults(opts.MaxResults)
	if err != nil {
		return nil, err
	}
	relSet := relationSet(opts.Relations)

	// arrived：实体 → 它的到达记录。一个实体只有一条记录。
	arrived := make(map[string]*arrival)
	// visited：已到达集合。**起点也在其中**——起点是查询本身，
	// 不是「到达的实体」，不该出现在结果里。
	visited := make(map[string]bool)
	order := 0

	var frontier []frontierItem
	for _, s := range from {
		if s == "" || visited[s] {
			continue
		}
		visited[s] = true
		frontier = append(frontier, frontierItem{
			entityID: s,
			path:     []string{s},
			relation: "",
			weight:   1,
		})
	}

	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		var next []frontierItem
		nextIndex := make(map[string]int)

		for _, f := range frontier {
			// 取数按**单个实体**发起，而不是整个前沿一次取：
			// 后端的默认条数上限落在单实体出边集上，而不是整层前沿上——
			// 前沿整体取数时，上限会**静默截断**（少了哪条边看不出来），
			// 而那正是「关系少了」这类症状的来源。
			edges, err := t.store.ListEdges(ctx, ns, store.EdgeFilter{
				FromEntityIDs: []string{f.entityID},
				Relations:     opts.Relations,
				Scope:         opts.Scope,
				At:            opts.At,
			})
			if err != nil {
				return nil, err
			}
			for _, e := range edges {
				if e == nil {
					continue
				}
				// 归属复查（纵深防御）：存储层已经过滤过，这里再挡一道。
				if !e.Scope.VisibleTo(opts.Scope) {
					continue
				}
				// 时间复查：存储层按 At 取过，这里按边的生效区间再判一次。
				if !opts.At.IsZero() && !e.EffectiveAt(opts.At) {
					continue
				}
				// 关系复查：存储层按关系名取过，这里再挡一道。
				if relSet != nil && !relSet[e.Relation] {
					continue
				}
				to := e.ToEntityID
				if to == "" {
					continue
				}
				path := make([]string, 0, len(f.path)+1)
				path = append(path, f.path...)
				path = append(path, to)
				weight := f.weight * e.Strength()

				if !visited[to] {
					visited[to] = true
					arrived[to] = &arrival{
						depth:    depth,
						path:     path,
						relation: e.Relation,
						weight:   weight,
						order:    order,
					}
					order++
					nextIndex[to] = len(next)
					next = append(next, frontierItem{
						entityID: to,
						path:     path,
						relation: e.Relation,
						weight:   weight,
					})
					continue
				}
				// 同一深度再次到达：只有**更强**的路径才值得换。
				// 同深度才有得比——更浅的路径先到（更近、更可信），
				// 更深的路径再强也不该把已定的最短距离改掉。
				rec, ok := arrived[to]
				if !ok || rec.depth != depth || weight <= rec.weight {
					continue
				}
				rec.path = path
				rec.relation = e.Relation
				rec.weight = weight
				// 前沿项一并换掉：子节点的路径前缀必须与父节点一致，
				// 否则结果里会出现「父路径是弱的那条、子路径是强的那条」的拼接。
				if idx, ok := nextIndex[to]; ok {
					next[idx].path = path
					next[idx].relation = e.Relation
					next[idx].weight = weight
				}
			}
		}
		frontier = next
	}

	out := make([]Traversal, 0, len(arrived))
	for entityID, rec := range arrived {
		out = append(out, Traversal{
			EntityID: entityID,
			Depth:    rec.depth,
			Path:     rec.path,
			Relation: rec.relation,
			Weight:   rec.weight,
		})
	}
	// 强度高的在前；同强度按到达顺序稳定排序。
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := arrived[out[i].EntityID], arrived[out[j].EntityID]
		if oi.weight != oj.weight {
			return oi.weight > oj.weight
		}
		return oi.order < oj.order
	})
	if len(out) > maxResults {
		out = out[:maxResults]
	}
	return out, nil
}

// resolveDepth 归一跳数上限。
func resolveDepth(d int) (int, error) {
	switch {
	case d < 0:
		return 0, fmt.Errorf("%w: %d", ErrInvalidDepth, d)
	case d == 0:
		return 1, nil
	case d > MaxDepthLimit:
		return 0, fmt.Errorf("%w: %d 超过上限 %d", ErrInvalidDepth, d, MaxDepthLimit)
	default:
		return d, nil
	}
}

// resolveMaxResults 归一结果上限。
func resolveMaxResults(n int) (int, error) {
	switch {
	case n < 0:
		return 0, fmt.Errorf("%w: %d", ErrInvalidMaxResults, n)
	case n == 0:
		return defaultMaxResults, nil
	default:
		return n, nil
	}
}

// relationSet 把关系名过滤条件变成集合（空=不限，返回 nil）。
func relationSet(rels []string) map[string]bool {
	if len(rels) == 0 {
		return nil
	}
	set := make(map[string]bool, len(rels))
	for _, r := range rels {
		set[r] = true
	}
	return set
}
