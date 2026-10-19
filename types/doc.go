// Package types 记忆领域的结构单源：捕获事件、提炼候选、事实卡、实体与页面、
// 情节摘要与日志条目。
//
// # 分层位置
//
// 本包是**最底层**：不 import 本仓任何其他包，也不 import 任何第三方包。
// storage / pipeline / adapters 全部依赖它，它谁都不依赖——这条方向由
// architecture-policy.yaml 的模块清单强制。
//
// # 数据流（与 docs/AGENT_MEMORY_PLAN_20261019.md §1.1 对标 octop-memory）
//
//	RawEvent ──extraction──→ Candidate ──promotion──→ AtomCard ──dirty/regen──→ EntityPage
//	    └──episode extraction──→ Episode
//
// 事实与情节是**两条独立的路**：一条走晋升、一条走摘要，互不依赖——
// 没有情节一样能记住事实，反之亦然。
//
// # 与 octop-memory 的两处刻意偏离
//
//  1. **Provenance 是类型，不是 payload 里的一个键**。
//     octop 用 `payload.octop_runtime_error=true` 这种字符串键标运行期错误，
//     判读面靠约定。本包把它做成 Provenance 结构并嵌进 RawEvent/AtomCard：
//     消费方读字段，不解析字符串；写错键名在编译期就暴露。
//     背景是本仓的一条既有纪律——**判定面能换成结构就别拿文本匹配**。
//
//  2. **证据是必填的引用，不是可选的自由文本**。
//     AtomCard.QuoteEventID 与 VerbatimQuote 成对存在：任何进入召回的事实
//     都必须能回溯到一条具体的捕获事件，否则它无法被审计也无法被推翻。
//
//  3. **没有树投影**。octop 有 root/branch/leaf 的组织视图，本仓**刻意不做**
//     （方案 §5：对本仓的适用场景价值有限，等真出现「同一事实挂多锚点」再启）。
//     故本包不声明 MemoryNode——留着它等于让人以为那是个待启用的能力。
package types
