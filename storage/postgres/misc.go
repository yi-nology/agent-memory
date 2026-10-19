package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var (
	errEmptyAlias        = errors.New("别名与实体 ID 都不能为空")
	errEmptyActiveEntity = errors.New("线程 ID 与实体 ID 都不能为空")
)

// itoa 是 strconv.Itoa 的本地小别名（占位符拼接处用得很多）。
func itoa(n int) string { return fmt.Sprint(n) }

// ── 实体页面 ──

const pageCols = `entity_id, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure`

func (s *pageStore) scanPage(rows *sql.Rows) (*types.EntityPage, error) {
	var (
		p       types.EntityPage
		atomIDs string
		updated sql.NullTime
		regen   sql.NullTime
	)
	if err := rows.Scan(&p.EntityID, &p.Headline, &p.Body, &atomIDs, &p.Dirty,
		&updated, &regen, &p.RegenFailure); err != nil {
		return nil, err
	}
	p.AtomIDs = parseStrings(atomIDs)
	p.UpdatedAt = fromTSPtr(updated)
	p.LastRegenAt = fromTSPtr(regen)
	return &p, nil
}

func (s *pageStore) UpsertEntityPage(ctx context.Context, ns store.Namespace, p *types.EntityPage) error {
	if p.EntityID == "" {
		return errors.New("页面必须有归属实体")
	}
	return s.writePage(ctx, ns, p.EntityID, p.Headline, p.Body, p.AtomIDs, p.Dirty,
		ts(p.UpdatedAt), tsPtr(p.LastRegenAt), p.RegenFailure)
}

func (s *pageStore) writePage(ctx context.Context, ns store.Namespace, entityID, headline, body string,
	atomIDs []string, dirty bool, updated any, regen any, failure string) error {
	_, err := s.exec(ctx, `INSERT INTO entity_pages
		(namespace, entity_id, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(namespace, entity_id) DO UPDATE SET
			headline = excluded.headline, body = excluded.body,
			atom_ids = excluded.atom_ids, dirty = excluded.dirty,
			updated_at = excluded.updated_at, last_regen_at = excluded.last_regen_at,
			regen_failure = excluded.regen_failure`,
		nsOf(ns), entityID, headline, body, stringsJSON(atomIDs), dirty, updated, regen, failure)
	return err
}

func (s *pageStore) GetEntityPage(ctx context.Context, ns store.Namespace, entityID string) (*types.EntityPage, error) {
	rows, err := s.query(ctx, `SELECT `+pageCols+` FROM entity_pages WHERE namespace = $1 AND entity_id = $2`,
		nsOf(ns), entityID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanPage(rows)
}

func (s *pageStore) ListDirtyEntityPages(ctx context.Context, ns store.Namespace, limit int) ([]*types.EntityPage, error) {
	rows, err := s.query(ctx,
		`SELECT `+pageCols+` FROM entity_pages WHERE namespace = $1 AND dirty
		 ORDER BY updated_at ASC LIMIT $2`, nsOf(ns), limitOf(limit, 50))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.EntityPage
	for rows.Next() {
		p, err := s.scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListEntityPages 列出全部页面。
func (s *Store) ListEntityPages(ctx context.Context, ns store.Namespace, limit int) ([]*types.EntityPage, error) {
	rows, err := s.query(ctx,
		`SELECT `+pageCols+` FROM entity_pages WHERE namespace = $1 ORDER BY entity_id LIMIT $2`,
		nsOf(ns), limitOf(limit, 1000))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.EntityPage
	for rows.Next() {
		p, err := s.scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkEntityPageDirty 标脏。
//
// 页面不存在时**不报错而是建一条空脏页**：晋升先于首次生成是常态，
// 而「实体还没有页面」不该让晋升流程失败——那会把一个正常的写入变成错误。
func (s *pageStore) MarkEntityPageDirty(ctx context.Context, ns store.Namespace, entityID string, when time.Time) error {
	if when.IsZero() {
		when = time.Now().UTC()
	}
	res, err := s.exec(ctx,
		`UPDATE entity_pages SET dirty = TRUE, updated_at = $1 WHERE namespace = $2 AND entity_id = $3`,
		ts(when), nsOf(ns), entityID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return s.writePage(ctx, ns, entityID, "", "", nil, true, ts(when), nil, "")
}

// ApplyEntityPageRegen 生成成功：落正文、清脏、清失败原因。
func (s *pageStore) ApplyEntityPageRegen(ctx context.Context, ns store.Namespace, entityID string,
	p *types.EntityPage, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	p.EntityID = entityID
	p.Dirty = false
	p.LastRegenAt = at
	p.RegenFailure = ""
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = at
	}
	return s.writePage(ctx, ns, entityID, p.Headline, p.Body, p.AtomIDs, false,
		ts(p.UpdatedAt), ts(at), "")
}

// RecordEntityPageRegenFailure 生成失败：记原因、**保持脏**。
//
// 保持脏是关键：清掉脏标记会让这条页面永远不再进重生成队列，
// 而用户看到的是「页面存在、内容过时」——与「页面正常」长得一模一样。
func (s *pageStore) RecordEntityPageRegenFailure(ctx context.Context, ns store.Namespace, entityID, reason string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.exec(ctx,
		`UPDATE entity_pages SET dirty = TRUE, regen_failure = $1, last_regen_at = $2
		 WHERE namespace = $3 AND entity_id = $4`,
		reason, ts(at), nsOf(ns), entityID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return s.writePage(ctx, ns, entityID, "", "", nil, true, ts(at), ts(at), reason)
}

// ── 情节与周期摘要 ──

func (s *episodeStore) SaveEpisode(ctx context.Context, ns store.Namespace, e *types.Episode) error {
	if e.ID == "" {
		return errors.New("情节 ID 不能为空")
	}
	_, err := s.exec(ctx, `INSERT INTO episodes
		(namespace, id, session_id, title, summary, decisions, emotions, occurred_at, created_at, raw_event_ids)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT(namespace, id) DO UPDATE SET
			title = excluded.title, summary = excluded.summary,
			decisions = excluded.decisions, emotions = excluded.emotions,
			raw_event_ids = excluded.raw_event_ids`,
		nsOf(ns), e.ID, e.SessionID, e.Title, e.Summary, stringsJSON(e.Decisions),
		stringsJSON(e.Emotions), ts(e.OccurredAt), ts(e.CreatedAt), stringsJSON(e.RawEventIDs))
	return err
}

func (s *episodeStore) ListEpisodes(ctx context.Context, ns store.Namespace, session string, limit int) ([]*types.Episode, error) {
	q := `SELECT id, session_id, title, summary, decisions, emotions, occurred_at, created_at, raw_event_ids
		FROM episodes WHERE namespace = $1`
	args := []any{nsOf(ns)}
	if session != "" {
		q += ` AND session_id = $2`
		args = append(args, session)
	}
	q += ` ORDER BY occurred_at DESC LIMIT $` + itoa(len(args)+1)
	args = append(args, limitOf(limit, 50))
	return s.queryEpisodes(ctx, q, args...)
}

func (s *episodeStore) ListEpisodesInWindow(ctx context.Context, ns store.Namespace, from, to time.Time) ([]*types.Episode, error) {
	return s.queryEpisodes(ctx,
		`SELECT id, session_id, title, summary, decisions, emotions, occurred_at, created_at, raw_event_ids
		 FROM episodes WHERE namespace = $1 AND occurred_at >= $2 AND occurred_at < $3
		 ORDER BY occurred_at ASC`,
		nsOf(ns), ts(from), ts(to))
}

func (s *episodeStore) queryEpisodes(ctx context.Context, q string, args ...any) ([]*types.Episode, error) {
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.Episode
	for rows.Next() {
		var (
			e             types.Episode
			dec, emo, raw string
			occurred      sql.NullTime
			created       sql.NullTime
		)
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Title, &e.Summary, &dec, &emo,
			&occurred, &created, &raw); err != nil {
			return nil, err
		}
		e.Decisions = parseStrings(dec)
		e.Emotions = parseStrings(emo)
		e.RawEventIDs = parseStrings(raw)
		e.OccurredAt = fromTSPtr(occurred)
		e.CreatedAt = fromTSPtr(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *episodeStore) SaveDigest(ctx context.Context, ns store.Namespace, d *types.DigestRecord) error {
	_, err := s.exec(ctx, `INSERT INTO digests
		(namespace, period, period_key, markdown, episode_ids, window_start, window_end, generated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(namespace, period, period_key) DO UPDATE SET
			markdown = excluded.markdown, episode_ids = excluded.episode_ids,
			window_start = excluded.window_start, window_end = excluded.window_end,
			generated_at = excluded.generated_at`,
		nsOf(ns), string(d.Period), d.PeriodKey, d.Markdown, stringsJSON(d.EpisodeIDs),
		ts(d.WindowStart), ts(d.WindowEnd), ts(d.GeneratedAt))
	return err
}

func (s *episodeStore) GetDigest(ctx context.Context, ns store.Namespace, period types.DigestPeriod, key string) (*types.DigestRecord, error) {
	rows, err := s.query(ctx,
		`SELECT period, period_key, markdown, episode_ids, window_start, window_end, generated_at
		 FROM digests WHERE namespace = $1 AND period = $2 AND period_key = $3`,
		nsOf(ns), string(period), key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	var (
		d          types.DigestRecord
		ids        string
		start, end sql.NullTime
		gen        sql.NullTime
	)
	if err := rows.Scan(&d.Period, &d.PeriodKey, &d.Markdown, &ids, &start, &end, &gen); err != nil {
		return nil, err
	}
	d.EpisodeIDs = parseStrings(ids)
	d.WindowStart = fromTSPtr(start)
	d.WindowEnd = fromTSPtr(end)
	d.GeneratedAt = fromTSPtr(gen)
	return &d, nil
}

// ListDigests 列出全部周期摘要。
func (s *Store) ListDigests(ctx context.Context, ns store.Namespace, limit int) ([]*types.DigestRecord, error) {
	rows, err := s.query(ctx,
		`SELECT period, period_key, markdown, episode_ids, window_start, window_end, generated_at
		 FROM digests WHERE namespace = $1 ORDER BY period, period_key LIMIT $2`,
		nsOf(ns), limitOf(limit, 1000))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.DigestRecord
	for rows.Next() {
		var (
			d          types.DigestRecord
			ids        string
			start, end sql.NullTime
			gen        sql.NullTime
		)
		if err := rows.Scan(&d.Period, &d.PeriodKey, &d.Markdown, &ids, &start, &end, &gen); err != nil {
			return nil, err
		}
		d.EpisodeIDs = parseStrings(ids)
		d.WindowStart = fromTSPtr(start)
		d.WindowEnd = fromTSPtr(end)
		d.GeneratedAt = fromTSPtr(gen)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// ── 审计与元数据 ──

func (s *journalStore) AppendJournal(ctx context.Context, ns store.Namespace, e *types.JournalEntry) error {
	if e.ID == "" {
		return errors.New("审计条目 ID 不能为空")
	}
	_, err := s.exec(ctx, `INSERT INTO journal
		(namespace, id, at, kind, subject, actor, summary, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		nsOf(ns), e.ID, ts(e.At), string(e.Kind), e.Subject, string(e.Actor),
		e.Summary, mapJSON(e.Detail))
	return err
}

func (s *journalStore) ListJournal(ctx context.Context, ns store.Namespace, subject string, limit int) ([]*types.JournalEntry, error) {
	q := `SELECT id, at, kind, subject, actor, summary, detail FROM journal WHERE namespace = $1`
	args := []any{nsOf(ns)}
	if subject != "" {
		q += ` AND subject = $2`
		args = append(args, subject)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT $` + itoa(len(args)+1)
	args = append(args, limitOf(limit, 50))

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.JournalEntry
	for rows.Next() {
		var (
			e   types.JournalEntry
			at  sql.NullTime
			det string
		)
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Subject, &e.Actor, &e.Summary, &det); err != nil {
			return nil, err
		}
		e.At = fromTSPtr(at)
		e.Detail = parseMap(det)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *metaStore) GetMeta(ctx context.Context, ns store.Namespace, key string) (string, error) {
	var v string
	err := s.queryRow(ctx, `SELECT value FROM meta WHERE namespace = $1 AND key = $2`,
		nsOf(ns), key).Scan(&v)
	if err != nil {
		return "", notFound(err)
	}
	return v, nil
}

func (s *metaStore) SetMeta(ctx context.Context, ns store.Namespace, key, value string) error {
	_, err := s.exec(ctx,
		`INSERT INTO meta (namespace, key, value) VALUES ($1, $2, $3)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value`,
		nsOf(ns), key, value)
	return err
}
