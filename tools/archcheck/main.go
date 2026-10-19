// archcheck 主程序：读 policy → 扫仓 → 报违规 → 非零退出。
//
// # 为什么要自己维护一个门禁而不是靠 review
//
// 三条纪律都是「写错了也不会自己报警」的类型：
//   - 领域词渗入内核（只多了一个变量名，编译全绿）；
//   - 模块依赖方向写反（多一个 import，零报错零告警）；
//   - god object 重新长起来（方法一个个加，每次都有正当理由）。
//
// 它们唯一可靠的拦截时机是提交前自动执行。本工具就是那个时机。
//
// 用法：
//
//	go run ./tools/archcheck            # 在仓根执行
//	go run ./tools/archcheck -policy x.yaml -root .
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func main() {
	var (
		policyPath = flag.String("policy", "architecture-policy.yaml", "policy 文件路径")
		root       = flag.String("root", ".", "仓根目录（相对路径按 cwd 解析）")
		quiet      = flag.Bool("quiet", false, "只输出结论行")
	)
	flag.Parse()

	p, err := loadPolicy(*policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archcheck: policy 不可用:", err)
		os.Exit(2)
	}

	violations, err := run(p, *root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "archcheck: 扫描失败:", err)
		os.Exit(2)
	}

	if len(violations) == 0 {
		fmt.Printf("archcheck: 通过（模块 %d / 规则 %d / 例外 %d）\n",
			len(p.Modules), len(p.Rules), len(p.Exceptions))
		return
	}

	byRule := map[string]int{}
	for _, v := range violations {
		byRule[v.Rule]++
	}
	if !*quiet {
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  "+v.String())
		}
		fmt.Fprintln(os.Stderr)
	}

	rules := make([]string, 0, len(byRule))
	for r := range byRule {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, fmt.Sprintf("%s×%d", r, byRule[r]))
	}
	fmt.Fprintf(os.Stderr, "archcheck: %d 处违规 —— %s\n",
		len(violations), strings.Join(parts, " "))
	os.Exit(1)
}
