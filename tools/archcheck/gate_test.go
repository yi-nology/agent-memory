package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 门禁自身的测试。
//
// # 为什么必须有这组用例
//
// 门禁最大的失败模式不是「误报」，是**假通过**：规则写错了、scope 写偏了、
// 正则写崩了，工具一声不吭地退出 0。半年后有人往内核里塞了一整套运维词表，
// 提交照样绿。
//
// 所以这里不测「门禁在好代码上通过」，测的是**门禁在坏代码上必须红**——
// 只有会红这件事被钉住了，「在好代码上通过」才有意义。

const repoPolicy = "../../architecture-policy.yaml"

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loadRepoPolicy 读真实仓的 policy（门禁必须对**真实配置**生效，
// 用测试内造的 policy 测等于自己骗自己）。
func loadRepoPolicy(t *testing.T) *policy {
	t.Helper()
	p, err := loadPolicy(repoPolicy)
	if err != nil {
		t.Fatalf("真实 policy 必须可加载: %v", err)
	}
	return p
}

func TestGateGoesRedOnDomainVocabulary(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	writeFile(t, root, "types/x.go", `package types

// 这台主机的巡检结论 —— 门禁必须抓住「主机」「巡检」
type Note struct{}
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	hit := 0
	for _, v := range vs {
		if v.Rule == "memory-no-domain-vocabulary" {
			hit++
		}
	}
	if hit == 0 {
		t.Fatal("域词代码必须让门禁变红：这是门禁唯一的有效性证明")
	}
}

// 注释里的域词同样要拦——只扫标识符的检查会被注释里的词表绕过去，
// 而那是最常见的渗入路径。
func TestGateGoesRedOnDomainWordInComment(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	writeFile(t, root, "types/x.go", `package types

// 目标类型沿用宿主的集群分类（词表在宿主侧）。
type Kind string
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "memory-no-domain-vocabulary" {
			return
		}
	}
	t.Fatal("注释里的域词必须让门禁变红")
}

// 测试数据同样计入（ignore_test=false 是刻意的：夹具里的领域词表也是词表）。
func TestGateGoesRedOnDomainWordInTestFile(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	writeFile(t, root, "types/x_test.go", `package types

var 样例目标 = []string{"主机A", "主机B"}
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "memory-no-domain-vocabulary" {
			return
		}
	}
	t.Fatal("测试数据里的域词必须让门禁变红")
}

// 记忆内核不得依赖编排内核。两者是平级，不是父子。
func TestGateGoesRedOnHarnessImport(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	writeFile(t, root, "pipeline/recall/x.go", `package recall

import "github.com/yi-nology/agent-harness/protocol"

func F() {}
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "memory-no-harness-dependency" {
			return
		}
	}
	t.Fatal("import 编排内核必须让门禁变红")
}

// 模块依赖方向：types 是地基，任何模块都允许 import 它；
// 但反向（types import store）必须红——这条箭头写反时零编译错误。
func TestGateGoesRedOnReversedModuleDependency(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	writeFile(t, root, "types/x.go", `package types

import "github.com/yi-nology/agent-memory/store"

var _ = store.Namespace("")
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "module-dependency-direction" {
			return
		}
	}
	t.Fatal("模块依赖方向写反必须让门禁变红")
}

// 允许的方向必须放行。门禁只有会红还不够——满屏红会逼人整体关掉它，
// 而那等于没有门禁。
func TestGateAllowsDeclaredDirection(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	writeFile(t, root, "store/x.go", `package store

import "github.com/yi-nology/agent-memory/types"

var _ types.AtomCard
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "module-dependency-direction" {
			t.Fatalf("声明允许的依赖方向不得报错: %s", v)
		}
	}
}

// 假红是门禁的头号死法：一次假红之后，团队就开始习惯性绕它。
// 本仓有三个与领域同形、但确属正统概念的词——它们必须**永远**放行。
func TestGateDoesNotFireOnLegitimateLoadBearingWords(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	// 注意这里**不能**出现任何被禁的词——连「解释禁词的那行注释」都会被拦。
	// 这条限制是真的（首轮实跑已撞到），所以本用例只写承重词本身：
	// 产出方标识、记忆命名空间、记忆树节点。
	writeFile(t, root, "store/x.go", `package store

// Namespace 记忆命名空间：同一后端内相互隔离的一组记忆。
type S struct {
	Host      string
	Namespace string
	node      int
}
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "memory-no-domain-vocabulary" {
			t.Fatalf("承重词不得误伤（这条一旦假红，门禁就会被整体绕开）: %s", v)
		}
	}
}

// 方言 SQL 只许出现在 storage/ 下。换后端时 SQL 会跟着漏出去，
// 而漏出去的那几处永远没人记得回来改。
func TestGateGoesRedOnSQLOutsideStorage(t *testing.T) {
	p := loadRepoPolicy(t)
	root := t.TempDir()

	writeFile(t, root, "pipeline/recall/x.go", `package recall

const q = "SELECT id FROM atoms WHERE active = 1"
`)

	vs, err := run(p, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		if v.Rule == "memory-store-single-source" {
			return
		}
	}
	t.Fatal("pipeline 层的 SQL 必须让门禁变红")
}

// 本仓自己必须干净。这条用例是 CI 的最后一道：它把「仓里没有违规」
// 变成随每次提交自动复核的断言，而不是靠人记得跑。
func TestRepoItselfIsClean(t *testing.T) {
	p := loadRepoPolicy(t)
	vs, err := run(p, "../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) > 0 {
		var b strings.Builder
		for _, v := range vs {
			b.WriteString("\n  " + v.String())
		}
		t.Fatalf("本仓存在门禁违规:%s", b.String())
	}
}
