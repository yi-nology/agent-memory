package portable

// 本文件：**与 octop-memory 的字段契约**（键名 + 取值）。
//
// # 为什么要有这一层（而不是靠机械翻译）
//
// 原先的导出把 Go 字段名**机械**翻成 snake_case ✓，
// 看上去两侧就通了 ✓——**但那是假的通** ✓：
// 两边的模型是「同一个设计的两个实现」✓，**不是同一个模型** ✓，
// 有一批字段**两边的名字本来就不一样** ✓：
//
//	本仓 Type            ↔ octop event_type      （raw_events）
//	本仓 Type            ↔ octop candidate_type  （candidates）
//	本仓 Body            ↔ octop summary_markdown（entity_pages）
//	本仓 AtomIDs         ↔ octop topics          （entity_pages）
//	本仓 At              ↔ octop timestamp       （journal）
//	本仓 Kind            ↔ octop action          （journal）
//	本仓 Summary         ↔ octop note            （journal）
//
// 而 octop 的读端是**按名字取**的 ✓（`d["event_type"]` ✓），
// 取不到就抛 `missing required field` ✓ ⇒ **那一整张表都导不进去** ✓。
//
// ⚠️ 更糟的是它**不响** ✓：octop 的导入器把这类错误收进 summary.errors ✓
// 然后 `continue` ✓ ⇒ 宿主看到的是「导入成功，0 条」✓。
//
// ⇒ 所以这一层是**逐字段对照出来的** ✓，不是推出来的 ✓。
// 对照的来源是对方仓库的源码：
// `src/octop_memory/types.py`（dataclass 字段名 = 导出面）
// 与 `src/octop_memory/operations/migration/import_.py`（`_build_*` = 读入面）。
//
// # 归属：带归属的记录**不导出**（防泄漏 ✓）
//
// 本仓的记录级归属（`types.Scope` ✓）在 octop 那边**没有对应字段** ✓。
// 而归属的语义是「这条只属于某一方」✓ ⇒ 导过去若丢掉归属 ✓，
// 那条记录就变成**全局可见** ✓ ⇒ 那是**内容泄漏** ✓，不是「少了个字段」✓。
//
// ⇒ 规则：**带归属的记录不导出** ✓（宁可少，不可漏 ✓）。
// 对方那边若要支持归属，那是对方要加字段的事 ✓（不是我们替它加 ✓）。
//
// # 取值翻译：只翻「确实是同一个概念」的 ✓
//
// 两边的枚举词表不同 ✓（事件类型、候选类型、候选状态、审计动作 ✓），
// 但 confidence / importance 两边完全一样 ✓（low/medium/high ✓）⇒ 不翻 ✓。
//
// 翻不准的**原样带过去** ✓：对方那边存的是自由字符串 ✓（枚举只用于静态标注 ✓），
// 原样过去至少不丢信息 ✓；硬翻成一个「看起来对」的词反而是造假 ✓。

// octopEventTypeTo 本仓事件类型 → octop 事件类型。
//
// 只收「确实是同一个概念」的 ✓：
//
//	turn_end      → session_end     （一轮结束 ≈ 会话结束；本仓没有会话级事件 ✓）
//	agent_message → assistant_message（同一件事，两边叫法不同 ✓）
//	external      → host_memory_write（外部推送 ⇒ 宿主写入 ✓）
//
// 其余原样带过去 ✓（manual / user_message / tool_result 两边同名 ✓）。
var octopEventTypeTo = map[string]string{
	"turn_end":      "session_end",
	"agent_message": "assistant_message",
	"external":      "host_memory_write",
}

// octopEventTypeFrom octop 事件类型 → 本仓事件类型。
//
// ⚠️ 对面多出来的那几类（tool_call / session_start / compaction ✓）
// 本仓没有对应 ✓ ⇒ 落到本仓最接近的一类 ✓：
//
//	tool_call      → tool_result（本仓只有「工具产出」这一类 ✓）
//	session_start  → turn_end
//	session_end    → turn_end
//	compaction     → turn_end
var octopEventTypeFrom = map[string]string{
	"assistant_message": "agent_message",
	"tool_call":         "tool_result",
	"host_memory_write": "external",
	"session_start":     "turn_end",
	"session_end":       "turn_end",
	"compaction":        "turn_end",
}

// octopCandidateTypeTo 本仓候选类型 → octop 候选类型。
//
// ⚠️ 大小写**不一样** ✓（本仓 fact ↔ octop Fact ✓）——
// 那不是笔误，是两边的词表不同 ✓ ⇒ 必须显式翻 ✓。
var octopCandidateTypeTo = map[string]string{
	"fact":       "Fact",
	"decision":   "Decision",
	"preference": "Preference",
	"procedure":  "Task",
}

// octopCandidateTypeFrom octop 候选类型 → 本仓候选类型。
var octopCandidateTypeFrom = map[string]string{
	"Fact":              "fact",
	"Decision":          "decision",
	"Preference":        "preference",
	"Task":              "procedure",
	"ConflictCandidate": "fact",
}

// octopCandidateStatusTo 本仓候选状态 → octop 候选状态。
//
// 本仓有「合并」这一档 ✓，对方没有 ✓ ⇒ 落到 promoted ✓（合并也是「定了」✓）。
var octopCandidateStatusTo = map[string]string{
	"merged":  "promoted",
	"dropped": "rejected",
	"review":  "needs_review",
}

// octopCandidateStatusFrom octop 候选状态 → 本仓候选状态。
var octopCandidateStatusFrom = map[string]string{
	"rejected":         "dropped",
	"needs_review":     "review",
	"conflict":         "review",
	"extractor_failed": "dropped",
}

// octopDecidedByTo 本仓判定者 → octop 判定者。
//
// 本仓有「模型」✓，对方叫 auto ✓（自动判定 ✓）⇒ 对齐 ✓。
var octopDecidedByTo = map[string]string{
	"model": "auto",
}

// octopDecidedByFrom octop 判定者 → 本仓判定者。
var octopDecidedByFrom = map[string]string{
	"auto":     "model",
	"portable": "rule",
}

// octopJournalKindTo 本仓审计动作 → octop 审计动作。
//
// 只翻确实对应的 ✓；翻不准的（export ✓）原样带过去 ✓。
var octopJournalKindTo = map[string]string{
	"drop":      "reject",
	"supersede": "update",
	"accept":    "promote",
	"retire":    "deprecate",
}

// octopJournalKindFrom octop 审计动作 → 本仓审计动作。
var octopJournalKindFrom = map[string]string{
	"reject":    "drop",
	"update":    "supersede",
	"create":    "accept",
	"user_edit": "accept",
	"delete":    "retire",
}

// octopJournalTargetKind 本仓审计动作 → 对面 target_* 槽的种类。
//
// 本仓的 Subject 是「事实卡 ID / 候选 ID / 实体 ID」三合一 ✓（没法直接对上 ✓），
// 但**动作本身说明了对象是哪类** ✓ ⇒ 按动作分流 ✓（不是猜 ✓）。
var octopJournalTargetKind = map[string]string{
	"promote":   "atom",
	"supersede": "atom",
	"deprecate": "atom",
	"retire":    "atom",
	"merge":     "entity",
	"drop":      "candidate",
	"review":    "candidate",
	"accept":    "candidate",
	"reject":    "candidate",
}

// octopTable 一张表的字段契约。
//
// To 与 From 都不是「机械翻译」✓，而是**逐字段对照**出来的 ✓。
type octopTable struct {
	// To 导出方向：本仓记录（键是 Go 字段名）→ octop 记录（键是 octop 字段名）。
	// 返回 ok=false 表示这一条**不导出**（带归属 ⇒ 会变成全局可见 ⇒ 泄漏 ✓）。
	To func(m map[string]any) (map[string]any, bool)
	// From 读入方向：octop 记录 → 本仓记录（键是 Go 字段名）。
	From func(m map[string]any) map[string]any
}

// octopTables 表名 → 字段契约。
var octopTables = map[string]octopTable{
	"raw_events":   {To: rawEventToOctop, From: rawEventFromOctop},
	"candidates":   {To: candidateToOctop, From: candidateFromOctop},
	"atoms":        {To: atomToOctop, From: atomFromOctop},
	"entities":     {To: entityToOctop, From: entityFromOctop},
	"aliases":      {To: aliasToOctop, From: aliasFromOctop},
	"entity_pages": {To: entityPageToOctop, From: entityPageFromOctop},
	"journal":      {To: journalToOctop, From: journalFromOctop},
}

// ── 对方的取值域（从 octop 的 types.py 抄下来的 Literal 列表 ✓）──
//
// 为什么要抄全：兜底前得先判「这个值对方认不认识」✓ ——
// 只看翻译表不够 ✓（对方认的值里，有一部分我们**用不到** ✓，
// 于是它不在翻译表里 ✓，可它**是合法的** ✓ ⇒ 原样带过去 ✓，
// 而不是被兜底成另一个值 ✓）。
//
// ⚠️ 抄错了会怎样：本仓这边看不出异常 ✓（值都是合法字符串 ✓），
// 而对方那边那个值落在**域外** ✓ ⇒ 参与逻辑的地方**看不见它** ✓ ⇒ 静默 ✓。

// octopEventTypes octop 认的事件类型。
var octopEventTypes = map[string]bool{
	"user_message": true, "assistant_message": true, "tool_call": true,
	"tool_result": true, "host_memory_write": true, "session_start": true,
	"session_end": true, "compaction": true, "manual": true,
}

// octopCandidateTypes octop 认的候选类型。
var octopCandidateTypes = map[string]bool{
	"Fact": true, "Decision": true, "Task": true,
	"Preference": true, "ConflictCandidate": true,
}

// octopCandidateStatuses octop 认的候选状态。
var octopCandidateStatuses = map[string]bool{
	"pending": true, "promoted": true, "rejected": true,
	"needs_review": true, "conflict": true, "extractor_failed": true,
}

// octopDecidedBys octop 认的判定者。
var octopDecidedBys = map[string]bool{
	"auto": true, "user": true, "rule": true, "portable": true,
}

// octopRecommendedActions octop 认的建议处置。
var octopRecommendedActions = map[string]bool{
	"promote": true, "merge": true, "update": true,
	"reject": true, "conflict": true, "needs_review": true,
}

// ── 取值与取字段的小工具 ──

// pick 取一个字段的值（没有则零值）。
func pick(m map[string]any, key string) any { return m[key] }

// pickStr 取一个字符串字段。
func pickStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// pickBool 取一个布尔字段。
func pickBool(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

// pickNum 取一个数值字段。
func pickNum(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}

// pickSlice 取一个字符串数组字段。
func pickSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// strSlice 把字符串数组装回 any（导出面用）。
func strSlice(v []string) []any {
	out := make([]any, 0, len(v))
	for _, s := range v {
		out = append(out, s)
	}
	return out
}

// translate 按表翻译取值；两边同名则原样保留 ✓。
func translate(v string, table map[string]string) string {
	if v == "" {
		return v
	}
	if to, ok := table[v]; ok {
		return to
	}
	return v
}

// translateOr 翻译取值；不认识的落到**对方的兜底值** ✓。
//
// 为什么要有兜底：对方那边有些枚举**参与逻辑** ✓
// （按事件类型过滤 ✓、按候选类型判定 ✓、按候选状态分流 ✓），
// 而本仓的词表是**超集**或**不同代** ✓ ⇒ 送一个对方不认的值过去 ✓
// ⇒ 那条记录在对方的逻辑里**等于不存在** ✓ ⇒ 静默 ✓。
//
// ⇒ 不认识的**不许原样送** ✓ ⇒ 落到对方的兜底值 ✓（可见、可用、且不假装精确 ✓）。
//
// ⚠️ 只给「参与逻辑」的那几个用兜底 ✓：
// 审计动作（journal action）不参与检索与判定 ✓ ⇒ 原样带过去 ✓（信息不丢 ✓）。
func translateOr(v string, table map[string]string, domain map[string]bool, fallback string) string {
	if v == "" {
		return fallback
	}
	if to, ok := table[v]; ok {
		return to
	}
	if domain[v] {
		return v
	}
	return fallback
}

// scopedOver 检查一条记录是否带归属（带 ⇒ 不导出 ✓）。
func scopedOver(m map[string]any) bool {
	return pickStr(m, "Scope") != ""
}

// ── raw_events ──

// rawEventToOctop 本仓事件 → octop 事件。
func rawEventToOctop(m map[string]any) (map[string]any, bool) {
	if scopedOver(m) {
		return nil, false
	}
	out := map[string]any{
		"id":         pickStr(m, "ID"),
		"host":       pickStr(m, "Host"),
		"session_id": pickStr(m, "SessionID"),
		"thread_id":  pickStr(m, "ThreadID"),
		"user":       pickStr(m, "User"),
		"timestamp":  pick(m, "Timestamp"),
		// 对方叫 event_type ✓，本仓叫 Type ✓。
		"event_type": translateOr(pickStr(m, "Type"), octopEventTypeTo, octopEventTypes, "manual"),
		"content":    pickStr(m, "Content"),
		"payload":    pick(m, "Payload"),
	}
	return out, true
}

// rawEventFromOctop octop 事件 → 本仓事件。
func rawEventFromOctop(m map[string]any) map[string]any {
	return map[string]any{
		"ID":        pickStr(m, "id"),
		"Host":      pickStr(m, "host"),
		"SessionID": pickStr(m, "session_id"),
		"ThreadID":  pickStr(m, "thread_id"),
		"User":      pickStr(m, "user"),
		"Timestamp": pick(m, "timestamp"),
		// 对方叫 event_type ✓，本仓叫 Type ✓。
		"Type":    translate(pickStr(m, "event_type"), octopEventTypeFrom),
		"Content": pickStr(m, "content"),
		"Payload": pick(m, "payload"),
	}
}

// ── candidates ──

// candidateToOctop 本仓候选 → octop 候选。
func candidateToOctop(m map[string]any) (map[string]any, bool) {
	if scopedOver(m) {
		return nil, false
	}
	out := map[string]any{
		"id":            pickStr(m, "ID"),
		"raw_event_ids": strSlice(pickSlice(m, "RawEventIDs")),
		// 对方叫 candidate_type ✓，本仓叫 Type ✓。
		"candidate_type":      translateOr(pickStr(m, "Type"), octopCandidateTypeTo, octopCandidateTypes, "Fact"),
		"status":              translateOr(pickStr(m, "Status"), octopCandidateStatusTo, octopCandidateStatuses, "pending"),
		"title":               pickStr(m, "Title"),
		"assertion":           pickStr(m, "Assertion"),
		"verbatim_quote":      pickStr(m, "VerbatimQuote"),
		"quote_event_id":      pickStr(m, "QuoteEventID"),
		"subject_name":        pickStr(m, "SubjectName"),
		"subject_entity_type": pickStr(m, "SubjectEntityType"),
		"target_entity_id":    pickStr(m, "TargetEntityID"),
		"confidence":          pickStr(m, "Confidence"),
		"importance":          pickStr(m, "Importance"),
		"recommended_action":  translateOr(pickStr(m, "RecommendedAction"), nil, octopRecommendedActions, "promote"),
		"promotion_reason":    pickStr(m, "PromotionReason"),
		"extractor_version":   pickStr(m, "ExtractorVersion"),
		"created_at":          pick(m, "CreatedAt"),
		"decided_at":          pick(m, "DecidedAt"),
		"decided_by":          translateOr(pickStr(m, "DecidedBy"), octopDecidedByTo, octopDecidedBys, "auto"),
		"session_id":          pickStr(m, "SessionID"),
		"payload":             pick(m, "Payload"),
	}
	return out, true
}

// candidateFromOctop octop 候选 → 本仓候选。
func candidateFromOctop(m map[string]any) map[string]any {
	return map[string]any{
		"ID":                pickStr(m, "id"),
		"RawEventIDs":       pickSlice(m, "raw_event_ids"),
		"Type":              translate(pickStr(m, "candidate_type"), octopCandidateTypeFrom),
		"Status":            translate(pickStr(m, "status"), octopCandidateStatusFrom),
		"Title":             pickStr(m, "title"),
		"Assertion":         pickStr(m, "assertion"),
		"VerbatimQuote":     pickStr(m, "verbatim_quote"),
		"QuoteEventID":      pickStr(m, "quote_event_id"),
		"SubjectName":       pickStr(m, "subject_name"),
		"SubjectEntityType": pickStr(m, "subject_entity_type"),
		"TargetEntityID":    pickStr(m, "target_entity_id"),
		"Confidence":        pickStr(m, "confidence"),
		"Importance":        pickStr(m, "importance"),
		"RecommendedAction": pickStr(m, "recommended_action"),
		"PromotionReason":   pickStr(m, "promotion_reason"),
		"ExtractorVersion":  pickStr(m, "extractor_version"),
		"CreatedAt":         pick(m, "created_at"),
		"DecidedAt":         pick(m, "decided_at"),
		"DecidedBy":         translate(pickStr(m, "decided_by"), octopDecidedByFrom),
		"SessionID":         pickStr(m, "session_id"),
		"Payload":           pick(m, "payload"),
	}
}

// ── atoms ──

// atomToOctop 本仓事实卡 → octop 事实卡。
func atomToOctop(m map[string]any) (map[string]any, bool) {
	if scopedOver(m) {
		return nil, false
	}
	return map[string]any{
		"id":             pickStr(m, "ID"),
		"entity_id":      pickStr(m, "EntityID"),
		"candidate_id":   pickStr(m, "CandidateID"),
		"raw_event_ids":  strSlice(pickSlice(m, "RawEventIDs")),
		"assertion":      pickStr(m, "Assertion"),
		"verbatim_quote": pickStr(m, "VerbatimQuote"),
		"quote_event_id": pickStr(m, "QuoteEventID"),
		"search_terms":   strSlice(pickSlice(m, "SearchTerms")),
		"occurred_at":    pick(m, "OccurredAt"),
		"confidence":     pickStr(m, "Confidence"),
		"importance":     pickStr(m, "Importance"),
		"created_at":     pick(m, "CreatedAt"),
		"superseded_by":  pickStr(m, "SupersededBy"),
		"deprecated_at":  pick(m, "DeprecatedAt"),
		// 本仓多出的 Scope / Kind / ValidFrom / ValidTo / Version
		// 在对面没有对应字段 ⇒ 不带过去 ✓（归属那条已在上面拦掉 ✓）。
	}, true
}

// atomFromOctop octop 事实卡 → 本仓事实卡。
func atomFromOctop(m map[string]any) map[string]any {
	return map[string]any{
		"ID":            pickStr(m, "id"),
		"EntityID":      pickStr(m, "entity_id"),
		"CandidateID":   pickStr(m, "candidate_id"),
		"RawEventIDs":   pickSlice(m, "raw_event_ids"),
		"Assertion":     pickStr(m, "assertion"),
		"VerbatimQuote": pickStr(m, "verbatim_quote"),
		"QuoteEventID":  pickStr(m, "quote_event_id"),
		"SearchTerms":   pickSlice(m, "search_terms"),
		"OccurredAt":    pick(m, "occurred_at"),
		"Confidence":    pickStr(m, "confidence"),
		"Importance":    pickStr(m, "importance"),
		"CreatedAt":     pick(m, "created_at"),
		"SupersededBy":  pickStr(m, "superseded_by"),
		"DeprecatedAt":  pick(m, "deprecated_at"),
	}
}

// ── entities ──

// entityToOctop 本仓实体 → octop 实体。
func entityToOctop(m map[string]any) (map[string]any, bool) {
	if scopedOver(m) {
		return nil, false
	}
	return map[string]any{
		"id":               pickStr(m, "ID"),
		"entity_type":      pickStr(m, "EntityType"),
		"canonical_name":   pickStr(m, "CanonicalName"),
		"aliases":          strSlice(pickSlice(m, "Aliases")),
		"atom_count":       pickNum(m, "AtomCount"),
		"created_at":       pick(m, "CreatedAt"),
		"last_promoted_at": pick(m, "LastPromotedAt"),
	}, true
}

// entityFromOctop octop 实体 → 本仓实体。
func entityFromOctop(m map[string]any) map[string]any {
	return map[string]any{
		"ID":             pickStr(m, "id"),
		"EntityType":     pickStr(m, "entity_type"),
		"CanonicalName":  pickStr(m, "canonical_name"),
		"Aliases":        pickSlice(m, "aliases"),
		"AtomCount":      int(pickNum(m, "atom_count")),
		"CreatedAt":      pick(m, "created_at"),
		"LastPromotedAt": pick(m, "last_promoted_at"),
	}
}

// ── aliases ──

// aliasToOctop 本仓别名 → octop 别名。
func aliasToOctop(m map[string]any) (map[string]any, bool) {
	if scopedOver(m) {
		return nil, false
	}
	return map[string]any{
		"alias":       pickStr(m, "Alias"),
		"entity_id":   pickStr(m, "EntityID"),
		"entity_type": pickStr(m, "EntityType"),
		"created_by":  translateOr(pickStr(m, "CreatedBy"), octopDecidedByTo, octopDecidedBys, "auto"),
		"created_at":  pick(m, "CreatedAt"),
	}, true
}

// aliasFromOctop octop 别名 → 本仓别名。
func aliasFromOctop(m map[string]any) map[string]any {
	return map[string]any{
		"Alias":      pickStr(m, "alias"),
		"EntityID":   pickStr(m, "entity_id"),
		"EntityType": pickStr(m, "entity_type"),
		"CreatedBy":  translate(pickStr(m, "created_by"), octopDecidedByFrom),
		"CreatedAt":  pick(m, "created_at"),
	}
}

// ── entity_pages ──

// entityPageToOctop 本仓画像 → octop 画像。
//
// ⚠️ 本仓画像**带归属**（每个视角一页 ✓），而对方只有一个页面槽位 ✓
// ⇒ 带归属的页**不导出** ✓（导出会把「某方的事实」变成「全局可见」✓ = 泄漏 ✓）。
//
// ⚠️ 对方要求 id 与 created_at ✓（缺任一 ⇒ 整表导不进去 ✓），
// 而本仓页面没有这两个字段 ✓ ⇒ **合成** ✓：
//
//	id         ← entity_id（对方的页面本来就是按实体 upsert 的 ✓）
//	created_at ← updated_at（本仓不记页面创建时刻 ✓）
func entityPageToOctop(m map[string]any) (map[string]any, bool) {
	if scopedOver(m) {
		return nil, false
	}
	return map[string]any{
		"id":                  pickStr(m, "EntityID"),
		"entity_id":           pickStr(m, "EntityID"),
		"summary_markdown":    pickStr(m, "Body"),
		"headline":            pickStr(m, "Headline"),
		"topics":              strSlice(pickSlice(m, "AtomIDs")),
		"dirty":               pickBool(m, "Dirty"),
		"regen_attempt_count": 0,
		"summary_version":     0,
		"created_at":          pick(m, "UpdatedAt"),
		"updated_at":          pick(m, "UpdatedAt"),
		"last_regen_at":       pick(m, "LastRegenAt"),
	}, true
}

// entityPageFromOctop octop 画像 → 本仓画像。
//
// ⚠️ 对面的 topics 是**主题词**✓，本仓的 AtomIDs 是**事实卡标识**✓
// ⇒ 不是一回事 ✓ ⇒ 不互相映射 ✓（硬映射会把主题词当事实卡标识存进去 ✓）。
func entityPageFromOctop(m map[string]any) map[string]any {
	return map[string]any{
		"EntityID":    pickStr(m, "entity_id"),
		"Headline":    pickStr(m, "headline"),
		"Body":        pickStr(m, "summary_markdown"),
		"Dirty":       pickBool(m, "dirty"),
		"UpdatedAt":   pick(m, "updated_at"),
		"LastRegenAt": pick(m, "last_regen_at"),
	}
}

// ── journal ──

// journalToOctop 本仓审计 → octop 审计。
func journalToOctop(m map[string]any) (map[string]any, bool) {
	kind := pickStr(m, "Kind")
	subject := pickStr(m, "Subject")
	out := map[string]any{
		"id":        pickStr(m, "ID"),
		"timestamp": pick(m, "At"),
		"action":    translate(kind, octopJournalKindTo),
		"actor":     translateOr(pickStr(m, "Actor"), octopDecidedByTo, octopDecidedBys, "auto"),
		"note":      pickStr(m, "Summary"),
		"after":     pick(m, "Detail"),
	}
	// 对象槽位：按**动作**分流 ✓（本仓的 Subject 是三类标识的合集 ✓，
	// 动作本身才说明它指哪一类 ✓ ⇒ 不是猜 ✓）。
	switch octopJournalTargetKind[kind] {
	case "atom":
		out["target_atom_id"] = subject
	case "entity":
		out["target_entity_id"] = subject
	case "candidate":
		out["target_candidate_id"] = subject
	}
	return out, true
}

// journalFromOctop octop 审计 → 本仓审计。
func journalFromOctop(m map[string]any) map[string]any {
	kind := translate(pickStr(m, "action"), octopJournalKindFrom)
	subject := pickStr(m, "target_atom_id")
	if subject == "" {
		subject = pickStr(m, "target_entity_id")
	}
	if subject == "" {
		subject = pickStr(m, "target_candidate_id")
	}
	return map[string]any{
		"ID":      pickStr(m, "id"),
		"At":      pick(m, "timestamp"),
		"Kind":    kind,
		"Subject": subject,
		"Actor":   translate(pickStr(m, "actor"), octopDecidedByFrom),
		"Summary": pickStr(m, "note"),
		"Detail":  pick(m, "after"),
	}
}

// octopRequiredFields 对方**必填**的字段（缺一个 ⇒ 那一整表导不进去 ✓）。
//
// 这张表是**从对方源码抄下来的** ✓（`_build_*` 里的 `d[...]` ✓），
// 不是我们「觉得应该有哪些」✓ —— 后者是猜 ✓。
//
// ⚠️ 它同时是**自检清单** ✓：本仓导出时逐条核对 ✓，
// 少一个就在本仓这边先红 ✓，而不是等对方那边「导入成功但 0 条」✓。
var octopRequiredFields = map[string][]string{
	"raw_events":   {"id", "host", "timestamp", "event_type"},
	"candidates":   {"id", "candidate_type", "created_at"},
	"atoms":        {"id", "entity_id", "candidate_id", "assertion", "verbatim_quote", "quote_event_id", "occurred_at", "created_at"},
	"entities":     {"id", "entity_type", "canonical_name", "created_at"},
	"aliases":      {"alias", "entity_id", "entity_type", "created_at"},
	"entity_pages": {"id", "entity_id", "created_at", "updated_at"},
	"journal":      {"id", "timestamp", "action"},
}

// checkOctopRequired 核对一条记录是否满足对方的必填字段。
//
// 返回缺哪些字段（空 = 齐了 ✓）。
func checkOctopRequired(table string, rec map[string]any) []string {
	var missing []string
	for _, f := range octopRequiredFields[table] {
		if _, ok := rec[f]; !ok {
			missing = append(missing, f)
		}
	}
	return missing
}

// octopFieldIsSet 这条记录里这个键有没有值（用于「必填但空」的检查 ✓）。
func octopFieldIsSet(rec map[string]any, field string) bool {
	v, ok := rec[field]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case float64:
		return t != 0
	}
	return true
}

// isOctopTable 这张表是不是对方有的表。
func isOctopTable(table string) bool {
	_, ok := octopTables[table]
	return ok
}
