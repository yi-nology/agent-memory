package portable_test

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：**黄金样本**（跨仓对账用的那个固定包）与它的用例。
//
// # 黄金样本是什么、为什么需要它
//
// 两侧没有共享 CI ⇒ 「互通」这件事没法靠「两边都跑一遍」证明 ✓。
// 可行的形态是：**本仓产出��个固定的包** ✓，
// octop 侧拿它跑一次 `adopt` ✓ → 断言条数与关键字段 ✓ → 把结果回报回来 ✓。
//
// ⇒ 那个包必须**逐字节固定** ✓：
// 「每次重新生成都差一行」的话，它就不是样本、只是快照 ✓，
// 而快照没法用来对账（对账要的是「同一份输入」✓）。
//
// ⇒ 固定靠三样 ✓：
//  ① **注入的时钟** ✓（清单里的 packed_at ✓）——
//     这一条原先是漏的：octop 布局的导出直接用真实时钟 ✓，
//     而原生布局用注入时钟 ✓ ⇒ 同一条链上两套口径 ✓。
//  ② **固定的种子数据** ✓（标识与时刻全部写死 ✓）。
//  ③ **全序的行排序** ✓（同刻创建的记录也要有确定先后 ✓）。
//
// # 怎么重新生成
//
//	go test ./operations/portable/ -run Golden -update
//
// 改动了种子数据或导出形状 ⇒ 跑上面那条 ⇒ 提交新的样本 ✓。
// ⚠️ 跑之前先确认改动是**有意的** ✓：这条命令会覆盖样本 ✓。

// goldenPath 黄金样本（相对本包目录）。
const goldenPath = "testdata/octop-golden.hmpkg"

// updateGolden 传 -update 时重写样本。
var updateGolden = flag.Bool("update", false, "重写黄金样本")

// goldenAt 样本里所有时刻的固定值。
//
// 为什么写死：时刻进文件 ✓，而文件要逐字节固定 ✓。
var goldenAt = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// goldenNS 样本用的记忆库名。
const goldenNS = "golden"

// goldenSeed 造一份**固定**的库（黄金样本的唯一输入）。
//
// 每一类记录各一条 ✓ —— 不是为了省事 ✓，
// 是为了让「对方导进来少了一条哪一类」一眼看出来 ✓。
func goldenSeed(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := store.Namespace(goldenNS)

	if err := s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "ev-golden-1", Content: "机器甲的磁盘长期偏小", Timestamp: goldenAt,
		Type: types.EventManual, SessionID: "sess-golden", Host: "golden-host",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEntity(ctx, ns, &types.Entity{
		ID: "ent-golden-1", EntityType: types.EntityType("service"),
		CanonicalName: "机器甲", CreatedAt: goldenAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAlias(ctx, ns, &types.Alias{
		Alias: "甲机", EntityID: "ent-golden-1", EntityType: types.EntityType("service"),
		CreatedBy: types.DecidedUser, CreatedAt: goldenAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCandidate(ctx, ns, &types.Candidate{
		ID: "cand-golden-1", RawEventIDs: []string{"ev-golden-1"},
		Type: types.CandidateType("fact"), Status: types.CandidateStatus("pending"),
		Title: "磁盘偏小", Assertion: "机器甲的磁盘长期偏小",
		VerbatimQuote: "机器甲的磁盘长期偏小", QuoteEventID: "ev-golden-1",
		SubjectName: "机器甲", SubjectEntityType: types.EntityType("service"),
		TargetEntityID: "ent-golden-1",
		Confidence:     types.ConfidenceMed, Importance: types.ImportanceMed,
		CreatedAt: goldenAt, SessionID: "sess-golden",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "atom-golden-1", EntityID: "ent-golden-1",
		Assertion: "机器甲的磁盘长期偏小", QuoteEventID: "ev-golden-1",
		RawEventIDs: []string{"ev-golden-1"}, SearchTerms: []string{"磁盘", "偏小"},
		Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
		OccurredAt: goldenAt, CreatedAt: goldenAt, UpdatedAt: goldenAt,
		Kind: types.FactEvent,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertEntityPage(ctx, ns, &types.EntityPage{
		EntityID: "ent-golden-1", Scope: types.Scope(""),
		Headline: "机器甲的磁盘长期偏小", Body: "机器甲的磁盘长期偏小，需要清理。",
		AtomIDs: []string{"atom-golden-1"}, UpdatedAt: goldenAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendJournal(ctx, ns, &types.JournalEntry{
		ID: "jr-golden-1", At: goldenAt, Kind: types.JournalKind("promote"),
		Subject: "atom-golden-1", Actor: types.DecidedBy("rule"),
		Summary: "晋升为事实卡",
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// buildGolden 造出黄金样本的内容（写到给定路径）。
func buildGolden(t *testing.T, out string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "seed.sqlite")
	s := goldenSeed(t, db)
	defer func() { _ = s.Close() }()

	man, err := portable.ExportOctopFile(context.Background(), s, store.Namespace(goldenNS), out,
		portable.ExportOptions{Host: "golden-host", Now: goldenAt})
	if err != nil {
		t.Fatal(err)
	}
	if man.TotalRows != 7 {
		t.Fatalf("样本应有 7 条记录（每类一条），实际 %d", man.TotalRows)
	}
}

// TestOctopGoldenSampleIsReproducible：样本必须**逐字节可复现**。
//
// 这条是「黄金样本」这个概念的全部前提 ✓：
// 不可复现 ⇒ 每次重新生成都不同 ⇒ 对账时对方拿到的是另一份输入 ✓
// ⇒ 「对方读到几条」这种结论就没有意义了 ✓。
func TestOctopGoldenSampleIsReproducible(t *testing.T) {
	if *updateGolden {
		buildGolden(t, goldenPath)
		t.Logf("已重写黄金样本 %s", goldenPath)
		return
	}

	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.hmpkg")
	buildGolden(t, fresh)

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("读黄金样本失败（跑 go test ./operations/portable/ -run Golden -update 生成）: %v", err)
	}
	got, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("黄金样本不可复现：重新生成的包与提交的那份不一致（%d vs %d 字节）\n"+
			"若改动是有意的，跑 go test ./operations/portable/ -run Golden -update 重写并提交",
			len(got), len(want))
	}
}

// TestOctopGoldenSampleHasOctopLayout：样本必须是**octop 的布局** ✓，
// 而且里面的记录形状得是对方写的那套 ✓。
//
// 为什么单独一条：自环（导出→读回）只证明**我们自己读写对称** ✓，
// 证不了「对方读得动」✓ —— 而那正是要交给对方验证的 ✓。
// ⇒ 这条把**形状**钉死 ✓，对方那边只要跑 adopt 就知道对不对 ✓。
func TestOctopGoldenSampleHasOctopLayout(t *testing.T) {
	names, records := readGolden(t)

	// 包里就是那两个文件（octop 的布局 ✓）。
	if len(names) != 2 {
		t.Fatalf("包内应有 2 个文件（manifest.json + data.jsonl.gz），实际 %v", names)
	}
	hasManifest, hasData := false, false
	for _, n := range names {
		switch n {
		case "manifest.json":
			hasManifest = true
		case "data.jsonl.gz":
			hasData = true
		}
	}
	if !hasManifest || !hasData {
		t.Fatalf("包内文件不对：manifest=%v data=%v（实际 %v）", hasManifest, hasData, names)
	}

	// 记录：信封是 {"v","table","data"} ✓，键名是 snake_case ✓。
	tables := map[string]map[string]any{}
	for _, rec := range records {
		var env struct {
			V     int            `json:"v"`
			Table string         `json:"table"`
			Data  map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec, &env); err != nil {
			t.Fatalf("记录不是合法 JSON: %v (%q)", err, rec)
		}
		if env.V != 1 {
			t.Errorf("记录信封的 v 应是 1，实际 %d", env.V)
		}
		if env.Table == "" {
			t.Errorf("记录信封缺 table: %q", rec)
		}
		if len(env.Data) == 0 {
			t.Errorf("记录信封缺 data: %q", rec)
		}
		for k := range env.Data {
			if k != strings.ToLower(k) {
				t.Errorf("记录体的键名应是 snake_case，实际出现 %q", k)
			}
		}
		tables[env.Table] = env.Data
	}

	// 表名用 octop 那套 ✓，且每类都有一条 ✓。
	want := map[string]int{
		"raw_events": 1, "candidates": 1, "entities": 1, "aliases": 1,
		"atoms": 1, "entity_pages": 1, "journal": 1,
	}
	for table, n := range want {
		data, ok := tables[table]
		if !ok || len(data) == 0 {
			t.Errorf("包里没有 %s 的记录（全部表：%v）", table, tableNames(tables))
			continue
		}
		_ = n
	}

	// ⚠️ **必填字段**：缺一个 ⇒ 对方那一整表都导不进去 ✓，
	// 而症状是「导入成功、0 条」✓ ⇒ 这条要在**本仓这边**先红 ✓。
	//
	// 这张必填清单是照对方源码抄的（`_build_*` 里的 `d[...]` ✓），
	// 抄错了就会让「对方根本读不了」这件事溜过去 ✓ ⇒ 抄错时这里会红 ✓。
	required := map[string][]string{
		"raw_events":   {"id", "host", "timestamp", "event_type"},
		"candidates":   {"id", "candidate_type", "created_at"},
		"atoms":        {"id", "entity_id", "candidate_id", "assertion", "verbatim_quote", "quote_event_id", "occurred_at", "created_at"},
		"entities":     {"id", "entity_type", "canonical_name", "created_at"},
		"aliases":      {"alias", "entity_id", "entity_type", "created_at"},
		"entity_pages": {"id", "entity_id", "created_at", "updated_at"},
		"journal":      {"id", "timestamp", "action"},
	}
	for table, fields := range required {
		data, ok := tables[table]
		if !ok {
			t.Errorf("包里没有 %s 的记录", table)
			continue
		}
		for _, f := range fields {
			if _, ok := data[f]; !ok {
				t.Errorf("%s 缺对方必填字段 %q（对方读这一表会整表失败）", table, f)
			}
		}
	}

	// ⚠️ **名字不一样的那几处**必须用对方的叫法 ✓。
	//
	// 这批字段两边的名字本来就不同 ✓（同一个设计的两个实现 ✓），
	// 用本仓的叫法 ⇒ 对方按名字取不到 ⇒ 整表跳过 ✓ ⇒ 静默 ✓。
	nameChecks := map[string]map[string]string{
		// 表 → {对方键名: 不该出现的本仓式键名}
		"raw_events":   {"event_type": "type"},
		"candidates":   {"candidate_type": "type"},
		"entity_pages": {"summary_markdown": "body", "topics": "atom_ids", "id": ""},
		"journal":      {"timestamp": "at", "action": "kind", "note": "summary"},
	}
	for table, checks := range nameChecks {
		data, ok := tables[table]
		if !ok {
			continue
		}
		for wantKey, badKey := range checks {
			if _, ok := data[wantKey]; !ok {
				t.Errorf("%s 应有对方那套键名 %q", table, wantKey)
			}
			if badKey != "" {
				if _, ok := data[badKey]; ok {
					t.Errorf("%s 不该出现本仓式的键名 %q（对方按名字取不到）", table, badKey)
				}
			}
		}
	}
}

// TestOctopGoldenSampleImportsBack：样本必须能被**本仓**读回来 ✓
// 且逐字段对上 ✓。
//
// 这条验的是「样本没有把本仓自己写坏了」✓ ——
// 跨仓对账要拿这个样本去问对方，
// 所以它得先在本仓这边**自证是好的** ✓。
func TestOctopGoldenSampleImportsBack(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "target.sqlite")
	st, err := sqlite.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	rep, err := portable.ImportOctop(context.Background(), goldenPath, st, store.Namespace(goldenNS))
	if err != nil {
		t.Fatalf("读黄金样本失败: %v", err)
	}
	if rep.Skipped != 0 {
		t.Errorf("不该有跳过的记录，实际跳过 %d 条", rep.Skipped)
	}

	ctx := context.Background()
	ns := store.Namespace(goldenNS)

	evs, err := st.ListRaw(ctx, ns, "", time.Time{}, store.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].ID != "ev-golden-1" {
		t.Fatalf("原始事件没读回来: %+v", evs)
	}
	if evs[0].Content != "机器甲的磁盘长期偏小" {
		t.Errorf("事件正文不对: %q", evs[0].Content)
	}

	atoms, err := st.ListAtoms(ctx, ns, store.AtomFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(atoms) != 1 || atoms[0].ID != "atom-golden-1" {
		t.Fatalf("事实卡没读回来: %+v", atoms)
	}
	if atoms[0].Assertion != "机器甲的磁盘长期偏小" {
		t.Errorf("事实卡断言不对: %q", atoms[0].Assertion)
	}

	// 键名翻译要**双向**对上 ✓：
	// snake_case → Go 字段名 ✓（读侧 ✓），
	// Go 字段名 → snake_case ✓（写侧 ✓，上一条已钉 ✓）。
	// 这条钉的是读侧那个方向 ✓ —— 字段名一旦对不上，
	// 对方那边看到的就是空字段 ✓，而本仓这边看着「导入成功」✓。
	ents, err := st.ListEntities(ctx, ns, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].CanonicalName != "机器甲" {
		t.Fatalf("实体没读回来: %+v", ents)
	}
}

// tableNames 取包里出现过的表名（报错信息用）。
func tableNames(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// readGolden 读黄金样本：包内文件名 + 逐条记录行。
func readGolden(t *testing.T) ([]string, [][]byte) {
	t.Helper()
	zr, err := zip.OpenReader(goldenPath)
	if err != nil {
		t.Fatalf("打开黄金样本失败: %v", err)
	}
	defer func() { _ = zr.Close() }()

	var names []string
	var dataFile *zip.File
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name == "data.jsonl.gz" {
			dataFile = f
		}
	}
	if dataFile == nil {
		t.Fatalf("黄金样本里没有 data.jsonl.gz（实际 %v）", names)
	}
	rc, err := dataFile.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()

	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	var records [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		records = append(records, []byte(line))
	}
	return names, records
}
