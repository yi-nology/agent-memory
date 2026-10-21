package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var (
	errEmptyAlias        = errors.New("别名与实体 ID 都不能为空")
	errEmptyActiveEntity = errors.New("线程 ID 与实体 ID 都不能为空")
)

// itoa 是 strconv.Itoa 的本地小别名（占位符拼接处用得很多）。
func itoa(n int) string { return fmt.Sprint(n) }

// edgeStore 关系边存取（**派生物**）。
//
// 刻意单列一个窄结构而不是平铺在 Store 上：
// 仓里「每张表一个内嵌窄结构」那条纪律的兑现——
// 平铺会让 Store 的方法数往上走，而门禁的阈值就是按「一个类型几个方法」判的。
type edgeStore struct{ *Store }

// ── 实体页面 ──
//
// # 页面按归属视角分页
//
// 页面的逻辑主键是 (namespace, entity_id, scope)：同一实体在不同归属下各有一页。
// 事实卡已经按归属隔离，页面是事实的摘要——摘要若不分视角，
// 等于把别家的事实揉进一张人人可见的画像里，而画像是人读的一整段话，
// 读者分不清哪句属于自己。
//
// 下面各方法里的 scope 是**键的一部分**（页面视角），不是过滤面：
// 空 = 全局视角（这一页只汇总全局事实）。
// 它与 AtomFilter.Scope 的空值语义（空=不隔离）刻意不同，
// 理由见 types.EntityPage.Scope 的注释。

// pageCols 页面列。scope 也在里面：取出来的页面必须带着自己的视角，
// 否则调用方分不清手里是哪一页。
const pageCols = `entity_id, scope, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure`

func (s *pageStore) scanPage(rows *sql.Rows) (*types.EntityPage, error) {
	var (
		p       types.EntityPage
		atomIDs string
		updated sql.NullTime
		regen   sql.NullTime
	)
	if err := rows.Scan(&p.EntityID, &p.Scope, &p.Headline, &p.Body, &atomIDs, &p.Dirty,
		&updated, &regen, &p.RegenFailure); err != nil {
		return nil, err
	}
	p.AtomIDs = parseStrings(atomIDs)
	p.UpdatedAt = fromTSPtr(updated)
	p.LastRegenAt = fromTSPtr(regen)
	return &p, nil
}

// UpsertEntityPage 写入页面。归属取 p.Scope（页面自带的视角）。
func (s *pageStore) UpsertEntityPage(ctx context.Context, ns store.Namespace, p *types.EntityPage) error {
	if p.EntityID == "" {
		return errors.New("页面必须有归属实体")
	}
	return s.writePage(ctx, ns, p.EntityID, p.Scope, p.Headline, p.Body, p.AtomIDs, p.Dirty,
		ts(p.UpdatedAt), tsPtr(p.LastRegenAt), p.RegenFailure)
}

func (s *pageStore) writePage(ctx context.Context, ns store.Namespace, entityID string, scope types.Scope,
	headline, body string, atomIDs []string, dirty bool, updated any, regen any, failure string) error {
	_, err := s.exec(ctx, `INSERT INTO entity_pages
		(namespace, entity_id, scope, headline, body, atom_ids, dirty, updated_at, last_regen_at, regen_failure)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT(namespace, entity_id, scope) DO UPDATE SET
			headline = excluded.headline, body = excluded.body,
			atom_ids = excluded.atom_ids, dirty = excluded.dirty,
			updated_at = excluded.updated_at, last_regen_at = excluded.last_regen_at,
			regen_failure = excluded.regen_failure`,
		nsOf(ns), entityID, string(scope), headline, body, stringsJSON(atomIDs), dirty, updated, regen, failure)
	return err
}

// GetEntityPage 取 (namespace, entity_id, scope) 这一页。
//
// **精确取键**：别的视角的页面取不到（取不到就是取不到，不回退）。
// 回退策略归召回层管——它知道读者是谁；存储层回退等于把别家的画像
// 递到读者手里。
func (s *pageStore) GetEntityPage(ctx context.Context, ns store.Namespace, entityID string,
	scope types.Scope) (*types.EntityPage, error) {
	rows, err := s.query(ctx, `SELECT `+pageCols+` FROM entity_pages
		 WHERE namespace = $1 AND entity_id = $2 AND scope = $3`,
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
//
// scope 是精确视角：全局视角（空）只列全局那一页，不列各归属的页面。
func (s *pageStore) ListDirtyEntityPages(ctx context.Context, ns store.Namespace, scope types.Scope,
	limit int) ([]*types.EntityPage, error) {
	rows, err := s.query(ctx,
		`SELECT `+pageCols+` FROM entity_pages WHERE namespace = $1 AND scope = $2 AND dirty
		 ORDER BY updated_at ASC LIMIT $3`, nsOf(ns), string(scope), limitOf(limit, 50))
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
//
// **跨视角全列**：导包要的是这个命名空间的全部记忆，
// 只导一个视角等于导出时把别家的画像悄悄丢了（而导入方看不出来）。
func (s *Store) ListEntityPages(ctx context.Context, ns store.Namespace, limit int) ([]*types.EntityPage, error) {
	rows, err := s.query(ctx,
		`SELECT `+pageCols+` FROM entity_pages WHERE namespace = $1 ORDER BY entity_id, scope LIMIT $2`,
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
// scope 是**这条事实的归属**，两种情形的处置不同：
//
//   - scope 非空 → 只把该视角那一页标脏；
//   - scope 为空（全局事实）→ 把该实体的**所有视角**都标脏。
//
// 第二条的理由：全局事实出现在每个视角的画像里（它对谁都可见），
// 它一变，所有视角的摘要都过时了。只标全局那一页的话，
// 各归属的画像会停在旧版本，而看板上「页面存在且是旧的」
// 与「页面正常」长得一模一样。
//
// 页面不存在时**不报错而是建一条空脏页**：晋升先于首次生成是常态，
// 而「实体还没有页面」不该让晋升流程失败——那会把一个正常的写入变成错误。
func (s *pageStore) MarkEntityPageDirty(ctx context.Context, ns store.Namespace, entityID string,
	scope types.Scope, when time.Time) error {
	if when.IsZero() {
		when = time.Now().UTC()
	}
	q := `UPDATE entity_pages SET dirty = TRUE, updated_at = $1 WHERE namespace = $2 AND entity_id = $3`
	args := []any{ts(when), nsOf(ns), entityID}
	if scope != "" {
		q += ` AND scope = $4`
		args = append(args, string(scope))
	}
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// 一行都没有：该实体的页面还没建过（晋升先于首次生成）。
	return s.writePage(ctx, ns, entityID, scope, "", "", nil, true, ts(when), nil, "")
}

// ApplyEntityPageRegen 生成成功：落正文、清脏、清失败原因。
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
	return s.writePage(ctx, ns, entityID, scope, p.Headline, p.Body, p.AtomIDs, false,
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
		`UPDATE entity_pages SET dirty = TRUE, regen_failure = $1, last_regen_at = $2
		 WHERE namespace = $3 AND entity_id = $4 AND scope = $5`,
		reason, ts(at), nsOf(ns), entityID, string(scope))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	return s.writePage(ctx, ns, entityID, scope, "", "", nil, true, ts(at), ts(at), reason)
}

// ── 关系边 ──
//
// 边是**派生物**：真源是事实卡，投影丢了可以重投影，事实卡丢了投影救不回来。
// 故这一面只有「落边与读边」，没有任何改写事实的入口。
//
// # 为什么归属过滤必须在 SQL 里
//
// 边也参与召回的配额（多跳取边有 TopK），后置过滤的话，
// 被别家占掉的配额已经花掉了，于是「该给我的那条」根本没进候选池——
// 症状只是「边少了」，看着像召回不够，根因却在取数。

// edgeCols 关系边列。
const edgeCols = `id, from_entity_id, to_entity_id, relation, scope,
	valid_from, valid_to, weight, source_atom_id, created_at`

func (s *Store) scanEdge(rows *sql.Rows) (*types.EntityEdge, error) {
	var (
		e                  types.EntityEdge
		validFrom, validTo sql.NullTime
		created            sql.NullTime
	)
	if err := rows.Scan(&e.ID, &e.FromEntityID, &e.ToEntityID, &e.Relation, &e.Scope,
		&validFrom, &validTo, &e.Weight, &e.SourceAtomID, &created); err != nil {
		return nil, err
	}
	// 「未设」编码是 NULL，读出来就是零值时刻：
	// 归一（零值当 1 的权重、零值即未设的生效点）由 types.EntityEdge 兜，
	// 存储层不另抄一份判据。
	e.ValidFrom = fromTSPtr(validFrom)
	e.ValidTo = fromTSPtr(validTo)
	e.CreatedAt = fromTSPtr(created)
	return &e, nil
}

// UpsertEdge 写入或更新一条边（按标识幂等）。
func (s *edgeStore) UpsertEdge(ctx context.Context, ns store.Namespace, e *types.EntityEdge) error {
	if e.ID == "" {
		return errors.New("边必须有标识")
	}
	if e.FromEntityID == "" || e.ToEntityID == "" {
		return errors.New("边的两端都不能为空")
	}
	// upsert 而不是纯 INSERT：投影层重算一条边时给的是**新值**，
	// 而边没有「合并」语义——同一条边的两端、关系名、归属、生效区间、
	// 权重都以最后一次写入为准。
	//
	// created_at 不在更新列表里：它是这条边**首次**落库的时刻，
	// 重投影不改变「这条边存在多久了」。
	_, err := s.exec(ctx, `INSERT INTO entity_edges
		(namespace, id, from_entity_id, to_entity_id, relation, scope,
		 valid_from, valid_to, weight, source_atom_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT(namespace, id) DO UPDATE SET
			from_entity_id = excluded.from_entity_id, to_entity_id = excluded.to_entity_id,
			relation = excluded.relation, scope = excluded.scope,
			valid_from = excluded.valid_from, valid_to = excluded.valid_to,
			weight = excluded.weight, source_atom_id = excluded.source_atom_id`,
		nsOf(ns), e.ID, e.FromEntityID, e.ToEntityID, e.Relation, string(e.Scope),
		ts(e.ValidFrom), tsPtr(e.ValidTo), e.Weight, e.SourceAtomID, ts(e.CreatedAt))
	return err
}

// GetEdge 取一条边。取不到返回 store.ErrNotFound。
func (s *edgeStore) GetEdge(ctx context.Context, ns store.Namespace, id string) (*types.EntityEdge, error) {
	rows, err := s.query(ctx,
		`SELECT `+edgeCols+` FROM entity_edges WHERE namespace = $1 AND id = $2`, nsOf(ns), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanEdge(rows)
}

// ListEdges 按条件列边（多跳检索的取数面）。
//
// 末尾的 id ASC 是**并列时的稳定序**：同一时刻建的边不止一条时，
// 没有它时返回顺序不确定——而多跳遍历的顺序会跟着变，
// 于是「同样的一批边，每次走出来的路径不一样」。
func (s *edgeStore) ListEdges(ctx context.Context, ns store.Namespace, f store.EdgeFilter) ([]*types.EntityEdge, error) {
	where := []string{"namespace = $1"}
	args := []any{nsOf(ns)}
	cond, condArgs := edgeConditions(f, len(args)+1)
	if cond != "" {
		where = append(where, cond)
		args = append(args, condArgs...)
	}
	q := `SELECT ` + edgeCols + ` FROM entity_edges WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY created_at ASC, id ASC LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limitOf(f.Limit, 100))

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.EntityEdge
	for rows.Next() {
		e, err := s.scanEdge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// edgeConditions 关系边的过滤条件（SQL 片段 + 参数）。
//
// 各切片空=不限；归属非空=只取「全局 + 同归属」；
// 时刻非零=只取该时刻成立的边。
//
// start 是第一个占位符的编号（$1 是 namespace，故从 $2 起）。
func edgeConditions(f store.EdgeFilter, start int) (string, []any) {
	var (
		where = []string{}
		args  = []any{}
	)
	if len(f.FromEntityIDs) > 0 {
		where = append(where, "from_entity_id IN ("+numberedPlaceholders(start, len(f.FromEntityIDs))+")")
		for _, id := range f.FromEntityIDs {
			args = append(args, id)
		}
		start += len(f.FromEntityIDs)
	}
	if len(f.ToEntityIDs) > 0 {
		where = append(where, "to_entity_id IN ("+numberedPlaceholders(start, len(f.ToEntityIDs))+")")
		for _, id := range f.ToEntityIDs {
			args = append(args, id)
		}
		start += len(f.ToEntityIDs)
	}
	if len(f.Relations) > 0 {
		where = append(where, "relation IN ("+numberedPlaceholders(start, len(f.Relations))+")")
		for _, r := range f.Relations {
			args = append(args, r)
		}
		start += len(f.Relations)
	}
	if f.Scope != "" {
		// 归属条件：**全局的 + 同归属的**，别家的不给。
		// 「归属为空=不过滤」这条语义在 store.EdgeFilter.Scope 的注释里，
		// 这里是它的可执行形态。
		where = append(where, "(scope = '' OR scope = $"+fmt.Sprint(start)+")")
		args = append(args, string(f.Scope))
		start++
	}
	if !f.At.IsZero() {
		// 生效判据：区间含 At，**左闭右开**——
		// At 恰等于失效时刻时已不成立（那正是「到这一刻为止有效」）。
		//
		// valid_from 的 NULL 是「未设」：边是投影物，它**存在**的时刻
		// （created_at）就是它生效的时刻，故退回 created_at。
		where = append(where,
			"(COALESCE(valid_from, created_at) IS NULL OR COALESCE(valid_from, created_at) <= $"+fmt.Sprint(start)+")")
		args = append(args, ts(f.At))
		start++
		where = append(where, "(valid_to IS NULL OR $"+fmt.Sprint(start)+" < valid_to)")
		args = append(args, ts(f.At))
		start++
	}
	return strings.Join(where, " AND "), args
}

// DeleteEdge 删一条边。
//
// 删不到不报错：删投影是清理动作，同一条边被清两次是常态
// （事实卡被推翻两次、清理流程重跑），报错只会让清理路径变脆。
func (s *edgeStore) DeleteEdge(ctx context.Context, ns store.Namespace, id string) error {
	_, err := s.exec(ctx, `DELETE FROM entity_edges WHERE namespace = $1 AND id = $2`, nsOf(ns), id)
	return err
}

// DeleteEdgesBySource 删掉由某张事实卡投影出来的全部边。
//
// 事实卡被推翻时用它清投影：留着旧边等于「关系还在」，
// 而事实已经不成立了——那是一张说谎的图。
func (s *edgeStore) DeleteEdgesBySource(ctx context.Context, ns store.Namespace, sourceAtomID string) error {
	if sourceAtomID == "" {
		// 空来源会命中「没有溯源的边」（source_atom_id 为空的那批），
		// 一次清理就把它们全删了——那是不可逆的数据损失，故响亮失败。
		return errors.New("来源事实卡 ID 不能为空：空来源会误删没有溯源的边")
	}
	_, err := s.exec(ctx,
		`DELETE FROM entity_edges WHERE namespace = $1 AND source_atom_id = $2`, nsOf(ns), sourceAtomID)
	return err
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

// ── 向量索引 ──
//
// 向量是**派生物**：真源是事实卡（向量是事实的函数），
// 索引丢了可以从事实卡重算，事实卡丢了索引救不回来。
// 故这一面只有「落向量与读向量」，没有任何改写事实的入口。

// vectorStore 向量索引存取（**派生物**）。
//
// 刻意单列一个窄结构而不是平铺在 Store 上：
// 仓里「每张表一个内嵌窄结构」那条纪律的兑现——
// 平铺会让 Store 的方法数往上走，而门禁的阈值就是按「一个类型几个方法」判的。
type vectorStore struct{ *Store }

// Upsert 写入或更新一条向量（scope 是这条向量的归属）。
func (s *vectorStore) Upsert(ctx context.Context, namespace, id, scope string, vec []float32) error {
	if id == "" {
		return errors.New("向量标识不能为空")
	}
	if len(vec) == 0 {
		return errors.New("向量不能为空")
	}
	// 同一个命名空间只许一种维度：维度不同的两条向量没法比余弦，
	// 而「比不出来」若被静默吞掉，症状是「语义召回像是没生效」。
	if err := s.checkVecDim(ctx, namespace, len(vec)); err != nil {
		return err
	}
	_, err := s.exec(ctx, `INSERT INTO atom_vectors (namespace, id, scope, dim, vec, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT(namespace, id) DO UPDATE SET
			scope = excluded.scope, dim = excluded.dim, vec = excluded.vec,
			updated_at = excluded.updated_at`,
		namespace, id, scope, len(vec), encodeVec(vec), ts(time.Now().UTC()))
	return err
}

// checkVecDim 校验命名空间里的维度与给定维度一致。
//
// 维度不一致时报 ports.ErrDimensionMismatch 而不是各算各的：
// 静默「比不出来」的失效形态是「语义召回永远空」，而调用方看不出是维度错了。
func (s *vectorStore) checkVecDim(ctx context.Context, namespace string, dim int) error {
	rows, err := s.query(ctx, `SELECT DISTINCT dim FROM atom_vectors WHERE namespace = $1`, namespace)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d int
		if err := rows.Scan(&d); err != nil {
			return err
		}
		if d != dim {
			return ports.ErrDimensionMismatch
		}
	}
	return rows.Err()
}

// Search 返回最相似的若干条。
//
// # 归属过滤**进 SQL**
//
// scope 非空时取「全局的 + 同归属的」，别家的一条都不进候选池。
// 放在调用方后置的话，被别家占掉的 topK 槽位已经花掉了——
// 于是「该给我的那条」根本没进候选池，而症状只是「语义召回像是没生效」。
// scope 为空 = **不隔离**（与 store.AtomFilter.Scope 同一条语义）。
//
// # 暴力扫描的适用面
//
// 这是「把该命名空间的向量取回来、在 Go 里算余弦」的暴力实现：
// 它是**小库够用**的那一档——零外部依赖、跟着记忆库一起活（跨进程可用）。
// 大规模部署的宿主应注入自己的向量库（Chroma / Qdrant / PG 的向量扩展……），
// 本仓不替宿主选向量技术（核心不引向量库 SDK，见 architecture-policy.yaml）。
func (s *vectorStore) Search(ctx context.Context, namespace, scope string, vec []float32,
	topK int) ([]ports.VectorHit, error) {
	if len(vec) == 0 {
		return nil, errors.New("查询向量不能为空")
	}
	// 维度先对齐：库里是 3 维、查询给 4 维时**必须报错**，
	// 静默返回空集的失效形态是「语义召回永远空」，而调用方看不出是维度错了。
	if err := s.checkVecDim(ctx, namespace, len(vec)); err != nil {
		return nil, err
	}
	q := `SELECT id, vec FROM atom_vectors WHERE namespace = $1`
	args := []any{namespace}
	if scope != "" {
		q += ` AND (scope = '' OR scope = $2)`
		args = append(args, scope)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hits []ports.VectorHit
	for rows.Next() {
		var (
			id  string
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		score, ok := cosineScore(decodeVec(raw), vec)
		if !ok {
			// 零向量没有方向，无从比较（跳过，不当命中）。
			continue
		}
		hits = append(hits, ports.VectorHit{ID: id, Score: float32(score)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortHits(hits)
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits, nil
}

// sortHits 按分数倒序排；同分按标识升序。
//
// 同分时的标识序是**可复现性**的一部分：没有它，同样的查询每次返回的
// 次序可能不同，而融合层按名次算分——于是「召回结果不可复现」。
func sortHits(hits []ports.VectorHit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
}

// Delete 删一条向量。
//
// 删不到不报错：删索引是清理动作，同一条被清两次是常态
// （事实卡被推翻两次、清理流程重跑），报错只会让清理路径变脆。
func (s *vectorStore) Delete(ctx context.Context, namespace, id string) error {
	_, err := s.exec(ctx, `DELETE FROM atom_vectors WHERE namespace = $1 AND id = $2`,
		namespace, id)
	return err
}
