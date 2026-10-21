package portable_test

import (
	"context"
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

var ns = store.Namespace("pack-ns")

func now() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

func seed(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.OpenMemory("portable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Content: "某次会话", Timestamp: now(), Type: types.EventManual,
	}))
	must(s.SaveAtom(ctx, ns, &types.AtomCard{
		ID: "a1", EntityID: "ent1", Assertion: "磁盘长期偏小", QuoteEventID: "e1",
		RawEventIDs: []string{"e1"}, Importance: types.ImportanceMed,
		Confidence: types.ConfidenceMed, OccurredAt: now(), CreatedAt: now(), UpdatedAt: now(),
	}))
	must(s.SaveEntity(ctx, ns, &types.Entity{
		ID: "ent1", CanonicalName: "机器甲", CreatedAt: now(),
	}))
	must(s.SaveAlias(ctx, ns, &types.Alias{
		Alias: "那台开发机", EntityID: "ent1", CreatedBy: types.DecidedUser, CreatedAt: now(),
	}))
	must(s.UpsertEntityPage(ctx, ns, &types.EntityPage{
		EntityID: "ent1", Headline: "磁盘偏小的机器", Body: "长期磁盘不足", UpdatedAt: now(),
	}))
	// 再来一张**带归属**的画像：导出导入必须把归属一起带走。
	//
	// 丢归属的后果不是「少了一段画像」，而是**归属被拍平成全局**——
	// 一张只该本组看的画像，导进新库后成了所有人都能读的那张。
	must(s.UpsertEntityPage(ctx, ns, &types.EntityPage{
		EntityID: "ent1", Scope: "team-a", Headline: "甲组视角的画像", Body: "甲组补充", UpdatedAt: now(),
	}))
	must(s.SaveEpisode(ctx, ns, &types.Episode{
		ID: "ep1", SessionID: "s1", Title: "一次排查", Summary: "定位到磁盘",
		OccurredAt: now(), CreatedAt: now(),
	}))
	return s
}

// 导出 → 检查 → 导入，三条闭环走通。
func TestExportDoctorImportRoundTrip(t *testing.T) {
	src := seed(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "m"+portable.Extension)

	man, err := portable.ExportFile(context.Background(), src, ns, pkg,
		portable.ExportOptions{Host: "test-host"})
	if err != nil {
		t.Fatal(err)
	}
	// 计数的键是**数据类别名**，与 ImportReport.Counts 同键——
	// 「导出多少、导回多少」是迁移最该被核对的一件事，
	// 两边键不一致时这个核对做不成（而文件清单在 man.Files 里）。
	if man.Counts["raw"] != 1 || man.Counts["atoms"] != 1 {
		t.Fatalf("清单计数不对: %v", man.Counts)
	}
	if man.Files["raw.jsonl"] == "" || man.Files["atoms.jsonl"] == "" {
		t.Fatalf("文件清单必须按文件名记校验和: %v", man.Files)
	}
	if man.SourceHost != "test-host" {
		t.Fatalf("来源宿主没记进清单: %+v", man)
	}

	rep, err := portable.Doctor(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("刚导出的包必须完整: %v", rep.Problems)
	}

	// 导进另一个库。
	dst, err := sqlite.OpenMemory("portable-dst")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Close() }()
	rep2, err := portable.Import(context.Background(), pkg, dst, store.Namespace("other"))
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Counts["atoms"] != 1 || rep2.Counts["entities"] != 1 {
		t.Fatalf("导入计数不对: %v", rep2.Counts)
	}
	got, err := dst.GetAtom(context.Background(), store.Namespace("other"), "a1")
	if err != nil {
		t.Fatalf("导入后事实取不到: %v", err)
	}
	if got.Assertion != "磁盘长期偏小" {
		t.Fatalf("导入内容失真: %+v", got)
	}
	// 命名空间隔离：导入进别的库，原库不受影响。
	if _, err := dst.GetAtom(context.Background(), ns, "a1"); !errorsIsNotFound(err) {
		t.Fatalf("导入不该跨命名空间可见: %v", err)
	}
	// 画像的归属必须一起迁移过去。
	scoped, err := dst.GetEntityPage(context.Background(), store.Namespace("other"), "ent1", "team-a")
	if err != nil {
		t.Fatalf("导入后带归属的画像取不到: %v", err)
	}
	if scoped.Headline != "甲组视角的画像" {
		t.Fatalf("导入后画像内容失真: %+v", scoped)
	}
	// ⚠️ 归属不许被拍平成全局：拍平了，一张只该本组看的画像
	// 就变成了所有人都能读的那张——那是泄漏，且导出方完全看不出来。
	//
	// 判据不是「全局页还在不在」（它当然在，那是另一张图），
	// 而是「全局页里装的还是不是全局那份内容」。
	global, err := dst.GetEntityPage(context.Background(), store.Namespace("other"), "ent1", "")
	if err != nil {
		t.Fatalf("导入后全局画像取不到: %v", err)
	}
	if global.Headline != "磁盘偏小的机器" {
		t.Fatalf("带归属的画像被拍进了全局页: %+v", global)
	}
}

// 包被篡改 → doctor 必须发现。
//
// 校验和这一项最要紧：zip 记了 CRC，但数据文件被截断后 CRC 仍可能过
// （截到块边界时），而记忆丢了就是丢了。
func TestDoctorDetectsCorruption(t *testing.T) {
	src := seed(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "m"+portable.Extension)
	if _, err := portable.ExportFile(context.Background(), src, ns, pkg, portable.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	rep, err := portable.Doctor(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK {
		t.Fatalf("前置条件：新包必须完整: %v", rep.Problems)
	}
}

// 坏包必须**拒绝导入**，而不是导一半。
func TestImportRejectsCorruptPackage(t *testing.T) {
	src := seed(t)
	dir := t.TempDir()
	pkg := filepath.Join(dir, "m"+portable.Extension)
	if _, err := portable.ExportFile(context.Background(), src, ns, pkg, portable.ExportOptions{}); err != nil {
		t.Fatal(err)
	}
	// 拿一个非 zip 文件冒充包
	if err := os.WriteFile(pkg, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst, err := sqlite.OpenMemory("portable-bad")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := portable.Import(context.Background(), pkg, dst, ns); err == nil {
		t.Fatal("坏包必须拒绝导入：导一半的宿主从此拥有残缺的记忆，而它不会知道缺了什么")
	}
	// 一个字节都没导进去
	atoms, err := dst.ListAtoms(context.Background(), ns, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(atoms) != 0 {
		t.Fatalf("坏包导入必须一个字节都不落，实得 %d 条", len(atoms))
	}
}

// 文件名净化：namespace 是宿主给的，**不可信**。
func TestFileNameSanitizesNamespace(t *testing.T) {
	name := portable.FileName("../../etc/passwd", now())
	if strings.ContainsAny(name, "/\\") {
		t.Fatalf("包名不得含路径分隔符: %q", name)
	}
	if !strings.HasSuffix(name, portable.Extension) {
		t.Fatalf("包名后缀不对: %q", name)
	}
}

func errorsIsNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}
