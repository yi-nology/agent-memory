package ports

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

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
// # minLen 是**安全地板**，不是注释
//
// 上游 stringx.Masker 的 Keep 档在「匹配长度 ≤ 2*Keep」时，头尾两段保留位
// 会拼回**完整原文**（头 Keep 个 + 尾 Keep 个，恰好覆盖整段），而掩码主体
// `****` 只是插在中间的装饰。同一形态在 guardx/logredact 的 MaskHint 上
// 没有这个问题（它按 HintMinLenRatio=4 要求长度 ≥ keep*4 才留线索）。
//
// 故凡 keep>0 的规则都必须声明自己的命中长度地板 minLen，且
// **minLen ≥ 4*keep**（留 8 藏 1 等于没藏）。这条不变量在构造期校验，
// 不满足直接 panic——它一旦破了，暴露出去的是凭据明文。
// structuralPattern 一条形态规则。
type structuralPattern struct {
	name string
	re   *regexp.Regexp
	keep int // 保留前后各几个字符作为定位线索（封顶）
	// minLen 该规则命中的最小长度。keep>0 时必填，且须满足 minLen ≥ 4*keep。
	minLen int
}

var structuralPatterns = []structuralPattern{
	// URL 里内嵌的凭据：https://user:pass@host
	// 类型名用 userinfo（凭据所在的 URL 部位）而不用 credential——后者在键名表里。
	{"url-userinfo", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`), 0, 0},
	// key=value / key: value 形态的敏感赋值。键名只收通用词。
	{"secret-assignment", regexp.MustCompile(`(?i)\b(token|password|passwd|secret|api[_-]?key|access[_-]?key|private[_-]?key|credential)\b\s*[=:]\s*"?([^\s"',;]{4,})"?`), 0, 0},
	// 裸长十六进制串（≥32 位）：密钥/令牌的典型形态
	{"long-hex", regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), 4, 32},
	// 私钥块
	{"private-key-block", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), 0, 0},
	// 已知凭据前缀族：形态确定、零误伤，且这一类**一旦落库即不可逆**。
	//
	// 这一条是被用例逼出来的：初版只收 key=value 与 AWS 前缀，于是
	// 「部署用的访问凭据是 ghp_XXXX」这种**散文形态**整个漏掉——而它恰是
	// 最常见的一种。兵底漏掉不可逆的那一类，兵底就没有意义。
	{"known-token-prefix", regexp.MustCompile(
		`\b(ghp_|gho_|ghs_|ghr_|github_pat_|glpat-|xoxb-|xoxp-|xoxa-|` +
			`sk-|sk_live_|sk_test_|AKIA|ASIA|AIza)[A-Za-z0-9_\-]{16,}\b`), 0, 0},
}

// structuralMasker 本仓形态规则表的遮蔽器。
//
// 构造一次、全局复用：规则表是编译期常量，每次处置重建只会把正则编译
// 成本乘到每条事件上，而脱敏在捕获面是**逐条**跑的。
var structuralMasker = mustNewStructuralMasker()

// shortValueFloor 说明长度地板的来历与实现位置。
//
// 上游 stringx.Masker 的 Keep 档在「匹配长度 ≤ 2*Keep」时，头尾两段保留位
// 恰好拼回**完整原文**，掩码主体 `****` 只是插在中间的装饰。
// 而 upstream 的 `stringx.Rule` **没有 MinLen 字段**（v0.16.0 实测），
// 所以长度地板无法交给上游执行——上游修好后本仓的兜底可以撤掉。
//
// 原先的做法是「声明 minLen + 构造期校验 minLen ≥ 4*keep」，那是
// **拿声明比声明**：地板从未传进下游（Rule 只有 Name/Re/Keep 三个字段），
// 运行期零作用；而构造期校验的也只是「声明值够不够」，
// 不是「正则的真实下限」——真正保护秘密的物理量是**实际命中长度**，
// 它只有运行期按实际匹配值才知道。
//
// 典型失效形态（一个人「为了多抓一点」的常见小改）：
// 把 long-hex 的量词从 {32,} 改成 {8,}，声明的 minLen 仍是 32（构造期不炸），
// 但 8 位密钥会被掩成 `0123«long-hex:****»4567`——**完整可复原**。
//
// 故地板改为**运行期按实际命中长度判定**：长度不足地板的命中，
// 整段全量打码（不留任何线索）。实现见 maskStructural。
//
// ⚠️ 声明式地板（minLen 字段）**不能删**：它是构造期的自检信号——
// 量词写错时立刻炸，而不是静默等到线上。运行期兜底防的是「量词没错但值本身短」。
// 两者是互补的：声明管「规则写对没有」，运行期管「这次命中够长没有」。

// minLenRatio 长度地板倍率。
//
// 取 4：命中长度 < keep*4 时全量打码。口径与同族 guardx/logredact 的
// HintMinLenRatio=4 一致（那是上游同族的正确做法——按**实际命中长度**在
// 运行期判定，不是拿声明互比）。
const minLenRatio = 4

// mustNewStructuralMasker 构造遮蔽器，并把安全不变量卡在构造期。
//
// 构造期就炸，而不是等某条短凭据真的落库：这条不变量破了，漏出去的是明文。
//
// ⚠️ 这里的校验是**必要不充分**的：它只保证「声明的地板与保留位自洽」，
// 不保证「正则的真实下限 ≥ 地板」——后者由运行期的 shortValueMasker 兜底。
// 两者缺一不可：只有声明校验 = 上面说的「拿声明比声明」；
// 只有运行期兜底 = 量词写错时没有任何信号（构造期不炸，问题推迟到线上）。
func mustNewStructuralMasker() *stringx.Masker {
	rules := make([]stringx.Rule, 0, len(structuralPatterns))
	for _, p := range structuralPatterns {
		if p.keep > 0 && p.minLen < minLenRatio*p.keep {
			panic(fmt.Sprintf("ports: 规则 %q 的 keep=%d 要求命中长度地板 ≥ %d，实际声明 %d",
				p.name, p.keep, minLenRatio*p.keep, p.minLen))
		}
		rules = append(rules, stringx.Rule{Name: p.name, Re: p.re, Keep: p.keep})
	}
	m, err := stringx.NewMasker(rules)
	if err != nil {
		// 规则表是编译期常量，构造失败只可能是表本身写错了——那必须在
		// 进程启动时就炸，不该等到第一条事件进来才暴露。
		panic(fmt.Sprintf("ports: 形态规则表构造失败: %v", err))
	}
	return m
}

// maskStructural 本仓形态规则的处置：**长度地板在运行期生效**。
//
// 流程（顺序不可颠倒）：
//
//  1. 先找出所有「命中长度不足地板」的匹配（长度按 rune 数算，与上游
//     maskValue 的收缩口径一致）；
//  2. 把这些匹配**整段全量打码**（不留线索）——它们是最危险的那一批：
//     上游 Keep 档在短值上会把原文拼回来；
//  3. 再跑正常的形态遮蔽（带保留位）。
//
// 为什么第 1 步要在第 3 步之前：反向做的话，第 3 步已经把匹配换成了
// `«long-hex:****»` 这样的形态，第 1 步就再也认不出哪段是原来的短值。
func maskStructural(text string) string {
	return maskStructuralWith(text, structuralPatterns, structuralMasker)
}

// maskStructuralWith 是 maskStructural 的可注入版本（测试用）。
//
// 为什么留这个缝：长度地板要能被**真实验证**，
// 而「量词被改短」这个失效场景需要替换规则表才能构造。
// 若没有这个缝，测试只能断言「当前量词下的行为」——
// 那种断言在有人把量词改短后照样绿，钉不住任何东西。
func maskStructuralWith(text string, patterns []structuralPattern, masker *stringx.Masker) string {
	if text == "" {
		return text
	}
	// 收集所有短命中的区间（按位置升序）。
	type hitSpan struct{ start, end int }
	var shorts []hitSpan
	for _, p := range patterns {
		if p.keep == 0 {
			continue // 无保留位的规则不存在「拼回原文」问题
		}
		floor := minLenRatio * p.keep
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			if utf8.RuneCountInString(text[loc[0]:loc[1]]) < floor {
				shorts = append(shorts, hitSpan{loc[0], loc[1]})
			}
		}
	}
	if len(shorts) == 0 {
		return masker.Mask(text)
	}
	// 合并重叠区间（同一段可能命中多条规则）。
	sort.Slice(shorts, func(i, j int) bool { return shorts[i].start < shorts[j].start })
	merged := make([]hitSpan, 0, len(shorts))
	for _, s := range shorts {
		if n := len(merged); n > 0 && s.start <= merged[n-1].end {
			if s.end > merged[n-1].end {
				merged[n-1].end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	// 逐段替换成全量掩码（无保留位、不带形态标签——形态标签会暴露是哪条规则）。
	var b strings.Builder
	last := 0
	for _, s := range merged {
		b.WriteString(text[last:s.start])
		b.WriteString(maskPlaceholder)
		last = s.end
	}
	b.WriteString(text[last:])
	return masker.Mask(b.String())
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
// 换成 stringx.Masker 后有三条性质是自研版给不了的：
//
//  1. 掩码主体恒为 `****`，长度固定，不随原值变化 → 长度不泄露；
//  2. **单次扫描合并**（起点最左 → 长度最长 → 注册顺序最前），替换结果
//     不再参与匹配 → 规则之间不会自污染（自研版逐条顺序替换，规则 N 的
//     输出可能被规则 N+1 再处理一次）；
//  3. 保留位数是**封顶**语义：保留的字符总数与原值长度无关。
//
// 代价：上游那个 Keep 档在短值上会把原文拼回来（见 structuralPatterns 的
// minLen 说明），故带保留位的规则必须声明长度地板，构造期卡死。
//
// 代价二：两种掩码风格会同时出现在一段文本里（`«secret-assignment:****»` 与
// `Authorization=****`）。这是刻意的取舍——收录标准第三条说「漏报代价不可逆」，
// 而定位线索只是「少几个字符就能补」的那一档。
type PatternRedactor struct{}

// Redact 逐条形态匹配并替换。
//
// 先跑本仓的 5 条形态规则（stringx.Masker），再跑上游 guardx/logredact
// 的规则表（取并集）。顺序理由见 PatternRedactor 的类型注释。
func (PatternRedactor) Redact(_ context.Context, req RedactRequest) (string, error) {
	return logredact.Redact(maskStructural(req.Text)), nil
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
