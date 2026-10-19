package sqlite

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
		occurred, created, updated int64
		deprecated                 sql.NullInt64
		prov                       provenanceFields
	)
	if err := rows.Scan(&a.ID, &a.EntityID, &a.CandidateID, &rawIDs, &a.Assertion,
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
	now := a.UpdatedAt
	if now.IsZero() {
		now = a.CreatedAt
	}
	if _, err := s.exec(ctx, `INSERT INTO atoms
		(namespace, id, entity_id, candidate_id, raw_event_ids, assertion, verbatim_quote,
		 quote_event_id, search_terms, occurred_at, confidence, importance,
		 created_at, updated_at, superseded_by, deprecated_at, provenance, signature)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		nsOf(ns), a.ID, a.EntityID, a.CandidateID, stringsJSON(a.RawEventIDs), a.Assertion,
		a.VerbatimQuote, a.QuoteEventID, stringsJSON(a.SearchTerms), ts(a.OccurredAt),
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
	var (
		where = []string{"namespace = ?"}
		args  = []any{nsOf(ns)}
	)
	if len(f.EntityIDs) > 0 {
		where = append(where, "entity_id IN ("+placeholders(len(f.EntityIDs))+")")
		for _, e := range f.EntityIDs {
			args = append(args, e)
		}
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
	q := `SELECT ` + atomCols + ` FROM atoms WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY importance DESC, occurred_at DESC LIMIT ?`
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

func (s *atomStore) SearchAtoms(ctx context.Context, ns store.Namespace, query string, limit int) ([]*types.AtomCard, error) {
	ids, err := s.searchFTS(ctx, "atom_fts", nsOf(ns), query, limit)
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
		(SELECT COUNT(*) FROM episodes    WHERE namespace = ?),
		(SELECT COUNT(*) FROM memory_nodes WHERE namespace = ?)`,
		nsOf(ns), nsOf(ns), nsOf(ns), string(types.CandidatePending), nsOf(ns),
		nsOf(ns), nsOf(ns), nsOf(ns), nsOf(ns), nsOf(ns), nsOf(ns))
	err := row.Scan(&st.RawEvents, &st.Candidates, &st.Pending, &st.Atoms,
		&st.ActiveAtoms, &st.Entities, &st.Pages, &st.DirtyPages, &st.Episodes, &st.Nodes)
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
