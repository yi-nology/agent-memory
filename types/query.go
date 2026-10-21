package types

import "time"

// 本文件：查询理解的产物结构。
//
// # 为什么是**结构**而不是一段提示词
//
// 查询理解的三个消费面——时间窗过滤、代词消解、实体限定——都要**按字段取值**。
// 而提示词里的时间窗只能靠模型再理解一遍：
// 同一句话两次解析出两个时间窗，召回结果就不可复现，
// 而「召回结果可复现」是本仓最基本的一条承诺（评测面靠它成立）。
// 故凡是**要拿来做判据的**，一律进结构，不留在提示词里。
//
// # 零值语义：每个字段的零值都是「这一维度不约束」
//
// 空时间窗 = 不按时间过滤；空实体集合 = 不限定实体。
// 与既有过滤面（EntityIDs 空=不限）一致，故调用方不必区分
// 「没解析出来」与「解析出来是空的」——后者没有意义。

// QueryPlan 查询理解的结果。
type QueryPlan struct {
	// Raw 原查询（未加工）。
	Raw string
	// Terms 检索词面：去掉时间词与代词后的剩余词。
	//
	// 时间词必须去掉：把「上周」丢进 FTS 检索，它匹配不到任何事实，
	// 还会稀释真正的词面（多一个词，词面重叠率就低一截）。
	Terms []string
	// Entities 实体提示：本轮涉及哪些实体。
	//
	// 来源有三处：宿主直接给的、活跃实体栈里消解出来的、
	// 以及原句里点名提到的。三者合并去重。
	Entities []string
	// Time 时间窗（零值 = 不按时间过滤）。
	Time TimeWindow
	// Pronouns 代词消解的记录（消成了哪个实体）。
	//
	// 刻意**留痕**：代词消解是「猜」的（栈里的最近提及未必就是指的那个），
	// 消错了症状是「召回里没有用户指的那件事」，
	// 而用户看到的是「它记着但没想起来」。留痕才查得动。
	Pronouns []PronounRef
}

// TimeWindow 时间窗。
//
// 用零值表示「无界」而不是额外两个布尔位：
// 少两个字段就少两处可能填错的组合，
// 而「上界为零」与「没有上界」在时间轴上是同一件事。
type TimeWindow struct {
	// From 下界（含）。
	From time.Time
	// To 上界（不含）。
	To time.Time
}

// Empty 是否不约束任何时刻。
func (w TimeWindow) Empty() bool { return w.From.IsZero() && w.To.IsZero() }

// Contains t 是否落在窗口内。
//
// 零值时间窗包含一切（不约束）——这与「零值 = 不约束」的总纪律一致。
func (w TimeWindow) Contains(t time.Time) bool {
	if !w.From.IsZero() && t.Before(w.From) {
		return false
	}
	if !w.To.IsZero() && !t.Before(w.To) {
		return false
	}
	return true
}

// PronounRef 一次代词消解。
type PronounRef struct {
	// Pronoun 原句里的代词（「它」「那个」「这台」）。
	Pronoun string
	// EntityID 消解到的实体（空 = 没消出来）。
	EntityID string
	// Source 消解依据（"stack" = 活跃实体栈；"host" = 宿主直接给的）。
	//
	// 记依据是为了区分两种失败：**消错了**（有依据但猜错）
	// 与**没消出来**（栈是空的）——两者的处置完全不同。
	Source string
}
