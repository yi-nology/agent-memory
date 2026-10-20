package ports

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/oagentkit/guardx/logredact"
	"github.com/oekit/pkg/stringx"
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
//
// # 类型名（kind）有一条硬约束
//
// **类型名里不许出现敏感键名表里的任何一个词**——掩码形如 `«kind:****»`，
// 而 kind 后面跟着冒号：若 kind 里含 `credential`，那么 secret-assignment
// 那条规则（键名表里有 credential）会**把自己的掩码再打码一次**，产出
// `«url-«secret-assignment:****»»` 这种嵌套形态。
//
// 后果不是泄漏（明文照样没了），而是**审计失真**：读掩码的人看到的是
// 「secret-assignment」，而实际被打码的是 URL 里的凭据——
// 形态标签说的和事实不是一回事。故类型名一律避开键名表。
//
// # 短值全量打码是**上游的保证**，本仓不再自带地板
//
// 上游 v0.17.0 起，Keep 档的判据是**运行期按实际命中长度**：命中长度
// n < Keep*4 一律**整段全量打码**（不留定位线索），n >= Keep*4 才保留首尾
// 各 Keep 个。（此前 v0.16.0 的收缩逻辑在 n <= 2*Keep 时让头尾两段首尾相接
// 覆盖整段原文，原值被完整留在输出里——上游 issue #31，已修。）
//
// 本仓曾为这个缺陷自带过两层兜底（「声明 minLen + 构造期校验」与
// 「运行期按实际命中长度预扫」），**现已全部撤除**：上游修好后，
// 本仓再抄一份就是双源漂移——抄的那份迟早与上游的修法脱节，
// 而脱节的方向恰恰是「本仓以为有地板、实际没有」。
//
// 本仓只负责一件事：带 Keep 的规则，其正则的命中长度要够长，
// 否则定位线索根本不会出现（保留位只在 n >= Keep*4 时才有）。
// 这条由 `TestRedactorLongHitKeepsLocator` 钉住。
type structuralPattern struct {
	name string
	re   *regexp.Regexp
	keep int // 保留前后各几个字符作为定位线索（封顶）
}

var structuralPatterns = []structuralPattern{
	// URL 里内嵌的凭据：https://user:pass@host
	// 类型名用 userinfo（凭据所在的 URL 部位）而不用 credential——后者在键名表里。
	{"url-userinfo", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), 0},
	// key=value / key: value 形态的敏感赋值。键名只收通用词。
	{"secret-assignment", regexp.MustCompile(`(?i)\b(token|password|passwd|secret|api[_-]?key|access[_-]?key|private[_-]?key|credential)\b\s*[=:]\s*"?([^\s"',;]{4,})"?`), 0},
	// 裸长十六进制串（≥32 位）：密钥/令牌的典型形态。
	//
	// 32 位既是「什么算凭据」的下限，也是定位线索能出现的前提：
	// Keep=4 时上游的保留位门槛是 4*4=16，32 >= 16，故命中即带线索。
	// 而 31 位不算凭据——短十六进串在正常文本里太常见，
	// 误伤代价高于漏报代价，这条不适用收录标准第三条。
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

// structuralMasker 本仓形态规则表的遮蔽器。
//
// 构造一次、全局复用：规则表是编译期常量，每次处置重建只会把正则编译
// 成本乘到每条事件上，而脱敏在捕获面是**逐条**跑的。
var structuralMasker = mustNewStructuralMasker()

// mustNewStructuralMasker 构造遮蔽器。
//
// 构造失败只可能是规则表本身写错了（重名 / 空标签 / 正则为 nil）——那必须在
// 进程启动时就炸，不该等到第一条事件进来才暴露。
func mustNewStructuralMasker() *stringx.Masker {
	rules := make([]stringx.Rule, 0, len(structuralPatterns))
	for _, p := range structuralPatterns {
		rules = append(rules, stringx.Rule{Name: p.name, Re: p.re, Keep: p.keep})
	}
	m, err := stringx.NewMasker(rules)
	if err != nil {
		panic(fmt.Sprintf("ports: 形态规则表构造失败: %v", err))
	}
	return m
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
//
// # 两道处置，顺序是「本仓在前、上游在后」
//
// 本包的 5 条形态规则**不是**上游规则表的子集，两边各有独占形态：
//
//	形态              本仓          上游 guardx/logredact
//	URL 内嵌凭据      任意 scheme   11 个 scheme 白名单（s3:// 等会漏）
//	key=value 键名    8 个          更多（passphrase / secret_key / client_key）
//	长十六进制 ≥32    有            **无**
//	私钥块（完整）    有            有
//	私钥块（截断）    **无**        有
//	Authorization 头  **无**        有
//	裸 Bearer        **无**        有
//	裸 JWT           **无**        有
//	已知前缀族        15 族         9 族（另有 sk-ant- / sk-proj- / xoxr- …）
//
// 故两道都跑，取并集。**顺序不可颠倒**，理由三条：
//
//  1. 本仓的掩码把整段匹配（含键名）换掉，键名随之消失，上游规则不会再
//     命中它 → 不会二次打码；
//  2. 反过来则相反：上游把 `token=xxx` 变成 `token=****`（键名还在、值已
//     掩码），本仓的规则随后会再把它整段换成 `«secret-assignment:****»」——
//     两层掩码叠在一起，形状变得不可预期；
//  3. 本仓的掩码**保留首尾各 ≤4 字符**作定位线索，上游一律 `****`。
//     本仓在前，已知前缀族（ghp_ / sk- / AKIA …）由本仓掩掉并留下线索；
//     若上游在前，这几族会先被抹成 `****`，定位能力就此丢失。
//
// # 本仓这一道用上游 stringx.Masker，不自研
//
// 自研的那版把「被掩掉多少字符」直接写成等长的星号串，于是
// **掩码长度 = 原值长度**——凭据多长，掩码就多长，攻击者据此知道原值长度，
// 而注释里「不可复原」的承诺只兑现了一半（复原不了，但长度泄了）。
//
// 换成 stringx.Masker 后有四条性质是自研版给不了的：
//
//  1. 掩码主体恒为 `****`，长度固定，不随原值变化 → 长度不泄露；
//  2. **单次扫描合并**（起点最左 → 长度最长 → 注册顺序最前），替换结果
//     不再参与匹配 → 规则之间不会自污染（自研版逐条顺序替换，规则 N 的
//     输出可能被规则 N+1 再处理一次）；
//  3. 保留位数是**封顶**语义：保留的字符总数与原值长度无关；
//  4. **短值整段全量打码**（v0.17.0 起）：命中长度不足 Keep*4 时不做头尾
//     保留——此前上游在 n <= 2*Keep 时会让头尾两段首尾相接覆盖整段原文，
//     脱敏原语等于没生效（上游 issue #31，已修）。
//
// 代价：两种掩码风格会同时出现在一段文本里（`«secret-assignment:****»` 与
// `Authorization=****`）。这是刻意的取舍——收录标准第三条说「漏报代价不可逆」，
// 而定位线索只是「少几个字符就能补」的那一档。
type PatternRedactor struct{}

// Redact 逐条形态匹配并替换。
//
// 先跑本仓的 5 条形态规则（stringx.Masker），再跑上游 guardx/logredact
// 的规则表（取并集）。顺序理由见 PatternRedactor 的类型注释。
func (PatternRedactor) Redact(_ context.Context, req RedactRequest) (string, error) {
	return logredact.Redact(structuralMasker.Mask(req.Text)), nil
}

// maskPlaceholder 是掩码主体。
//
// 恒为固定长度。掩码的长度**不得随原值长度变化**——那不是「保守一点」的
// 风格问题，是长度泄露：凭据多长，掩码就多长。
const maskPlaceholder = "****"

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
	u.User = url.UserPassword(u.User.Username(), maskPlaceholder)
	return u.String()
}
