package cli_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// 本文件钉住 CLI 的**库管理面**。
//
// 这些命令的存在理由：内核里的生命周期/实体/情节能力**实现了却没有入口**时，
// 它们既不会被用，也不会被验证——而「部件造好了、链没连上」
// 是本仓最贵的一种病：它让人以为这些能力可用。
//
// 这条用例同时钉住一件事：这些命令**能真的跑起来**。
// 接线之前，pipeline/entity 与 pipeline/page 要的存储方法
// （按实体取事实卡）在存储层从未实现，两个包根本无法用真后端实例化。

// 库管理面全套：别名登记 → 列表 → 合并 → 情节 → 生命周期三件套 → 物理整理。
func TestAdminCommandsRun(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")

	code, out, errb := run(t, "capture", "--db", db, "--namespace", "t",
		"--content", "磁盘长期偏小需要清理", "--session", "s1")
	if code != 0 {
		t.Fatalf("capture 失败: %s %s", out, errb)
	}

	// 实体：登记别名（人确认「那台研发机」就是「机器甲」）
	code, out, errb = run(t, "entity", "alias", "--db", db, "--namespace", "t",
		"--entity", "机器甲", "--alias", "那台研发机", "--entity-type", "service")
	if code != 0 {
		t.Fatalf("entity alias 失败: %s %s", out, errb)
	}
	if !strings.Contains(out, "那台研发机") {
		t.Fatalf("别名登记输出不对: %q", out)
	}

	// 实体：列表
	code, out, errb = run(t, "entity", "list", "--db", db, "--namespace", "t")
	if code != 0 {
		t.Fatalf("entity list 失败: %s %s", out, errb)
	}
	if !strings.Contains(out, "机器甲") {
		t.Fatalf("entity list 应列出实体: %q", out)
	}

	// 生命周期三件套 + 物理整理：都必须能跑通（哪怕是空转）
	for _, c := range []string{"consolidate", "retention", "vacuum"} {
		code, out, errb = run(t, c, "--db", db, "--namespace", "t")
		if code != 0 {
			t.Fatalf("%s 失败: %s %s", c, out, errb)
		}
	}

	// 回收：按龄清理素材面
	code, out, errb = run(t, "gc", "--db", db, "--namespace", "t", "--before", "1h")
	if code != 0 {
		t.Fatalf("gc 失败: %s %s", out, errb)
	}
	if !strings.Contains(out, "回收") {
		t.Fatalf("gc 输出不对: %q", out)
	}

	// 情节：只读面，空库也不该报错
	code, out, errb = run(t, "episode", "list", "--db", db, "--namespace", "t")
	if code != 0 {
		t.Fatalf("episode list 失败: %s %s", out, errb)
	}
}

// 子命令缺参数时必须明确报用法，不能静默做别的事。
func TestAdminSubcommandNeedsArgument(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")

	code, _, errb := run(t, "entity")
	if code == 0 {
		t.Fatal("entity 缺子命令必须返回非零")
	}
	if !strings.Contains(errb, "子命令") {
		t.Fatalf("缺子命令要说清: %q", errb)
	}

	code, _, errb = run(t, "entity", "merge", "--db", db, "--namespace", "t")
	if code == 0 {
		t.Fatal("entity merge 缺 --to/--from 必须返回非零")
	}
	if !strings.Contains(errb, "--to") {
		t.Fatalf("缺参数要说清: %q", errb)
	}
}
