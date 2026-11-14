package parity

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：分页取全（store.PagedLister 可选能力）的跨后端共享行为断言。
//
// # 为什么不挂在 Run 上
//
// 分页取全不是 store.Store 的一翼：它是 store/optional.go 里的**可选能力**
// （issue #61）——四个取数签名在 store.* 冻结面上放不下 offset，
// 翻页参数落在独立的小接口里。故与 RunVector 同款：单独入口，
// 各后端在自己的测试文件里调用本函数，断言本体只有**这一份**。
//
// # 这一组断言守的是什么
//
// 分页取全是导出的**前提**（缺了它导出拿不出完整集合）：翻页必须
// **不重不漏**、与单次全量取**同序**、末页**恰好取尽即停**。
// 任何一条破了，导出的包就静默残缺——正是 #61 要根除的失效形态。

// RunPaged 跑分页取全这一组共享断言。
//
// open 每个用例给一份**干净的**后端（各后端自己开）。
// 后端实现了分页取全却不接这套断言 = 「两个后端翻页语义一致」这句话没人验证。
func RunPaged(t *testing.T, open OpenFn) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, store.PagedLister, store.Store)
	}{
		{"分页取全：跨页取尽不重不漏", tPagedExhaustion},
		{"分页取全：offset 越界返回空集", tPagedOffsetBeyondEnd},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			paged, ok := store.PagedListerOf(s)
			if !ok {
				t.Fatalf("%T 未实现分页取全面（导出面的前提，见 store/optional.go）", s)
			}
			tc.fn(t, paged, s)
		})
	}
}

// tPagedExhaustion 小页宽翻页取尽：页的拼接与单次全量取**逐条同序**。
//
// 7 条 / 页宽 3 ⇒ 0/3/6 三页（末页 1 条 < 页宽即停），拼接结果必须与
// 大 Limit 单次取完全一致——翻页重行或漏行在这里当场红。
func tPagedExhaustion(t *testing.T, paged store.PagedLister, s store.Store) {
	ctx := bg()
	const n = 7
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := s.SaveEntity(ctx, nsA, &types.Entity{
			ID:            fmtID("ent", i),
			EntityType:    types.EntityType("service"),
			CanonicalName: fmtName("机器", i),
			CreatedAt:     at,
		}); err != nil {
			t.Fatalf("写实体(%d): %v", i, err)
		}
		if err := s.SaveCandidate(ctx, nsA, &types.Candidate{
			ID:          fmtID("cand", i),
			RawEventIDs: []string{fmtID("ev", i)},
			Type:        types.CandidateType("fact"),
			Status:      types.CandidatePending,
			Title:       fmtName("候选", i),
			CreatedAt:   at,
		}); err != nil {
			t.Fatalf("写候选(%d): %v", i, err)
		}
		if err := s.UpsertEntityPage(ctx, nsA, &types.EntityPage{
			EntityID: fmtID("ent", i), Headline: fmtName("画像", i), UpdatedAt: at,
		}); err != nil {
			t.Fatalf("写页面(%d): %v", i, err)
		}
		if err := s.AppendJournal(ctx, nsA, &types.JournalEntry{
			ID: fmtID("jr", i), At: at, Kind: types.JournalPromote,
			Subject: fmtID("cand", i), Actor: types.DecidedRule,
			Summary: fmtName("晋升", i),
		}); err != nil {
			t.Fatalf("写审计(%d): %v", i, err)
		}
	}

	const pageWidth = 3
	// 全量参照：一次大 Limit（7 条远小于后端缺省上限，参照自身不碰截断）。
	wantCands, err := s.ListCandidates(ctx, nsA, store.CandidateFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(wantCands) != n {
		t.Fatalf("参照取回应有 %d 条候选，实得 %d", n, len(wantCands))
	}

	// candidates / entities / pages / journal 四张表逐一翻页取尽、逐条比对。
	var gotCands []*types.Candidate
	for offset := 0; ; offset += pageWidth {
		batch, err := paged.ListCandidatesPaged(ctx, nsA, store.CandidateFilter{}, offset, pageWidth)
		if err != nil {
			t.Fatal(err)
		}
		gotCands = append(gotCands, batch...)
		if len(batch) < pageWidth {
			break
		}
	}
	if len(gotCands) != n {
		t.Fatalf("翻页取尽候选应得 %d 条，实得 %d", n, len(gotCands))
	}
	for i := range wantCands {
		if wantCands[i].ID != gotCands[i].ID {
			t.Fatalf("候选翻页拼接与全量取不同序：第 %d 条 应 %s / 实 %s", i, wantCands[i].ID, gotCands[i].ID)
		}
	}

	wantEnts, err := s.ListEntities(ctx, nsA, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var gotEnts []*types.Entity
	for offset := 0; ; offset += pageWidth {
		batch, err := paged.ListEntitiesPaged(ctx, nsA, "", offset, pageWidth)
		if err != nil {
			t.Fatal(err)
		}
		gotEnts = append(gotEnts, batch...)
		if len(batch) < pageWidth {
			break
		}
	}
	if len(gotEnts) != n || !sameIDOrder(entityIDs(wantEnts), entityIDs(gotEnts)) {
		t.Fatalf("实体翻页取尽与全量取应逐条同序：%d 条 vs %d 条", len(wantEnts), len(gotEnts))
	}

	var gotPages []*types.EntityPage
	for offset := 0; ; offset += pageWidth {
		batch, err := paged.ListEntityPagesPaged(ctx, nsA, offset, pageWidth)
		if err != nil {
			t.Fatal(err)
		}
		gotPages = append(gotPages, batch...)
		if len(batch) < pageWidth {
			break
		}
	}
	if len(gotPages) != n {
		t.Fatalf("页面翻页取尽应得 %d 条，实得 %d", n, len(gotPages))
	}

	var gotJrnl []*types.JournalEntry
	for offset := 0; ; offset += pageWidth {
		batch, err := paged.ListJournalPaged(ctx, nsA, "", offset, pageWidth)
		if err != nil {
			t.Fatal(err)
		}
		gotJrnl = append(gotJrnl, batch...)
		if len(batch) < pageWidth {
			break
		}
	}
	if len(gotJrnl) != n {
		t.Fatalf("审计翻页取尽应得 %d 条，实得 %d", n, len(gotJrnl))
	}
	for i := range gotJrnl {
		if got := slices.IndexFunc(gotJrnl, func(e *types.JournalEntry) bool { return e.ID == fmtID("jr", i) }); got < 0 {
			t.Fatalf("审计翻页丢了第 %d 条", i)
		}
	}
}

// tPagedOffsetBeyondEnd offset 越过末尾：返回空集（不是错误）。
//
// 翻页循环靠「本页不满一页」停机，越界页的形状是实现取尽判据的一部分。
func tPagedOffsetBeyondEnd(t *testing.T, paged store.PagedLister, _ store.Store) {
	ctx := bg()
	batch, err := paged.ListCandidatesPaged(ctx, nsA, store.CandidateFilter{}, 1000, 10)
	if err != nil {
		t.Fatalf("offset 越界不该报错: %v", err)
	}
	if len(batch) != 0 {
		t.Fatalf("offset 越界应返回空集，实得 %d 条", len(batch))
	}
}

func entityIDs(es []*types.Entity) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

// sameIDOrder 两个标识序列**按序逐条相同**。
//
// ⚠️ 与 parity_edge 的 sameIDs（不看顺序）不同名不同义：翻页取全断的
// 恰恰是「页的拼接与单次全量取**同序**」——offset 切的是同一份取数序，
// 序不一致就是翻页错位（重行/漏行），不是排序方言问题。
func sameIDOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func fmtID(prefix string, i int) string { return fmt.Sprintf("%s-%02d", prefix, i) }

func fmtName(prefix string, i int) string { return fmt.Sprintf("%s-%02d", prefix, i) }
