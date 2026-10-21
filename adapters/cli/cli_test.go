package cli_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yi-nology/agent-memory/adapters/cli"
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
