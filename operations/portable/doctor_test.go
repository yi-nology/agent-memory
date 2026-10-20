package portable_test

import (
	"archive/zip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oekit/pkg/pack"

	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
)

// 本仓清单与上游 pack.Manifest 的字段必须**一一对应**。
//
// 读面走 pack（体检与读口都靠它），而 pack 按 pack.Manifest 解析清单——
// 两边的字段名一旦漂移，pack 解析出来的清单会全空，体检会报
// 「缺少文件：xxx」。那是**响亮的失败**（不会静默放过），但已经晚了：
// 包会「体检不通过」而没人知道是字段漂移。
//
// 这条用例把漂移提前变成红灯：字段名对不上，这里立刻红。
func TestManifestSchemaMatchesPack(t *testing.T) {
	ts := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	pm := &pack.Manifest{
		FormatVersion: 1,
		CreatedAt:     ts,
		Counts:        map[string]int{"raw": 1},
		Files:         map[string]string{"raw.jsonl": "abc"},
	}
	b, err := json.Marshal(pm)
	if err != nil {
		t.Fatal(err)
	}
	var am portable.Manifest
	if err := json.Unmarshal(b, &am); err != nil {
		t.Fatalf("上游清单读不进本仓结构: %v", err)
	}
	if am.FormatVersion != pm.FormatVersion {
		t.Fatalf("format_version 对不上: 本仓 %d 上游 %d", am.FormatVersion, pm.FormatVersion)
	}
	if !am.CreatedAt.Equal(pm.CreatedAt) {
		t.Fatalf("created_at 对不上: 本仓 %v 上游 %v", am.CreatedAt, pm.CreatedAt)
	}
	if am.Counts["raw"] != 1 {
		t.Fatalf("counts 对不上: %v", am.Counts)
	}
	if am.Files["raw.jsonl"] != "abc" {
		t.Fatalf("files 对不上: %v", am.Files)
	}
	// 反向：本仓清单里的那两个溯源字段，**不能**出现在上游清单里——
	// 上游没有它们的位置，本仓多出来的字段是本仓的事。
	up, err := json.Marshal(pm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(up), "source_namespace") {
		t.Fatal("上游清单不该带本仓的溯源字段")
	}
}

// 体检必须查出**条数不符**：清单里记的条数与文件里的实际行数对不上。
//
// 这条是「清单自身记账坏了」的形态——文件内容没变（摘要照过），
// 但清单说它有 99 条、文件里其实只有 1 条。迁移是**最容易出静默错误**
// 的环节：导出多少、导回多少这个核对就靠它。
//
// ⚠️ 判据不能拿「体检不通过」就完事——那分不清是条数查出来的还是别的
// 问题查出来的。故断言里点名「条数」。
func TestDoctorDetectsCountMismatch(t *testing.T) {
	src := seed(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "m"+portable.Extension)
	if _, err := portable.ExportFile(context.Background(), src, ns, pkg, portable.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	entries := readZipEntries(t, pkg)
	var man portable.Manifest
	if err := json.Unmarshal(entries["manifest.json"], &man); err != nil {
		t.Fatal(err)
	}
	man.Counts["raw"] = 99 // 文件里其实只有 1 条
	b, err := json.Marshal(&man)
	if err != nil {
		t.Fatal(err)
	}
	entries["manifest.json"] = b
	writeZipEntries(t, pkg, entries)

	rep, err := portable.Doctor(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatal("条数不符必须被查出：清单说 99 条、文件里只有 1 条")
	}
	if !problemsMention(rep.Problems, "条数") {
		t.Fatalf("问题清单里应有「条数」相关结论，实得: %v", rep.Problems)
	}
}

// 体检必须查出**包内多出来的文件**：清单没记的文件不该在包里。
//
// 这条查的是「包里塞了清单之外的东西」——既可能是导出时的残留，
// 也可能是有人动过包。
func TestDoctorDetectsExtraFile(t *testing.T) {
	src := seed(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "m"+portable.Extension)
	if _, err := portable.ExportFile(context.Background(), src, ns, pkg, portable.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	entries := readZipEntries(t, pkg)
	entries["stowaway.jsonl"] = []byte("{\"x\":1}\n")
	writeZipEntries(t, pkg, entries)

	rep, err := portable.Doctor(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatal("清单之外的文件必须被查出")
	}
	if !problemsMention(rep.Problems, "stowaway.jsonl") {
		t.Fatalf("问题清单里应点名那个多余文件，实得: %v", rep.Problems)
	}
}

// 体检必须查出**非法文件名**（zip 路径穿越形态）。
//
// 包会被下载、被解压、被再导入——一个叫 `../../etc/passwd` 的条目
// 若原样透传，就是一次路径穿越。导出侧的文件名是本仓给的（安全），
// 但包是**可被替换的产物**，读面必须自己查。
func TestDoctorDetectsIllegalFileName(t *testing.T) {
	src := seed(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "m"+portable.Extension)
	if _, err := portable.ExportFile(context.Background(), src, ns, pkg, portable.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	entries := readZipEntries(t, pkg)
	entries["../../etc/passwd"] = []byte("x\n")
	writeZipEntries(t, pkg, entries)

	rep, err := portable.Doctor(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatal("非法文件名必须被查出：包是可被替换的产物，读面得自己查")
	}
	if !problemsMention(rep.Problems, "passwd") {
		t.Fatalf("问题清单里应点名那个非法文件名，实得: %v", rep.Problems)
	}
}

// 本仓的**必需数据文件名单**仍由本仓自己查：上游 pack 不知道
// 哪些数据类别对记忆库是必需的。
func TestDoctorDetectsMissingRequiredFile(t *testing.T) {
	src := seed(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "m"+portable.Extension)
	if _, err := portable.ExportFile(context.Background(), src, ns, pkg, portable.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	entries := readZipEntries(t, pkg)
	delete(entries, "atoms.jsonl")
	writeZipEntries(t, pkg, entries)

	rep, err := portable.Doctor(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK {
		t.Fatal("缺必需数据文件必须被查出")
	}
	if !problemsMention(rep.Problems, "atoms.jsonl") {
		t.Fatalf("问题清单里应点名缺失的文件，实得: %v", rep.Problems)
	}
	// 体检不通过 → 导入必须整体拒绝（导一半比不导更糟）。
	dst, err := sqlite.OpenMemory("portable-missing")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Close() }()
	if _, err := portable.Import(context.Background(), pkg, dst, store.Namespace("other")); err == nil {
		t.Fatal("体检不通过的包必须拒绝导入")
	}
}

// readZipEntries 读出包内全部条目。
func readZipEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = b
	}
	return out
}

// writeZipEntries 写回包内条目（清单放在最后，与导出侧一致）。
func writeZipEntries(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	names := make([]string, 0, len(entries))
	for n := range entries {
		if n != "manifest.json" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	write := func(name string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		write(n)
	}
	if b, ok := entries["manifest.json"]; ok {
		entries["manifest.json"] = b
		write("manifest.json")
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// problemsMention 问题清单里是否有提到某关键词。
func problemsMention(problems []string, kw string) bool {
	for _, p := range problems {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
}
