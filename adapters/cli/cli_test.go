package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/adapters/cli"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	code := cli.Run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// 捕获 → 召回 → 统计，一条闭环。
func TestCaptureRecallStats(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")

	code, out, errb := run(t, "capture", "--db", db, "--namespace", "t",
		"--content", "磁盘长期偏小需要清理", "--session", "s1")
	if code != 0 {
		t.Fatalf("capture 失败: %s %s", out, errb)
	}
	if !strings.Contains(out, "已收录") {
		t.Fatalf("capture 输出不对: %q", out)
	}

	code, out, _ = run(t, "recall", "--db", db, "--namespace", "t", "磁盘")
	if code != 0 {
		t.Fatalf("recall 失败: %s", out)
	}
	if !strings.Contains(out, "磁盘") {
		t.Fatalf("recall 没召回内容: %q", out)
	}

	code, out, _ = run(t, "stats", "--db", db, "--namespace", "t")
	if code != 0 {
		t.Fatalf("stats 失败: %s", out)
	}
	if !strings.Contains(out, "RawEvents") {
		t.Fatalf("stats 输出不对: %q", out)
	}
}

// CLI 同样必须打码——命令行是宿主最容易绕过 capture 的入口。
func TestCaptureOverCLIRedacts(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	code, out, errb := run(t, "capture", "--db", db, "--namespace", "t",
		"--content", "凭据是 "+secret)
	if code != 0 {
		t.Fatalf("capture 失败: %s %s", out, errb)
	}
	if !strings.Contains(out, "脱敏=true") {
		t.Fatalf("CLI 捕获必须打码: %q", out)
	}
}

// 导出 → 检查 → 导入。
func TestExportImportDoctor(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")
	pkg := filepath.Join(dir, "p.hmpkg")

	code, out, errb := run(t, "capture", "--db", db, "--namespace", "t", "--content", "某条素材")
	if code != 0 {
		t.Fatalf("capture 失败: %s %s", out, errb)
	}
	code, out, errb = run(t, "export", "--db", db, "--namespace", "t", "--out", pkg)
	if code != 0 {
		t.Fatalf("export 失败: %s %s", out, errb)
	}
	code, out, errb = run(t, "doctor", "--pkg", pkg)
	if code != 0 {
		t.Fatalf("doctor 失败: %s %s", out, errb)
	}
	db2 := filepath.Join(dir, "m2.sqlite")
	code, out, errb = run(t, "import", "--db", db2, "--namespace", "t2", "--pkg", pkg)
	if code != 0 {
		t.Fatalf("import 失败: %s %s", out, errb)
	}
	code, out, _ = run(t, "stats", "--db", db2, "--namespace", "t2")
	if code != 0 || !strings.Contains(out, "RawEvents") {
		t.Fatalf("导入后计数不对: %q", out)
	}
}

// 未知命令与缺参数：退出码 2（用法错），不是 1（运行失败）。
func TestUsageErrorsExitWith2(t *testing.T) {
	if code, _, _ := run(t, "nope"); code != 2 {
		t.Fatalf("未知命令应退出 2，实得 %d", code)
	}
	if code, _, _ := run(t, "capture", "--content", ""); code != 2 {
		t.Fatalf("缺 content 应退出 2，实得 %d", code)
	}
	if code, _, _ := run(t, "doctor"); code != 2 {
		t.Fatalf("缺 pkg 应退出 2，实得 %d", code)
	}
}

// 查询理解的自查口：一句问句进去，翻译结果出来。
//
// 它是**诊断**命令：不动库、不召回，只回答「这句被理解成了什么」。
// 断言只钉「有输出且含时间窗」——刻意不钉具体时刻
// （那会把用例绑在「此刻」上，而它天然依赖当下时刻）。
func TestQueryCommandPrintsPlan(t *testing.T) {
	code, out, errOut := run(t, "query", "上周那个问题解决了吗")
	if code != 0 {
		t.Fatalf("query 命令应成功: %d %s", code, errOut)
	}
	if !strings.Contains(out, "Time") {
		t.Fatalf("输出里应看得见时间窗: %s", out)
	}
}

// memory reindex 给存量事实补算向量。
//
// # 为什么要有它
//
// 向量是**写路径**产生的（晋升成事实时顺手写），
// 于是**存量**事实没有向量——而语义召回靠索引，
// 索引里没有它们就一条都捞不到。症状是「语义轨像是没生效」，
// 而查不出原因（它确实在跑，只是索引是空的）。
//
// # 夹具不走提炼链
//
// 提炼与晋升要模型，而模型没配时那条链跑不起来——
// 用它当夹具会让本用例**整个跳过**，而跳过的用例不证明任何东西。
// 故直接写事实卡（这正是「存量事实」的样子：早就入库了，没有向量）。
//
// 判据：补算后索引里有向量，且**再跑一遍结果一样**（幂等）。
func TestReindexFillsVectors(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")

	// 先造一条「存量事实」：直接入库，不经晋升（故没有向量）。
	st, err := sqlite.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.SaveAtom(context.Background(), store.Namespace("t"), &types.AtomCard{
		ID: "a-old", EntityID: "e1", Assertion: "磁盘长期偏小需要清理",
		QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
		Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
		OccurredAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errb := run(t, "reindex", "--db", db, "--namespace", "t")
	if code != 0 {
		t.Fatalf("reindex 失败: %s %s", out, errb)
	}
	if !strings.Contains(out, "已补算 1") {
		t.Fatalf("reindex 输出不对: %q", out)
	}
	// 幂等：再跑一遍，条数一样。
	code, out2, errb := run(t, "reindex", "--db", db, "--namespace", "t")
	if code != 0 {
		t.Fatalf("第二次 reindex 失败: %s %s", out2, errb)
	}
	if out != out2 {
		t.Fatalf("reindex 应幂等，两次输出不同: %q vs %q", out, out2)
	}
}

// memory expire 按龄退役**事件类**事实；长期经验与状态不受影响。
//
// 这条钉的是「按类别分治」：一套策略套所有事实必然两头不讨好
// （事件被永久留着污染召回，经验被按时间淘汰丢掉知识）。
func TestExpireRetiresOnlyEvents(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")

	// 造两条事实卡：一条事件类（很老）、一条长期经验（同样很老）。
	st, err := sqlite.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -60)
	mk := func(id, kind string) *types.AtomCard {
		return &types.AtomCard{
			ID: id, EntityID: "e1", Assertion: "磁盘 清理 " + id, Kind: types.FactKind(kind),
			QuoteEventID: "ev1", RawEventIDs: []string{"ev1"},
			Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
			OccurredAt: old, CreatedAt: old, UpdatedAt: old,
		}
	}
	for _, a := range []*types.AtomCard{mk("a-event", "event"), mk("a-lesson", "lesson")} {
		if err := st.SaveAtom(context.Background(), store.Namespace("t"), a); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errb := run(t, "expire", "--db", db, "--namespace", "t", "--days", "30")
	if code != 0 {
		t.Fatalf("expire 失败: %s %s", out, errb)
	}
	if !strings.Contains(out, "已按龄退役 1 张") {
		t.Fatalf("只应退役那一条事件类事实: %q", out)
	}

	// 长期经验不该被按龄退役（它不因时间失效）。
	st2, err := sqlite.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	lesson, err := st2.GetAtom(context.Background(), store.Namespace("t"), "a-lesson")
	if err != nil {
		t.Fatalf("长期经验不该被退役: %v", err)
	}
	if !lesson.Active() {
		t.Fatal("长期经验不该被按龄退役")
	}
}
