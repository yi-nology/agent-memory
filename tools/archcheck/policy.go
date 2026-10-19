// archcheck — agent-memory 的架构门禁执行器。
//
// 本文件描述 policy 文件的 schema：模块清单、规则、阈值、例外登记。
//
// 为什么不自己写 YAML 解析：门禁工具的依赖面与**核心**的依赖面必须分开——
// 核心仓的 go.mod 刻意保持 require 段为空（「零核心依赖」要能被 `go list -m all`
// 结构性验证，而不是靠嘴说）。所以本工具是独立模块
// （tools/archcheck/go.mod），只依赖一个 YAML 解析库。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// negFollow 负向后续约束：命中 forbid_regex 后还要看下一行是否落在
// forbid_follow 里，是则**豁免**（不报）。
//
// 用途：给「确实需要出现这个词的地方」一条合法出口，且要求出口写明条件。
// 没有它，例外就只能靠整条 nolint 跳过——那会把规则附近的真问题一起放过。
type negFollow struct {
	Anchor       string `yaml:"anchor"`
	ForbidFollow string `yaml:"forbid_follow"`
}

// allowEntry 核心层第三方白名单的一条登记。
//
// # 为什么白名单必须带四个字段
//
// 一个光秃秃的路径列表是最容易变成死条款的东西：半年后没人记得它为什么
// 在那里，也没人说得清它覆盖了哪个包、什么时候该撤。故每条都必须写明
// 「放行什么、为什么、到什么时候、什么条件下拆掉」——缺一条就在加载期报错，
// 而不是默默放行。
type allowEntry struct {
	// Path 放行的 import 路径前缀（精确仓路径或其中的子包）。
	Path string `yaml:"path"`
	// Reason 为什么放行（写进报告，半年后还有人看得懂）。
	Reason string `yaml:"reason"`
	// Until 复核日期（YYYY-MM-DD）。到期即让门禁变红，逼人处理。
	Until string `yaml:"until"`
	// Removal 拆除条件：什么情况下这条白名单应该消失。
	Removal string `yaml:"removal"`
	// Paths 允许引用的**仓内目录**前缀。留空 = 该规则作用域内任意目录。
	//
	// 留空不是省事，是**放弃了这道粒度**：白名单项就会在 8 个核心目录里
	// 到处可用，包括作为地基的 types。指定了它，policy 自己就说清了
	// 「这个包只许谁用」。
	Paths []string `yaml:"paths"`
}

// rule 一条检测规则。
type rule struct {
	ID           string      `yaml:"id"`
	Desc         string      `yaml:"desc"`
	Severity     string      `yaml:"severity"` // error（默认，非零退出）| warn（仅报告）
	Scope        []string    `yaml:"scope"`
	ScopeExclude []string    `yaml:"scope_exclude"`
	IgnoreTest   bool        `yaml:"ignore_test"`
	ForbidImport string      `yaml:"forbid_import_regex"` // 打在 import 路径上
	ForbidRegex  []string    `yaml:"forbid_regex"`        // 打在整行文本上（含注释）
	NegFollow    []negFollow `yaml:"negative_follow"`

	// AllowThirdParty 放行到核心层的第三方包（见 allowEntry）。
	//
	// 它是「核心零依赖」这条纪律的**受控开口**，不是它的废除：默认仍然
	// 只许 stdlib 与本仓，每一条放行都必须逐条登记理由与到期日。
	AllowThirdParty []allowEntry `yaml:"allow_third_party"`

	// ForbidThirdPartyIn 列出「只准 stdlib 与本仓」的目录前缀。
	//
	// 为什么不直接查 go.mod 的 require 段：require 是**整个模块**的账，
	// 而本仓的纪律是「核心零依赖、第三方只许出现在存储层」——同一个 go.mod 里
	// 两种纪律并存是可接受的，判据得落在 import 面上。
	//
	// stdlib 判据用「首段路径不含点」：github.com/x、golang.org/x、
	// modernc.org/x 都含点，context/encoding/json 不含。这是 Go 社区通用启发式，
	// 与 `go list std` 的一致性足够，且不引依赖。
	ForbidThirdPartyIn []string `yaml:"forbid_third_party_in"`
}

// threshold 体量阈值。写入方只被告警不自动拆——拆分是人的决定。
type threshold struct {
	MaxFileLines      int      `yaml:"max_file_lines"`
	MaxMethodsPerType int      `yaml:"max_methods_per_type"`
	ScanRoots         []string `yaml:"scan_roots"`
	Enforce           bool     `yaml:"enforce"`
}

// policyException 例外登记。
//
// 纪律：例外必须带 reason 与 removal（拆除条件）且注明 until（何时复核）。
// 无期限的例外等于规则失效。
type policyException struct {
	Rule      string `yaml:"rule"`
	Path      string `yaml:"path"`
	Line      int    `yaml:"line"`
	Violation string `yaml:"violation"`
	Reason    string `yaml:"reason"`
	Until     string `yaml:"until"`
	Removal   string `yaml:"removal"`
}

// moduleDecl 模块清单的一条登记：roots 定位包目录，requires 声明**允许**的
// 内部依赖方向。
//
// 为什么这段必须被真的执行：requires 是一张拓扑声明，写错或漏写时它不会自己
// 报警——半年后没人知道它已经不成立，而依赖缠绕恰好是最难靠 review 发现的债
// 类型（零报错、零告警、构建全绿）。
type moduleDecl struct {
	ID       string   `yaml:"id"`
	Roots    []string `yaml:"roots"`
	Requires []string `yaml:"requires"`
	Owner    string   `yaml:"owner"`
	Managed  bool     `yaml:"managed"`
	Notes    string   `yaml:"notes"`
}

type policy struct {
	Version    int               `yaml:"version"`
	Modules    []moduleDecl      `yaml:"modules"`
	Rules      []rule            `yaml:"rules"`
	Thresholds threshold         `yaml:"thresholds"`
	Exceptions []policyException `yaml:"exceptions"`
}

// loadPolicy 读入并做最小校验。
//
// 「policy 写错了」必须**响亮地**失败，而不是被当成「规则为空所以全过」——
// 后者会让门禁在某次改配置后静默失效，而没人会发现。
func loadPolicy(path string) (*policy, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- 路径由命令行给出
	if err != nil {
		return nil, fmt.Errorf("读 policy 失败: %w", err)
	}
	var p policy
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("解析 policy 失败: %w", err)
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("policy 版本 %d 不受支持（本工具只认 1）", p.Version)
	}
	if len(p.Modules) == 0 {
		return nil, fmt.Errorf("policy 的 modules 段为空：门禁会退化成'什么都不检查'")
	}
	if len(p.Thresholds.ScanRoots) == 0 {
		return nil, fmt.Errorf("policy 的 thresholds.scan_roots 为空")
	}
	seen := map[string]bool{}
	for _, m := range p.Modules {
		if m.ID == "" {
			return nil, fmt.Errorf("存在无 id 的模块登记")
		}
		if seen[m.ID] {
			return nil, fmt.Errorf("模块 id 重复: %s", m.ID)
		}
		seen[m.ID] = true
	}
	if err := validateAllowlist(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// validateAllowlist 校验白名单登记。
//
// # 校验面为什么这么严
//
// 白名单是唯一能让第三方包进核心的口子，而它一旦写错，失效形态是**静默**：
// 路径写错 → 放行不生效（还好，只是没放行）；理由/到期日写漏 → 放行生效
// 且永远不过期（**危险的那侧**）。所以缺字段一律在加载期响亮失败，
// 而不是先跑起来再说。
func validateAllowlist(p *policy) error {
	for _, r := range p.Rules {
		if len(r.AllowThirdParty) == 0 {
			continue
		}
		// 有白名单却没执行体 = 白名单是死条款：写的人以为放行了，
		// 实际上那条规则根本不存在。必须响亮失败。
		if len(r.ForbidThirdPartyIn) == 0 {
			return fmt.Errorf("规则 %s 登记了 allow_third_party，但它没有 forbid_third_party_in 执行体——白名单无从生效", r.ID)
		}
		seen := map[string]bool{}
		for _, e := range r.AllowThirdParty {
			switch {
			case e.Path == "":
				return fmt.Errorf("规则 %s 的白名单条目缺 path", r.ID)
			case e.Reason == "":
				return fmt.Errorf("规则 %s 的白名单条目 %s 缺 reason（半年后没人看得懂它为什么在）", r.ID, e.Path)
			case e.Until == "":
				return fmt.Errorf("规则 %s 的白名单条目 %s 缺 until（无期限的例外等于规则失效）", r.ID, e.Path)
			case e.Removal == "":
				return fmt.Errorf("规则 %s 的白名单条目 %s 缺 removal（拆除条件）", r.ID, e.Path)
			}
			if _, err := time.Parse("2006-01-02", e.Until); err != nil {
				return fmt.Errorf("规则 %s 的白名单条目 %s 的 until 不是 YYYY-MM-DD: %q", r.ID, e.Path, e.Until)
			}
			if seen[e.Path] {
				return fmt.Errorf("规则 %s 的白名单路径重复: %s", r.ID, e.Path)
			}
			seen[e.Path] = true
		}
	}
	return nil
}

// allowExpired 判断白名单条目是否已过期。
//
// 比较用**日期字符串**而不是时间点：until 写的是「复核到哪一天」，
// 那天当天仍算有效，次日才过期。now 由调用方注入——
// 否则这条判据在测试里不可控。
func allowExpired(now time.Time, e allowEntry) bool {
	return now.UTC().Format("2006-01-02") > e.Until
}

// moduleOf 判断某个仓内包路径属于哪个模块（最长根前缀优先）。
func (p *policy) moduleOf(pkgPath string) (string, bool) {
	best, bestLen := "", -1
	for _, m := range p.Modules {
		for _, root := range m.Roots {
			if pkgPath == root || len(pkgPath) > len(root) && pkgPath[:len(root)] == root && pkgPath[len(root)] == '/' {
				if len(root) > bestLen {
					best, bestLen = m.ID, len(root)
				}
			}
		}
	}
	return best, bestLen >= 0
}

// allows 判断 from 模块是否允许依赖 to 模块。
func (p *policy) allows(from, to string) bool {
	if from == to {
		return true
	}
	for _, m := range p.Modules {
		if m.ID != from {
			continue
		}
		for _, r := range m.Requires {
			if r == to {
				return true
			}
		}
	}
	return false
}

func (m moduleDecl) dir(repoRoot string) string {
	return filepath.Join(repoRoot, filepath.Join(m.Roots...))
}
