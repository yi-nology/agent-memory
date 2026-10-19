package ports

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// ErrNoRedactor 未注入脱敏器时的哨兵。
var ErrNoRedactor = errors.New("ports: no redactor injected")

// Surface 需要处置的内容面。
//
// **本枚举不认识任何宿主领域**。它说的是「这段文本以什么形态出现」，
// 而不是「这段文本关于什么」——后者是宿主的知识，写进本包就成领域词了。
type Surface string

const (
	// SurfaceBody 事件正文。
	SurfaceBody Surface = "body"
	// SurfacePayload 附加载荷里的字符串值（工具产出、命令回显常在这里）。
	SurfacePayload Surface = "payload"
	// SurfaceQuote 逐字引用。
	//
	// 单独成一个面，是因为引用会原样进入事实卡并长期驻留——它是**留存量**，
	// 而正文至少会被时间线冲走。
	SurfaceQuote Surface = "quote"
)

// RedactRequest 一次脱敏请求。
type RedactRequest struct {
	// Surface 内容面。
	Surface Surface
	// Text 待处置文本。
	Text string
	// Namespace 所属记忆库（供宿主按库配不同策略）。
	Namespace string
	// EventID 来源事件标识（供审计：要知道是谁被打码了）。
	EventID string
}

// Redactor 脱敏端口。
//
// # 为什么它是**必需依赖**
//
// 记忆一旦跨会话持久化并可导出成包，它就是一份**留存的、可能被复制出去的**资产。
// 方案里记录的真实现状是：宿主现有实现把会话终稿蒸馏出的断言与**逐字引用**
// 原样落库，且那条路径上没有脱敏接线（见方案 §7.1）。
//
// 记忆内核不认识宿主的敏感面，**但它能强制宿主表态**：构造捕获器时不提供
// 脱敏器就直接失败，好过每个宿主各自忘记一次。方向与 Bander 一致——
// 缺失的默认值不是「宽容」，是「事故」。
type Redactor interface {
	// Redact 返回处置后的文本。
	Redact(ctx context.Context, req RedactRequest) (string, error)
}

// ── 兵底实现 ──

// 结构性敏感信息：与领域无关、只看形态就能认出来的那一类。
//
// 收录标准三条同时成立：① 不认识任何领域词；② 误伤的代价只是少几个字符；
// ③ 漏判的代价不可逆（凭据一旦落库就已经泄漏了）。
var structuralPatterns = []struct {
	name string
	re   *regexp.Regexp
	keep int // 保留前后各几个字符作为定位线索
}{
	// URL 里内嵌的凭据：https://user:pass@host
	{"url-credential", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), 0},
	// key=value / key: value 形态的敏感赋值。键名只收通用词。
	{"secret-assignment", regexp.MustCompile(`(?i)\b(token|password|passwd|secret|api[_-]?key|access[_-]?key|private[_-]?key|credential)\b\s*[=:]\s*"?([^\s"',;]{4,})"?`), 0},
	// 裸长十六进制串（≥32 位）：密钥/令牌的典型形态
	{"long-hex", regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), 4},
	// 私钥块
	{"private-key-block", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), 0},
	// 已知凭据前缀族：形态确定、零误伤，且这一类**一旦落库即不可逆**。
	//
	// 这一条是被用例逼出来的：初版只收 key=value 与 AWS 前缀，于是
	// 「部署用的访问凭据是 ghp_XXXX」这种**散文形态**整个漏掉——而它恰是
	// 最常见的一种。兵底漏掉不可逆的那一类，兵底就没有意义。
	{"known-token-prefix", regexp.MustCompile(
		`\b(ghp_|gho_|ghs_|ghr_|github_pat_|glpat-|xoxb-|xoxp-|xoxa-|` +
			`sk-|sk_live_|sk_test_|AKIA|ASIA|AIza)[A-Za-z0-9_\-]{16,}\b`), 0},
}

// PatternRedactor 结构性敏感信息的兵底处置。
//
// # 这是**地板不是天花板**
//
// 它只认形态、不认语义：机器名、账号、业务编号一律放过。因此宿主**必须**
// 在它之上叠自己的规则（人名、工单号、内部地址……那些是领域知识，写进本包
// 会被门禁拦）。
//
// 它存在的意义是：宿主没配自己的规则时，**凭据那一类仍然不出库**——
// 而凭据是唯一一类「一旦落库即不可逆」的（其他内容删了就没了，凭据一旦进了
// 备份与导出包，要靠轮换来补救，而轮换往往来不及）。
type PatternRedactor struct{}

// Redact 逐条形态匹配并替换。
func (PatternRedactor) Redact(_ context.Context, req RedactRequest) (string, error) {
	out := req.Text
	for _, p := range structuralPatterns {
		out = p.re.ReplaceAllStringFunc(out, func(m string) string {
			return maskMatch(p.name, m, p.keep)
		})
	}
	return out, nil
}

// maskMatch 把一处命中替换成可定位但不可复原的掩码。
//
// 保留首尾少量字符是刻意的：全量打码会让处置后的文本失去定位能力，
// 用户问「我那条凭据呢」时没人能指出是哪一条；而掩码必须**不可复原**——
// 所以保留位数一律封顶 4，不随原值长度变化。
func maskMatch(kind, match string, keep int) string {
	r := []rune(match)
	if len(r) <= keep*2+1 {
		return "«" + kind + ":" + mask(len(r)) + "»"
	}
	head := string(r[:keep])
	tail := string(r[len(r)-keep:])
	return head + "«" + kind + ":" + mask(len(r)-keep*2) + "»" + tail
}

func mask(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("*", n)
}

// RedactionApplied 判断处置是否发生过。
//
// 捕获面用它决定要不要把 Provenance.Redacted 置真——那个位是给审计看的
// （导出、对外共享时按它决定能不能带出去），不是给判定逻辑看的。
func RedactionApplied(before, after string) bool { return before != after }

// SanitizeURLUserinfo 单独处置 URL 里的 userinfo。
//
// 单独导出是因为它有个坑：**必须按 URL 解析而不是正则切**——正则分不清
// `user@host/path` 与 `user:pass@host/path`，会把没有凭据的地址也打码。
// 正则版已收进 structuralPatterns 只处理**带冒号**的那种，故不会误伤。
func SanitizeURLUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	if _, hasPass := u.User.Password(); !hasPass {
		return raw
	}
	u.User = url.UserPassword(u.User.Username(), mask(8))
	return u.String()
}
