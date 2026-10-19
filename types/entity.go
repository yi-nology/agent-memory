package types

import (
	"strings"
	"time"
)

// EntityType 实体类型面。
//
// **刻意不预置任何领域类型**：宿主的目标对象词表（各类基础设施对象）
// 抄进来就是门禁第一条要拦的事，而门禁拦不住「以注释形式抄进来」的版本。
// 类型值由宿主投影时给定，本包只当不透明字符串处理。
type EntityType string

// Entity 一个被归一过的实体：同一件事的多种叫法收敛到一张卡上。
//
// 归一是长期记忆能用的前提。没有它，「10.44.56.107」「那台研发机」「web-03」
// 会各自长出一份事实，召回时又互相稀释——每个名字都命中一点，合起来谁也不显著。
type Entity struct {
	// ID 实体标识。
	ID string
	// EntityType 实体类型（宿主给定）。
	EntityType EntityType
	// CanonicalName 规范名。
	CanonicalName string
	// Aliases 别名集合（见 Alias 表；此处是投影）。
	Aliases []string
	// AtomCount 归属该实体的事实卡数量（分页与页重生成的触发依据）。
	AtomCount int
	// CreatedAt 创建时刻。
	CreatedAt time.Time
	// LastPromotedAt 最近一次有事实卡晋升的时刻。
	LastPromotedAt time.Time
}

// Alias 实体别名。别名表与实体表分表：归一判定要按别名查，
// 与「列出全部别名」是两种访问形态，合表会让热路径扫到冷数据。
type Alias struct {
	// Alias 别名字面。
	Alias string
	// EntityID 指向的实体标识。
	EntityID string
	// EntityType 实体类型（便于跨类型消歧）。
	EntityType EntityType
	// CreatedBy 谁建立的这个别名（模型猜的与人工确认的不是一回事）。
	CreatedBy DecidedBy
	// CreatedAt 建立时刻。
	CreatedAt time.Time
}

// ActiveEntity 活跃实体栈的一条：召回期代词消解用。
//
// # 它回答什么问题
//
// 用户说「那台机器呢？」——「那台」指什么？本仓不猜，靠两条入栈路径：
// 召回命中的实体（重排后的 Top-K）与查询里**字面提到**的实体名。
// 两条都入栈，栈顶即当前话题。
//
// # 为什么要常驻而不是每次重算
//
// 代词消解的时间跨度是「跨轮」，而召回是「跨会话」。每轮重算的话，
// 「那个」在下一轮就指错了——上一轮的召回结果未必还在命中集里。常驻栈才能
// 把话题延续下去。
//
// # 淘汰与钉住
//
// 容量有上限，超限时按 LRU 淘汰；但被用户显式确认过的实体有 PinTime，
// 在钉住窗口内不参与淘汰（否则用户刚纠正完「就是那台」，它就滑出去了）。
type ActiveEntity struct {
	// ThreadID 所属线程。
	ThreadID string
	// EntityID 实体标识。
	EntityID string
	// CanonicalName 实体规范名（投影，避免渲染时回表）。
	CanonicalName string
	// LastSeen 最近一次被提及或命中。
	LastSeen time.Time
	// Pinned 是否被显式钉住。
	Pinned bool
	// PinTime 钉住时刻（Zero 值=从未钉住）。
	PinTime time.Time
}

// NormalizeAlias 别名归一（查找用）。
//
// 只做首尾裁剪 + 空白折叠 + 小写，**不做**同义词扩展、拼音、分词。
// 理由与 AtomCard.Signature 一致：归一越激进，误合并的代价越大，
// 而误合并掉两条互相矛盾的事实，比多留两个别名难查得多。
func NormalizeAlias(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// NormalizeAssertion 断言归一（签名与去重用）。
//
// 折叠内部连续空白并裁剪首尾。**不做**去标点、不做大小写折叠：
// 「磁盘剩 2G」与「磁盘剩 2 g」可以合并（空白折叠即够），
// 而「A. 完成」与「B. 完成」在去标点后会撞成一条——那是两个不同的事实。
func NormalizeAssertion(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
