package graph

import (
	"context"
	"errors"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 哨兵错误（消费方一律 errors.Is 判定，不做字符串匹配）。
var (
	// ErrMissingSource 边缺溯源：没有出处的边不许落。
	ErrMissingSource = errors.New("graph: 边缺溯源")
	// ErrMissingID 边缺标识。
	ErrMissingID = errors.New("graph: 边缺标识")
	// ErrInvalidDepth 跳数上限非法。
	ErrInvalidDepth = errors.New("graph: 跳数上限非法")
)

// Writer 落边与清投影。
//
// # 落边不是真源操作
//
// 它只写投影，**不写事实卡**：关系是否成立由事实卡说了算，
// 投影层只负责把宿主给的声明落成可查询的边。
type Writer struct {
	store store.EdgeStore
	now   func() time.Time
}

// Option 落边器的构造选项。
type Option func(*Writer)

// WithClock 注入时钟（建边时刻）。
//
// 刻意注入而不是直接 time.Now()：落库时刻会进库里，
// 用例要能断言它，否则「幂等落两次」这类断言只能靠比 ID，
// 而比不出「第二次落有没有把建边时刻改掉」。
func WithClock(now func() time.Time) Option {
	return func(w *Writer) {
		if now != nil {
			w.now = now
		}
	}
}

// NewWriter 造落边器。
func NewWriter(s store.EdgeStore, opts ...Option) (*Writer, error) {
	if s == nil {
		return nil, errors.New("graph: 存储未装配")
	}
	w := &Writer{store: s, now: func() time.Time { return time.Now().UTC() }}
	for _, o := range opts {
		o(w)
	}
	return w, nil
}

// Put 落一条边（幂等，按 ID upsert）。
//
// # 为什么边必须有溯源
//
// SourceAtomID 指向这条边是从哪张事实卡投影出来的。**没有溯源的边不许落**：
// 事实卡被推翻时，清理面按溯源删边；没有溯源的边就成了「没人敢删的垃圾」——
// 事实已经不成立了，而图还在说那条关系成立。图一旦开始说谎，
// 宿主就没法再用它做判断，且这种失效**没有任何报错**。
//
// # 补齐哪些派生字段
//
//   - CreatedAt 零值时取注入的时钟（建边时刻是派生事实，不该由调用方编）。
//   - Scope **保持原样**：空就是全局可见，本层不替调用方决定归属。
//
// 字段就地补齐在传入的边对象上：调用方手上这一份就是落库的那一份，
// 不会出现「库里是一份、调用方手上是另一份」。
func (w *Writer) Put(ctx context.Context, ns store.Namespace, e *types.EntityEdge) error {
	if e == nil {
		return errors.New("graph: 边为空")
	}
	if e.ID == "" {
		return ErrMissingID
	}
	if e.SourceAtomID == "" {
		return ErrMissingSource
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = w.now()
	}
	return w.store.UpsertEdge(ctx, ns, e)
}

// ClearSource 清掉某张事实卡投影出来的全部边。
//
// 事实卡被推翻时调用它：留着旧边等于「关系还在」，
// 而事实已经不成立了——那是一张说谎的图。
func (w *Writer) ClearSource(ctx context.Context, ns store.Namespace, sourceAtomID string) error {
	if sourceAtomID == "" {
		return ErrMissingSource
	}
	return w.store.DeleteEdgesBySource(ctx, ns, sourceAtomID)
}
