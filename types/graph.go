package types

import "time"

// 本文件：实体关系边（**派生物**）。
//
// # 它是投影，不是真源
//
// 边的真源是事实卡：投影丢了可以从事实卡重投影，
// 而事实卡丢了投影救不回来——两者的地位不对称。
// 故边的增删归投影层，**关系事实的判定归宿主**：
// 本仓不认识「依赖/属于/部署在」这类宿主概念，
// 只提供「把宿主给的声明落成边、并按时间过滤、多跳取」这套机制。

// EntityEdge 实体之间的一条关系边。
type EntityEdge struct {
	// ID 边标识（宿主或投影层给定）。
	ID string
	// FromEntityID 起点实体。
	FromEntityID string
	// ToEntityID 终点实体。
	ToEntityID string
	// Relation 关系名（宿主定义，如「依赖」「属于」）。
	//
	// 刻意**不枚举**：本仓不认识任何领域关系，
	// 枚举关系名等于把宿主的词汇表写进内核。
	Relation string
	// Scope 记录级归属（空=全局可见）。
	//
	// 边的可见性与事实卡同口径：归属非空且不等于读者的边**一条都不给**。
	Scope Scope
	// ValidFrom 边生效的时刻（零值 = 从建库起一直有效）。
	ValidFrom time.Time
	// ValidTo 边失效的时刻（零值 = 仍有效）。
	//
	// 「A 依赖 B」这类关系会随时间变（B 被换掉了），
	// 故边也有区间；且区间是**查询面**的一部分：
	// 「现在还依赖吗」与「以前依赖过吗」是两个不同的问题。
	ValidTo time.Time
	// Weight 权重（宿主给的强度，零值=1）。
	//
	// 零值当 1 是为了让它**可省**：宿主不关心强度时不必填。
	Weight float64
	// SourceAtomID 溯源：这条边是从哪张事实卡投影出来的。
	//
	// 有它才能回答「这条边凭什么在」——
	// 派生物的每一行都要能追回它的出处，否则它就成了没人敢删的垃圾。
	SourceAtomID string
	// CreatedAt 建边时刻。
	CreatedAt time.Time
}

// EffectiveAt 这条边在 t 时刻是否成立。
func (e *EntityEdge) EffectiveAt(t time.Time) bool {
	if !e.ValidFrom.IsZero() && t.Before(e.ValidFrom) {
		return false
	}
	if !e.ValidTo.IsZero() && !t.Before(e.ValidTo) {
		return false
	}
	return true
}

// Strength 归一后的权重（零值当 1）。
func (e *EntityEdge) Strength() float64 {
	if e.Weight == 0 {
		return 1
	}
	return e.Weight
}
