package parity

import (
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：事实卡的**时效类别**（见 types.FactKind）与**事件窗口**
// （见 store.AtomFilter.EventSince）的跨后端行为断言。
//
// # 为什么单独成文件
//
// parity.go 已经接近本仓的文件行数上限（门禁 threshold-file-lines），
// 而这几组断言要连同「为什么」一起写——挤进 parity.go 会把上限顶破。
//
// # 这一组断言在守什么
//
// 三类事实的生命周期不同（事件会过期、状态被取代、经验永久），
// 而「一套策略套所有事实」必然两头不讨好。故判据必须**按类别**分治。
//
// 这类缺陷的共同形状：**在数据上、结果上完全看不出来**——
// 少召回一条没人察觉，多召回一条也没人察觉，
// 除非断言把「哪一条该在、哪一条不该在」写死。
// 故这里的每条断言都写死**具体哪几张卡**该出现。

// ── 事实卡的时效类别 ──

// tAtomKindRoundTrip 时效类别必须**逐字往返**。
//
// 零值那一半尤其要紧：既有事实卡没有这个维度，
// 它们的类别读出来必须是**空串**（= 默认 = 长期经验），
// 而不是「被猜成某一类」——猜错的后果是升级一次就丢一批老事实。
func tAtomKindRoundTrip(t *testing.T, s store.Store) {
	ev := kindAtom("k-event", "昨天重启过一次", types.FactEvent, base)
	st := kindAtom("k-state", "容量是 2G", types.FactState, base)
	ls := kindAtom("k-lesson", "先看日志再动手", types.FactLesson, base)
	plain := kindAtom("k-plain", "没有标注类别的事实", types.FactDefault, base)
	for _, a := range []*types.AtomCard{ev, st, ls, plain} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}
	for _, tc := range []struct {
		id   string
		want types.FactKind
	}{
		{"k-event", types.FactEvent},
		{"k-state", types.FactState},
		{"k-lesson", types.FactLesson},
		// 零值往返后必须还是零值：不是「没写就当成事件」。
		{"k-plain", types.FactDefault},
	} {
		got, err := s.GetAtom(bg(), nsA, tc.id)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", tc.id, err)
		}
		if got.Kind != tc.want {
			t.Fatalf("%s 的时效类别往返失真: 期望 %q，实得 %q", tc.id, tc.want, got.Kind)
		}
	}
}

// tAtomEventSinceFilter 事件窗口：**只**作用于事件类，且含下界。
//
// 三张卡：
//
//	事件类新（发生时刻恰在下界上）——在窗口内；
//	事件类旧——太老，不该出现；
//	非事件类旧——老，但**不受事件窗口约束**。
//
// 边界含下界：发生时刻**恰好等于**下界的那条要出现。
// 下界取闭区间还是开区间，两种都自洽，但必须**两个后端一样**——
// 而「一样」只有写成断言才保得住。
func tAtomEventSinceFilter(t *testing.T, s store.Store) {
	evNew := kindAtom("ev-new", "昨天重启过一次", types.FactEvent, base)
	evOld := kindAtom("ev-old", "上周重启过一次", types.FactEvent, base.Add(-48*time.Hour))
	lsOld := kindAtom("ls-old", "重启前先看日志", types.FactLesson, base.Add(-48*time.Hour))
	for _, a := range []*types.AtomCard{evNew, evOld, lsOld} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}
	want := []string{"ev-new", "ls-old"}

	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{EventSince: base, Limit: 100})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if ids := atomIDs(got); !sameIDs(ids, want) {
		t.Fatalf("事件窗口应只留下 %v，实得 %v", want, ids)
	}

	// 检索路要守**同一套**判据：SearchAtoms 漏了事件窗口的话，
	// 太老的事件照样会被搜出来——症状是「召回里混进过期的事件」，
	// 那在结果里完全看不出来。
	hits, err := s.SearchAtoms(bg(), nsA, "重启", store.AtomFilter{EventSince: base, Limit: 100})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if ids := atomIDs(hits); !sameIDs(ids, want) {
		t.Fatalf("检索路应同样按事件窗口过滤，应得 %v，实得 %v", want, ids)
	}
}

// tAtomEventSinceKeepsNonEvent 事件窗口**不影响非事件类**。
//
// 事件会过期，而状态的「过期」由取代链表达、经验永久保留。
// 若事件窗口误伤它们，症状是「容量这条状态类事实某天突然查不到了」——
// 而它没有报错、没有告警，只是内容少了。
//
// 三张卡分别是状态类、经验类与零值默认类，且**都很老**。
func tAtomEventSinceKeepsNonEvent(t *testing.T, s store.Store) {
	old := base.Add(-365 * 24 * time.Hour)
	st := kindAtom("nk-state", "容量是 2G", types.FactState, old)
	ls := kindAtom("nk-lesson", "先看日志再动手", types.FactLesson, old)
	plain := kindAtom("nk-plain", "没有标注类别的事实", types.FactDefault, old)
	for _, a := range []*types.AtomCard{st, ls, plain} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}
	want := []string{"nk-state", "nk-lesson", "nk-plain"}
	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{EventSince: base, Limit: 100})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if ids := atomIDs(got); !sameIDs(ids, want) {
		t.Fatalf("事件窗口不该影响非事件类，应得 %v，实得 %v", want, ids)
	}
}

// tAtomEventSinceZeroValue 不给 EventSince 时，行为与从前**完全一致**。
//
// 零值 = 不限。这条是「加一个维度不该改判既有事实」的钉子：
// 不配它时取数结果必须与配它之前逐条相同，
// 否则升级一次，同一个库召回到的东西就变了。
func tAtomEventSinceZeroValue(t *testing.T, s store.Store) {
	evOld := kindAtom("zv-event", "很久以前重启过一次", types.FactEvent, base.Add(-48*time.Hour))
	evNew := kindAtom("zv-new", "刚刚重启过一次", types.FactEvent, base)
	lsOld := kindAtom("zv-lesson", "重启前先看日志", types.FactLesson, base.Add(-48*time.Hour))
	for _, a := range []*types.AtomCard{evOld, evNew, lsOld} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}
	want := []string{"zv-event", "zv-new", "zv-lesson"}

	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if ids := atomIDs(got); !sameIDs(ids, want) {
		t.Fatalf("不给事件窗口时不该过滤，应得 %v，实得 %v", want, ids)
	}
	// 显式给一个零值时刻，结果必须与不给**逐条相同**。
	zero, err := s.ListAtoms(bg(), nsA, store.AtomFilter{EventSince: time.Time{}, Limit: 100})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if ids := atomIDs(zero); !sameIDs(ids, want) {
		t.Fatalf("零值事件窗口不该过滤，应得 %v，实得 %v", want, ids)
	}
}

// tAtomEventSinceAndTimeWindow 事件窗口与时间窗是**两个维度**（AND 关系）。
//
// EventSince 问的是「默认别把太老的事件端上来」（召回的默认策略），
// OccurredFrom/OccurredTo 问的是「用户要哪个时间段」（显式提问）。
// 两者同时给时是 AND：都要满足。
//
// 三张卡各自只违反其中一条：
//
//	两条都满足——留下；
//	违反时间窗但满足事件窗口——排除（否则说明事件窗口吞掉了时间窗）；
//	满足时间窗但违反事件窗口——排除（否则说明事件窗口被忽略了）。
func tAtomEventSinceAndTimeWindow(t *testing.T, s store.Store) {
	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)

	both := kindAtom("dw-both", "重启过一次并且落在窗口里", types.FactEvent, base)
	outside := kindAtom("dw-outside", "重启过一次但落在窗口外", types.FactEvent, base.Add(2*time.Hour))
	// 落在时间窗内，却早于事件窗口的下界：只有事件窗口生效时它才该被排除。
	tooOld := kindAtom("dw-old", "重启过一次但比事件下界更早", types.FactEvent, base.Add(-30*time.Minute))
	for _, a := range []*types.AtomCard{both, outside, tooOld} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}
	want := []string{"dw-both"}
	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{
		EventSince:   base,
		OccurredFrom: from,
		OccurredTo:   to,
		Limit:        100,
	})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if ids := atomIDs(got); !sameIDs(ids, want) {
		t.Fatalf("事件窗口与时间窗是 AND 关系，应得 %v，实得 %v", want, ids)
	}
}

// kindAtom 造一张带**时效类别**与**发生时刻**的事实卡。
//
// 类别与发生时刻都是这一组断言的**判别特征**：卡的内容不同
// （断言文本不同 ⇒ 签名不同 ⇒ 不会被去重闸塌成一条），
// 所以「哪张卡该出现」只由类别与时刻决定。
func kindAtom(id, assertion string, kind types.FactKind, occurred time.Time) *types.AtomCard {
	a := atom(id, "e1", assertion, "ev-"+id, types.ImportanceMed)
	a.Kind = kind
	a.OccurredAt = occurred
	return a
}
