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
var errAtomVectorNotStored = errors.New("事实卡带了向量，但本后端不落向量（向量轨未接）：" +
	"请把它留在自己的存储里，或换一个会落向量的后端")

const atomCols = `id, entity_id, scope, candidate_id, raw_event_ids, assertion, verbatim_quote,
	quote_event_id, search_terms, occurred_at, confidence, importance,
	created_at, updated_at, superseded_by, deprecated_at, provenance`

func (s *atomStore) scanAtom(rows *sql.Rows) (*types.AtomCard, error) {
	var (
		a                          types.AtomCard
		rawIDs                     string
		terms                      string
		occurred, created, updated int64
		deprecated                 sql.NullInt64
		prov                       provenanceFields
	)
	if err := rows.Scan(&a.ID, &a.EntityID, &a.Scope, &a.CandidateID, &rawIDs, &a.Assertion,
		&a.VerbatimQuote, &a.QuoteEventID, &terms, &occurred, &a.Confidence, &a.Importance,
		&created, &updated, &a.SupersededBy, &deprecated, provScan{&prov}); err != nil {
		return nil, err
	}
	a.RawEventIDs = parseStrings(rawIDs)
	a.SearchTerms = parseStrings(terms)
	a.OccurredAt = fromTS(occurred)
	a.CreatedAt = fromTS(created)
	a.UpdatedAt = fromTS(updated)
	a.DeprecatedAt = fromTSPtr(deprecated)
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
	// **upsert 而不是纯 INSERT**：事实卡会被更新——晋升并入时补证据、
	// 退役时改状态、合并时迁移归属。这些都是「改既有卡」而不是「新增一张」。
	// 用纯 INSERT 的话，第二步就撞唯一键——
	// 而这个 bug 潜伏了很久：它只在「真的发生并入」时才暴露，
	// 第一次撞上它的是生命周期层的合并用例。
	if _, err := s.exec(ctx, `INSERT INTO atoms
		(namespace, id, entity_id, scope, candidate_id, raw_event_ids, assertion, verbatim_quote,
		 quote_event_id, search_terms, occurred_at, confidence, importance,
		 created_at, updated_at, superseded_by, deprecated_at, provenance, signature)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(namespace, id) DO UPDATE SET
			entity_id = excluded.entity_id, scope = excluded.scope,
			candidate_id = excluded.candidate_id,
			raw_event_ids = excluded.raw_event_ids, assertion = excluded.assertion,
			verbatim_quote = excluded.verbatim_quote, quote_event_id = excluded.quote_event_id,
			search_terms = excluded.search_terms, occurred_at = excluded.occurred_at,
			confidence = excluded.confidence, importance = excluded.importance,
			updated_at = excluded.updated_at, superseded_by = excluded.superseded_by,
			deprecated_at = excluded.deprecated_at, provenance = excluded.provenance,
			signature = excluded.signature`,
		nsOf(ns), a.ID, a.EntityID, string(a.Scope), a.CandidateID, stringsJSON(a.RawEventIDs),
		a.Assertion, a.VerbatimQuote, a.QuoteEventID, stringsJSON(a.SearchTerms), ts(a.OccurredAt),
		string(a.Confidence), string(a.Importance), ts(a.CreatedAt), ts(now),
		a.SupersededBy, tsPtr(a.DeprecatedAt),
		provenanceJSON(provenanceOf(a.Provenance)), a.Signature(),
	); err != nil {
		return err
	}
	return s.indexUpsert(ctx, "atom_fts", nsOf(ns), a.ID,
		ftsBody(a.Assertion, a.VerbatimQuote, strings.Join(a.SearchTerms, " ")))
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
	q := `SELECT ` + atomCols + ` FROM atoms WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + importanceOrderExpr() + ` DESC, occurred_at DESC, id ASC LIMIT ?`
	args = append(args, limitOf(f.Limit, 50))

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
	if f.Scope != "" {
		// 归属条件：**全局的 + 同归属的**，别家的不给。
		//
		// 「归属为空=不过滤」这条语义在 store.AtomFilter.Scope 的注释里，
		// 这里是它的可执行形态。
		where = append(where, "(scope = '' OR scope = ?)")
		args = append(args, string(f.Scope))
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

func (s *atomStore) ListAtomsByEntity(ctx context.Context, ns store.Namespace, entityID string) ([]*types.AtomCard, error) {
	if entityID == "" {
		return nil, nil
	}
	// 不按退役态过滤：实体合并要把名下**所有**卡（含已退役的）一起迁走。
	return s.ListAtoms(ctx, ns, store.AtomFilter{
		EntityIDs: []string{entityID},
		Limit:     100000,
	})
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
	return s.searchFTS(ctx, "atom_fts", nsOf(ns), query, f.Limit, extra, condArgs...)
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
			`UPDATE atoms SET superseded_by = ?, deprecated_at = ?, updated_at = ?
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
