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
	// Scope 记录级归属（空=全局可见，非空=仅同归属可见）。
	//
	// 与 EntityID 是两回事：EntityID 说「这条事实说的是谁」，
	// Scope 说「这条事实对谁可见」。见 types/scope.go。
	//
	// 零值即全局：既有事实卡全是全局的，故加这个维度不改变老数据的可见性。
	Scope Scope
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
	// Vector 可选向量（**当前不进默认持久化**）。
	//
	// # 它为什么在这儿
	//
	// 走语义轨的宿主需要一个「向量放哪儿」的落脚点。
	// 没有它，宿主只能在自己的模型上**另加一列**（另加一个平行结构），
	// 于是本仓的领域结构与宿主的模型对不上，两边都要维护。
	//
	// # 它为什么是「可选的」
	//
	// 本仓的定案是「向量轨不接」（见 AGENTS.md）：向量检索是第二阶段的替换件，
	// 不是第一阶段的前提。故默认序列化**不含**它（`omitempty`），
	// 持久化形态一字不变。
	//
	// # 不许把它交给本仓的存储
	//
	// 两个后端都不落向量（见 store.AtomStore.SaveAtom 的说明）：
	// 写了会被**静默丢掉**——那比报错更坏（宿主以为存上了，
	// 症状是「相似度检索永远空」）。
	// 故 SaveAtom 见到非空向量**直接报错**。
	//
	// 留着的意义：宿主可以在**内存里**用本仓的结构带着向量
	// （自己落库、自己检索），不必为了带一个向量而复制一份平行模型。
	Vector []float32 `json:"Vector,omitempty"`
	// ValidFrom 事实**生效**的时刻（零值 = 取 OccurredAt）。
	//
	// # 与 OccurredAt 的区别
	//
	// OccurredAt 是「事情发生的那一刻」，ValidFrom 是「这条断言从什么时候开始算数」。
	// 「上周就坏了，今天才发现」——两者的 OccurredAt 相同，
	// 但 ValidFrom 不同（它是今天才被写下来的）。
	// 检索「现在的状态」时，前者要后者的判据。
	//
	// 零值取 OccurredAt 是为了**老数据不变**：加这个维度不该改判既有事实，
	// 否则同一个库升级前后召回到的东西不一样。
	ValidFrom time.Time
	// ValidTo 事实**失效**的时刻（零值 = 仍有效）。
	//
	// 取代关系用它：旧事实不是被删掉，而是「到这一刻为止有效」。
	// 于是「现在的状态」与「历史上曾经的状态」能同时被查到——
	// 而只软退役（SupersededBy）的话，旧事实只会变成「查得到但不算数」，
	// 追问「当初为什么这么判」时反而丢了它**什么时候对过**这个信息。
	ValidTo time.Time
	// Kind 时效类别（零值=按长期经验处理）。
	//
	// 它决定两件事：**保留期**（事件按龄退役、经验永久）与
	// **召回窗口**（事件默认只看近期，否则模型会把三周前的一次重启
	// 当成当前状态）。
	//
	// 零值=默认（长期经验）是最保守的一侧：既有事实卡没有这个维度，
	// 升级不该让它们开始被淘汰。详见 FactKind。
	Kind FactKind
	// Version 版本号（同一主体 + 谓词的第几版；1 起）。
	//
	// 零值当 1。版本号的作用不是排序，而是**溯源**：
	// 「这条是第 3 版」让「升级到 1.36」这类事实有一串可查的前身，
	// 而不必靠「哪条 supersede 了哪条」反推（那条链在退役链被裁剪后会断）。
	Version int
}

// EffectiveAt 这条事实在 t 时刻是否成立。
//
// 判据：生效区间含 t，且未被软退役。
// 零值 ValidFrom 取 OccurredAt，故老数据（没有 ValidFrom）也有确定的区间。
func (a *AtomCard) EffectiveAt(t time.Time) bool {
	if !a.Active() {
		return false
	}
	from, to := a.ValidRange()
	if !from.IsZero() && t.Before(from) {
		return false
	}
	if !to.IsZero() && !t.Before(to) {
		return false
	}
	return true
}

// ValidRange 归一后的生效区间（ValidFrom 零值时取 OccurredAt）。
func (a *AtomCard) ValidRange() (from, to time.Time) {
	from = a.ValidFrom
	if from.IsZero() {
		from = a.OccurredAt
	}
	return from, a.ValidTo
}

// VersionOrOne 归一后的版本号（零值当 1）。
func (a *AtomCard) VersionOrOne() int {
	if a.Version <= 0 {
		return 1
	}
	return a.Version
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

// SignatureSep 签名的分隔符。
//
// 刻意**不是** NUL（0x00）：SQLite 的 TEXT 能装 NUL，**PostgreSQL 不能**
// （PG 的 TEXT 禁止 0x00，插进去直接报 "invalid byte sequence for encoding UTF8"）。
// 而签名是要**落库**的——它得在两个后端里长得一模一样，
// 否则「同一批记忆换个部署形态」的去重行为就变了。
//
// 用 ASCII 单元分隔符（0x1F）：两个后端都接受，
// 且它是控制字符，实际文本里不会出现——与用 NUL 的原意相同。
const SignatureSep = "\x1f"

// Signature 事实签名：实体 + 归属 + 断言的归一形式。
//
// 用途是**重复检测**：同一件事被反复记录时，签名相同即候选重复，
// 晋升层据此合并而不是新增一卡。
//
// 归属参与签名：同一条断言在两个归属下是**两张不同的卡**
// （它们说的是各自环境里的事实）。签名不含归属的话，跨归属的重复检测
// 会把一组的卡片并进另一组的卡片——那是**内容泄漏**，不是重复。
//
// 归一规则刻意保守（只做空白折叠与首尾裁剪）：过度归一会把「磁盘剩 2G」与
// 「磁盘剩 3G」判成同一条，而它们恰恰互相矛盾——该走的是送审不是合并。
func (a *AtomCard) Signature() string {
	return AtomSignature(a.EntityID, a.Scope, a.Assertion)
}

// AtomSignature 事实签名（实体 + 归属 + 断言的归一形式）。
//
// 与 AtomCard.Signature **同规则**，收成一处的原因：晋升层要在事实卡存在
// **之前**算出签名（用来查重），那时没有卡片可调方法——
// 两处各写一份，规则一改就漂，而漂了的后果是「跨归属的卡被并成一张」。
//
// # 归属为空时不插那一段
//
// 早期签名只有「实体 + 断言」。加了归属维度之后若无条件插一段，
// 已落库事实卡的签名就全变了——而签名是**存起来的列**，
// 变了就意味着「旧卡再也查不到」→ 同一件事被重新记一次时会多出一张重复卡。
// 故归属为空时保持字节级一致（旧的全局卡照常查得到），非空时才插。
//
// 残留的碰撞面：全局卡的签名是「实体 + 断言」，带归属的是「实体 + 归属 + 断言」，
// 两者相等当且仅当断言里含分隔符（0x1F）——实际文本里不会出现。
func AtomSignature(entityID string, scope Scope, assertion string) string {
	norm := NormalizeAssertion(assertion)
	if scope == "" {
		return entityID + SignatureSep + norm
	}
	return entityID + SignatureSep + string(scope) + SignatureSep + norm
}

// FactKind 事实的**时效类别**。
//
// # 为什么要有这个维度
//
// 同一件事，三类的生命周期完全不同，而**一套策略套所有事实**必然两头不讨好：
//
//   - 短期事件（FactEvent）：发生过一次的事（「昨天重启过一次」）。
//     它会**过期**：过了保留期就没有召回价值——留着只会占预算、还会误导
//     （模型看到三周前的一次重启，会当成当前状态）。
//   - 有效期状态（FactState）：现在成立的属性（「容量是 2G」）。
//     它**被新值取代**而不是过期（取代链已经能表达这件事，见 AtomCard.ValidTo）。
//   - 长期经验（FactLesson）：稳定的知识与教训。
//     它**不因时间失效**——只在被推翻时失效。
//
// 有了这个维度，保留期与召回窗口才能**按类分别配**：
// 事件按龄退役，状态等到被取代，经验永久保留。
//
// # 零值 = 默认（按长期经验处理）
//
// 既有事实卡没有这个维度 ⇒ 零值必须落在**最保守**的一类：
// 长期经验（不因时间被淘汰、也不被默认召回窗口挡掉）。
// 零值落到「会被淘汰」的那一类，等于升级一次就丢一批老事实。
type FactKind string

const (
	// FactDefault 零值：按长期经验处理。
	FactDefault FactKind = ""
	// FactEvent 短期事件：发生过一次的事，会过期。
	FactEvent FactKind = "event"
	// FactState 有效期状态：现在成立的属性，被新值取代。
	FactState FactKind = "state"
	// FactLesson 长期经验：稳定的知识与教训。
	FactLesson FactKind = "lesson"
)

// IsEvent 是否是短期事件。
func (k FactKind) IsEvent() bool { return k == FactEvent }

// IsState 是否是有效期状态。
func (k FactKind) IsState() bool { return k == FactState }

// Durable 是否不按时间失效（长期经验与状态）。
//
// 零值（默认）落在 durable 一侧——理由见 FactKind 的说明。
func (k FactKind) Durable() bool { return !k.IsEvent() }
