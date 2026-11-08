package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/types"
)

// 本文件：查询理解的用例。
//
// # 断言口径
//
// 时间窗的期望值**手写**（不调被测代码算）——
// 「实现算自己算」恒等于真，那样的用例钉不住任何东西。
//
// 固定时刻与固定时区：相对时间（「昨天」）依赖当下时刻，
// 断言必须钉在确定的那一刻上，否则用例只在某个钟点绿。

var tz = time.FixedZone("T8", 8*3600)

// 基准时刻：2026-10-21 是周三，上周一是 2026-10-12。
var base = time.Date(2026, 10, 21, 15, 30, 0, 0, tz)

func newAnalyzer() *Analyzer {
	return New(Config{Now: base, Loc: tz})
}

func at(t *testing.T, y int, m time.Month, d, h int) time.Time {
	t.Helper()
	return time.Date(y, m, d, h, 0, 0, 0, tz)
}

// 今天。
func TestParseToday(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "今天那个盘怎么了"})
	want := types.TimeWindow{From: at(t, 2026, 10, 21, 0), To: at(t, 2026, 10, 22, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 昨天：左闭右开，to 是今天零点。
func TestParseYesterday(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "昨天的事"})
	want := types.TimeWindow{From: at(t, 2026, 10, 20, 0), To: at(t, 2026, 10, 21, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 上周：周一 00:00 到本周一 00:00（2026-10-12 → 2026-10-19）。
func TestParseLastWeek(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "上周那个问题解决了吗"})
	want := types.TimeWindow{From: at(t, 2026, 10, 12, 0), To: at(t, 2026, 10, 19, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 上个月：9 月 1 日 → 10 月 1 日。
func TestParseLastMonth(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "上个月的处理"})
	want := types.TimeWindow{From: at(t, 2026, 9, 1, 0), To: at(t, 2026, 10, 1, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 最近三天：含今天在内的三天。
func TestParseRecentDays(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "最近三天有没有异常"})
	want := types.TimeWindow{From: at(t, 2026, 10, 19, 0), To: at(t, 2026, 10, 22, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 三天前（中文数字）。
func TestParseDaysAgo(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "三天前那次改动"})
	want := types.TimeWindow{From: at(t, 2026, 10, 18, 0), To: at(t, 2026, 10, 19, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 绝对日期。
func TestParseAbsoluteDate(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "2026-10-01 的变更"})
	want := types.TimeWindow{From: at(t, 2026, 10, 1, 0), To: at(t, 2026, 10, 2, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 时段词收窄天级窗口。
func TestParseDayPartNarrows(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "今天下午那次重启"})
	want := types.TimeWindow{From: at(t, 2026, 10, 21, 12), To: at(t, 2026, 10, 21, 18)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 没有时间词 → 空窗口（零值 = 不约束）。
func TestNoTimeExpression(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "磁盘长期偏小"})
	if !plan.Time.Empty() {
		t.Fatalf("没有时间词时窗口应为空: %+v", plan.Time)
	}
}

// 代词消解：指向最近提及的实体。
func TestPronounResolvesToRecent(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{
		Query:          "那个还能用吗",
		RecentEntities: []string{"e-disk", "e-mem"},
	})
	if len(plan.Pronouns) != 1 {
		t.Fatalf("应有一条代词记录: %+v", plan.Pronouns)
	}
	if plan.Pronouns[0].EntityID != "e-disk" {
		t.Fatalf("应指向最近提及的实体: %+v", plan.Pronouns[0])
	}
	if len(plan.Entities) == 0 || plan.Entities[0] != "e-disk" {
		t.Fatalf("消出来的实体应进实体提示: %+v", plan.Entities)
	}
}

// 没有活跃实体 → **不消**（宁可少一条线索，也不要猜错）。
func TestPronounUnresolvedWhenNoStack(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "那个还能用吗"})
	if len(plan.Pronouns) != 1 {
		t.Fatalf("应有一条代词记录（哪怕没消出来）: %+v", plan.Pronouns)
	}
	if plan.Pronouns[0].EntityID != "" {
		t.Fatalf("没有可指的实体时不该消解: %+v", plan.Pronouns[0])
	}
	if len(plan.Entities) != 0 {
		t.Fatalf("没消出来就不该进实体提示: %+v", plan.Entities)
	}
}

// 宿主给的实体优先于消解结果。
func TestHostEntitiesWin(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{
		Query:          "它呢",
		HostEntities:   []string{"e-host"},
		RecentEntities: []string{"e-recent"},
	})
	if len(plan.Entities) == 0 || plan.Entities[0] != "e-host" {
		t.Fatalf("宿主给的实体应排最前: %+v", plan.Entities)
	}
}

// 引号里点名的实体。
func TestQuotedEntity(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "「机器甲」的磁盘"})
	found := false
	for _, e := range plan.Entities {
		if e == "机器甲" {
			found = true
		}
	}
	if !found {
		t.Fatalf("引号里的实体应被识别: %+v", plan.Entities)
	}
}

// 时间词与代词要从词面里去掉：它们在事实正文里不存在，
// 留着只会稀释词面匹配。
func TestTermsExcludeTimeAndPronoun(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "上周那个磁盘的问题"})
	for _, term := range plan.Terms {
		if term == "上周" || term == "那个" {
			t.Fatalf("时间词与代词不该留在词面里: %+v", plan.Terms)
		}
	}
	if len(plan.Terms) == 0 {
		t.Fatal("词面不该为空")
	}
}

// 时间词要**整段**吃掉，不能只吃掉前缀。
//
// 这条钉的是一个真实踩过的坑：时间表达式的正则写成 `今|今天` 时，
// 最左优先会先匹配短的「今」——于是「天」留在词面里，
// 而「天」在事实正文里同样不存在（只是更隐蔽：它不像「上周」那么显眼）。
//
// 故断言的是「时间词一个残片都不许留」，而不是「不许留某个整词」。
func TestTermsExcludeTimeFragments(t *testing.T) {
	a := newAnalyzer()
	for _, q := range []string{"今天那个盘怎么了", "昨天的事", "明天会怎样", "今年的情况"} {
		plan := a.Analyze(context.Background(), Input{Query: q})
		for _, term := range plan.Terms {
			switch term {
			case "天", "年", "今", "昨", "明":
				t.Fatalf("时间词被截断了（%q → %+v）: %q", q, plan.Terms, term)
			}
		}
	}
}

// ── 中文数字与周起点（第三轮 issue 清零）──

// 中文数字与阿拉伯数字必须解析出**同一个**窗口。
//
// 钉的缺陷：parseCount 的单字分支对多字输入提前 return 0, false，
// 「二十」「二十三」两条组合分支永远走不到——「最近二十天」抽不出窗口
// （零值=不约束 ⇒ 时间过滤整体失效，召回退化成全量），
// 时间词还留在词面里继续稀释匹配。两头都输，且没有任何报错。
func TestParseChineseCountMatchesArabic(t *testing.T) {
	for _, tc := range []struct{ arabic, chinese string }{
		{"3", "三"}, {"12", "十二"}, {"20", "二十"}, {"23", "二十三"},
	} {
		pa := newAnalyzer().Analyze(context.Background(), Input{Query: "最近" + tc.arabic + "天的事"})
		pc := newAnalyzer().Analyze(context.Background(), Input{Query: "最近" + tc.chinese + "天的事"})
		if pa.Time.Empty() || pc.Time.Empty() {
			t.Fatalf("%q/%q 应解析出窗口: 阿拉伯=%+v 中文=%+v", tc.arabic, tc.chinese, pa.Time, pc.Time)
		}
		if !pa.Time.From.Equal(pc.Time.From) || !pa.Time.To.Equal(pc.Time.To) {
			t.Fatalf("%q 与 %q 的窗口应逐字相同: %+v vs %+v", tc.arabic, tc.chinese, pa.Time, pc.Time)
		}
		// 时间词必须被整段消费，不许留在词面里。
		for _, term := range pc.Terms {
			if strings.Contains(term, tc.chinese) {
				t.Fatalf("时间词 %q 不该留在词面里: %+v", tc.chinese, pc.Terms)
			}
		}
	}
}

// 「二十天前」同样要出窗（parseCount 的另一条消费路）。
func TestParseTwentyDaysAgo(t *testing.T) {
	a := newAnalyzer()
	plan := a.Analyze(context.Background(), Input{Query: "二十天前那次改动"})
	want := types.TimeWindow{From: at(t, 2026, 10, 1, 0), To: at(t, 2026, 10, 2, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}

// 负对照：无意义的中文数字组合仍须拒绝（宁可零值不约束，也不猜错）。
func TestParseCountRejectsMalformedChinese(t *testing.T) {
	for _, s := range []string{"二三", "十十", "二十十"} {
		if n, ok := parseCount(s); ok {
			t.Fatalf("parseCount(%q) 应拒绝，实得 (%d, true)", s, n)
		}
	}
}

// 周起点可配：WeekStart=Sunday 时三条周规则的窗口两端都应落在周日，
// 且与默认（周一）**不同**。
//
// 钉的缺陷：Config.WeekStart 曾是只被赋值、从不被读的死配置——三条周规则
// 写死 time.Monday，宿主配成周日照样得到周一窗口，且没有任何报错。
// 「配了但没生效」比「没这个配置」更坏：调用方会以为行为已按本地习惯对齐。
//
// 字段因此做成指针：time.Weekday 的零值恰好是周日（0），
// 值类型区分不了「没配」与「配成周日」——那正是当年静默落回周一的根源。
func TestWeekStartSundayChangesWeekWindows(t *testing.T) {
	sunday := time.Sunday
	monday := newAnalyzer()
	sundayCfg := New(Config{Now: base, Loc: tz, WeekStart: &sunday})
	for _, q := range []string{"上周的事", "本周的事", "下周的事"} {
		wm := monday.Analyze(context.Background(), Input{Query: q}).Time
		ws := sundayCfg.Analyze(context.Background(), Input{Query: q}).Time
		if wm.Empty() || ws.Empty() {
			t.Fatalf("%q 应解析出窗口: 周一=%+v 周日=%+v", q, wm, ws)
		}
		if ws.From.Weekday() != time.Sunday || ws.To.Weekday() != time.Sunday {
			t.Fatalf("WeekStart=Sunday 时 %q 的窗口两端都应落在周日: %+v", q, ws)
		}
		if wm.From.Equal(ws.From) && wm.To.Equal(ws.To) {
			t.Fatalf("WeekStart=Sunday 的 %q 窗口与周一配置逐字相同（配置没生效）: %+v", q, ws)
		}
	}
}

// 周日起点的具体窗口（手写期望值，不调被测代码算）：
// 2026-10-21 是周三，上个周日是 2026-10-11。
func TestWeekStartSundayLastWeekWindow(t *testing.T) {
	sunday := time.Sunday
	a := New(Config{Now: base, Loc: tz, WeekStart: &sunday})
	plan := a.Analyze(context.Background(), Input{Query: "上周的事"})
	want := types.TimeWindow{From: at(t, 2026, 10, 11, 0), To: at(t, 2026, 10, 18, 0)}
	if !plan.Time.From.Equal(want.From) || !plan.Time.To.Equal(want.To) {
		t.Fatalf("时间窗不对: %+v", plan.Time)
	}
}
