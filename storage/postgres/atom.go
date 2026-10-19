package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

const atomCols = `id, entity_id, candidate_id, raw_event_ids, assertion, verbatim_quote,
	quote_event_id, search_terms, occurred_at, confidence, importance,
	created_at, updated_at, superseded_by, deprecated_at, provenance`

func (s *atomStore) scanAtom(rows *sql.Rows) (*types.AtomCard, error) {
	var (
		a                          types.AtomCard
		rawIDs                     string
		terms                      string
		occurred, created, updated sql.NullTime
		deprecated                 sql.NullTime
		prov                       provenanceFields
	)
	if err := rows.Scan(&a.ID, &a.EntityID, &a.CandidateID, &rawIDs, &a.Assertion,
		&a.VerbatimQuote, &a.QuoteEventID, &terms, &occurred, &a.Confidence, &a.Importance,
		&created, &updated, &a.SupersededBy, &deprecated, provScan{&prov}); err != nil {
		return nil, err
	}
	a.RawEventIDs = parseStrings(rawIDs)
	a.SearchTerms = parseStrings(terms)
	a.OccurredAt = fromTSPtr(occurred)
	a.CreatedAt = fromTSPtr(created)
	a.UpdatedAt = fromTSPtr(updated)
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
	now := a.UpdatedAt
	if now.IsZero() {
		now = a.CreatedAt
	}
	body := ftsBody(a.Assertion, a.VerbatimQuote, strings.Join(a.SearchTerms, " "))
	// upsert 而不是纯 INSERT：事实卡会被更新——晋升并入时补证据、
	// 退役时改状态、合并时迁移归属。
	_, err := s.exec(ctx, `INSERT INTO atoms
		(namespace, id, entity_id, candidate_id, raw_event_ids, assertion, verbatim_quote,
		 quote_event_id, search_terms, occurred_at, confidence, importance,
		 created_at, updated_at, superseded_by, deprecated_at, provenance, signature,
		 body, body_tsv)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
		        to_tsvector('simple', $19))
		ON CONFLICT(namespace, id) DO UPDATE SET
			entity_id = excluded.entity_id, candidate_id = excluded.candidate_id,
			raw_event_ids = excluded.raw_event_ids, assertion = excluded.assertion,
			verbatim_quote = excluded.verbatim_quote, quote_event_id = excluded.quote_event_id,
			search_terms = excluded.search_terms, occurred_at = excluded.occurred_at,
			confidence = excluded.confidence, importance = excluded.importance,
			created_at = excluded.created_at, updated_at = excluded.updated_at,
			superseded_by = excluded.superseded_by, deprecated_at = excluded.deprecated_at,
			provenance = excluded.provenance, signature = excluded.signature,
			body = excluded.body, body_tsv = excluded.body_tsv`,
		nsOf(ns), a.ID, a.EntityID, a.CandidateID, stringsJSON(a.RawEventIDs), a.Assertion,
		a.VerbatimQuote, a.QuoteEventID, stringsJSON(a.SearchTerms), ts(a.OccurredAt),
		string(a.Confidence), string(a.Importance), ts(a.CreatedAt), ts(now),
		a.SupersededBy, tsPtr(a.DeprecatedAt),
		provenanceJSON(provenanceOf(a.Provenance)), a.Signature(), body)
	return err
}

func (s *atomStore) GetAtom(ctx context.Context, ns store.Namespace, id string) (*types.AtomCard, error) {
	rows, err := s.query(ctx, `SELECT `+atomCols+` FROM atoms WHERE namespace = $1 AND id = $2`,
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
	var (
		where = []string{"namespace = $1"}
		args  = []any{nsOf(ns)}
	)
	if len(f.EntityIDs) > 0 {
		where = append(where, "entity_id IN ("+numberedPlaceholders(len(args)+1, len(f.EntityIDs))+")")
		for _, e := range f.EntityIDs {
			args = append(args, e)
		}
	}
	if f.ActiveOnly {
		// 软退役两态合起来才是「不参与召回」：只看其中一列会漏掉另一种。
		where = append(where, "superseded_by = ''", "deprecated_at IS NULL")
	}
	if len(f.Importance) > 0 {
		where = append(where, "importance IN ("+numberedPlaceholders(len(args)+1, len(f.Importance))+")")
		for _, i := range f.Importance {
			args = append(args, string(i))
		}
	}
	if !f.OccurredFrom.IsZero() {
		where = append(where, "occurred_at >= $"+fmt.Sprint(len(args)+1))
		args = append(args, ts(f.OccurredFrom))
	}
	if !f.OccurredTo.IsZero() {
		where = append(where, "occurred_at < $"+fmt.Sprint(len(args)+1))
		args = append(args, ts(f.OccurredTo))
	}
	// 末尾的 id ASC 是**并列时的稳定序**：重要度与发生时刻都相同的两张卡，
	// 没有它时返回顺序不确定——而「同签名合并时保留哪一张」正是由这个顺序
	// 决定的，于是合并结果会随机变。
	//
	// 重要度排序用 CASE 表达式（importanceOrderExpr）而不是字典序：
	// importance 列是 TEXT，字典序下 'med' > 'low' > 'high'，最重要的会排最后。
	q := `SELECT ` + atomCols + ` FROM atoms WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY ` + importanceOrderExpr() + ` DESC, occurred_at DESC, id ASC LIMIT $` + fmt.Sprint(len(args)+1)
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
		WHERE namespace = $1 AND signature = $2 ORDER BY created_at DESC LIMIT 1`,
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

func (s *atomStore) SearchAtoms(ctx context.Context, ns store.Namespace, query string, limit int) ([]*types.AtomCard, error) {
	ids, err := s.searchFTS(ctx, "atoms", nsOf(ns), query, limit)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	return s.atomsByIDsOrdered(ctx, ns, ids)
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
// 退役**不删正文**：被推翻的事实仍然可以被搜到并看到「已被继任者取代」，
// 因为「当初为什么这么判」是记忆系统必须能回答的问题。
func (s *atomStore) retire(ctx context.Context, ns store.Namespace, id string, mutate func(*types.AtomCard) error) error {
	// 读改写三步必须在同一个事务里，且用的是回调收到的 tx。
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
			`UPDATE atoms SET superseded_by = $1, deprecated_at = $2, updated_at = $3
			 WHERE namespace = $4 AND id = $5`,
			a.SupersededBy, tsPtr(a.DeprecatedAt), ts(a.UpdatedAt), nsOf(ns), a.ID)
		return err
	})
}

func (s *atomStore) CountStats(ctx context.Context, ns store.Namespace) (store.Stats, error) {
	var st store.Stats
	row := s.queryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM raw_events   WHERE namespace = $1),
		(SELECT COUNT(*) FROM candidates  WHERE namespace = $1),
		(SELECT COUNT(*) FROM candidates  WHERE namespace = $1 AND status = $2),
		(SELECT COUNT(*) FROM atoms       WHERE namespace = $1),
		(SELECT COUNT(*) FROM atoms       WHERE namespace = $1 AND superseded_by = '' AND deprecated_at IS NULL),
		(SELECT COUNT(*) FROM entities    WHERE namespace = $1),
		(SELECT COUNT(*) FROM entity_pages WHERE namespace = $1),
		(SELECT COUNT(*) FROM entity_pages WHERE namespace = $1 AND dirty),
		(SELECT COUNT(*) FROM episodes    WHERE namespace = $1)`,
		nsOf(ns), string(types.CandidatePending))
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
		`SELECT `+atomCols+` FROM atoms WHERE namespace = $1 AND id IN (`+numberedPlaceholders(2, len(ids))+`)`,
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

// importanceOrderExpr 重要度的排序表达式。
//
// 为什么不直接 `ORDER BY importance DESC`：importance 列是 TEXT，
// 字典序下 'med' > 'low' > 'high'，于是**最重要的排在最后**——
// 而召回的候选池就是 ListAtoms 的前 N 条，最重要的那批最先被裁掉。
//
// 映射从 types.Importance 的枚举值**程序化**生成，而不是手写一遍：
// 真源始终是 types.Importance.Rank()，不会出现「SQL 里一套、Go 里另一套」的漂移。
func importanceOrderExpr() string {
	parts := make([]string, 0, 3)
	for _, imp := range []types.Importance{types.ImportanceHigh, types.ImportanceMed, types.ImportanceLow} {
		parts = append(parts, fmt.Sprintf("WHEN '%s' THEN %d", imp, imp.Rank()))
	}
	return "CASE importance " + strings.Join(parts, " ") + " ELSE 0 END"
}
