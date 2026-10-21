package query

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oekit/pkg/timex"

	"github.com/yi-nology/agent-memory/types"
)

// 本文件：相对时间与绝对时间的解析。
//
// # 为什么表驱动
//
// 时间表达式的形态很多（今天/今日/今儿、最近三天/近 3 天/过去三天、三天前……），
// 而它们最终只落到**有限几种窗口**上。用表驱动把它们一一映射过去，
// 比写一大坨 if-else 好在：新增一种说法只要加一行，
// 且每种说法都能单独被用例钉住。
//
// # 为什么走正则而不是分词
//
// 时间词在中文里没有空格边界（「上周那个问题」里「上周」是连着的），
// 而本仓不引分词器（core 零第三方依赖）。
// 正则够用：时间表达式的形态是**有限且稳定**的，
// 不像正文那样需要真正的分词。
//
// # 窗口是左闭右开
//
// [from, to)：to 是「下一段的起点」。
// 这样「上周」= 上周一 00:00 到本周一 00:00，
// 而事实的 OccurredAt 落在周一 00:00 整的那一条属于**本周**——
// 边界不含混是时间窗查询可复现的前提。

// timeMatcher 一条时间表达式的匹配规则。
type timeMatcher struct {
	// re 匹配表达式。
	re *regexp.Regexp
	// name 这条规则叫什么（用例里用来指认）。
	name string
	// resolve 解析成时间窗。groups 是正则的子匹配（groups[0] 是整段）。
	resolve func(groups []string, now time.Time) types.TimeWindow
	// dayLevel 这条规则给出的是「天级」窗口（可被时段词再收窄）。
	dayLevel bool
}

// timeMatchers 时间表达式规则表。
//
// 顺序即优先级：先匹配到的先用。故更具体的写法排前面
// （「最近三天」要先于「三天前」被看到）。
var timeMatchers = []timeMatcher{
	{
		name: "最近N天", re: regexp.MustCompile(`(最近|近|过去)\s*([0-9一二三四五六七八九十两]+)\s*天`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			n, ok := parseCount(g[2])
			if !ok || n <= 0 {
				return types.TimeWindow{}
			}
			dayStart := timex.BeginningOfDay(now)
			return types.TimeWindow{
				From: dayStart.AddDate(0, 0, -(n - 1)),
				To:   dayStart.AddDate(0, 0, 1),
			}
		},
		dayLevel: true,
	},
	{
		name: "N天前", re: regexp.MustCompile(`([0-9一二三四五六七八九十两]+)\s*天前`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			n, ok := parseCount(g[1])
			if !ok || n <= 0 {
				return types.TimeWindow{}
			}
			from := timex.BeginningOfDay(now).AddDate(0, 0, -n)
			return types.TimeWindow{From: from, To: from.AddDate(0, 0, 1)}
		},
		dayLevel: true,
	},
	{
		name: "上周", re: regexp.MustCompile(`(上|上个|上一个)\s*(周|星期|礼拜)`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			start := timex.BeginningOfWeek(now, time.Monday)
			return types.TimeWindow{From: start.AddDate(0, 0, -7), To: start}
		},
	},
	{
		name: "本周", re: regexp.MustCompile(`(这|本|这个|这一)\s*(周|星期|礼拜)`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			start := timex.BeginningOfWeek(now, time.Monday)
			return types.TimeWindow{From: start, To: start.AddDate(0, 0, 7)}
		},
	},
	{
		name: "下周", re: regexp.MustCompile(`(下|下个|下一个)\s*(周|星期|礼拜)`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			start := timex.BeginningOfWeek(now, time.Monday)
			return types.TimeWindow{From: start.AddDate(0, 0, 7), To: start.AddDate(0, 0, 14)}
		},
	},
	{
		name: "上个月", re: regexp.MustCompile(`(上|上个|上一个)\s*月`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			return types.TimeWindow{
				From: timex.BeginningOfMonth(now.AddDate(0, -1, 0)),
				To:   timex.BeginningOfMonth(now),
			}
		},
	},
	{
		name: "本月", re: regexp.MustCompile(`(这|本|这个|当)\s*月`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			start := timex.BeginningOfMonth(now)
			return types.TimeWindow{From: start, To: start.AddDate(0, 1, 0)}
		},
	},
	{
		name: "下个月", re: regexp.MustCompile(`(下|下个|下一个)\s*月`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			start := timex.BeginningOfMonth(now.AddDate(0, 1, 0))
			return types.TimeWindow{From: start, To: start.AddDate(0, 1, 0)}
		},
	},
	{
		name: "去年", re: regexp.MustCompile(`(去|上)\s*年`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			return types.TimeWindow{
				From: timex.BeginningOfYear(now.AddDate(-1, 0, 0)),
				To:   timex.BeginningOfYear(now),
			}
		},
	},
	{
		name: "今年", re: regexp.MustCompile(`(今年|本年|这个年|今|本)\s*年`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			start := timex.BeginningOfYear(now)
			return types.TimeWindow{From: start, To: start.AddDate(1, 0, 0)}
		},
	},
	{
		name: "前天", re: regexp.MustCompile(`前天`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			day := timex.BeginningOfDay(now).AddDate(0, 0, -2)
			return types.TimeWindow{From: day, To: day.AddDate(0, 0, 1)}
		},
		dayLevel: true,
	},
	{
		name: "昨天", re: regexp.MustCompile(`(昨天|昨日|昨儿|昨)`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			day := timex.BeginningOfDay(now).AddDate(0, 0, -1)
			return types.TimeWindow{From: day, To: day.AddDate(0, 0, 1)}
		},
		dayLevel: true,
	},
	{
		name: "今天", re: regexp.MustCompile(`(今天|今日|今儿|今)`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			day := timex.BeginningOfDay(now)
			return types.TimeWindow{From: day, To: day.AddDate(0, 0, 1)}
		},
		dayLevel: true,
	},
	{
		name: "明天", re: regexp.MustCompile(`(明天|明日|明儿|明)`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			day := timex.BeginningOfDay(now).AddDate(0, 0, 1)
			return types.TimeWindow{From: day, To: day.AddDate(0, 0, 1)}
		},
		dayLevel: true,
	},
	{
		name: "具体日期", re: regexp.MustCompile(`([0-9]{4})\s*[-/年]\s*([0-9]{1,2})\s*[-/月]\s*([0-9]{1,2})\s*日?`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			y, err1 := strconv.Atoi(g[1])
			m, err2 := strconv.Atoi(g[2])
			d, err3 := strconv.Atoi(g[3])
			if err1 != nil || err2 != nil || err3 != nil {
				return types.TimeWindow{}
			}
			from := time.Date(y, time.Month(m), d, 0, 0, 0, 0, now.Location())
			return types.TimeWindow{From: from, To: from.AddDate(0, 0, 1)}
		},
		dayLevel: true,
	},
	{
		name: "月日", re: regexp.MustCompile(`([0-9]{1,2})\s*月\s*([0-9]{1,2})\s*日`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			m, err1 := strconv.Atoi(g[1])
			d, err2 := strconv.Atoi(g[2])
			if err1 != nil || err2 != nil {
				return types.TimeWindow{}
			}
			from := time.Date(now.Year(), time.Month(m), d, 0, 0, 0, 0, now.Location())
			return types.TimeWindow{From: from, To: from.AddDate(0, 0, 1)}
		},
		dayLevel: true,
	},
	{
		name: "某月", re: regexp.MustCompile(`([0-9]{1,2})\s*月`),
		resolve: func(g []string, now time.Time) types.TimeWindow {
			m, err := strconv.Atoi(g[1])
			if err != nil || m < 1 || m > 12 {
				return types.TimeWindow{}
			}
			from := time.Date(now.Year(), time.Month(m), 1, 0, 0, 0, 0, now.Location())
			return types.TimeWindow{From: from, To: from.AddDate(0, 1, 0)}
		},
	},
}

// parseTimeWindow 抽时间窗。
//
// 返回（窗口，被消费掉的原文）。窗口为零值表示没抽到。
func (a *Analyzer) parseTimeWindow(q string, now time.Time) (types.TimeWindow, string) {
	for _, m := range timeMatchers {
		groups := m.re.FindStringSubmatch(q)
		if groups == nil {
			continue
		}
		win := m.resolve(groups, now)
		if win.Empty() {
			continue
		}
		// 时段词收窄：只对「天级」窗口有意义
		// （「上个月下午」是没意义的说法）。
		if m.dayLevel {
			win = narrowByDayPart(q, win)
		}
		return win, groups[0]
	}
	return types.TimeWindow{}, ""
}

// dayPart 时段词。
type dayPart struct {
	re       *regexp.Regexp
	fromHour int
	toHour   int
}

var dayParts = []dayPart{
	{regexp.MustCompile(`凌晨`), 0, 6},
	{regexp.MustCompile(`早上|早晨|清晨`), 6, 12},
	{regexp.MustCompile(`上午`), 0, 12},
	{regexp.MustCompile(`中午`), 12, 14},
	{regexp.MustCompile(`下午`), 12, 18},
	{regexp.MustCompile(`傍晚`), 17, 19},
	{regexp.MustCompile(`晚上|夜里`), 18, 24},
}

// narrowByDayPart 用时段词收窄天级窗口。
//
// 为什么只收窄天级窗口：「上个月下午」不是合法说法，
// 而「今天下午」「昨天晚上」是——后者正是用户问「昨天那会儿」时的说法。
func narrowByDayPart(q string, win types.TimeWindow) types.TimeWindow {
	for _, p := range dayParts {
		if !p.re.MatchString(q) {
			continue
		}
		if win.To.Sub(win.From) > 24*time.Hour {
			return win
		}
		// 时段落在**窗口起始那一天**里：
		// 「今天下午」= 今天 12:00–18:00；
		// 「昨天晚上」= 昨天 18:00–昨天 24:00（= 今天 00:00）。
		//
		// 上一版把上界按 win.To 的那一天算，于是「今天下午」的终点
		// 跑到了**明天** 18:00——那是一个跨了两天的窗口。
		day := win.From
		from := time.Date(day.Year(), day.Month(), day.Day(), p.fromHour, 0, 0, 0, day.Location())
		to := time.Date(day.Year(), day.Month(), day.Day(), p.toHour, 0, 0, 0, day.Location())
		if !to.After(from) {
			return win
		}
		return types.TimeWindow{From: from, To: to}
	}
	return win
}

// parseCount 解析数字（阿拉伯数字与中文数字）。
//
// 支持到 99 —— 再大就没有实际意义了（没有谁会问「最近 300 天前」）。
func parseCount(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	digits := map[rune]int{'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
		'五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	r := []rune(s)
	if n, ok := digits[r[0]]; ok {
		if len(r) == 1 {
			return n, true
		}
		return 0, false
	}
	// 「十」「十二」「二十」「二十三」。
	if r[0] == '十' {
		if len(r) == 1 {
			return 10, true
		}
		if n, ok := digits[r[1]]; ok && len(r) == 2 {
			return 10 + n, true
		}
	}
	if len(r) == 3 && r[0] == '二' && r[1] == '十' {
		if n, ok := digits[r[2]]; ok {
			return 20 + n, true
		}
	}
	if len(r) == 2 && r[0] == '二' && r[1] == '十' {
		return 20, true
	}
	return 0, false
}
