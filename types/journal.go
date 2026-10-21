package types

import "time"

// JournalEntry 记忆平面的审计条目。
//
// # 为什么记忆要自带日志面
//
// 长期记忆会**改写自己**：一条事实被推翻、一张卡被人工作废、一个页面被重生成。
// 这些动作若不留痕，记忆库就变成一个无法追问的黑箱——
// 「它为什么这么记我」永远答不上来，而这恰恰是用户对记忆系统唯一的核心疑问。
//
// 追加面（append-only）：条目只增不改不删，与业务表分离存储。
type JournalEntry struct {
	// ID 条目标识。
	ID string
	// At 发生时刻。
	At time.Time
	// Kind 动作类型（见下）。
	Kind JournalKind
	// Subject 动作对象（事实卡 ID / 候选 ID / 实体 ID）。
	Subject string
	// Actor 谁干的（规则 / 模型 / 人）。
	Actor DecidedBy
	// Summary 一句话说明（渲染用，不是判据）。
	Summary string
	// Detail 结构化明细（见 Detail 字段约定）。
	Detail map[string]any
}

// JournalKind 审计动作类型面。
type JournalKind string

const (
	// JournalPromote 候选晋升为事实。
	JournalPromote JournalKind = "promote"
	// JournalMerge 候选并入既有事实。
	JournalMerge JournalKind = "merge"
	// JournalDrop 候选被丢弃。
	JournalDrop JournalKind = "drop"
	// JournalSupersede 事实被新事实推翻。
	JournalSupersede JournalKind = "supersede"
	// JournalDeprecate 事实被人工作废。
	JournalDeprecate JournalKind = "deprecate"
	// JournalReview 转入人工处置队列。
	JournalReview JournalKind = "review"
	// JournalAccept 人工接受（确认晋升或替换）。
	JournalAccept JournalKind = "accept"
	// JournalReject 人工驳回。
	JournalReject JournalKind = "reject"
	// JournalRetire 运行期错误事实批量退役（见 Provenance.RuntimeError）。
	JournalRetire JournalKind = "retire"
	// JournalExport 记忆导出。
	JournalExport JournalKind = "export"
	// JournalImport 记忆导入。
	JournalImport JournalKind = "import"
	// JournalPageDirty 实体画像标脏失败。
	//
	// 单独一类而不是并进 promote：那条 promote 记的是「事实已晋升」，
	// 而标脏失败是**另一件事**（派生标记没落上，画像会停在旧版本）。
	// 合成一条的话，看板上「晋升成功」与「画像已重算」就分不开了——
	// 而后者没发生，症状只是画像内容过时，看着与正常一模一样。
	JournalPageDirty JournalKind = "page-dirty"
	// JournalVectorSync 向量同步失败（向量轨写/删没落上）。
	//
	// 单独一类：事实**已经入库**了，只是它的向量没进索引——
	// 于是语义召回会少这一条，而症状是「召回里少了个相关的东西」，
	// 完全看不出来。合成进别的类里就没法按原因查了。
	JournalVectorSync JournalKind = "vector-sync"
)
