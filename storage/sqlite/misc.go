package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var (
	errEmptyAlias        = errors.New("别名与实体 ID 都不能为空")
	errEmptyActiveEntity = errors.New("线程 ID 与实体 ID 都不能为空")
	errEmptyNodeID       = errors.New("节点 ID 不能为空")
)

// ── 实体页面 ──
//
// 页面的逻辑主键是 (namespace, entity_id, scope)：归属是**键的一部分**，不是过滤面。
// 页面是事实的摘要，而事实已按归属隔离；摘要若不分视角，
// 等于把别家的事实揉进一张画像里（内容泄漏），而画像是人读的一整段话，
// 读者分不清哪句属于自己。所以取页是**精确取键**：别的视角取不到，不回退。

const pageCols = `entity_id, scope, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure`

func (s *pageStore) scanPage(rows *sql.Rows) (*types.EntityPage, error) {
	var (
		p       types.EntityPage
		atomIDs string
		updated int64
		regen   sql.NullInt64
	)
	if err := rows.Scan(&p.EntityID, &p.Scope, &p.Headline, &p.Body, &atomIDs, &p.Dirty,
		&updated, &regen, &p.RegenFailure); err != nil {
		return nil, err
	}
	p.AtomIDs = parseStrings(atomIDs)
	p.UpdatedAt = fromTS(updated)
	p.LastRegenAt = fromTSPtr(regen)
	return &p, nil
}

func (s *pageStore) UpsertEntityPage(ctx context.Context, ns store.Namespace, p *types.EntityPage) error {
	if p.EntityID == "" {
		return errors.New("页面必须有归属实体")
	}
	return s.writePage(ctx, ns, p.EntityID, p.Scope, p.Headline, p.Body, p.AtomIDs, boolToInt(p.Dirty),
		ts(p.UpdatedAt), tsPtr(p.LastRegenAt), p.RegenFailure)
}

func (s *pageStore) writePage(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope,
	headline, body string, atomIDs []string, dirty int, updated any, regen any, failure string) error {
	_, err := s.exec(ctx, `INSERT INTO entity_pages
		(namespace, entity_id, scope, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(namespace, entity_id, scope) DO UPDATE SET
			headline = excluded.headline, body = excluded.body,
			atom_ids = excluded.atom_ids, dirty = excluded.dirty,
			updated_at = excluded.updated_at, last_regen_at = excluded.last_regen_at,
			regen_failure = excluded.regen_failure`,
		nsOf(ns), entityID, string(scope), headline, body, stringsJSON(atomIDs), dirty, updated, regen, failure)
	return err
}

// GetEntityPage 精确取 (namespace, entity_id, scope) 这一页。
//
// **不回退**：别的视角的页面取不到就是取不到（store.ErrNotFound）。
// 回退策略归召回层管——它知道读者是谁；存储层替它猜，猜错就是内容泄漏。
func (s *pageStore) GetEntityPage(ctx context.Context, ns store.Namespace, entityID string,
	scope types.Scope) (*types.EntityPage, error) {
	rows, err := s.query(ctx,
		`SELECT `+pageCols+` FROM entity_pages WHERE namespace = ? AND entity_id = ? AND scope = ?`,
		nsOf(ns), entityID, string(scope))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanPage(rows)
}

// ListDirtyEntityPages 列出该视角下的脏页面。
func (s *pageStore) ListDirtyEntityPages(ctx context.Context, ns store.Namespace, scope types.Scope,
	limit int) ([]*types.EntityPage, error) {
	rows, err := s.query(ctx,
		`SELECT `+pageCols+` FROM entity_pages WHERE namespace = ? AND scope = ? AND dirty = 1
		 ORDER BY updated_at ASC, entity_id ASC, scope ASC LIMIT ?`,
		nsOf(ns), string(scope), limitOf(limit, 50))
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

// ListEntityPages 列出全部页面（**跨视角全列**）。
//
// 导包要的是这个命名空间的全部记忆：只导一个视角等于导出时把别家的画像
// 悄悄丢了，而导入方看不出来（包是完整的，只是少了几页）。
func (s *Store) ListEntityPages(ctx context.Context, ns store.Namespace, limit int) ([]*types.EntityPage, error) {
	rows, err := s.query(ctx,
		`SELECT `+pageCols+` FROM entity_pages WHERE namespace = ? ORDER BY entity_id ASC, scope ASC LIMIT ?`,
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
// scope 是**这条事实的归属**，不是目标页面的视角：
//
//   - 归属事实（非空）→ 只有本视角的页面收录它，故只标本视角那一页；
//   - 全局事实（空）→ 它出现在**每个视角的画像里**，故所有视角都过时。
//
// 只标一页的后果不是「少了一次重生成」，而是**别家的画像停在旧版本**，
// 而看板上「页面存在且是旧的」与「页面正常」长得一模一样。
//
// 页面不存在时**不报错而是建一条空脏页**：晋升先于首次生成是常态，
// 而「实体还没有页面」不该让晋升流程失败——那会把一个正常的写入变成错误。
func (s *pageStore) MarkEntityPageDirty(ctx context.Context, ns store.Namespace, entityID string,
	scope types.Scope, when time.Time) error {
	if when.IsZero() {
		when = time.Now().UTC()
	}
	if scope.IsGlobal() {
		// 全局事实：所有视角的页面都收录它，全都要标脏。
		res, err := s.exec(ctx,
			`UPDATE entity_pages SET dirty = 1, updated_at = ? WHERE namespace = ? AND entity_id = ?`,
			ts(when), nsOf(ns), entityID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		return s.writePage(ctx, ns, entityID, scope, "", "", nil, 1, ts(when), nil, "")
	}
	res, err := s.exec(ctx,
		`UPDATE entity_pages SET dirty = 1, updated_at = ?
		 WHERE namespace = ? AND entity_id = ? AND scope = ?`,
		ts(when), nsOf(ns), entityID, string(scope))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return s.writePage(ctx, ns, entityID, scope, "", "", nil, 1, ts(when), nil, "")
}

// ApplyEntityPageRegen 生成成功：落正文、清脏、清失败原因。
//
// scope 是**键的一部分**：生成结果写到 (entity_id, scope) 那一页，
// 不覆盖同实体别的视角的页面。
func (s *pageStore) ApplyEntityPageRegen(ctx context.Context, ns store.Namespace, entityID string,
	scope types.Scope, p *types.EntityPage, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	p.EntityID = entityID
	p.Scope = scope
	p.Dirty = false
	p.LastRegenAt = at
	p.RegenFailure = ""
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = at
	}
	return s.writePage(ctx, ns, entityID, scope, p.Headline, p.Body, p.AtomIDs, 0,
		ts(p.UpdatedAt), ts(at), "")
}

// RecordEntityPageRegenFailure 生成失败：记原因、**保持脏**。
//
// 保持脏是关键：清掉脏标记会让这条页面永远不再进重生成队列，
// 而用户看到的是「页面存在、内容过时」——与「页面正常」长得一模一样。
func (s *pageStore) RecordEntityPageRegenFailure(ctx context.Context, ns store.Namespace, entityID string,
	scope types.Scope, reason string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := s.exec(ctx,
		`UPDATE entity_pages SET dirty = 1, regen_failure = ?, last_regen_at = ?
		 WHERE namespace = ? AND entity_id = ? AND scope = ?`,
		reason, ts(at), nsOf(ns), entityID, string(scope))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return s.writePage(ctx, ns, entityID, scope, "", "", nil, 1, ts(at), ts(at), reason)
}

// ── 情节与周期摘要 ──

func (s *episodeStore) SaveEpisode(ctx context.Context, ns store.Namespace, e *types.Episode) error {
	if e.ID == "" {
		return errors.New("情节 ID 不能为空")
	}
	_, err := s.exec(ctx, `INSERT INTO episodes
		(namespace, id, session_id, title, summary, decisions, emotions, occurred_at, created_at, raw_event_ids)
		VALUES (?,?,?,?,?,?,?,?,?,?)
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
		FROM episodes WHERE namespace = ?`
	args := []any{nsOf(ns)}
	if session != "" {
		q += ` AND session_id = ?`
		args = append(args, session)
	}
	q += ` ORDER BY occurred_at DESC LIMIT ?`
	args = append(args, limitOf(limit, 50))
	return s.queryEpisodes(ctx, q, args...)
}

func (s *episodeStore) ListEpisodesInWindow(ctx context.Context, ns store.Namespace, from, to time.Time) ([]*types.Episode, error) {
	return s.queryEpisodes(ctx,
		`SELECT id, session_id, title, summary, decisions, emotions, occurred_at, created_at, raw_event_ids
		 FROM episodes WHERE namespace = ? AND occurred_at >= ? AND occurred_at < ?
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
			e                 types.Episode
			dec, emo, raw     string
			occurred, created int64
		)
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Title, &e.Summary, &dec, &emo,
			&occurred, &created, &raw); err != nil {
			return nil, err
		}
		e.Decisions = parseStrings(dec)
		e.Emotions = parseStrings(emo)
		e.RawEventIDs = parseStrings(raw)
		e.OccurredAt = fromTS(occurred)
		e.CreatedAt = fromTS(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *episodeStore) SaveDigest(ctx context.Context, ns store.Namespace, d *types.DigestRecord) error {
	_, err := s.exec(ctx, `INSERT INTO digests
		(namespace, period, period_key, markdown, episode_ids, window_start, window_end, generated_at)
		VALUES (?,?,?,?,?,?,?,?)
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
		 FROM digests WHERE namespace = ? AND period = ? AND period_key = ?`,
		nsOf(ns), string(period), key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	var (
		d     types.DigestRecord
		ids   string
		start int64
		end   int64
		gen   int64
	)
	if err := rows.Scan(&d.Period, &d.PeriodKey, &d.Markdown, &ids, &start, &end, &gen); err != nil {
		return nil, err
	}
	d.EpisodeIDs = parseStrings(ids)
	d.WindowStart = fromTS(start)
	d.WindowEnd = fromTS(end)
	d.GeneratedAt = fromTS(gen)
	return &d, nil
}

// ListDigests 列出全部周期摘要。
func (s *Store) ListDigests(ctx context.Context, ns store.Namespace, limit int) ([]*types.DigestRecord, error) {
	rows, err := s.query(ctx,
		`SELECT period, period_key, markdown, episode_ids, window_start, window_end, generated_at
		 FROM digests WHERE namespace = ? ORDER BY period, period_key LIMIT ?`,
		nsOf(ns), limitOf(limit, 1000))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.DigestRecord
	for rows.Next() {
		var (
			d               types.DigestRecord
			ids             string
			start, end, gen int64
		)
		if err := rows.Scan(&d.Period, &d.PeriodKey, &d.Markdown, &ids, &start, &end, &gen); err != nil {
			return nil, err
		}
		d.EpisodeIDs = parseStrings(ids)
		d.WindowStart = fromTS(start)
		d.WindowEnd = fromTS(end)
		d.GeneratedAt = fromTS(gen)
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
		VALUES (?,?,?,?,?,?,?,?)`,
		nsOf(ns), e.ID, ts(e.At), string(e.Kind), e.Subject, string(e.Actor),
		e.Summary, mapJSON(e.Detail))
	return err
}

func (s *journalStore) ListJournal(ctx context.Context, ns store.Namespace, subject string, limit int) ([]*types.JournalEntry, error) {
	q := `SELECT id, at, kind, subject, actor, summary, detail FROM journal WHERE namespace = ?`
	args := []any{nsOf(ns)}
	if subject != "" {
		q += ` AND subject = ?`
		args = append(args, subject)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
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
			at  int64
			det string
		)
		if err := rows.Scan(&e.ID, &at, &e.Kind, &e.Subject, &e.Actor, &e.Summary, &det); err != nil {
			return nil, err
		}
		e.At = fromTS(at)
		e.Detail = parseMap(det)
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *metaStore) GetMeta(ctx context.Context, ns store.Namespace, key string) (string, error) {
	var v string
	err := s.queryRow(ctx, `SELECT value FROM meta WHERE namespace = ? AND key = ?`,
		nsOf(ns), key).Scan(&v)
	if err != nil {
		return "", notFound(err)
	}
	return v, nil
}

func (s *metaStore) SetMeta(ctx context.Context, ns store.Namespace, key, value string) error {
	return s.upsertMeta(ctx, nsOf(ns), key, value)
}
