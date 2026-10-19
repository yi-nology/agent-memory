package types

import "time"

// AtomCard 一张事实卡：**长期记忆的真源**。
//
// 名字借自「原子卡片」——它不可再分，一条断言一张卡。树上任何 leaf、页面上
// 任何一行、召回结果里任何一条，都是对这张卡的引用或投影，不是副本。
//
// # 不变量（由 pipeline/promotion 保证，types 只声明形状）
//
//  1. QuoteEventID 非空且能解析到一条 RawEvent——**没有出处的记忆不许存在**；
//     无法回溯的事实既不能审计也不能推翻。
//  2. Confidence/Importance 必须是离散档位之一，空值即非法。
//  3. SupersededBy 与 DeprecatedAt 至多其一非空（见 Valid 校验）。
//
// # 生命周期
//
//	晋升 ──→ 活跃
//	被新事实推翻 ──→ 软退役（SupersededBy 指向继任者）→ 惰性清理
//	人工作废   ──→ 软退役（DeprecatedAt 落时刻）──────→ 惰性清理
//
// 刻意**不做物理删除**：一条被推翻的事实曾经是真的，硬删掉它会让「当初为什么
// 这么判」无从查证。保留软退役态，清理由 lifecycle 层按龄执行。
type AtomCard struct {
	// ID 事实卡标识。
	ID string
	// EntityID 归属实体标识（无归属时为空串——全局事实是合法记忆）。
	EntityID string
	// CandidateID 晋升自哪条候选（审计链闭合）。
	CandidateID string
	// RawEventIDs 证据来源事件标识。
	RawEventIDs []string
	// Assertion 断言正文——这条事实「说了什么」。
	Assertion string
	// VerbatimQuote 逐字引用。
	VerbatimQuote string
	// QuoteEventID 引用出处的事件标识（不变量 1）。
	QuoteEventID string
	// SearchTerms 检索词面（预抽好，避免召回时每次分词）。
	SearchTerms []string
	// OccurredAt 事实**发生**的时刻。
	//
	// 刻意与 CreatedAt 分开：三天前说「磁盘只剩 2G」是昨天发生的事实，
	// 两者混同会让 recency 排序把「新写进来的旧事实」排到最前。
	OccurredAt time.Time
	// Confidence 置信档位。
	Confidence Confidence
	// Importance 重要度档位。
	Importance Importance
	// CreatedAt 入库时刻。
	CreatedAt time.Time
	// UpdatedAt 最后更新时刻。
	UpdatedAt time.Time
	// SupersededBy 继任事实卡 ID（软退役·被推翻）。
	SupersededBy string
	// DeprecatedAt 软退役时刻（人工作废）。
	DeprecatedAt time.Time
	// Provenance 来源凭证。
	Provenance Provenance
}

// Active 事实卡是否参与召回。
//
// 判据是**是否落定**：待审（needs_review 语义落在候选侧）与已退役都不参与。
// 这条闸是召回侧最容易被绕过的地方——一个只查 status 字符串的查询会在
// 人工作废后仍把事实注入 prompt，故单独给一个方法而不是散落在各处比较。
func (a *AtomCard) Active() bool {
	return a.SupersededBy == "" && a.DeprecatedAt.IsZero()
}

// SoftRetired 是否已软退役（被推翻或人工作废）。
func (a *AtomCard) SoftRetired() bool {
	return !a.Active()
}

// Signature 事实签名：实体 + 断言的归一形式。
//
// 用途是**重复检测**：同一件事被反复记录时，签名相同即候选重复，
// 晋升层据此合并而不是新增一卡。
//
// 归一规则刻意保守（只做空白折叠与首尾裁剪）：过度归一会把「磁盘剩 2G」与
// 「磁盘剩 3G」判成同一条，而它们恰恰互相矛盾——该走的是送审不是合并。
func (a *AtomCard) Signature() string {
	return a.EntityID + "\x00" + NormalizeAssertion(a.Assertion)
}
