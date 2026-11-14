package store

import (
	"context"
	"time"

	"github.com/yi-nology/agent-memory/types"
)

// 本文件：可选能力面（optional capability）。
//
// # 契约演进规约（对外发布的接口不许加方法）
//
// 本仓的存储契约是**对外发布**的：仓外的宿主会自己实现它
// （宿主仓 bianque 的做法是 `var _ store.AtomStore = (*Store)(nil)`）。
// 于是在这样的接口上**加一个方法**，对实现者就是**隐式 breaking**：
//
//	v0.8.0 给 AtomStore 加 SearchAtomsLoose
//	⇒ 宿主仓那行编译期断言当场红
//	⇒ 上游发 tag 的当天，下游整个仓编译不过。
//
// 这与「拉新版本即可」的预期相反，也让宿主无法「只实现自己用得到的那几片」——
// 实现面被接口的形状绑架。
//
// 故本包把接口分成两类：
//
//   - **已发布的契约面**（store.go 里的那些）：**冻结**。
//     真要加方法 = 一次有意的破坏性变更 ⇒ 走大版本 + 在发布说明里点名
//     「这会让外部实现者编译失败」。
//   - **可选能力面**（本文件）：**新能力一律落这里**。
//
// # 可选能力的三条要件（缺一不可）
//
//  1. **独立的小接口**：不塞进任何已发布的接口 ⇒ 下游不实现也能编译。
//  2. **运行时探测**：消费方用类型断言问「你有没有这个能力」。
//  3. **明确的降级路径**：没有时怎么办，写在代码里，不许静默假装成功。
//
// 三条缺一不可：只有接口没有探测 = 换个地方编译红；
// 只有探测没有降级 = 宿主装了个半套就崩；
// 只有降级没有观测 = 降级悄悄发生，宿主以为自己在用新能力。
//
// # 降级的方向：少召回，不报错
//
// 缺能力时**回落**（少召回一些）而不是**报错**：
// 宽松路是**增强**（多话题查询的召回率），不是**前提**
// ——没有它召回照样跑，只是多话题查询打不中。
// 把增强当前提 ⇒ 宿主装不起就崩，那是把「可选」写成了「必需」。
//
// 但「少召回」必须**可观测**：宿主要能看见自己在降级，
// 否则症状只是「多话题查询打不中」，看着像内容不够。
// 本仓的可观测面在 pipeline/recall：Result.LooseSearchUnavailable + 指标。
//
// # 冻结清单
//
// 已发布契约面的方法集被**用例钉死**（见 application/contract_test.go）：
// 谁往里加方法，谁的 `make check` 就红，红在一条写着「去用可选接口」的提示上。
// 那份清单是本条纪律的**执行面**——本文件是它的**声明面**。

// AtomLooseSearcher 宽松词面检索（命中**任一**词就算，OR 匹配）。
//
// # 它是什么
//
// 与 SearchAtoms 只差**词的匹配方式**：严格路是隐式 AND（每个词都要在），
// 宽松路是命中任一词就算。
//
// # 为什么要有它
//
// 严格路是**隐式 AND** ⇒ 「多话题查询」几乎打不中：
// 「磁盘 内存 清理」里只要有一条事实没有同时含这三个词，它就不在命中集里
// ——而它对这条查询明明是相关的。
// 基线数字把这件事量出来了（docs/EVAL_BASELINE.md）：
// Recall@3 之后**不再上升**（部分匹配的事实进不来）。
//
// # 为什么是**另一路**而不是把 AND 改成 OR
//
// 直接改成 OR：噪声一次性灌进候选池，而池内排序只能靠打分器救——
// 而打分器是词面重叠，于是「命中任一词」与「词面重叠高」高度相关，
// 噪声反而排前面。
//
// 另加一路再交给融合器（RRF）合并：完全匹配的那条在**两路都出现**
// （融合分最高），部分匹配的那些从宽松路进来。
//
// # 过滤条件与 SearchAtoms **完全一致**
//
// 归属、活跃、时序、TopK 同一次求值（进 SQL，不许后置过滤）。
// 放宽的是**词的匹配方式**，不是**可见性**——后者是跨租户泄漏的防线。
type AtomLooseSearcher interface {
	// SearchAtomsLoose 宽松词法检索：命中任一词就算。
	//
	// 未实现这一面的存储**不是坏的**：召回链会回落到严格路
	// （见 pipeline/recall 的降级路径），只是多话题查询打不中。
	SearchAtomsLoose(ctx context.Context, ns Namespace, query string, f AtomFilter) ([]*types.AtomCard, error)
}

// LooseSearcher 探测一个实现有没有宽松检索能力。
//
// 用它而不是裸的类型断言，是为了让「可选」这件事**只有一个地方定义**：
// 探测语义、降级约定、命名都在本包，调用点不再各自解释一遍。
//
// 返回 ok=false 时调用方**必须**走降级路径，不许当它不存在地继续。
func LooseSearcher(v any) (AtomLooseSearcher, bool) {
	ls, ok := v.(AtomLooseSearcher)
	return ls, ok
}

// FTSIndexRebuilder 词面索引重建（backfill）能力。
//
// # 它是什么
//
// 词面索引（SQLite 后端的 FTS 虚表）是**派生物**：真源是 raw_events /
// candidates / atoms 三张表。行与索引在同一个事务里落库
// （见 storage/sqlite 各 Save 的实现），但历史存量可能已经失配——
// 事务化修复之前写下的行（行在而索引不在 ⇒ 永久静默漏命中）、
// 或拷贝/手工动过的库留下的幽灵索引行（搜得到但取不到）。
// RebuildFTSIndex 从真源**全量重建**，两种失配形态一次修平。
//
// # 为什么是可选能力
//
// 只有「索引与行分两处存」的后端才可能有失配：PG 的词元向量与正文
// 同一行落库，没有独立索引可失配，故不实现这一面——缺了不算实现不全。
//
// # 消费方式
//
// 运行时探测（FTSRebuilder），没有该能力就明确报「后端不支持」，
// 不许静默假装重建成功。它是**维护动作**（全量扫真源表，库大时是重操作），
// 走 CLI 的 rebuild-fts 或宿主自己的库管理口，不挂在召回热路径上。
type FTSIndexRebuilder interface {
	// RebuildFTSIndex 从真源表全量重建词面索引，返回重建的索引条数。
	//
	// 整段必须在一个事务里：重建中途失败不得留下半新半旧的索引。
	RebuildFTSIndex(ctx context.Context) (int64, error)
}

// FTSRebuilder 探测一个实现有没有词面索引重建能力（语义同 LooseSearcher）。
//
// 返回 ok=false 时调用方**必须**走「后端不支持」的明确报错，
// 不许当它不存在地继续。
func FTSRebuilder(v any) (FTSIndexRebuilder, bool) {
	rb, ok := v.(FTSIndexRebuilder)
	return rb, ok
}

// GlobalRawReader **全局读者**取原始事件的能力（issue #53）。
//
// # 它是什么
//
// ListRaw 的 scope 参数空值是「不隔离」（导出与库管理面靠它取**全量**，
// 该语义已随冻结接口发布），而**全局读者**按 types.Scope.VisibleTo
// 只看得见全局事件——同一个空值承载了两种相反的语义，ListRaw 又已冻结
// 不能加参数，故这条取数形态单独开一个可选口子：只取 scope = ” 的事件。
//
// # 为什么是可选能力
//
// 三个要件齐备：独立小接口（下游不实现也能编译）；消费方运行时探测
// （pipeline/recall 在 New 里探一次）；明确降级路径（没实现时，
// 召回对空读者回落 ListRaw(空 scope) + 后置 VisibleTo 滤除——
// **正确性不变**（滤除兜着，绝不漏别的家），代价只是 TopK 配额可能
// 被别家的事件占掉一部分，与修复前的行为一致）。
type GlobalRawReader interface {
	// ListRawGlobal 以全局读者身份取原始事件：只取全局（scope = ''）的，
	// 按时间倒序，语义同 ListRaw（before 为分页游标，p 为分页参数）。
	ListRawGlobal(ctx context.Context, ns Namespace, before time.Time, p Page) ([]*types.RawEvent, error)
}

// GlobalRawReaderOf 探测一个实现有没有全局读者取原始事件的能力
// （语义同 LooseSearcher）。
//
// 返回 ok=false 时调用方**必须**走降级路径（ListRaw + 后置滤除），
// 不许当它不存在地继续。
func GlobalRawReaderOf(v any) (GlobalRawReader, bool) {
	g, ok := v.(GlobalRawReader)
	return g, ok
}

// PagedLister 分页取全能力（issue #61）：按 (offset, limit) 逐页取数，
// 供「全量扫描」类动作翻到取尽。
//
// # 它是什么
//
// 四个取数方法的过滤条件与 store.go 里对应方法**逐字一致**，差别只在
// 翻页参数：offset 从 0 起，limit 是单页条数，返回**恰好这一页**——
// 不足一页即取尽，offset 越过末尾返回空集（不是错误）。
//
// # 为什么是可选能力
//
// 全量导出不能用写死的大 Limit 冒充：超过上限的行按取数序静默截断，
// 而迁移包残缺是**零信号**的（octop 导出曾以 Limit:1000000 冒充全量，
// issue #61）。翻页参数本该进各取数方法的签名，但那四个签名都在
// **冻结面**上（仓外宿主实现着，加参数 = 隐式 breaking）——
// 故按可选能力三要件单独立面：
//
//  1. 独立小接口（本面）：下游不实现也能编译；
//  2. 运行时探测（PagedListerOf）：消费方在入口探一次；
//  3. 明确的降级路径：写在消费方——导出面对缺失是**响亮报错**
//     （导出完整性是前提不是增强；见 operations/portable.paging.go）。
//
// in-repo 两个后端（sqlite / postgres）都实现它；
// 仓外自造存储的宿主补 4 个方法即可。
//
// ⚠️ **包装会吃掉能力**：任何内嵌 store.Store 接口的装饰器
// （如 application 的 vectorStore）都会把内层的这一面藏在外层类型断言
// 之外——装饰器必须**逐方法转发**本面，否则能力静默消失。
type PagedLister interface {
	// ListCandidatesPaged 按页取候选（语义同 CandidateStore.ListCandidates）。
	ListCandidatesPaged(ctx context.Context, ns Namespace, f CandidateFilter, offset, limit int) ([]*types.Candidate, error)
	// ListEntitiesPaged 按页取实体（语义同 EntityStore.ListEntities）。
	ListEntitiesPaged(ctx context.Context, ns Namespace, entityType types.EntityType, offset, limit int) ([]*types.Entity, error)
	// ListEntityPagesPaged 按页取实体页面（语义同 PageStore.ListEntityPages）。
	ListEntityPagesPaged(ctx context.Context, ns Namespace, offset, limit int) ([]*types.EntityPage, error)
	// ListJournalPaged 按页取审计条目（语义同 JournalStore.ListJournal）。
	ListJournalPaged(ctx context.Context, ns Namespace, subject string, offset, limit int) ([]*types.JournalEntry, error)
}

// PagedListerOf 探测一个实现有没有分页取全能力（语义同 LooseSearcher）。
//
// 返回 ok=false 时，导出类消费方**必须**响亮报错（不许拿单次大 Limit
// 硬取当降级——那会静默截断）；其它消费方按各自的前提处理。
func PagedListerOf(v any) (PagedLister, bool) {
	p, ok := v.(PagedLister)
	return p, ok
}
