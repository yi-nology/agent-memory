package portable_test

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：与 octop-memory（Python）的 .hmpkg 互通用例。
//
// # 验的是「**形状**」而不是「内容」
//
// 两侧的记录体字段几乎逐字段一致，但**信封**（{"v","table","data"}）、
// **表名**、**键名**（snake_case vs Go 字段名）都不同。
// 这三样正是互通要补的东西 ⇒ 用例逐个钉住它们 ✓。
//
// # 「自环」为什么够
//
// 本仓侧做「导出 → 读回 → 逐字段相等」的自环 ✓，
// 证明**本仓的 octop 布局读写对称** ✓。
// 真正的跨仓验证（对方的读端能不能读我们的包 ✓）需要对方配合 ✓，
// 形态是「黄金样本 + 一次对账」（见 docs/INTEGRATION_PLAN_20261021.md A-3 ✓）。

// octopSeed 造一份有内容的库（本仓侧）。
func octopSeed(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ns := store.Namespace("interop")
	if err := s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "ev1", Content: "磁盘长期偏小", Timestamp: now(), Type: types.EventManual,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "a1", EntityID: "e1", Assertion: "机器甲的磁盘长期偏小",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		SearchTerms: []string{"磁盘", "偏小"},
		Importance:  types.ImportanceHigh, Confidence: types.ConfidenceHigh,
		OccurredAt: now(), CreatedAt: now(), UpdatedAt: now(),
		Kind: types.FactLesson,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEntity(ctx, ns, &types.Entity{
		ID: "e1", EntityType: types.EntityType("service"), CanonicalName: "机器甲", CreatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// 导出的包必须是 **octop 的布局**：manifest.json + data.jsonl.gz。
func TestExportOctopUsesOctopLayout(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "src.sqlite")
	s := octopSeed(t, db)
	defer func() { _ = s.Close() }()

	pkg := filepath.Join(dir, "out.hmpkg")
	man, err := portable.ExportOctopFile(context.Background(), s, store.Namespace("interop"),
		pkg, portable.ExportOptions{Host: "interop-test"})
	if err != nil {
		t.Fatal(err)
	}
	if man.PkgVersion != portable.OctopPkgVersion {
		t.Fatalf("pkg_version 应为 %d: %d", portable.OctopPkgVersion, man.PkgVersion)
	}

	ok, err := portable.IsOctopPackage(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("导出的包应被识别为 octop 布局")
	}

	// 打开 zip：里面必须有 manifest.json 与 data.jsonl.gz ✓。
	zr, err := zip.OpenReader(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["manifest.json"] || !names["data.jsonl.gz"] {
		t.Fatalf("包里应有 manifest.json 与 data.jsonl.gz: %v", names)
	}

	// 记录行：{"v":1,"table":"atoms","data":{…}}，且键名是 snake_case ✓。
	var line map[string]any
	found := false
	for _, f := range zr.File {
		if f.Name != "data.jsonl.gz" {
			continue
		}
		rc, _ := f.Open()
		gz, err := gzip.NewReader(rc)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(gz)
		for {
			var row map[string]any
			if err := dec.Decode(&row); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if row["table"] == "atoms" {
				line = row
				found = true
				break
			}
		}
		_ = gz.Close()
		_ = rc.Close()
	}
	if !found {
		t.Fatal("包里应有 atoms 记录")
	}
	if line["v"] != float64(portable.OctopExportVersion) {
		t.Fatalf("记录行应带 v: %v", line["v"])
	}
	data, ok := line["data"].(map[string]any)
	if !ok {
		t.Fatalf("记录行应有 data 对象: %v", line)
	}
	if _, ok := data["entity_id"]; !ok {
		t.Fatalf("键名应是 snake_case（entity_id）: %v", keysOf(data))
	}
	if _, ok := data["EntityID"]; ok {
		t.Fatal("键名不该是 Go 字段名")
	}
}

// 自环：导出 → 读回 → 逐字段相等。
//
// 这是「本仓侧的 octop 读写对称」的证明 ✓；跨仓那一半靠黄金样本对账 ✓。
func TestExportOctopRoundTripsThroughOctopLayout(t *testing.T) {
	dir := t.TempDir()
	src := octopSeed(t, filepath.Join(dir, "src.sqlite"))
	defer func() { _ = src.Close() }()

	pkg := filepath.Join(dir, "out.hmpkg")
	if _, err := portable.ExportOctopFile(context.Background(), src, store.Namespace("interop"),
		pkg, portable.ExportOptions{Host: "interop-test"}); err != nil {
		t.Fatal(err)
	}

	dstPath := filepath.Join(dir, "dst.sqlite")
	dst, err := sqlite.OpenFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Close() }()
	ns := store.Namespace("imported")

	rep, err := portable.ImportOctop(context.Background(), pkg, dst, ns)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts["atoms"] != 1 {
		t.Fatalf("应导入 1 条 atoms: %+v", rep.Counts)
	}

	got, err := dst.GetAtom(context.Background(), ns, "a1")
	if err != nil {
		t.Fatalf("导入后取不到事实卡: %v", err)
	}
	want, err := src.GetAtom(context.Background(), store.Namespace("interop"), "a1")
	if err != nil {
		t.Fatal(err)
	}
	// 契约覆盖的字段：逐个对上。
	if got.Assertion != want.Assertion || got.EntityID != want.EntityID ||
		got.QuoteEventID != want.QuoteEventID ||
		got.Importance != want.Importance || got.Confidence != want.Confidence {
		t.Fatalf("往返后字段不一致:\n got=%+v\nwant=%+v", got, want)
	}
	if len(got.RawEventIDs) != len(want.RawEventIDs) || got.RawEventIDs[0] != want.RawEventIDs[0] {
		t.Fatalf("往返后证据不一致: %v vs %v", got.RawEventIDs, want.RawEventIDs)
	}
	if !got.OccurredAt.Equal(want.OccurredAt) {
		t.Fatalf("往返后时刻不一致: %v vs %v", got.OccurredAt, want.OccurredAt)
	}
	if len(got.SearchTerms) != len(want.SearchTerms) {
		t.Fatalf("往返后检索词不一致: %v vs %v", got.SearchTerms, want.SearchTerms)
	}
	// 对方**没有对应字段**的那些：必须落在**安全的一侧** ✓。
	//
	// ⚠️ 这不是「弱化断言」✓，是断言**该丢的确实丢了、且丢在安全侧** ✓：
	//   scope       → 空 = 全局（对方没有归属字段 ✓，带过去就是泄漏 ✓）
	//   kind        → 空 = 长期经验（**不淘汰**的那一类 ✓，见 types.FactKind ✓）
	//   valid_from  → 空 = 无区间（不因时效被排除 ✓）
	//
	// 零值落在「保守」那一侧 ✓ 是本仓的既有纪律 ✓
	// （「误命中方向决定代价」✓：宁可多留着，不可误淘汰 ✓）。
	if got.Scope != "" {
		t.Errorf("归属不该被带过去（带过去会变成全局可见）: %q", got.Scope)
	}
	if got.Kind != types.FactDefault {
		t.Errorf("事实类别对方没有 ⇒ 应落回缺省（长期经验、不淘汰）: %q", got.Kind)
	}
	if !got.ValidFrom.IsZero() || !got.ValidTo.IsZero() {
		t.Errorf("生效区间对方没有 ⇒ 应为零值: %v ~ %v", got.ValidFrom, got.ValidTo)
	}
}

// keysOf 取 map 的键（报错信息用）。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 带归属的记录**不导出**，且要**记账**。
//
// 为什么：不导就会变成全局可见 ⇒ 内容泄漏；
// 而不记账，宿主就只看到「导出成功、少了几条」，却不知道少的是什么。
func TestExportOctopSkipsScopedRecords(t *testing.T) {
	dir := t.TempDir()
	src := octopSeed(t, filepath.Join(dir, "src.sqlite"))
	defer func() { _ = src.Close() }()
	ctx := context.Background()
	ns := store.Namespace("interop")

	// 加一条带归属的事实卡。
	if err := src.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "a-scoped", EntityID: "e1", Scope: "team-a", Assertion: "只属于 team-a 的事实",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Importance: types.ImportanceHigh, Confidence: types.ConfidenceHigh,
		OccurredAt: now(), CreatedAt: now(), UpdatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}

	pkg := filepath.Join(dir, "out.hmpkg")
	man, err := portable.ExportOctopFile(ctx, src, ns, pkg, portable.ExportOptions{Host: "interop-test"})
	if err != nil {
		t.Fatal(err)
	}
	if man.SkippedScoped != 1 {
		t.Fatalf("应记账 1 条被跳过的带归属记录，实际 %d", man.SkippedScoped)
	}
	// 种子里有：1 事件 + 1 实体 + 1 事实卡（全局）+ 1 事实卡（带归属，跳过）✓
	// ⇒ 包里是 3 条 ✓（带归属那条不进包 ✓）。
	if man.TotalRows != 3 {
		t.Fatalf("包里的记录数应是 3（不含被跳过的），实际 %d", man.TotalRows)
	}

	// 包里**不该**出现那条带归属的事实。
	data := readOctopRows(t, pkg)
	for _, rec := range data {
		if rec["table"] == "atoms" {
			if id, _ := rec["data"].(map[string]any)["id"].(string); id == "a-scoped" {
				t.Fatal("带归属的事实卡不该出现在包里：导过去会变成全局可见")
			}
		}
	}
}

// readOctopRows 读包里的记录行（信封展开成 table + data）。
func readOctopRows(t *testing.T, pkg string) []map[string]any {
	t.Helper()
	zr, err := zip.OpenReader(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	var out []map[string]any
	for _, f := range zr.File {
		if f.Name != "data.jsonl.gz" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(rc)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(gz)
		for {
			var row map[string]any
			if err := dec.Decode(&row); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		_ = gz.Close()
		_ = rc.Close()
	}
	return out
}
