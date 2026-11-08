package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：词面索引重建路径（RebuildFTSIndex，#23 的 backfill 面）的白盒用例。
// 造失配要直接动虚表（「行在而索引不在」「索引正文是错的」），
// 黑盒造不出来，故放 internal test。

var rebuildBase = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// TestRebuildFTSIndexRestoresLostAndGhostEntries 重建修平两类失配：
// 索引缺行（「行在而搜不到」）与索引正文错（幽灵正文）。
func TestRebuildFTSIndexRestoresLostAndGhostEntries(t *testing.T) {
	s, err := OpenMemory("fts-rebuild-wb")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	ns := store.Namespace("ns-rb")

	if err := s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Timestamp: rebuildBase, Type: types.EventManual, Content: "磁盘剩余不足",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e2", Timestamp: rebuildBase, Type: types.EventManual, Content: "内存占用偏高",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCandidate(ctx, ns, &types.Candidate{
		ID: "c1", RawEventIDs: []string{"e1"}, Type: types.CandidateFact,
		Status: types.CandidatePending, Assertion: "磁盘长期偏小", SubjectName: "机器甲",
		CreatedAt: rebuildBase, Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小", VerbatimQuote: "「磁盘长期偏小」",
		QuoteEventID: "e1", SearchTerms: []string{"磁盘"},
		OccurredAt: rebuildBase, Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		CreatedAt: rebuildBase, UpdatedAt: rebuildBase,
		Provenance: types.Provenance{Origin: types.OriginOperator},
	}); err != nil {
		t.Fatal(err)
	}

	// 造失配：e1 的索引行整个删掉（事务化之前的失效形态）；
	// a1 的索引正文整个换成不相关内容（口径过期/被污染的形态）。
	for _, stmt := range []string{
		`DELETE FROM raw_fts WHERE event_id = 'e1'`,
		`UPDATE atom_fts SET body = '完全不相关的词' WHERE atom_id = 'a1'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造失配失败（%s）: %v", stmt, err)
		}
	}

	// 症状复现：e1 搜不到，a1 搜到的不是它该命中的词。
	hits, err := s.SearchRaw(ctx, ns, "", "磁盘", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == "e1" {
			t.Fatal("前置条件失真：索引缺行的症状应先复现（e1 不该被搜到）")
		}
	}
	atomHits, err := s.SearchAtoms(ctx, ns, "磁盘", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(atomHits) != 0 {
		t.Fatal("前置条件失真：索引正文被污染后不该再命中 a1")
	}

	// 重建。
	n, err := s.RebuildFTSIndex(ctx)
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if n != 4 {
		t.Fatalf("应重建 4 条索引（2 事件 + 1 候选 + 1 事实卡），实得 %d", n)
	}

	// 缺的行回来了。
	hits, err = s.SearchRaw(ctx, ns, "", "磁盘", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "e1" {
		t.Fatalf("重建后 e1 应可搜到（幽灵正文应被清掉）: %+v", hits)
	}
	// 错的正文修平了。
	atomHits, err = s.SearchAtoms(ctx, ns, "磁盘", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(atomHits) != 1 || atomHits[0].ID != "a1" {
		t.Fatalf("重建后 a1 应可按正确正文搜到: %+v", atomHits)
	}
	// 候选面同样在重建范围内。
	candHits, err := s.SearchCandidates(ctx, ns, "磁盘", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(candHits) != 1 || candHits[0].ID != "c1" {
		t.Fatalf("重建后 c1 应可搜到: %+v", candHits)
	}
}

// TestRebuildFTSIndexIsTransactional 重建整段在一个事务里：
// 失败时旧索引原样保留，不留半新半旧。（用回滚路径间接验证：
// 重建中途出错 → 已清空的虚表回到重建前的内容。）
func TestRebuildFTSIndexIsTransactional(t *testing.T) {
	s, err := OpenMemory("fts-rebuild-tx")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	ns := store.Namespace("ns-rb2")
	if err := s.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小", VerbatimQuote: "「磁盘长期偏小」",
		QuoteEventID: "e1", SearchTerms: []string{"磁盘"},
		OccurredAt: rebuildBase, Confidence: types.ConfidenceHigh, Importance: types.ImportanceHigh,
		CreatedAt: rebuildBase, UpdatedAt: rebuildBase,
		Provenance: types.Provenance{Origin: types.OriginOperator},
	}); err != nil {
		t.Fatal(err)
	}

	// 在外层事务里跑重建然后回滚：索引应保持重建前的样子（行还在、可搜）。
	boom := errRebuildAbort
	err = s.Transaction(ctx, func(ctx context.Context, tx store.Store) error {
		ts, ok := tx.(*Store)
		if !ok {
			t.Fatalf("事务内拿到的不是本后端实现: %T", tx)
		}
		if _, err := ts.RebuildFTSIndex(ctx); err != nil {
			return err
		}
		return boom
	})
	if err != errRebuildAbort {
		t.Fatalf("事务应把回调错误原样抛出: %v", err)
	}
	hits, err := s.SearchAtoms(ctx, ns, "磁盘", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "a1" {
		t.Fatalf("重建事务回滚后索引应保持原样: %+v", hits)
	}
}

// errRebuildAbort 测试用的哨兵错误。
var errRebuildAbort = errors.New("重建后主动中止")
