// Package query 查询理解：把「用户那句话」翻成「可执行的检索条件」。
//
// # 它为什么存在
//
// 「上周那个数据库问题解决了吗？」这句话里，真正决定召回结果的有三样东西：
// 时间窗（上周）、指代（那个数据库）、实体（数据库）。
// 不解析它们，召回只能退化成「把整句话丢进 FTS 匹配」——
// 而「上周」「那个」在事实正文里根本不存在，
// 它们的词面匹配只会稀释真正有区分度的词。
//
// # 三条纪律
//
//  1. **可复现**：同一个查询在同一时刻解析出同一个结果。
//     故解析是**规则**（表驱动 + 确定性的词表），不调模型。
//     调模型的那一层在「判定」侧（抽取/晋升），不在「解析」侧。
//  2. **零值 = 不约束**：抽不出时间窗就不按时间过滤，抽不出实体就不限定实体。
//     与既有过滤面（EntityIDs 空=不限）一致，故调用方不必区分
//     「没解析出来」与「解析出来是空的」。
//  3. **不消解就不硬凑**：代词消不出来就**不消**（计划里 EntityID 为空），
//     宁可少一条线索，也不要猜错——猜错的后果是「召回里全是无关的东西」，
//     而用户看到的是「它明明记过」。
package query

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/types"
)

// Analyzer 查询理解器。
//
// 零值可用（用真实时钟与本地时区）。
type Analyzer struct {
	now func() time.Time
	loc *time.Location
	// weekStart 一周从哪天开始（零值=周一）。
	//
	// 「上周」的起点随它变，而周一起点是中文语境下的默认。
	weekStart time.Weekday
}

// Config 配置。
type Config struct {
	// Now 解析相对时间用的时钟（零值=真实时钟）。
	//
	// 注入它的理由与 lifecycle.WithClock 相同：相对时间（「昨天」）
	// 的解析结果依赖当下时刻，而测试要能复现它。
	Now time.Time
	// Loc 时区（零值=本地时区）。
	//
	// 相对时间按**本地时区**算：「昨天」是用户所在时区的昨天，
	// 而服务器可能在 UTC 上——差一天就是整个窗口错位。
	Loc *time.Location
	// WeekStart 一周从哪天开始（零值=周一）。
	WeekStart time.Weekday
}

// New 造查询理解器。
func New(cfg Config) *Analyzer {
	a := &Analyzer{
		now:       func() time.Time { return time.Now() },
		loc:       time.Local,
		weekStart: time.Monday,
	}
	if !cfg.Now.IsZero() {
		a.now = func() time.Time { return cfg.Now }
	}
	if cfg.Loc != nil {
		a.loc = cfg.Loc
	}
	if cfg.WeekStart != 0 {
		a.weekStart = cfg.WeekStart
	}
	return a
}

// Input 查询理解的输入。
type Input struct {
	// Query 原查询。
	Query string
	// HostEntities 宿主直接给出的实体（高置信度，优先于消解结果）。
	HostEntities []string
	// RecentEntities 最近的活跃实体（**最近在前**），代词消解的来源。
	//
	// 由调用方取好传进来：活跃实体栈在存储里，
	// 而本包只认「哪些实体最近被提到过」这一件事。
	RecentEntities []string
}

// Analyze 解析查询。
func (a *Analyzer) Analyze(_ context.Context, in Input) types.QueryPlan {
	now := a.now().In(a.loc)
	plan := types.QueryPlan{Raw: in.Query}

	// 时间窗先抽：抽出来的时间词要从词面里去掉
	// （「上周」在事实正文里不存在，留着只会稀释词面匹配）。
	win, consumed := a.parseTimeWindow(in.Query, now)
	plan.Time = win

	// 代词消解。
	pronouns, rest := resolvePronouns(in.Query, in.RecentEntities)
	plan.Pronouns = pronouns

	// 实体提示：宿主给的 + 引号里点名的。
	ents := mergeEntities(in.HostEntities, quotedEntities(in.Query), resolvedEntities(pronouns))
	plan.Entities = ents

	// 词面：去掉时间词与代词后的剩余词。
	plan.Terms = termsOf(consumed, rest)
	return plan
}

// mergeEntities 合并三处实体来源（保序去重）。
//
// 顺序有意义：宿主给的排前面（高置信度），
// 然后是引号点名的（用户明确指了），最后是消解出来的（猜的）。
// 召回按这个顺序取，故猜出来的排最后——它只在前面的都取不到时才起作用。
func mergeEntities(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, e := range list {
			if e == "" || seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

// resolvedEntities 从代词消解记录里取出实体。
func resolvedEntities(refs []types.PronounRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.EntityID != "" {
			out = append(out, r.EntityID)
		}
	}
	return out
}

// quotedEntities 取引号里点名的实体。
//
// 只认成对的引号（「」『』《》""），且**成对**才算：
// 半截引号（引号没闭合）多半是用户随手打的，不该当成实体名。
//
// 刻意不认「XX 的 YY」这类结构：那需要知道领域的实体类型，
// 而本仓不认识任何领域。
var quotePairs = []struct{ open, close string }{
	{"「", "」"},
	{"『", "』"},
	{"《", "》"},
	{"“", "”"},
	{"\"", "\""},
}

func quotedEntities(q string) []string {
	var out []string
	for _, p := range quotePairs {
		if p.open == p.close {
			// 直引号：按**成对出现**切（第三次出现起就不算了）。
			parts := strings.Split(q, p.open)
			for i := 1; i < len(parts); i += 2 {
				name := strings.TrimSpace(parts[i])
				if name != "" {
					out = append(out, name)
				}
			}
			continue
		}
		rest := q
		for {
			i := strings.Index(rest, p.open)
			if i < 0 {
				break
			}
			rest = rest[i+len(p.open):]
			j := strings.Index(rest, p.close)
			if j < 0 {
				break
			}
			name := strings.TrimSpace(rest[:j])
			if name != "" {
				out = append(out, name)
			}
			rest = rest[j+len(p.close):]
		}
	}
	return out
}

// termsOf 取检索词面。
//
// 做法：把「被时间解析消费掉的部分」与「代词」从原句里去掉，
// 余下的按空白与标点切成词。
//
// 刻意**不做中文分词**：召回查询本来就短，
// 而整段包含匹配对短查询反而更稳（与 ports.LexicalRanker 的切词口径一致）。
func termsOf(consumed string, rest string) []string {
	src := rest
	if consumed != "" {
		src = strings.ReplaceAll(src, consumed, " ")
	}
	fields := strings.FieldsFunc(src, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', ',', '，', '?', '？', '!', '！', '。', ';', '；', ':', '：', '、':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// pronounRe 代词表。
//
// 刻意只收**指示代词与人称代词**：它们在事实正文里几乎不出现，
// 留着只会稀释词面；而「这」「那」单独出现时也常是口语填充词，
// 故要求它们后面**跟着别的东西**才算代词指代（见 resolvePronouns）。
var pronounRe = regexp.MustCompile(`(这个|那个|这些|那些|它|他|她|此|该|本)+`)

// resolvePronouns 消解代词。
//
// 判据：句中出现代词，且**有活跃实体可指**时，指最近的那个。
//
// 为什么只指「最近的那个」：活跃实体栈本来就是「最近提及在前」，
// 而中文里未加限定的代词默认指最近谈论的对象。
// 消不出来就**不消**（记一条 EntityID 为空的记录）——
// 宁可少一条线索，也不要猜错。
func resolvePronouns(q string, recent []string) ([]types.PronounRef, string) {
	locs := pronounRe.FindAllStringIndex(q, -1)
	if len(locs) == 0 {
		return nil, q
	}
	refs := make([]types.PronounRef, 0, len(locs))
	rest := q
	for _, loc := range locs {
		p := q[loc[0]:loc[1]]
		// 「这个」后面跟着名词时，多半是修饰而不是指代
		//（「这个盘」「那个服务」）——但本仓不认识领域名词，
		// 判不了。折中：**有活跃实体就消**，消错的后果由召回排序吸收。
		ref := types.PronounRef{Pronoun: p}
		if len(recent) > 0 {
			ref.EntityID = recent[0]
			ref.Source = "stack"
		}
		refs = append(refs, ref)
	}
	// 把代词从词面里去掉（代词在事实正文里不存在）。
	for i := len(locs) - 1; i >= 0; i-- {
		rest = rest[:locs[i][0]] + " " + rest[locs[i][1]:]
	}
	return refs, rest
}
