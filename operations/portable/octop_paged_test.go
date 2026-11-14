package portable_test

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：octop 导出的**分页取尽**用例（issue #61）。
//
// # 要钉住的是什么
//
// 全量导出不得用写死的大 Limit 冒充（那是 #61 撤掉的形态：超过上限的行
// 按取数序静默截断，包残缺且零信号）。两条判据：
//
//  1. **跨页取尽**：记录数 > 页宽时，导出集合与库里全量逐条相等——
//     且翻页参数真的按页推进（不是「一次大 Limit 硬取恰好没截断」的假绿），
//     故用探针记录每次取数的 (offset, limit) 序列；
//  2. **能力缺失响亮失败**：存储没有 PagedSource 时，导出宁可报错也不降级成
//     单次大 Limit（那等于把静默截断换个名字留下，见 paging.go 的前提说明）。
//
// 两条都走**真 SQLite 内存库**（fake 不替真后端背书，#51 的教训）；
// 探针只是取数调用的计数器，不伪造任何数据。

// pagedProbe 记录取数调用参数的探针：内嵌真库，只旁路记账，不改行为。
type pagedProbe struct {
	*sqlite.Store
	rawPages  []store.Page
	atomPages []store.AtomFilter
	candPages [][2]int
	entPages  [][2]int
	pagePages [][2]int
	jrnlPages [][2]int
}

func (p *pagedProbe) ListRaw(ctx context.Context, ns store.Namespace, scope types.Scope,
	before time.Time, pg store.Page) ([]*types.RawEvent, error) {
	p.rawPages = append(p.rawPages, pg)
	return p.Store.ListRaw(ctx, ns, scope, before, pg)
}

func (p *pagedProbe) ListAtoms(ctx context.Context, ns store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error) {
	p.atomPages = append(p.atomPages, f)
	return p.Store.ListAtoms(ctx, ns, f)
}

func (p *pagedProbe) ListCandidatesPaged(ctx context.Context, ns store.Namespace,
	f store.CandidateFilter, offset, limit int) ([]*types.Candidate, error) {
	p.candPages = append(p.candPages, [2]int{offset, limit})
	return p.Store.ListCandidatesPaged(ctx, ns, f, offset, limit)
}

func (p *pagedProbe) ListEntitiesPaged(ctx context.Context, ns store.Namespace,
	entityType types.EntityType, offset, limit int) ([]*types.Entity, error) {
	p.entPages = append(p.entPages, [2]int{offset, limit})
	return p.Store.ListEntitiesPaged(ctx, ns, entityType, offset, limit)
}

func (p *pagedProbe) ListEntityPagesPaged(ctx context.Context, ns store.Namespace,
	offset, limit int) ([]*types.EntityPage, error) {
	p.pagePages = append(p.pagePages, [2]int{offset, limit})
	return p.Store.ListEntityPagesPaged(ctx, ns, offset, limit)
}

func (p *pagedProbe) ListJournalPaged(ctx context.Context, ns store.Namespace,
	subject string, offset, limit int) ([]*types.JournalEntry, error) {
	p.jrnlPages = append(p.jrnlPages, [2]int{offset, limit})
	return p.Store.ListJournalPaged(ctx, ns, subject, offset, limit)
}

// pagedSeed 造一份每类 12 条的库（12 > 页宽 5 ⇒ 每表跨 3 页：5+5+2）。
//
// 全部**全局记录**（Scope 空）：带归属的记录 octop 布局本来就不导（防泄漏），
// 混进来只会让「取尽」的断言平白多一个变量。时刻互不相同：
// 各表的取数序带时刻键，导入侧要按同一序复原才可比。
func pagedSeed(t *testing.T, s *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	ns := store.Namespace("paged")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 12
	for i := range n {
		at := base.Add(time.Duration(i) * time.Minute)
		id := func(prefix string) string { return fmt.Sprintf("%s-%02d", prefix, i) }
		if err := s.SaveRaw(ctx, ns, &types.RawEvent{
			ID: id("ev"), Content: fmt.Sprintf("第 %d 号事件的正文", i),
			Timestamp: at, Type: types.EventManual,
		}); err != nil {
			t.Fatalf("SaveRaw(%d): %v", i, err)
		}
		if err := s.SaveCandidate(ctx, ns, &types.Candidate{
			ID: id("cand"), RawEventIDs: []string{id("ev")},
			Type: types.CandidateType("fact"), Status: types.CandidatePending,
			Title: fmt.Sprintf("候选 %d", i), QuoteEventID: id("ev"),
			CreatedAt: at,
		}); err != nil {
			t.Fatalf("SaveCandidate(%d): %v", i, err)
		}
		if err := s.SaveEntity(ctx, ns, &types.Entity{
			ID: id("ent"), EntityType: types.EntityType("service"),
			CanonicalName: fmt.Sprintf("机器-%02d", i), CreatedAt: at,
		}); err != nil {
			t.Fatalf("SaveEntity(%d): %v", i, err)
		}
		if err := s.SaveAtom(ctx, ns, &types.AtomCard{
			ID: id("atom"), EntityID: id("ent"),
			Assertion: fmt.Sprintf("机器-%02d 的事实", i), QuoteEventID: id("ev"),
			Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
			OccurredAt: at, CreatedAt: at, UpdatedAt: at,
			Kind: types.FactLesson,
		}); err != nil {
			t.Fatalf("SaveAtom(%d): %v", i, err)
		}
		if err := s.SaveAlias(ctx, ns, &types.Alias{
			Alias: fmt.Sprintf("别名-%02d", i), EntityID: id("ent"),
			EntityType: types.EntityType("service"), CreatedBy: types.DecidedUser,
			CreatedAt: at,
		}); err != nil {
			t.Fatalf("SaveAlias(%d): %v", i, err)
		}
		if err := s.UpsertEntityPage(ctx, ns, &types.EntityPage{
			EntityID: id("ent"), Headline: fmt.Sprintf("机器-%02d 的一句话画像", i),
			UpdatedAt: at,
		}); err != nil {
			t.Fatalf("UpsertEntityPage(%d): %v", i, err)
		}
		if err := s.AppendJournal(ctx, ns, &types.JournalEntry{
			ID: id("jr"), At: at, Kind: types.JournalPromote,
			Subject: id("atom"), Actor: types.DecidedBy("rule"),
			Summary: fmt.Sprintf("第 %d 条晋升", i),
		}); err != nil {
			t.Fatalf("AppendJournal(%d): %v", i, err)
		}
	}
}

// TestExportOctopExhaustsAcrossPages 跨页取尽：>页宽条数下导出集合与全量逐字相等。
func TestExportOctopExhaustsAcrossPages(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	ns := store.Namespace("paged")

	src, err := sqlite.OpenMemory("octop-paged-src")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	pagedSeed(t, src)
	probe := &pagedProbe{Store: src}

	const pageWidth = 5
	pkg := filepath.Join(dir, "paged.hmpkg")
	man, err := portable.ExportOctopFile(ctx, probe, ns, pkg,
		portable.ExportOptions{Host: "paged-test", PageSize: pageWidth})
	if err != nil {
		t.Fatal(err)
	}

	// ① 每类 12 条全部在包里（少一条即截断）。
	for _, tbl := range []string{"raw_events", "candidates", "entities", "atoms", "aliases", "entity_pages", "journal"} {
		if got := man.RowCounts[tbl]; got != 12 {
			t.Fatalf("%s 应导出 12 条（页宽 %d 跨 3 页取尽），实得 %d", tbl, pageWidth, got)
		}
	}
	if man.TotalRows != 84 {
		t.Fatalf("7 类 × 12 条应为 84 行，实得 %d", man.TotalRows)
	}

	// ② 翻页真的按页推进了——不是「一次大 Limit 硬取恰好没截断」的假绿：
	//    12 条 / 页宽 5 ⇒ 每表恰 3 次取数，offset 序列 0/5/10（末页 2 条 < 页宽即止）。
	//    entities 走两遍（entities 表一遍、aliases 表按实体取别名要先把实体取尽），
	//    故它的序列是 6 个。
	wantSeq := [][2]int{{0, pageWidth}, {pageWidth, pageWidth}, {2 * pageWidth, pageWidth}}
	if !reflect.DeepEqual(probe.candPages, wantSeq) ||
		!reflect.DeepEqual(probe.pagePages, wantSeq) ||
		!reflect.DeepEqual(probe.jrnlPages, wantSeq) {
		t.Fatalf("三张表的翻页序列应为 %v：candidates=%v pages=%v journal=%v",
			wantSeq, probe.candPages, probe.pagePages, probe.jrnlPages)
	}
	wantEntSeq := append(append([][2]int{}, wantSeq...), wantSeq...)
	if !reflect.DeepEqual(probe.entPages, wantEntSeq) {
		t.Fatalf("entities 的翻页序列应为 %v（entities 与 aliases 各翻一遍），实得 %v",
			wantEntSeq, probe.entPages)
	}
	if len(probe.rawPages) != 3 {
		t.Fatalf("raw_events 应分 3 页取尽，实得 %d 页: %+v", len(probe.rawPages), probe.rawPages)
	}
	if len(probe.atomPages) != 3 {
		t.Fatalf("atoms 应分 3 页取尽（store.ListAllAtoms），实得 %d 页: %+v", len(probe.atomPages), probe.atomPages)
	}

	// ③ 导入一个干净的库，逐表取全量对比——「导出集合与全量逐字相等」：
	//    序列一致（各表取数序稳定且随包走）且内容一致。
	tgt, err := sqlite.OpenMemory("octop-paged-tgt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tgt.Close() }()
	rep, err := portable.ImportOctop(ctx, pkg, tgt, ns)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"raw_events", "candidates", "entities", "atoms", "aliases", "entity_pages", "journal"} {
		if got := rep.Counts[tbl]; got != 12 {
			t.Fatalf("导入侧 %s 应还原 12 条，实得 %d（counts=%v）", tbl, got, rep.Counts)
		}
	}

	const big = 100 // 断言用的取数上限：12 条远小于它，测试自身不碰截断

	srcRaws, err := src.ListRaw(ctx, ns, "", time.Time{}, store.Page{Limit: big})
	if err != nil {
		t.Fatal(err)
	}
	tgtRaws, err := tgt.ListRaw(ctx, ns, "", time.Time{}, store.Page{Limit: big})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcRaws) != len(tgtRaws) {
		t.Fatalf("raw_events 条数应一致：源 %d / 导入 %d", len(srcRaws), len(tgtRaws))
	}
	for i := range srcRaws {
		if srcRaws[i].ID != tgtRaws[i].ID || srcRaws[i].Content != tgtRaws[i].Content {
			t.Fatalf("raw_events 第 %d 条不一致：源 (%s,%s) / 导入 (%s,%s)",
				i, srcRaws[i].ID, srcRaws[i].Content, tgtRaws[i].ID, tgtRaws[i].Content)
		}
	}

	srcAtoms, err := src.ListAtoms(ctx, ns, store.AtomFilter{Limit: big})
	if err != nil {
		t.Fatal(err)
	}
	tgtAtoms, err := tgt.ListAtoms(ctx, ns, store.AtomFilter{Limit: big})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcAtoms) != 12 || len(srcAtoms) != len(tgtAtoms) {
		t.Fatalf("atoms 条数应一致且为 12：源 %d / 导入 %d", len(srcAtoms), len(tgtAtoms))
	}
	for i := range srcAtoms {
		if srcAtoms[i].ID != tgtAtoms[i].ID || srcAtoms[i].Assertion != tgtAtoms[i].Assertion ||
			srcAtoms[i].EntityID != tgtAtoms[i].EntityID {
			t.Fatalf("atoms 第 %d 条不一致：源 %v / 导入 %v", i, srcAtoms[i], tgtAtoms[i])
		}
	}

	srcEnts, err := src.ListEntities(ctx, ns, "", big)
	if err != nil {
		t.Fatal(err)
	}
	tgtEnts, err := tgt.ListEntities(ctx, ns, "", big)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcEnts) != 12 || len(srcEnts) != len(tgtEnts) {
		t.Fatalf("entities 条数应一致且为 12：源 %d / 导入 %d", len(srcEnts), len(tgtEnts))
	}
	for i := range srcEnts {
		if srcEnts[i].ID != tgtEnts[i].ID || srcEnts[i].CanonicalName != tgtEnts[i].CanonicalName {
			t.Fatalf("entities 第 %d 条不一致：源 %v / 导入 %v", i, srcEnts[i], tgtEnts[i])
		}
		// 别名跟着实体走：导出集合也要逐字相等。
		srcAls, err := src.ListAliases(ctx, ns, srcEnts[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		tgtAls, err := tgt.ListAliases(ctx, ns, tgtEnts[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(srcAls) != len(tgtAls) {
			t.Fatalf("实体 %s 的别名条数应一致：源 %d / 导入 %d", srcEnts[i].ID, len(srcAls), len(tgtAls))
		}
		for j := range srcAls {
			if srcAls[j].Alias != tgtAls[j].Alias {
				t.Fatalf("实体 %s 的第 %d 个别名不一致：源 %s / 导入 %s",
					srcEnts[i].ID, j, srcAls[j].Alias, tgtAls[j].Alias)
			}
		}
	}

	srcCands, err := src.ListCandidates(ctx, ns, store.CandidateFilter{Limit: big})
	if err != nil {
		t.Fatal(err)
	}
	tgtCands, err := tgt.ListCandidates(ctx, ns, store.CandidateFilter{Limit: big})
	if err != nil {
		t.Fatal(err)
	}
	if len(srcCands) != 12 || len(srcCands) != len(tgtCands) {
		t.Fatalf("candidates 条数应一致且为 12：源 %d / 导入 %d", len(srcCands), len(tgtCands))
	}
	for i := range srcCands {
		if srcCands[i].ID != tgtCands[i].ID || srcCands[i].Title != tgtCands[i].Title {
			t.Fatalf("candidates 第 %d 条不一致：源 %v / 导入 %v", i, srcCands[i], tgtCands[i])
		}
	}

	srcPages, err := src.ListEntityPages(ctx, ns, big)
	if err != nil {
		t.Fatal(err)
	}
	tgtPages, err := tgt.ListEntityPages(ctx, ns, big)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcPages) != 12 || len(srcPages) != len(tgtPages) {
		t.Fatalf("entity_pages 条数应一致且为 12：源 %d / 导入 %d", len(srcPages), len(tgtPages))
	}
	for i := range srcPages {
		if srcPages[i].EntityID != tgtPages[i].EntityID || srcPages[i].Headline != tgtPages[i].Headline {
			t.Fatalf("entity_pages 第 %d 条不一致：源 %v / 导入 %v", i, srcPages[i], tgtPages[i])
		}
	}

	srcJrnl, err := src.ListJournal(ctx, ns, "", big)
	if err != nil {
		t.Fatal(err)
	}
	tgtJrnl, err := tgt.ListJournal(ctx, ns, "", big)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcJrnl) != 12 || len(srcJrnl) != len(tgtJrnl) {
		t.Fatalf("journal 条数应一致且为 12：源 %d / 导入 %d", len(srcJrnl), len(tgtJrnl))
	}
	for i := range srcJrnl {
		if srcJrnl[i].ID != tgtJrnl[i].ID || srcJrnl[i].Summary != tgtJrnl[i].Summary {
			t.Fatalf("journal 第 %d 条不一致：源 %v / 导入 %v", i, srcJrnl[i], tgtJrnl[i])
		}
	}
}

// stubSource 只实现 Source 窄接口的最小桩：**不实现** PagedSource。
//
// 用途单一：钉住「没有分页取全能力 ⇒ 响亮报错」。它不伪造任何数据语义，
// 也不替真后端背书（#51 的教训）——数据语义的用例全走真 SQLite（见上）。
type stubSource struct {
	raws []*types.RawEvent
}

func (s *stubSource) ListRaw(context.Context, store.Namespace, types.Scope, time.Time, store.Page) ([]*types.RawEvent, error) {
	return s.raws, nil
}
func (s *stubSource) ListCandidates(context.Context, store.Namespace, store.CandidateFilter) ([]*types.Candidate, error) {
	return nil, nil
}
func (s *stubSource) ListAtoms(context.Context, store.Namespace, store.AtomFilter) ([]*types.AtomCard, error) {
	return nil, nil
}
func (s *stubSource) ListEntities(context.Context, store.Namespace, types.EntityType, int) ([]*types.Entity, error) {
	return nil, nil
}
func (s *stubSource) ListAliases(context.Context, store.Namespace, string) ([]*types.Alias, error) {
	return nil, nil
}
func (s *stubSource) ListEntityPages(context.Context, store.Namespace, int) ([]*types.EntityPage, error) {
	return nil, nil
}
func (s *stubSource) ListEpisodes(context.Context, store.Namespace, string, int) ([]*types.Episode, error) {
	return nil, nil
}
func (s *stubSource) ListDigests(context.Context, store.Namespace, int) ([]*types.DigestRecord, error) {
	return nil, nil
}
func (s *stubSource) ListJournal(context.Context, store.Namespace, string, int) ([]*types.JournalEntry, error) {
	return nil, nil
}

// TestExportOctopRejectsUnpagedSource 没有分页取全能力 ⇒ 导出响亮失败。
//
// 为什么是报错而不是降级：导出完整性是前提不是增强（见 paging.go），
// 降级形态只能是「单次大 Limit 硬取」——那正是 #61 撤掉的静默截断。
func TestExportOctopRejectsUnpagedSource(t *testing.T) {
	var buf stubWriter
	_, err := portable.ExportOctop(context.Background(), &stubSource{},
		store.Namespace("stub"), &buf, portable.ExportOptions{})
	if err == nil {
		t.Fatal("没有 PagedSource 能力时导出必须报错，不该静默产出可能残缺的包")
	}
	if !strings.Contains(err.Error(), "PagedSource") {
		t.Fatalf("报错须点名 PagedSource（教宿主怎么补），实得: %v", err)
	}
}

// stubWriter io.Writer 最小桩。
type stubWriter struct{}

func (stubWriter) Write(p []byte) (int, error) { return len(p), nil }
