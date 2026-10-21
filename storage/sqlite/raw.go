package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

const rawCols = `id, host, session_id, thread_id, user, scope, ts, type, content, payload, provenance`

func (s *rawStore) scanRaw(rows *sql.Rows) (*types.RawEvent, error) {
	var (
		ev      types.RawEvent
		tsVal   int64
		payload string
		prov    provenanceFields
	)
	if err := rows.Scan(&ev.ID, &ev.Host, &ev.SessionID, &ev.ThreadID, &ev.User, &ev.Scope,
		&tsVal, &ev.Type, &ev.Content, &payload, provScan{&prov}); err != nil {
		return nil, err
	}
	ev.Timestamp = fromTS(tsVal)
	ev.Payload = parseMap(payload)
	ev.Provenance = types.Provenance{
		Origin:       types.OriginKind(prov.Origin),
		Host:         prov.Host,
		RuntimeError: prov.RuntimeError,
		Redacted:     prov.Redacted,
		Tool:         prov.Tool,
	}
	return &ev, nil
}

// provScan 让 sql.Scanner 直接吃 provenance 列（存的是 JSON 串）。
//
// 解析失败返回 nil 而不是 error：一行坏 JSON 不该让整次召回失败——
// 那条事件的来源信息读不到，按「无来源」处理即可（Capturable 仍为真，
// 判据看的是 RuntimeError 字段，读不到时它是零值 false，与「没有运行期错误」
// 语义一致）。
type provScan struct{ p *provenanceFields }

func (v provScan) Scan(src any) error {
	s, _ := src.(string)
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	v.p.Origin, _ = m["origin"].(string)
	v.p.Host, _ = m["host"].(string)
	v.p.Tool, _ = m["tool"].(string)
	v.p.RuntimeError, _ = m["runtime_error"].(bool)
	v.p.Redacted, _ = m["redacted"].(bool)
	return nil
}

func provenanceOf(p types.Provenance) provenanceFields {
	return provenanceFields{
		Origin:       string(p.Origin),
		Host:         p.Host,
		RuntimeError: p.RuntimeError,
		Redacted:     p.Redacted,
		Tool:         p.Tool,
	}
}

func (s *rawStore) SaveRaw(ctx context.Context, ns store.Namespace, ev *types.RawEvent) error {
	if err := ev.Valid(); err != nil {
		return err
	}
	if _, err := s.exec(ctx, `INSERT INTO raw_events
		(namespace, id, host, session_id, thread_id, user, scope, ts, type, content, payload, provenance)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		nsOf(ns), ev.ID, ev.Host, ev.SessionID, ev.ThreadID, ev.User, string(ev.Scope),
		ts(ev.Timestamp), string(ev.Type), ev.Content, mapJSON(ev.Payload),
		provenanceJSON(provenanceOf(ev.Provenance)),
	); err != nil {
		return err
	}
	return s.indexUpsert(ctx, "raw_fts", nsOf(ns), ev.ID, ftsBody(ev.Content, ev.User))
}

func (s *rawStore) SaveRawBatch(ctx context.Context, ns store.Namespace, evs []*types.RawEvent) error {
	for _, ev := range evs {
		if err := ev.Valid(); err != nil {
			return err
		}
	}
	// 注意用的是回调收到的 tx 而不是外层 s：外层 s.tx 为 nil，它的 exec 会走 db，
	// 于是「在一个还没提交的事务之外又开了独立语句」。连接池只有一条连接时这会
	// 直接死锁（事务持着唯一连接，外层语句等不到），连接池宽裕时它更糟——
	// 静默地写到事务外，原子性没了且**不报任何错**。
	return s.Transaction(ctx, func(ctx context.Context, tx store.Store) error {
		for _, ev := range evs {
			if err := tx.SaveRaw(ctx, ns, ev); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *rawStore) GetRaw(ctx context.Context, ns store.Namespace, id string) (*types.RawEvent, error) {
	rows, err := s.query(ctx, `SELECT `+rawCols+` FROM raw_events WHERE namespace = ? AND id = ?`,
		nsOf(ns), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, absentOr(rows.Err())
	}
	return s.scanRaw(rows)
}

func (s *rawStore) GetRawByIDs(ctx context.Context, ns store.Namespace, ids []string) ([]*types.RawEvent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph := placeholders(len(ids))
	args := []any{nsOf(ns)}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.query(ctx,
		`SELECT `+rawCols+` FROM raw_events WHERE namespace = ? AND id IN (`+ph+`) ORDER BY ts DESC`,
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.RawEvent
	for rows.Next() {
		ev, err := s.scanRaw(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *rawStore) ListRaw(ctx context.Context, ns store.Namespace, scope types.Scope,
	before time.Time, p store.Page) ([]*types.RawEvent, error) {
	q := `SELECT ` + rawCols + ` FROM raw_events WHERE namespace = ?`
	args := []any{nsOf(ns)}
	if scope != "" {
		// 归属条件：**全局的 + 同归属的**，别家的不给。
		// 语义见 store.AtomFilter.Scope。
		q += ` AND (scope = '' OR scope = ?)`
		args = append(args, string(scope))
	}
	if !before.IsZero() {
		q += ` AND ts < ?`
		args = append(args, ts(before))
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limitOf(p.Limit, 50), p.Offset)
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*types.RawEvent
	for rows.Next() {
		ev, err := s.scanRaw(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *rawStore) SearchRaw(ctx context.Context, ns store.Namespace, scope types.Scope,
	query string, limit int) ([]*types.RawEvent, error) {
	extra := ""
	var extraArgs []any
	if scope != "" {
		// 归属条件进 SQL，与 LIMIT 同一次求值（理由见 ListRaw 同处）。
		extra = "event_id IN (SELECT id FROM raw_events WHERE namespace = ? AND (scope = '' OR scope = ?))"
		extraArgs = []any{nsOf(ns), string(scope)}
	}
	ids, err := s.searchFTS(ctx, "raw_fts", nsOf(ns), query, limit, extra, false, extraArgs...)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	// FTS 只给标识与相关度，取回实体时**保留相关度顺序**，不重排。
	return s.rawByIDsOrdered(ctx, ns, ids)
}

func (s *rawStore) DeleteRawBefore(ctx context.Context, ns store.Namespace, before time.Time) (int, error) {
	res, err := s.exec(ctx, `DELETE FROM raw_events WHERE namespace = ? AND ts < ?`,
		nsOf(ns), ts(before))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// 索引是内容派生物：漏清会留下「搜得到但取不到」的幽灵结果，
	// 而那种不一致在排查时极难看出方向。
	//
	// 按「孤儿」清而不是按同一时间窗清：后者的条件一旦与上面的 DELETE 有任何
	// 细微差异（例如 ts 精度、时区归一），索引就会留下指向已删事件的残行——
	// 而那种 bug 表现为「搜到了但打开是空的」，不报任何错。
	if _, err := s.exec(ctx, `DELETE FROM raw_fts WHERE namespace = ?
		AND event_id NOT IN (SELECT id FROM raw_events WHERE namespace = ?)`,
		nsOf(ns), nsOf(ns)); err != nil {
		return int(n), err
	}
	return int(n), nil
}

func (s *rawStore) rawByIDsOrdered(ctx context.Context, ns store.Namespace, ids []string) ([]*types.RawEvent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	byID := map[string]*types.RawEvent{}
	ph := placeholders(len(ids))
	args := []any{nsOf(ns)}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.query(ctx,
		`SELECT `+rawCols+` FROM raw_events WHERE namespace = ? AND id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		ev, err := s.scanRaw(rows)
		if err != nil {
			return nil, err
		}
		byID[ev.ID] = ev
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]*types.RawEvent, 0, len(ids))
	for _, id := range ids {
		if ev, ok := byID[id]; ok {
			out = append(out, ev)
		}
	}
	return out, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
