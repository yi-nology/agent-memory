package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// violation 一条门禁违规。
type violation struct {
	Rule string
	Path string
	Line int
	Text string
	Note string
}

func (v violation) String() string {
	at := fmt.Sprintf("%s:%d", v.Path, v.Line)
	if v.Text == "" {
		at += " (" + v.Rule + ")"
	} else {
		at += fmt.Sprintf(" [%s] %s", v.Rule, truncate(v.Text, 90))
	}
	if v.Note != "" {
		at += "  ← " + v.Note
	}
	return at
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// modulePrefix 本仓的模块导入前缀。
const modulePrefix = "github.com/yi-nology/agent-memory"

// run 执行全部规则与阈值，返回违规列表。
func run(p *policy, repoRoot string) ([]violation, error) {
	var out []violation

	exempt := map[string]bool{}
	for _, e := range p.Exceptions {
		exempt[e.Rule+"@"+e.Path+":"+strconv.Itoa(e.Line)] = true
	}

	// 逐文件扫描（规则）。
	fset := token.NewFileSet()
	for _, r := range p.Rules {
		for _, f := range collect(r, repoRoot) {
			b, err := os.ReadFile(f) // #nosec G304 -- f 来自本仓内的 walk
			if err != nil {
				return nil, err
			}
			violations, err := applyRule(fset, r, f, b)
			if err != nil {
				return nil, err
			}
			for _, v := range violations {
				if exempt[v.Rule+"@"+v.Path+":"+strconv.Itoa(v.Line)] {
					continue
				}
				out = append(out, v)
			}
		}
	}

	// 模块依赖方向。
	dv, err := checkDeps(p, fset, repoRoot)
	if err != nil {
		return nil, err
	}
	out = append(out, dv...)

	// 体量阈值。
	if p.Thresholds.MaxFileLines > 0 {
		for _, f := range collect(rule{Scope: p.Thresholds.ScanRoots}, repoRoot) {
			b, err := os.ReadFile(f) // #nosec G304
			if err != nil {
				return nil, err
			}
			if n := len(strings.Split(string(b), "\n")); n > p.Thresholds.MaxFileLines {
				out = append(out, violation{
					Rule: "threshold-file-lines",
					Path: relPath(f, repoRoot), Line: 1,
					Text: fmt.Sprintf("%d 行 > 上限 %d", n, p.Thresholds.MaxFileLines),
					Note: "文件长到某个程度就会有人把两件事塞进同一文件而只改一半",
				})
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Rule < out[j].Rule
	})
	return out, nil
}

// collect 按 scope/scope_exclude/ignore_test 收集待扫文件。
//
// scope 里的每一项既可能是目录（递归），也可能是单文件——规则声明时两种都
// 允许，免得为了点名一个文件就得把它单独列进 scan_roots。
func collect(r rule, repoRoot string) []string {
	var files []string
	seen := map[string]bool{}

	add := func(path string, info os.FileInfo) {
		if info.IsDir() || !strings.HasSuffix(path, ".go") || seen[path] {
			return
		}
		if r.IgnoreTest && strings.HasSuffix(path, "_test.go") {
			return
		}
		seen[path] = true
		files = append(files, path)
	}

	for _, sc := range r.Scope {
		s := filepath.Join(repoRoot, sc)
		st, err := os.Stat(s)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			add(s, st)
			continue
		}
		_ = filepath.Walk(s, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				base := filepath.Base(path)
				if base == ".git" || strings.HasPrefix(base, ".gomodcache") || strings.HasPrefix(base, ".gocache") {
					return filepath.SkipDir
				}
				for _, ex := range r.ScopeExclude {
					if base == ex {
						return filepath.SkipDir
					}
				}
				return nil
			}
			add(path, info)
			return nil
		})
	}

	sort.Strings(files)
	return files
}

// applyRule 对单个文件跑一条规则。
func applyRule(fset *token.FileSet, r rule, path string, content []byte) ([]violation, error) {
	var out []violation

	if r.ForbidRegex != nil {
		compiled := make([]*regexp.Regexp, 0, len(r.ForbidRegex))
		for _, re := range r.ForbidRegex {
			c, err := regexp.Compile(re)
			if err != nil {
				return nil, fmt.Errorf("规则 %s 的正则 %q 编译失败: %w", r.ID, re, err)
			}
			compiled = append(compiled, c)
		}
		follows := make([]*regexp.Regexp, 0, len(r.NegFollow))
		for _, nf := range r.NegFollow {
			c, err := regexp.Compile(nf.ForbidFollow)
			if err != nil {
				return nil, fmt.Errorf("规则 %s 的 negative_follow %q 编译失败: %w", r.ID, nf.ForbidFollow, err)
			}
			follows = append(follows, c)
		}
		lines := strings.Split(string(content), "\n")
		for i, line := range lines {
			for _, re := range compiled {
				if !re.MatchString(line) {
					continue
				}
				exempt := false
				for _, f := range follows {
					if i+1 < len(lines) && f.MatchString(lines[i+1]) {
						exempt = true
						break
					}
				}
				if exempt {
					continue
				}
				out = append(out, violation{Rule: r.ID, Path: path, Line: i + 1, Text: strings.TrimSpace(line)})
				break
			}
		}
	}

	if len(r.ForbidThirdPartyIn) > 0 {
		f, err := parser.ParseFile(fset, path, content, parser.ImportsOnly)
		if err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			if strings.HasPrefix(p, modulePrefix) || isStdlib(p) {
				continue
			}
			out = append(out, violation{
				Rule: r.ID, Path: path, Line: fset.Position(im.Pos()).Line, Text: p,
				Note: "核心层只准 stdlib 与本仓；第三方包只许出现在 storage/ 下",
			})
		}
	}

	if r.ForbidImport != "" {
		re, err := regexp.Compile(r.ForbidImport)
		if err != nil {
			return nil, fmt.Errorf("规则 %s 的 forbid_import_regex 编译失败: %w", r.ID, err)
		}
		f, err := parser.ParseFile(fset, path, content, parser.ImportsOnly)
		if err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			if re.MatchString(p) {
				out = append(out, violation{Rule: r.ID, Path: path, Line: fset.Position(im.Pos()).Line, Text: p})
			}
		}
	}
	return out, nil
}

// checkDeps 校验模块间的内部依赖方向。
func checkDeps(p *policy, fset *token.FileSet, repoRoot string) ([]violation, error) {
	var out []violation
	for _, m := range p.Modules {
		if !m.Managed {
			continue
		}
		dir := m.dir(repoRoot)
		if _, err := os.Stat(dir); err != nil {
			continue // 未落地的模块（policy 先行的正常状态）
		}
		// 嵌套的模块根归**它们自己**管：外层模块遍历时必须跳过，
		// 否则同一个文件会被算成两个模块的，依赖方向检查随之失真。
		nested := map[string]bool{}
		for _, other := range p.Modules {
			if other.ID == m.ID {
				continue
			}
			for _, root := range other.Roots {
				if len(root) > len(m.Roots) && strings.HasPrefix(root, m.Roots[0]+"/") {
					nested[root] = true
				}
			}
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				rel, rerr := filepath.Rel(dir, path)
				if rerr == nil && rerr == nil && nested[rel] {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			b, err := os.ReadFile(path) // #nosec G304
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, b, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, im := range f.Imports {
				imp := strings.Trim(im.Path.Value, `"`)
				if !strings.HasPrefix(imp, modulePrefix+"/") {
					continue
				}
				pkg := strings.TrimPrefix(imp, modulePrefix+"/")
				to, ok := p.moduleOf(pkg)
				if !ok || !p.allows(m.ID, to) {
					out = append(out, violation{
						Rule: "module-dependency-direction",
						Path: relPath(path, repoRoot),
						Line: fset.Position(im.Pos()).Line,
						Text: fmt.Sprintf("%s（%s → %s，requires=%v）", imp, m.ID, to, m.Requires),
					})
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// god object：按类型统计方法数。
	if p.Thresholds.MaxMethodsPerType > 0 {
		counts := map[string]int{}
		locs := map[string]string{}
		for _, sr := range p.Thresholds.ScanRoots {
			root := filepath.Join(repoRoot, sr)
			if _, err := os.Stat(root); err != nil {
				continue
			}
			_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				b, err := os.ReadFile(path) // #nosec G304
				if err != nil {
					return err
				}
				f, err := parser.ParseFile(fset, path, b, 0)
				if err != nil {
					return nil
				}
				for _, d := range f.Decls {
					fd, ok := d.(*ast.FuncDecl)
					if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
						continue
					}
					var names []string
					for _, field := range fd.Recv.List {
						switch t := field.Type.(type) {
						case *ast.StarExpr:
							if id, ok := t.X.(*ast.Ident); ok {
								names = append(names, id.Name)
							}
						case *ast.Ident:
							names = append(names, t.Name)
						}
					}
					for _, n := range names {
						counts[n]++
						if _, ok := locs[n]; !ok {
							locs[n] = fmt.Sprintf("%s:%d", relPath(path, repoRoot), fset.Position(fd.Pos()).Line)
						}
					}
				}
				return nil
			})
		}
		for name, n := range counts {
			if n > p.Thresholds.MaxMethodsPerType {
				out = append(out, violation{
					Rule: "threshold-god-object",
					Path: locs[name], Line: 0,
					Text: fmt.Sprintf("%s 有 %d 个方法 > 上限 %d", name, n, p.Thresholds.MaxMethodsPerType),
				})
			}
		}
	}
	return out, nil
}

// isStdlib 判定是否为标准库包：首段路径不含点即为标准库。
func isStdlib(p string) bool {
	first, _, ok := strings.Cut(p, "/")
	return !ok || !strings.Contains(first, ".")
}

func relPath(path, root string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return r
	}
	return path
}
