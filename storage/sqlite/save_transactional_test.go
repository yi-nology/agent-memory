package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：行与 FTS 索引同事务落库（#23）的用例。
//
// SaveAtom / SaveRaw / SaveCandidate 的「行 + 索引」封装在内部 Transaction 里：
// 事务回滚时两样一起消失；事务外直呼时两样一起出现。修复前直呼路径是
// 三个独立 autocommit——行 INSERT 与索引 DELETE/INSERT 之间任何一步崩溃
// 或断连，真源表与索引就永久失配（行在而搜不到，静默漏命中）。

var saveTxBase = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func saveTxAtom(id string) *types.AtomCard {
	return &types.AtomCard{
		ID: id, EntityID: "e1", Assertion: "磁盘长期偏小", VerbatimQuote: "「磁盘长期偏小」",
		QuoteEventID: "ev1", SearchTerms: []string{"磁盘"},
		OccurredAt: saveTxBase, Confidence: types.ConfidenceHigh,
		Importance: types.ImportanceHigh, CreatedAt: saveTxBase, UpdatedAt: saveTxBase,
		Provenance: types.Provenance{Origin: types.OriginOperator},
	}
}

// TestSaveRollbackLeavesNoRowNorIndex 事务回滚必须把行与索引**一起**回滚：
// 行不在、检索也不命中——「行在而搜不到」的失配形态不允许出现。
func TestSaveRollbackLeavesNoRowNorIndex(t *testing.T) {
	s := openFresh(t)
	ns := store.Namespace("ns-tx")
	ctx := context.Background()
	boom := errors.New("中途失败")

	err := s.Transaction(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.SaveRaw(ctx, ns, &types.RawEvent{
			ID: "ev-tx", Timestamp: saveTxBase, Type: types.EventManual, Content: "磁盘长期偏小",
		}); err != nil {
			return err
		}
		if err := tx.SaveCandidate(ctx, ns, &types.Candidate{
			ID: "c-tx", RawEventIDs: []string{"ev-tx"}, Type: types.CandidateFact,
			Status: types.CandidatePending, Assertion: "磁盘长期偏小", SubjectName: "机器甲",
			CreatedAt: saveTxBase, Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		}); err != nil {
			return err
		}
		if err := tx.SaveAtom(ctx, ns, saveTxAtom("a-tx")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("事务应把回调错误原样抛出: %v", err)
	}

	// 行不存在……
	if _, err := s.GetRaw(ctx, ns, "ev-tx"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("raw 的行应随事务回滚消失: %v", err)
	}
	if _, err := s.GetCandidate(ctx, ns, "c-tx"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("candidate 的行应随事务回滚消失: %v", err)
	}
	if _, err := s.GetAtom(ctx, ns, "a-tx"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("atom 的行应随事务回滚消失: %v", err)
	}

	// ……索引也不在（三条检索都不得命中）。
	rawHits, err := s.SearchRaw(ctx, ns, "", "磁盘", 5)
	if err != nil {
		t.Fatal(err)
	}
	candHits, err := s.SearchCandidates(ctx, ns, "磁盘", 5)
	if err != nil {
		t.Fatal(err)
	}
	atomHits, err := s.SearchAtoms(ctx, ns, "磁盘", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(rawHits)+len(candHits)+len(atomHits) != 0 {
		t.Fatalf("索引应随事务回滚消失，仍有命中: raw=%d cand=%d atom=%d",
			len(rawHits), len(candHits), len(atomHits))
	}

	// 正常提交路径：行与索引一起可见。
	if err := s.SaveAtom(ctx, ns, saveTxAtom("a-direct")); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchAtoms(ctx, ns, "磁盘", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "a-direct" {
		t.Fatalf("直呼路径的行与索引应一起落库: %+v", hits)
	}
}
