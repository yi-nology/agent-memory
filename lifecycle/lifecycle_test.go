package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yi-nology/agent-memory/lifecycle"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("lc")

func now() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

func seed(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.OpenMemory("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func atom(id, entity, assertion, evidence string, runtimeErr bool) *types.AtomCard {
	return &types.AtomCard{
		ID: id, EntityID: entity, Assertion: assertion, QuoteEventID: evidence,
		RawEventIDs: []string{evidence}, Importance: types.ImportanceMed,
		Confidence: types.ConfidenceMed, OccurredAt: now(), CreatedAt: now(), UpdatedAt: now(),
		Provenance: types.Provenance{Origin: types.OriginModel, RuntimeError: runtimeErr},
	}
}

// eventAtom 造一张**事件类**事实卡（发生时刻可指定）。
//
// 类别与发生时刻是按龄过期这件事的**判别特征**：两者不同，
// 同一批事实里就分得出「哪条该过期、哪条不该」。
func eventAtom(id, assertion string, occurred time.Time) *types.AtomCard {
	a := atom(id, "ent1", assertion, "ev-"+id, false)
	a.Kind = types.FactEvent
	a.OccurredAt = occurred
	return a
}

// 回收：原始事件按龄删除。
func TestGCDeletesOldRaw(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "old", Content: "很久以前", Timestamp: now().Add(-48 * time.Hour), Type: types.EventManual})
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "new", Content: "刚刚", Timestamp: now(), Type: types.EventManual})

	res, err := lifecycle.GC(ctx, s, ns, now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.RawDeleted != 1 {
		t.Fatalf("应删 1 条，实得 %d", res.RawDeleted)
	}
	if _, err := s.GetRaw(ctx, ns, "old"); err == nil {
		t.Fatal("旧事件不该还在")
	}
	if _, err := s.GetRaw(ctx, ns, "new"); err != nil {
		t.Fatalf("窗口外的事件不该被删: %v", err)
	}
}

// 回收：证据全被清掉的事实要退役（它成了一条无法回溯的断言）。
func TestGCRetiresAtomsWhoseEvidenceVanished(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Content: "旧素材", Timestamp: now().Add(-48 * time.Hour), Type: types.EventManual})
	_ = s.SaveAtom(ctx, ns, atom("a1", "ent1", "某条事实", "e1", false))

	res, err := lifecycle.GC(ctx, s, ns, now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.AtomsRetired != 1 {
		t.Fatalf("证据全没了的事实应退役，实得 %d", res.AtomsRetired)
	}
	got, err := s.GetAtom(ctx, ns, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Active() {
		t.Fatal("退役的事实不该再参与召回")
	}
}

// 证据还在的事实**不能**被退役。
func TestGCKeepsAtomsWithLiveEvidence(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Content: "素材", Timestamp: now(), Type: types.EventManual})
	_ = s.SaveAtom(ctx, ns, atom("a1", "ent1", "某条事实", "e1", false))

	if _, err := lifecycle.GC(ctx, s, ns, now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAtom(ctx, ns, "a1")
	if !got.Active() {
		t.Fatal("证据还在的事实不该被退役：误判「查不了」的代价比多查几次大得多")
	}
}

// 合并：同签名事实归到一张，其余软退役（不硬删）。
func TestConsolidateMergesSameSignature(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, atom("a1", "ent1", "磁盘长期偏小", "e1", false))
	_ = s.SaveAtom(ctx, ns, atom("a2", "ent1", "磁盘长期偏小", "e2", false))

	res, err := lifecycle.Consolidate(ctx, s, ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged != 1 {
		t.Fatalf("应合并掉 1 张，实得 %d", res.Merged)
	}
	// 断言「恰好一张活跃、另一张指向它」，而不断言具体是哪张：
	// 保留哪一张由「创建更早者胜、同刻按 ID 决胜」决定，
	// 但**测试不该把那个决胜规则复述一遍**——它一变，测试就该变，
	// 而它没变时测试也不该因为遍历顺序而红。
	var live, retired *types.AtomCard
	for _, id := range []string{"a1", "a2"} {
		got, err := s.GetAtom(ctx, ns, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Active() {
			live = got
		} else {
			retired = got
		}
	}
	if live == nil || retired == nil {
		t.Fatalf("应恰好一张活跃一张退役，实得 live=%v retired=%v", live, retired)
	}
	if retired.SupersededBy != live.ID {
		t.Fatalf("退役必须指向继任者: retired=%s live=%s", retired.SupersededBy, live.ID)
	}
	if len(live.RawEventIDs) != 2 {
		t.Fatalf("证据应合并到存活的那张: %+v", live.RawEventIDs)
	}
}

// 保留期：退役「证据全部来自运行期错误」的事实。
//
// 判据读的是**证据事件**的来源标记，不是事实卡自己那个——
// 事实卡的标记会随证据被更换/补上而过时，按它判会误伤。
// 故这里把证据事件真的建出来。
func TestRetentionRetiresRuntimeErrorAtoms(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Content: "合成出来的错误", Timestamp: now(),
		Type: types.EventToolResult, Provenance: types.Provenance{
			Origin: types.OriginModel, RuntimeError: true,
		}})
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e2", Content: "真实发生的事", Timestamp: now(),
		Type: types.EventManual, Provenance: types.Provenance{Origin: types.OriginUser}})
	// e3：正常的事件，但事实卡自己的标记说是运行期错误（标记过时了）。
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e3", Content: "真实发生的事", Timestamp: now(),
		Type: types.EventManual, Provenance: types.Provenance{Origin: types.OriginUser}})
	_ = s.SaveAtom(ctx, ns, atom("a-bad", "ent1", "合成出来的错误", "e1", true))
	_ = s.SaveAtom(ctx, ns, atom("a-good", "ent1", "真实发生的事", "e2", false))
	_ = s.SaveAtom(ctx, ns, atom("a-stale", "ent1", "真实发生的事", "e3", true))

	res, err := lifecycle.Retention(ctx, s, ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retired != 1 {
		t.Fatalf("应退役 1 条，实得 %d", res.Retired)
	}
	bad, _ := s.GetAtom(ctx, ns, "a-bad")
	if bad.Active() {
		t.Fatal("运行期错误来源的事实应退役")
	}
	good, _ := s.GetAtom(ctx, ns, "a-good")
	if !good.Active() {
		t.Fatal("正常来源的事实不该被退役")
	}
	stale, _ := s.GetAtom(ctx, ns, "a-stale")
	if !stale.Active() {
		t.Fatal("事实卡自己的标记过时了（证据是正常事件），不该被退役")
	}
}

// 压缩：后端专属能力，不支持的必须明说而不是静默跳过。
func TestVacuumOnUnsupportedBackend(t *testing.T) {
	// 参数是接口而不是 any：调用方知道自己拿的是哪个后端，
	// 「这个后端不支持」由传 nil 表达。
	err := lifecycle.Vacuum(context.Background(), nil)
	if err == nil {
		t.Fatal("不支持的后端必须明确报错：静默跳过会让人以为压过了")
	}
}

// SQLite 后端支持压缩。
func TestVacuumOnSQLite(t *testing.T) {
	s := seed(t)
	if err := lifecycle.Vacuum(context.Background(), s); err != nil {
		t.Fatalf("SQLite 应支持 VACUUM: %v", err)
	}
}

// 退役时刻必须**可断言**：时钟写死成 time.Now() 时，
// 测试只能断言「大概刚刚」，而「大概」在断言里等于「没断言」。
func TestRetentionUsesInjectedClock(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Content: "合成出来的错误", Timestamp: now(), Type: types.EventToolResult,
		Provenance: types.Provenance{Origin: types.OriginModel, RuntimeError: true},
	})
	_ = s.SaveAtom(ctx, ns, atom("a-bad", "ent1", "合成出来的错误", "e1", true))

	fixed := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if _, err := lifecycle.Retention(ctx, s, ns, lifecycle.WithClock(func() time.Time { return fixed })); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListJournal(ctx, ns, "a-bad", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("退役应写审计条目")
	}
	if !entries[0].At.Equal(fixed) {
		t.Fatalf("审计条目的时刻应取注入的时钟，期望 %v，实得 %v", fixed, entries[0].At)
	}
}

// ── 按龄过期 ──

// 过期：事件类事实过了保留期就退役。
func TestExpireRetiresOldEvents(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, eventAtom("a-old", "两天前重启过一次", now().Add(-48*time.Hour)))
	_ = s.SaveAtom(ctx, ns, eventAtom("a-fresh", "刚刚重启过一次", now()))

	res, err := lifecycle.Expire(ctx, s, ns,
		lifecycle.WithClock(func() time.Time { return now() }),
		lifecycle.WithEventRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsRetired != 1 {
		t.Fatalf("应退役 1 条事件类事实，实得 %d", res.EventsRetired)
	}
	old, err := s.GetAtom(ctx, ns, "a-old")
	if err != nil {
		t.Fatal(err)
	}
	if old.Active() {
		t.Fatal("过了保留期的事件类事实应退役")
	}
	fresh, err := s.GetAtom(ctx, ns, "a-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Active() {
		t.Fatal("还在保留期内的事件类事实不该被退役")
	}
}

// 过期：**只**退役事件类。
//
// 状态等取代（取代链表达），经验永久保留——它们不按时间失效。
// 误伤它们的症状是「某条老经验突然查不到了」，而它没有报错、没有告警。
func TestExpireKeepsNonEvents(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	st := atom("a-state", "ent1", "容量是 2G", "e1", false)
	st.Kind = types.FactState
	st.OccurredAt = now().Add(-48 * time.Hour)
	ls := atom("a-lesson", "ent1", "先看日志再动手", "e2", false)
	ls.Kind = types.FactLesson
	ls.OccurredAt = now().Add(-48 * time.Hour)
	plain := atom("a-plain", "ent1", "没有标注类别的老事实", "e3", false)
	plain.OccurredAt = now().Add(-48 * time.Hour)
	for _, a := range []*types.AtomCard{st, ls, plain} {
		_ = s.SaveAtom(ctx, ns, a)
	}

	res, err := lifecycle.Expire(ctx, s, ns,
		lifecycle.WithClock(func() time.Time { return now() }),
		lifecycle.WithEventRetention(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsRetired != 0 {
		t.Fatalf("非事件类事实不该被按龄退役，实得 %d", res.EventsRetired)
	}
	for _, id := range []string{"a-state", "a-lesson", "a-plain"} {
		got, err := s.GetAtom(ctx, ns, id)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Active() {
			t.Fatalf("%s 不是事件类事实，不该被按龄退役", id)
		}
	}
}

// 缺省保留期是 30 天：不配它时也要有确定的判据，
// 而「记得住的事实」比「忘得干净的事实」重要（忘了可以再看日志）。
func TestExpireDefaultRetention(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, eventAtom("a-31d", "一个月前重启过一次", now().Add(-31*24*time.Hour)))
	_ = s.SaveAtom(ctx, ns, eventAtom("a-29d", "上个月重启过一次", now().Add(-29*24*time.Hour)))

	res, err := lifecycle.Expire(ctx, s, ns, lifecycle.WithClock(func() time.Time { return now() }))
	if err != nil {
		t.Fatal(err)
	}
	if res.EventsRetired != 1 {
		t.Fatalf("缺省保留期（30 天）应退役 1 条，实得 %d", res.EventsRetired)
	}
	old, _ := s.GetAtom(ctx, ns, "a-31d")
	if old.Active() {
		t.Fatal("超过缺省保留期的事件类事实应退役")
	}
	keep, _ := s.GetAtom(ctx, ns, "a-29d")
	if !keep.Active() {
		t.Fatal("还在缺省保留期内的事件类事实不该被退役")
	}
}

// 过期必须**幂等**：保留期动作是定时跑的，同一批事实会被一遍遍扫到。
//
// 重复退役的失效形态不是报错，而是「审计条目翻倍」——
// 「这张卡什么时候被退役的」有了两个答案，而读的人分不出哪个是真的。
func TestExpireIsIdempotent(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, eventAtom("a-old", "两天前重启过一次", now().Add(-48*time.Hour)))

	opt := []lifecycle.Option{
		lifecycle.WithClock(func() time.Time { return now() }),
		lifecycle.WithEventRetention(24 * time.Hour),
	}
	first, err := lifecycle.Expire(ctx, s, ns, opt...)
	if err != nil {
		t.Fatal(err)
	}
	if first.EventsRetired != 1 {
		t.Fatalf("第一遍应退役 1 条，实得 %d", first.EventsRetired)
	}
	second, err := lifecycle.Expire(ctx, s, ns, opt...)
	if err != nil {
		t.Fatalf("第二遍不该报错: %v", err)
	}
	if second.EventsRetired != 0 {
		t.Fatalf("第二遍不该再退役（幂等），实得 %d", second.EventsRetired)
	}
	entries, err := s.ListJournal(ctx, ns, "a-old", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("同一条事实的退役审计应只有一条，实得 %d 条", len(entries))
	}
}

// 退役是**软退役**：事实卡仍要查得到（追问「当初为什么这么判」是记忆系统
// 必须能回答的问题），只是不再参与召回。
func TestExpireRetiresButKeepsQueryable(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, eventAtom("a-old", "两天前重启过一次", now().Add(-48*time.Hour)))

	if _, err := lifecycle.Expire(ctx, s, ns,
		lifecycle.WithClock(func() time.Time { return now() }),
		lifecycle.WithEventRetention(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAtom(ctx, ns, "a-old")
	if err != nil {
		t.Fatalf("退役的事实不该被删（软退役不是删）: %v", err)
	}
	if got.Active() {
		t.Fatal("退役的事实不该再参与召回")
	}
	if got.Assertion == "" {
		t.Fatal("退役的事实不该被清空")
	}
}

// 退役要**留痕**：退役时刻取注入的时钟（它是审计链上的一环）。
func TestExpireWritesJournalWithInjectedClock(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, eventAtom("a-old", "两天前重启过一次", now().Add(-48*time.Hour)))

	fixed := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if _, err := lifecycle.Expire(ctx, s, ns,
		lifecycle.WithClock(func() time.Time { return fixed }),
		lifecycle.WithEventRetention(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListJournal(ctx, ns, "a-old", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("按龄退役应写审计条目")
	}
	if !entries[0].At.Equal(fixed) {
		t.Fatalf("审计条目的时刻应取注入的时钟，期望 %v，实得 %v", fixed, entries[0].At)
	}
	// 审计条目要说清是「按龄过期」——它与人工作废的处置不同，
	// 而看板上这两种长得一模一样。
	if entries[0].Summary == "" {
		t.Fatal("审计条目应写明处置原因")
	}
}

// ── 合并审计条目的签名展示（第四轮 issue 清零，#40）──

// 合并审计的 Summary 必须是**合法 UTF-8 且不含 0x1F 控制符**。
//
// 钉的缺陷：short() 用裸字节 s[:16] 截断，签名第三段是中文断言，
// 切在多字节字符中间产出非法 UTF-8（PG 侧 journal INSERT 当场报错被 `_ =` 吞掉，
// 整条审计静默丢失）；分隔符替换又还在替换 NUL 时代的 \x00，
// 0x1F 控制符原文漏进人读的审计。
func TestConsolidateJournalSummaryIsSafeText(t *testing.T) {
	cases := []struct {
		name string
		atom *types.AtomCard
	}{
		{"长中文断言（截断点落在多字节字符中间）", &types.AtomCard{
			ID: "a-cn", EntityID: "e1", Scope: "team-a",
			Assertion: "机器甲的内核版本号是最新长期支持版且已开启自动更新策略", QuoteEventID: "ev1",
			RawEventIDs: []string{"ev1"}, Importance: types.ImportanceMed,
			Confidence: types.ConfidenceMed, OccurredAt: now(), CreatedAt: now(), UpdatedAt: now(),
		}},
		{"短签名（不截断也要换掉分隔符）", &types.AtomCard{
			ID: "a-short", EntityID: "e", Assertion: "已修复", QuoteEventID: "ev1",
			RawEventIDs: []string{"ev1"}, Importance: types.ImportanceMed,
			Confidence: types.ConfidenceMed, OccurredAt: now(), CreatedAt: now(), UpdatedAt: now(),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := seed(t)
			ctx := context.Background()
			// 同签名两张 → 触发合并 → 审计 Summary 带截断后的签名。
			tc.atom.ID = tc.atom.ID + "-1"
			if err := s.SaveAtom(ctx, ns, tc.atom); err != nil {
				t.Fatal(err)
			}
			dup := *tc.atom
			dup.ID = tc.atom.ID[:len(tc.atom.ID)-1] + "2"
			dup.CreatedAt = now().Add(time.Hour)
			if err := s.SaveAtom(ctx, ns, &dup); err != nil {
				t.Fatal(err)
			}
			res, err := lifecycle.Consolidate(ctx, s, ns, lifecycle.WithClock(func() time.Time { return now() }))
			if err != nil {
				t.Fatal(err)
			}
			if res.Merged != 1 {
				t.Fatalf("应合并 1 张，实得 %+v", res)
			}
			journal, err := s.ListJournal(ctx, ns, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(journal) == 0 {
				t.Fatal("合并必须留审计条目")
			}
			for _, j := range journal {
				if !utf8.ValidString(j.Summary) {
					t.Fatalf("审计 Summary 必须是合法 UTF-8（截断切坏多字节字符的旧病）: %q", j.Summary)
				}
				if strings.Contains(j.Summary, "\x1f") {
					t.Fatalf("审计 Summary 不得残留签名分隔符 0x1F: %q", j.Summary)
				}
			}
		})
	}
}

// fakeJournalStore 只让 AppendJournal 失败的假存储（其余走真 SQLite）。
type failingJournalStore struct {
	*sqlite.Store
	appendErr error
}

func (f failingJournalStore) AppendJournal(ctx context.Context, ns store.Namespace, e *types.JournalEntry) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	return f.Store.AppendJournal(ctx, ns, e)
}

// 审计写失败**必须可见**：`_ =` 吞掉的话，「退役/合并必须留审计条目」
// 这条承诺静默失效——PG 侧写坏的年代正是这么丢的审计（issue #40）。
func TestJournalWriteFailureIsVisible(t *testing.T) {
	cases := []struct {
		name string
		run  func(ctx context.Context, s lifecycle.Store) error
	}{
		{"Consolidate", func(ctx context.Context, s lifecycle.Store) error {
			_, err := lifecycle.Consolidate(ctx, s, ns)
			return err
		}},
		{"Expire", func(ctx context.Context, s lifecycle.Store) error {
			_, err := lifecycle.Expire(ctx, s, ns)
			return err
		}},
		{"Retention", func(ctx context.Context, s lifecycle.Store) error {
			_, err := lifecycle.Retention(ctx, s, ns)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := seed(t)
			ctx := context.Background()
			// 造一张会触发动作的事实卡：同签名两张（合并）、运行期错误证据（保留期）、
			// 过期事件（按龄）三类各留一条，保证动作真的会写审计。
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			// 证据事件带运行期错误来源：Retention 的判据是「全部证据都是运行期错误」。
			must(s.SaveRaw(ctx, ns, &types.RawEvent{
				ID: "ev1", Content: "某次报错", Timestamp: now(), Type: types.EventManual,
				Provenance: types.Provenance{Origin: types.OriginModel, RuntimeError: true},
			}))
			must(s.SaveAtom(ctx, ns, &types.AtomCard{
				ID: "a1", EntityID: "e1", Assertion: "机器甲磁盘不足", QuoteEventID: "ev1",
				RawEventIDs: []string{"ev1"}, Importance: types.ImportanceMed,
				Confidence: types.ConfidenceMed, OccurredAt: now().AddDate(0, 0, -40),
				CreatedAt: now().AddDate(0, 0, -40), UpdatedAt: now(),
				Kind: types.FactEvent,
			}))
			if tc.name == "Consolidate" {
				must(s.SaveAtom(ctx, ns, &types.AtomCard{
					ID: "a2", EntityID: "e1", Assertion: "机器甲磁盘不足", QuoteEventID: "ev1",
					RawEventIDs: []string{"ev1"}, Importance: types.ImportanceMed,
					Confidence: types.ConfidenceMed, OccurredAt: now(), CreatedAt: now().Add(time.Hour), UpdatedAt: now(),
				}))
			}
			fake := failingJournalStore{Store: s, appendErr: errors.New("journal 后端不可写")}
			if err := tc.run(ctx, fake); err == nil {
				t.Fatal("审计写失败必须以 error 可见：吞掉等于「留痕承诺」静默失效")
			}
		})
	}
}

// 全量扫描必须**取尽**而不是拿大 Limit 冒充（issue #59）：
// GC 的取数走 store.ListAllAtoms 分页——把单页大小（500）跨过去
// （520 条），排在第一页之外的卡也必须被扫到并处置。
// 修复前这里是 Limit: 100000：超限时按取数序截断、无信号，
// 尾部（恰是最不重要的那批）永不回收。
func TestGCPaginatesBeyondFirstPage(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	const total = 520
	for i := range total {
		// 证据事件从不落库 ⇒ 每条事实的证据「全没了」，GC 应逐条退役。
		if err := s.SaveAtom(ctx, ns, atom(fmt.Sprintf("a-pg-%03d", i), "ent1",
			fmt.Sprintf("第 %d 条待回收事实", i), fmt.Sprintf("ev-missing-%03d", i), false)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := lifecycle.GC(ctx, s, ns, now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.AtomsRetired != total {
		t.Fatalf("跨页的 %d 条事实都应被 GC 扫到并退役，实得 %d", total, res.AtomsRetired)
	}
}
