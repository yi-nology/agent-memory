package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

const rawCols = `id, host, session_id, thread_id, user_name, ts, type, content, payload, provenance`

func (s *rawStore) scanRaw(rows *sql.Rows) (*types.RawEvent, error) {
	var (
		ev      types.RawEvent
		tsVal   sql.NullTime
		payload string
		prov    provenanceFields
	)
	if err := rows.Scan(&ev.ID, &ev.Host, &ev.SessionID, &ev.ThreadID, &ev.User,
		&tsVal, &ev.Type, &ev.Content, &payload, provScan{&prov}); err != nil {
		return nil, err
	}
	ev.Timestamp = fromTSPtr(tsVal)
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

// provenanceFields 是 provenance 的中间形态。
type provenanceFields = struct {
	Origin       string
	Host         string
	RuntimeError bool
	Redacted     bool
	Tool         string
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

func parseMap(s string) map[string]any {
	if s == "" || s == "null" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func parseStrings(s string) []string {
	if s == "" || s == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func (s *rawStore) SaveRaw(ctx context.Context, ns store.Namespace, ev *types.RawEvent) error {
	if err := ev.Valid(); err != nil {
		return err
	}
	body := ftsBody(ev.Content, ev.User)
	_, err := s.exec(ctx, `INSERT INTO raw_events
		(namespace, id, host, session_id, thread_id, user_name, ts, type, content, payload, provenance,
		 body, body_tsv)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12, to_tsvector('simple', $12))
		ON CONFLICT(namespace, id) DO UPDATE SET
			host = excluded.host, session_id = excluded.session_id, thread_id = excluded.thread_id,
			user_name = excluded.user_name, ts = excluded.ts, type = excluded.type, content = excluded.content,
			payload = excluded.payload, provenance = excluded.provenance,
			body = excluded.body, body_tsv = excluded.body_tsv`,
		nsOf(ns), ev.ID, ev.Host, ev.SessionID, ev.ThreadID, ev.User,
		ts(ev.Timestamp), string(ev.Type), ev.Content, mapJSON(ev.Payload),
		provenanceJSON(provenanceOf(ev.Provenance)), body)
	return err
}

func (s *rawStore) SaveRawBatch(ctx context.Context, ns store.Namespace, evs []*types.RawEvent) error {
	for _, ev := range evs {
		if err := ev.Valid(); err != nil {
			return err
		}
	}
	// 注意用的是回调收到的 tx 而不是外层 s：外层 s.tx 为 nil，它的 exec 会走 db，
	// 于是「在一个还没提交的事务之外又开了独立语句」。
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
	rows, err := s.query(ctx, `SELECT `+rawCols+` FROM raw_events WHERE namespace = $1 AND id = $2`,
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
	args := []any{nsOf(ns)}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.query(ctx,
		`SELECT `+rawCols+` FROM raw_events WHERE namespace = $1 AND id IN (`+numberedPlaceholders(2, len(ids))+`)
		 ORDER BY ts DESC`,
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

func (s *rawStore) ListRaw(ctx context.Context, ns store.Namespace, before time.Time, p store.Page) ([]*types.RawEvent, error) {
	q := `SELECT ` + rawCols + ` FROM raw_events WHERE namespace = $1`
	args := []any{nsOf(ns)}
	if !before.IsZero() {
		q += ` AND ts < $2`
		args = append(args, ts(before))
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT $` + fmt.Sprint(len(args)+1) + ` OFFSET $` + fmt.Sprint(len(args)+2)
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

func (s *rawStore) SearchRaw(ctx context.Context, ns store.Namespace, query string, limit int) ([]*types.RawEvent, error) {
	ids, err := s.searchFTS(ctx, "raw_events", nsOf(ns), query, limit)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	// 全文检索只给标识与相关度，取回实体时**保留相关度顺序**，不重排。
	return s.rawByIDsOrdered(ctx, ns, ids)
}

func (s *rawStore) DeleteRawBefore(ctx context.Context, ns store.Namespace, before time.Time) (int, error) {
	res, err := s.exec(ctx, `DELETE FROM raw_events WHERE namespace = $1 AND ts < $2`,
		nsOf(ns), ts(before))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// 词元向量与正文在同一次写入里落，故删行即等于删索引——
	// 不存在 SQLite 那边「索引是派生物、漏清会留幽灵」的问题。
	return int(n), nil
}

func (s *rawStore) rawByIDsOrdered(ctx context.Context, ns store.Namespace, ids []string) ([]*types.RawEvent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	byID := map[string]*types.RawEvent{}
	args := []any{nsOf(ns)}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.query(ctx,
		`SELECT `+rawCols+` FROM raw_events WHERE namespace = $1 AND id IN (`+numberedPlaceholders(2, len(ids))+`)`,
		args...)
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

// searchFTS 走全文检索，返回按相关度排序的标识。
//
// 查询串**先在 Go 侧切成词元**（store.LexicalTokens），再用 plainto_tsquery 合取：
// 与 FTS5 的空格分词语义一致（隐式 AND），且任意用户输入都不会变成语法
// （词元里只有字母/数字/下划线/汉字）。
func (s *Store) searchFTS(ctx context.Context, table, namespace, query string, limit int) ([]string, error) {
	tokens := store.LexicalTokens(query)
	if len(tokens) == 0 {
		// 查询里没有可检索的词元（全是标点/空白）：不是错误，是「没得可查」。
		return nil, nil
	}
	rows, err := s.query(ctx,
		`SELECT id FROM `+table+`
		 WHERE namespace = $1 AND body_tsv @@ plainto_tsquery('simple', $2)
		 ORDER BY ts_rank(body_tsv, plainto_tsquery('simple', $2)) DESC LIMIT $3`,
		namespace, ftsQuery(tokens), limitOf(limit, 10))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
