package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/yi-nology/agent-memory/store"
)

// 本文件：词面索引的重建（backfill）路径。
//
// # 为什么必须有
//
// FTS 虚表是**派生物**：真源是 raw_events / candidates / atoms 三张表。
// 行与索引如今在同一个事务里落库（见各 Save 的实现），但失配仍可能来自：
//
//   - 事务化修复**之前**写下的存量行（行在而索引不在 → 永久静默漏命中，
//     且读取侧完全看不出来——症状只是「明明记过却搜不到」）；
//   - 库文件被拷贝、手工动过、或历史版本的缺陷留下的幽灵索引行
//     （搜得到但取不到）。
//
// 增量写只能保证「从今往后不再失配」，修不了已经失配的存量——
// 所以要有一条从真源**全量重建**的路。这也是可选能力
// store.FTSIndexRebuilder 在本后端的实现（PG 的词元向量与正文同行落库，
// 没有独立索引可失配，故不实现该能力）。
//
// # 为什么是「清空重建」而不是「按行补」
//
// 按行补只能修「少」（索引缺行），修不了「错」（幽灵行、切词口径过期的行）。
// 全量清空重建对两种失配形态都成立，且口径只有一份（见各 *_FTSBody 辅助）。
//
// # 停写建议
//
// 重建整段在一个事务里：中途失败整体回滚，不留半新半旧的索引。
// 但它要扫三张真源表，库大时是重操作——宿主应把它当**维护动作**（CLI 的
// rebuild-fts 命令）在低峰跑，而不是挂在召回热路径上。
func (s *Store) RebuildFTSIndex(ctx context.Context) (int64, error) {
	var rebuilt int64
	err := s.Transaction(ctx, func(ctx context.Context, tx store.Store) error {
		tsStore, ok := tx.(*Store)
		if !ok {
			return fmt.Errorf("事务内拿到的不是本后端实现，词面索引无法重建: %T", tx)
		}
		rebuilt = 0
		// 先清后建，三张虚表一起：任一步失败整体回滚。
		for _, table := range []string{"raw_fts", "candidate_fts", "atom_fts"} {
			if _, err := tsStore.exec(ctx, `DELETE FROM `+table); err != nil {
				return fmt.Errorf("清空 %s 失败: %w", table, err)
			}
		}
		// 真源行 → 索引正文。tsStore.tx 在事务回调里必非空
		// （Transaction 对嵌套调用复用外层事务，对新事务刚开好）。
		for _, step := range []struct {
			source string
			load   func(ctx context.Context, q rowQuerier) ([]ftsEntry, error)
			table  string
		}{
			{"raw_events", loadRawFTSEntries, "raw_fts"},
			{"candidates", loadCandidateFTSEntries, "candidate_fts"},
			{"atoms", loadAtomFTSEntries, "atom_fts"},
		} {
			entries, err := step.load(ctx, tsStore.tx)
			if err != nil {
				return fmt.Errorf("读真源表 %s 失败: %w", step.source, err)
			}
			for _, e := range entries {
				if err := tsStore.indexUpsert(ctx, step.table, e.namespace, e.ref, e.body); err != nil {
					return fmt.Errorf("重建 %s 索引（%s）失败: %w", step.table, e.ref, err)
				}
				rebuilt++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return rebuilt, nil
}

// ftsEntry 一条待重建的索引行。
type ftsEntry struct {
	namespace string
	ref       string
	body      string
}

// loadRawFTSEntries 读原始事件真源，算出各自的索引正文。
//
// 正文拼法与 SaveRaw 共用 rawFTSBody 的口径（此处直接内联同一对输入，
// 不绕 types.RawEvent——重建读的是行不是对象，口径由 ftsBody 一处保证）。
func loadRawFTSEntries(ctx context.Context, q rowQuerier) ([]ftsEntry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT namespace, id, content, user FROM raw_events`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ftsEntry
	for rows.Next() {
		var ns, id, content, user string
		if err := rows.Scan(&ns, &id, &content, &user); err != nil {
			return nil, err
		}
		out = append(out, ftsEntry{namespace: ns, ref: id, body: ftsBody(content, user)})
	}
	return out, rows.Err()
}

// loadCandidateFTSEntries 读候选真源，算出各自的索引正文
// （与 SaveCandidate / candidateFTSBody 同一口径）。
func loadCandidateFTSEntries(ctx context.Context, q rowQuerier) ([]ftsEntry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT namespace, id, title, assertion, subject_name FROM candidates`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ftsEntry
	for rows.Next() {
		var ns, id, title, assertion, subject string
		if err := rows.Scan(&ns, &id, &title, &assertion, &subject); err != nil {
			return nil, err
		}
		out = append(out, ftsEntry{namespace: ns, ref: id,
			body: ftsBody(title, assertion, subject)})
	}
	return out, rows.Err()
}

// loadAtomFTSEntries 读事实卡真源，算出各自的索引正文
// （与 SaveAtom / atomFTSBody 同一口径；检索词列是 JSON 串，
// 解析走与 SaveAtom 同一份 parseStrings）。
func loadAtomFTSEntries(ctx context.Context, q rowQuerier) ([]ftsEntry, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT namespace, id, assertion, verbatim_quote, search_terms FROM atoms`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ftsEntry
	for rows.Next() {
		var ns, id, assertion, quote, terms string
		if err := rows.Scan(&ns, &id, &assertion, &quote, &terms); err != nil {
			return nil, err
		}
		out = append(out, ftsEntry{namespace: ns, ref: id,
			body: ftsBody(assertion, quote, strings.Join(parseStrings(terms), " "))})
	}
	return out, rows.Err()
}
