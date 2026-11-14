package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// errAtomVectorNotStored 向量不落库时的哨兵。
//
// 本仓的定案是「向量轨不接」：两个后端都不存向量。
// 卡上带着向量来存时**必须报错**——静默丢掉的话，症状是
// 「相似度检索永远空」，而它与「存错了地方」毫无相似之处。
var errAtomVectorNotStored = errors.New("事实卡带了向量，但本后端不落向量（向量是派生物，另有向量索引面）：" +
	"请把它留在自己的存储里，或换一个会落向量的后端")

const atomCols = `id, entity_id, scope, candidate_id, raw_event_ids, assertion, verbatim_quote,
	quote_event_id, search_terms, occurred_at, confidence, importance,
	created_at, updated_at, superseded_by, deprecated_at, provenance,
	valid_from, valid_to, version, kind`

func (s *atomStore) scanAtom(rows *sql.Rows) (*types.AtomCard, error) {
	var (
		a                          types.AtomCard
		rawIDs                     string
		terms                      string
		occurred, created, updated int64
		deprecated                 sql.NullInt64
		validFrom                  int64
		validTo                    sql.NullInt64
		version                    int64
		kind                       string
		prov                       provenanceFields
	)
	if err := rows.Scan(&a.ID, &a.EntityID, &a.Scope, &a.CandidateID, &rawIDs, &a.Assertion,
		&a.VerbatimQuote, &a.QuoteEventID, &terms, &occurred, &a.Confidence, &a.Importance,
		&created, &updated, &a.SupersededBy, &deprecated, provScan{&prov},
		&validFrom, &validTo, &version, &kind); err != nil {
		return nil, err
	}
	a.RawEventIDs = parseStrings(rawIDs)
	a.SearchTerms = parseStrings(terms)
	a.OccurredAt = fromTS(occurred)
	a.CreatedAt = fromTS(created)
	a.UpdatedAt = fromTS(updated)
	a.DeprecatedAt = fromTSPtr(deprecated)
	// 生效区间读出来就是「零值 = 未设」：valid_from 的 0 读成零值时刻，
	// 于是「按发生时刻算」这条老数据语义由 types.ValidRange 兜住
	// （判据只有一处，不在存储层再抄一份）。
	a.ValidFrom = fromTS(validFrom)
	a.ValidTo = fromTSPtr(validTo)
	a.Version = int(version)
	// 时效类别：读出来就是「零值 = 默认 = 长期经验」。
	// 列是 NOT NULL DEFAULT ''，故老库的既有行读出来是空串——
	// 它们的语义（不因时间失效）在升级前后**逐字相同**。
	a.Kind = types.FactKind(kind)
	a.Provenance = types.Provenance{
		Origin:       types.OriginKind(prov.Origin),
		Host:         prov.Host,
		RuntimeError: prov.RuntimeError,
		Redacted:     prov.Redacted,
		Tool:         prov.Tool,
	}
	return &a, nil
}

func (s *atomStore) SaveAtom(ctx context.Context, ns store.Namespace, a *types.AtomCard) error {
	if err := a.Valid(); err != nil {
		return err
	}
	if len(a.Vector) > 0 {
		// 向量不落库（见 store.AtomStore.SaveAtom 与 types.AtomCard.Vector）：
		// 静默丢掉的话，症状是「相似度检索永远空」——
		// 而它与「存错了地方」毫无相似之处。故响亮失败。
		return errAtomVectorNotStored
	}
	now := a.UpdatedAt
	if now.IsZero() {
		now = a.CreatedAt
	}
	// 行 + FTS 索引必须**同一个事务**落库：这是三个独立 autocommit 的话，
	// 行 INSERT 之后、索引 DELETE/INSERT 之间任何一步进程崩溃或连接断开，
	// 真源表与索引就永久失配（行在而搜不到，静默漏命中）。
	// 已在事务里（嵌套调用）时 Transaction 复用外层事务，不会重复 BEGIN。
	// 用回调收到的那份 store，不用外层 s（理由见 raw.go 的 SaveRawBatch 注释）。
	return s.Transaction(ctx, func(ctx context.Context, tx store.Store) error {
		tsStore, ok := tx.(*Store)
		if !ok {
			return fmt.Errorf("事务内拿到的不是本后端实现，事实卡无法落库: %T", tx)
		}
		// **upsert 而不是纯 INSERT**：事实卡会被更新——晋升并入时补证据、
		// 退役时改状态、合并时迁移归属。这些都是「改既有卡」而不是「新增一张」。
		// 用纯 INSERT 的话，第二步就撞唯一键——
		// 而这个 bug 潜伏了很久：它只在「真的发生并入」时才暴露，
		// 第一次撞上它的是生命周期层的合并用例。
		//
		// # 退役态只许单向置位（retirement one-way）
		//
		// superseded_by / deprecated_at / valid_to 三列是**退役态**，CASE 保护：
		// 库里的卡已经退役（被推翻或人工作废）时，再次 SaveAtom **不得改动这三列**。
		// 没有这层保护的两个真实失效（PG 与 SQLite 同病）：
		//
		//  1. 陈旧写者复活退役卡：写者拿退役前的读视图整卡回写
		//     （superseded_by 为空串、valid_to 为零值），已退役的事实重新变成活跃；
		//  2. 并发取代互相抹链：两个晋升器并发取代同一张旧卡，后提交者把
		//     superseded_by 写回自己的值，先提交者的取代链被静默抹掉。
		//
		// 退役只能**经由** SaveAtom 置位（晋升路径就是「读出旧卡 → 改退役态 →
		// 整卡回写」），故不能简单地「已退役就整行拒绝更新」——实体合并要把
		// 已退役的卡迁到新归属（pipeline/entity），只许挡退役态这三列。
		// created_at 不在更新列表里：它是卡片**首次**落库的时刻，
		// 陈旧回写不挪动它（与 SQLite 侧同款语义，FindAtomBySignature 靠它取最新卡）。
		if _, err := tsStore.exec(ctx, `INSERT INTO atoms
		(namespace, id, entity_id, scope, candidate_id, raw_event_ids, assertion, verbatim_quote,
		 quote_event_id, search_terms, occurred_at, confidence, importance,
		 created_at, updated_at, superseded_by, deprecated_at, provenance, signature,
		 valid_from, valid_to, version, kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(namespace, id) DO UPDATE SET
			entity_id = excluded.entity_id, scope = excluded.scope,
			candidate_id = excluded.candidate_id,
			raw_event_ids = excluded.raw_event_ids, assertion = excluded.assertion,
			verbatim_quote = excluded.verbatim_quote, quote_event_id = excluded.quote_event_id,
			search_terms = excluded.search_terms, occurred_at = excluded.occurred_at,
			confidence = excluded.confidence, importance = excluded.importance,
			updated_at = excluded.updated_at, provenance = excluded.provenance,
			signature = excluded.signature,
			valid_from = excluded.valid_from,
			valid_to = CASE WHEN atoms.superseded_by <> '' OR atoms.deprecated_at IS NOT NULL
			                THEN atoms.valid_to ELSE excluded.valid_to END,
			superseded_by = CASE WHEN atoms.superseded_by <> '' OR atoms.deprecated_at IS NOT NULL
			                     THEN atoms.superseded_by ELSE excluded.superseded_by END,
			deprecated_at = CASE WHEN atoms.superseded_by <> '' OR atoms.deprecated_at IS NOT NULL
			                     THEN atoms.deprecated_at ELSE excluded.deprecated_at END,
			version = excluded.version,
			kind = excluded.kind`,
			nsOf(ns), a.ID, a.EntityID, string(a.Scope), a.CandidateID, stringsJSON(a.RawEventIDs),
			a.Assertion, a.VerbatimQuote, a.QuoteEventID, stringsJSON(a.SearchTerms), ts(a.OccurredAt),
			string(a.Confidence), string(a.Importance), ts(a.CreatedAt), ts(now),
			a.SupersededBy, tsPtr(a.DeprecatedAt),
			provenanceJSON(provenanceOf(a.Provenance)), a.Signature(),
			ts(a.ValidFrom), tsPtr(a.ValidTo), a.VersionOrOne(),
			string(a.Kind),
		); err != nil {
			return err
		}
		return tsStore.indexUpsert(ctx, "atom_fts", nsOf(ns), a.ID, atomFTSBody(a))
	})
}

// atomFTSBody 事实卡的 FTS 索引正文。
//
// 增量写路（SaveAtom）与重建路（RebuildFTSIndex）共用同一份拼法：
// 两处各写一遍的话，重建出来的索引与增量写出的索引切词口径分叉
// ——而那种分叉在数据上完全看不出来，症状只是「重建之后命中率变了」。
func atomFTSBody(a *types.AtomCard) string {
	return ftsBody(a.Assertion, a.VerbatimQuote, strings.Join(a.SearchTerms, " "))
}

func (s *atomStore) GetAtom(ctx context.Context, ns store.Namespace, id string) (*types.AtomCard, error) {
	rows, err := s.query(ctx, `SELECT `+atomCols+` FROM atoms WHERE namespace = ? AND id = ?`,
		nsOf(ns), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanAtom(rows)
}

func (s *atomStore) ListAtoms(ctx context.Context, ns store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error) {
	where := []string{"namespace = ?"}
	args := []any{nsOf(ns)}
	cond, condArgs := atomConditions(f)
	if cond != "" {
		where = append(where, cond)
		args = append(args, condArgs...)
	}

	// 末尾的 id ASC 是**并列时的稳定序**：重要度与发生时刻都相同的两张卡，
	// 没有它时返回顺序不确定——而「同签名合并时保留哪一张」正是由这个顺序
	// 决定的，于是合并结果会随机变。同一类 bug 在原始事件面上修过一次
	// （时间存纳秒 + id 次序），这里是同一课的第二处。
	//
	// OFFSET 服务于「分页取全」的翻页（store.ListAllAtoms）：全量扫描类动作
	// 不能用一个大 Limit 冒充全量（issue #59）。它与 LIMIT 同一份 ORDER BY，
	// 翻页不重不漏的前提是取数期间数据不动——扫描是先取尽后处理。
	q := `SELECT ` + atomCols + ` FROM atoms WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + importanceOrderExpr() + ` DESC, occurred_at DESC, id ASC LIMIT ? OFFSET ?`
	args = append(args, limitOf(f.Limit, defaultFetchLimit), max(f.Offset, 0))

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.AtomCard
	for rows.Next() {
		a, err := s.scanAtom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// atomConditions 事实卡的过滤条件（SQL 片段 + 参数）。
//
// **单独成一处**，因为两条取数路都要用它们：列表路（ListAtoms）与
// 检索路（SearchAtoms）。两处各写一遍的话，改了其中一处就会让
// 「列表看到的」与「搜到的」口径分叉——而那种分叉在数据上完全看不出来。
//
// 不含 namespace 条件：两条路的 namespace 位置不同
// （列表路在顶层 WHERE，检索路在子查询里），由调用方各自拼。
//
// # 事件窗口与时间窗是**两个维度**
//
// EventSince（「默认别把太老的事件端上来」）与
// OccurredFrom/OccurredTo（「问哪个时间段」）问的不是同一件事：
// 前者是召回的**默认策略**，后者是用户的**显式提问**。
// 两者同时给时是 AND 关系（都要满足）——各管各的，谁也不许吞掉谁。
func atomConditions(f store.AtomFilter) (string, []any) {
	var (
		where = []string{}
		args  = []any{}
	)
	if len(f.EntityIDs) > 0 {
		where = append(where, "entity_id IN ("+placeholders(len(f.EntityIDs))+")")
		for _, e := range f.EntityIDs {
			args = append(args, e)
		}
	}
	if len(f.IDs) > 0 {
		// 标识集合：多路召回（词面 + 语义）融合之后，
		// 要把「融合出的那一批标识」**一次**取回事实卡。
		//
		// 它必须在**取数层**生效（而不是取回来再筛）：
		// 检索路有 TopK 配额，后置过滤时被别家的条目占掉的配额已经花掉了，
		// 于是「该给我的那条」根本没进候选池——症状是「召回里混进一堆不相关的东西」。
		where = append(where, "id IN ("+placeholders(len(f.IDs))+")")
		for _, id := range f.IDs {
			args = append(args, id)
		}
	}
	if f.GlobalReader {
		// **全局读者**：只取全局事实（issue #53）。
		//
		// 空 Scope 作为过滤条件是「不隔离」，而空读者按 types.Scope.VisibleTo
		// 只看得见全局卡——读者语义必须在这里生效（配额不能被别家占掉
		// 再后置滤掉），故单独一根条件。置位时 Scope 不生效（见字段注释）。
		where = append(where, "scope = ''")
	}
	if f.Scope != "" {
		// 归属条件：**全局的 + 同归属的**，别家的不给。
		//
		// 「归属为空=不过滤」这条语义在 store.AtomFilter.Scope 的注释里，
		// 这里是它的可执行形态。
		where = append(where, "(scope = '' OR scope = ?)")
		args = append(args, string(f.Scope))
	}
	if f.ScopeExact != "" {
		// 归属**严格等于**：连全局的也不要（同归属比较语义，见
		// store.AtomFilter.ScopeExact）。必须在这里生效而不是后置过滤：
		// 后置时被滤掉的条目已经占掉 LIMIT 配额（issue #32）。
		where = append(where, "scope = ?")
		args = append(args, string(f.ScopeExact))
	}
	if f.ActiveOnly {
		// 软退役两态合起来才是「不参与召回」：只看其中一列会漏掉另一种。
		where = append(where, "superseded_by = ''", "deprecated_at IS NULL")
	}
	if len(f.Importance) > 0 {
		where = append(where, "importance IN ("+placeholders(len(f.Importance))+")")
		for _, i := range f.Importance {
			args = append(args, string(i))
		}
	}
	if !f.OccurredFrom.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, ts(f.OccurredFrom))
	}
	if !f.OccurredTo.IsZero() {
		where = append(where, "occurred_at < ?")
		args = append(args, ts(f.OccurredTo))
	}
	if !f.EventSince.IsZero() {
		// 事件窗口：**只作用于事件类**。
		//
		// 判据写成「不是事件类，或者发生时刻不早于下界」，
		// 而不是「发生时刻不早于下界」——后者会把长期经验与状态
		// 也按时间筛掉，而它们的生命周期不由时间决定：
		// 状态的「过期」由取代链表达（见 types.AtomCard.ValidTo），
		// 经验永久保留（只在被推翻时失效）。
		//
		// 零值时整条条件不出现 ⇒ 不配它时行为与从前**逐条一致**。
		where = append(where, fmt.Sprintf("(kind <> '%s' OR occurred_at >= ?)", types.FactEvent))
		args = append(args, ts(f.EventSince))
	}
	if !f.At.IsZero() {
		// 生效判据：区间含 At，**左闭右开**。
		//
		// 左闭右开不是随手选的：失效时刻那一刻事实**已经不成立**
		// （「到这一刻为止有效」），而生效时刻那一刻**恰好成立**。
		// 两端都取闭区间的话，「A 的失效时刻」与「B 的生效时刻」
		// 会同时命中两张卡——而现实中它们是前后相继的两条事实。
		//
		// COALESCE(NULLIF(valid_from,0), occurred_at) 是「显式生效点，
		// 没有就退回发生时刻」：valid_from 的 0 是「未设」，
		// 老数据（没有生效点）因此仍按发生时刻算——升级前后判同一张卡必须一致。
		where = append(where,
			"COALESCE(NULLIF(valid_from, 0), occurred_at) <= ?",
			"(valid_to IS NULL OR ? < valid_to)")
		args = append(args, ts(f.At), ts(f.At))
	}
	return strings.Join(where, " AND "), args
}

// importanceOrderExpr 重要度的排序表达式。
//
// 为什么不直接 `ORDER BY importance DESC`：importance 列是 TEXT，
// 字典序下 'med' > 'low' > 'high'，于是**最重要的排在最后**——
// 而召回的候选池就是 ListAtoms 的前 N 条（N 默认 50），
// 事实卡一多，最重要的那批最先被裁掉，而它们恰恰是最该进 prompt 的。
// 晋升的矛盾检测、生命周期三件套、迁移包导出也都走 ListAtoms，
// 于是「按字典序取前 N 条」这件事会渗进所有依赖它的动作。
//
// 映射从 types.Importance 的枚举值**程序化**生成，而不是手写一遍：
// 真源始终是 types.Importance.Rank()，不会出现「SQL 里一套、Go 里另一套」的漂移。
//
// 为什么不把列改成 INTEGER：那要一条迁移路径，而本仓的 schema 是
// CREATE TABLE IF NOT EXISTS 建表、没有 ALTER 机制——改列会让**已有库**
// 的形状与新代码对不上，而「旧形状的库被新代码按新形状读」正是本仓
// 明确要防的失效（见 sqlite.go 的 schema_version 注释）。故列保持 TEXT，
// 排序在此归一。
func importanceOrderExpr() string {
	parts := make([]string, 0, 3)
	for _, imp := range []types.Importance{types.ImportanceHigh, types.ImportanceMed, types.ImportanceLow} {
		parts = append(parts, fmt.Sprintf("WHEN '%s' THEN %d", imp, imp.Rank()))
	}
	return "CASE importance " + strings.Join(parts, " ") + " ELSE 0 END"
}

// entityScanPage 实体名下取卡的单页大小（分页取尽，issue #59）。
const entityScanPage = 500

func (s *atomStore) ListAtomsByEntity(ctx context.Context, ns store.Namespace, entityID string) ([]*types.AtomCard, error) {
	if entityID == "" {
		return nil, nil
	}
	// 不按退役态过滤：实体合并要把名下**所有**卡（含已退役的）一起迁走。
	//
	// 按**分页取尽**（issue #59）：Limit: 100000 冒充全量的失效形态是
	// 「名下卡片超限时按取数序静默截断」——合并漏一张卡，那张卡就永远
	// 指向一个已不存在的实体（本契约存在理由里点名的那类失效）。
	f := store.AtomFilter{EntityIDs: []string{entityID}}
	out := make([]*types.AtomCard, 0, entityScanPage)
	for offset := 0; ; offset += entityScanPage {
		page := f
		page.Limit = entityScanPage
		page.Offset = offset
		batch, err := s.ListAtoms(ctx, ns, page)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < entityScanPage {
			return out, nil
		}
	}
}

func (s *atomStore) FindAtomBySignature(ctx context.Context, ns store.Namespace, signature string) (*types.AtomCard, error) {
	rows, err := s.query(ctx, `SELECT `+atomCols+` FROM atoms
		WHERE namespace = ? AND signature = ? ORDER BY created_at DESC LIMIT 1`,
		nsOf(ns), signature)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanAtom(rows)
}

func (s *atomStore) SearchAtoms(ctx context.Context, ns store.Namespace, query string, f store.AtomFilter) ([]*types.AtomCard, error) {
	ids, err := s.searchAtomIDs(ctx, ns, query, f)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return s.atomsByIDsOrdered(ctx, ns, ids)
}

// searchAtomIDs 事实卡检索：取标识（带过滤条件）。
//
// # 过滤条件在**这条 SQL 里**生效
//
// 归属过滤尤其不能后置：后置的话，被别家占掉的 TopK 配额已经花掉了，
// 于是「该给我的那条」根本没进候选池——症状是「召回变少了」，
// 看着像排序问题，根因却在取数。
//
// FTS5 的正文在虚表里（atom_fts），归属与实体在 atoms 表里，
// 故用一条子查询把两者接起来：命中集先按相关度排好，
// 过完条件再 LIMIT——顺序不能反，反了就等于后置过滤。
func (s *atomStore) searchAtomIDs(ctx context.Context, ns store.Namespace,
	query string, f store.AtomFilter) ([]string, error) {
	cond, condArgs := atomConditions(f)
	extra := ""
	if cond != "" {
		extra = "atom_id IN (SELECT id FROM atoms WHERE namespace = ? AND " + cond + ")"
		condArgs = append([]any{nsOf(ns)}, condArgs...)
	}
	return s.searchFTS(ctx, "atom_fts", nsOf(ns), query, f.Limit, extra, false, condArgs...)
}

// SearchAtomsLoose **宽松**词法检索：命中**任一**词就算（OR 匹配）。
//
// 与 SearchAtoms **完全同构**：同样的过滤条件（归属 / 活跃 / 时序 /
// 事件窗口 / 实体集合）、同样在 SQL 里生效、同样按相关度排序、同样 TopK。
// 唯一的差别是**词的匹配方式**：严格路是隐式 AND，宽松路是 OR。
//
// # 为什么放宽的是「词」而不是「可见性」
//
// 过滤条件少一条的后果是**泄漏**（别家的卡进了候选池），
// 而放宽词的匹配只是「多召回几条可能相关的」——后者由排序与融合兜着。
// 故两条路共用同一处条件构造（atomConditions），一处也不许少。
func (s *atomStore) SearchAtomsLoose(ctx context.Context, ns store.Namespace,
	query string, f store.AtomFilter) ([]*types.AtomCard, error) {
	ids, err := s.searchAtomIDsLoose(ctx, ns, query, f)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return s.atomsByIDsOrdered(ctx, ns, ids)
}

// searchAtomIDsLoose 事实卡的宽松检索：取标识（带过滤条件）。
//
// 与严格路（searchAtomIDs）只差 MATCH 串的拼法，其余（namespace 限定、
// extra 条件、排序、TopK）全部复用 searchFTS。
func (s *atomStore) searchAtomIDsLoose(ctx context.Context, ns store.Namespace,
	query string, f store.AtomFilter) ([]string, error) {
	cond, condArgs := atomConditions(f)
	extra := ""
	if cond != "" {
		extra = "atom_id IN (SELECT id FROM atoms WHERE namespace = ? AND " + cond + ")"
		condArgs = append([]any{nsOf(ns)}, condArgs...)
	}
	return s.searchFTS(ctx, "atom_fts", nsOf(ns), query, f.Limit, extra, true, condArgs...)
}

func (s *atomStore) SupersedeAtom(ctx context.Context, ns store.Namespace, id, supersededBy string) error {
	if supersededBy == "" {
		return fmt.Errorf("继任者 ID 不能为空：空继任者会让这条事实永远查不到自己被谁推翻了")
	}
	return s.retire(ctx, ns, id, func(a *types.AtomCard) error {
		a.SupersededBy = supersededBy
		a.DeprecatedAt = time.Time{} // 互斥：被推翻就不该同时是「人工作废」
		return nil
	})
}

func (s *atomStore) DeprecateAtom(ctx context.Context, ns store.Namespace, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.retire(ctx, ns, id, func(a *types.AtomCard) error {
		a.DeprecatedAt = at
		a.SupersededBy = "" // 互斥，理由同上
		return nil
	})
}

// retire 读改写一条事实卡的退役态。
//
// 退役**不删索引**：被推翻的事实仍然可以被搜到并看到「已被继任者取代」，
// 因为「当初为什么这么判」是记忆系统必须能回答的问题（见 types.AtomCard 注释）。
//
// # 退役态只许单向置位（与 SaveAtom 同款 CASE 守卫，issue #48）
//
// 读改写本身挡不住两类失效（SaveAtom 注释点名的「并发取代互相抹链」
// 在这条专职路径上原样存在）：
//
//  1. 二次退役改写既有退役态：DeprecateAtom 的 mutate 无条件把
//     superseded_by 清空——对一张已被取代的卡调人工作废，会把既有取代链
//     静默抹掉（继任指针丢失，卡还在退役态，但「当初为什么这么判」断链）；
//  2. 两个并发 retire 同一张卡：都先读到未退役态，后提交者覆盖先提交者。
//
// 故 UPDATE 用与 SaveAtom 相同的 CASE：库里的行已退役（superseded_by 非空
// 或 deprecated_at 已设）时，这两列保持库里的值，本次 mutate 的结果不落库。
// 读视图在事务里拿到，但判定以**行上的现值**为准——它是两后端唯一可靠的
// 仲裁点（PG READ COMMITTED 下两个事务都能读到未退役的旧视图）。
func (s *atomStore) retire(ctx context.Context, ns store.Namespace, id string, mutate func(*types.AtomCard) error) error {
	// 读改写三步必须在同一个事务里，且用的是回调收到的 tx（理由见 raw.go 同处注释）。
	return s.Transaction(ctx, func(ctx context.Context, tx store.Store) error {
		a, err := tx.GetAtom(ctx, ns, id)
		if err != nil {
			return err
		}
		if err := mutate(a); err != nil {
			return err
		}
		if err := a.Valid(); err != nil {
			return err
		}
		a.UpdatedAt = time.Now().UTC()
		tsStore, ok := tx.(*Store)
		if !ok {
			return fmt.Errorf("事务内拿到的不是本后端实现，退役无法落库: %T", tx)
		}
		_, err = tsStore.exec(ctx,
			`UPDATE atoms SET
				superseded_by = CASE WHEN superseded_by <> '' OR deprecated_at IS NOT NULL
				                     THEN superseded_by ELSE ? END,
				deprecated_at = CASE WHEN superseded_by <> '' OR deprecated_at IS NOT NULL
				                     THEN deprecated_at ELSE ? END,
				updated_at = ?
			 WHERE namespace = ? AND id = ?`,
			a.SupersededBy, tsPtr(a.DeprecatedAt), ts(a.UpdatedAt), nsOf(ns), a.ID)
		return err
	})
}

func (s *atomStore) CountStats(ctx context.Context, ns store.Namespace) (store.Stats, error) {
	var st store.Stats
	row := s.queryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM raw_events   WHERE namespace = ?),
		(SELECT COUNT(*) FROM candidates  WHERE namespace = ?),
		(SELECT COUNT(*) FROM candidates  WHERE namespace = ? AND status = ?),
		(SELECT COUNT(*) FROM atoms       WHERE namespace = ?),
		(SELECT COUNT(*) FROM atoms       WHERE namespace = ? AND superseded_by = '' AND deprecated_at IS NULL),
		(SELECT COUNT(*) FROM entities    WHERE namespace = ?),
		(SELECT COUNT(*) FROM entity_pages WHERE namespace = ?),
		(SELECT COUNT(*) FROM entity_pages WHERE namespace = ? AND dirty = 1),
		(SELECT COUNT(*) FROM episodes    WHERE namespace = ?)`,
		nsOf(ns), nsOf(ns), nsOf(ns), string(types.CandidatePending), nsOf(ns),
		nsOf(ns), nsOf(ns), nsOf(ns), nsOf(ns), nsOf(ns))
	err := row.Scan(&st.RawEvents, &st.Candidates, &st.Pending, &st.Atoms,
		&st.ActiveAtoms, &st.Entities, &st.Pages, &st.DirtyPages, &st.Episodes)
	if err != nil {
		return store.Stats{}, err
	}
	return st, nil
}

func (s *atomStore) atomsByIDsOrdered(ctx context.Context, ns store.Namespace, ids []string) ([]*types.AtomCard, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	byID := map[string]*types.AtomCard{}
	args := []any{nsOf(ns)}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.query(ctx,
		`SELECT `+atomCols+` FROM atoms WHERE namespace = ? AND id IN (`+placeholders(len(ids))+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		a, err := s.scanAtom(rows)
		if err != nil {
			return nil, err
		}
		byID[a.ID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*types.AtomCard, 0, len(ids))
	for _, id := range ids {
		if a, ok := byID[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}
