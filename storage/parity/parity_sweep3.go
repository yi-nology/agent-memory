package parity

import (
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：第三轮 issue 清零补的跨后端断言——
// 事实卡的退役态单向保护与 created_at 定格（#21）、列表全序兜底键（#26）、
// 活跃实体淘汰的 pinned 复核（#22）。行与索引的事务化（#23）是 SQLite 侧
// 的实现形态（PG 的词元向量与正文同行落库，没有独立索引可失配），
// 其断言在 storage/sqlite 的用例里。

// ── 事实卡：退役态只许单向置位（#21）──

// 陈旧写者拿退役前的读视图整卡回写，不得把已退役的卡「复活」；
// 并发取代的第二个晋升者，也不得抹掉先提交者的取代链。
//
// 钉的失效（修复前两后端同病）：SaveAtom 是整卡 upsert，superseded_by /
// deprecated_at / valid_to 无条件回写——晋升路径「读出旧卡 → 改退役态 →
// SaveAtom(old)」在 PG READ COMMITTED 下两个晋升器并发取代同一张旧卡时，
// 后提交者把 superseded_by 写回自己的值，先提交者的取代链被静默抹掉；
// 陈旧的读视图则能把已退役卡直接回写成活跃。
func tAtomStaleSaveCannotReviveRetired(t *testing.T, s store.Store) {
	a := atom("a-revive", "e1", "磁盘曾经很紧张", "ev1", types.ImportanceHigh)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatal(err)
	}
	// 正常退役（晋升路径的形态）：被 a-newer 取代，有效期到此刻为止。
	retired := *a
	retired.SupersededBy = "a-newer"
	retired.ValidTo = base.Add(time.Hour)
	retired.UpdatedAt = base.Add(time.Hour)
	if err := s.SaveAtom(bg(), nsA, &retired); err != nil {
		t.Fatal(err)
	}

	// 陈旧写者：拿退役**前**的读视图（取代链为空、有效期零值）整卡回写。
	stale := *a
	stale.UpdatedAt = base.Add(2 * time.Hour)
	if err := s.SaveAtom(bg(), nsA, &stale); err != nil {
		t.Fatalf("陈旧回写不该报错（它应被存储层的退役态保护挡下）: %v", err)
	}
	got, err := s.GetAtom(bg(), nsA, "a-revive")
	if err != nil {
		t.Fatal(err)
	}
	if got.Active() {
		t.Fatalf("退役卡被陈旧写者复活: %+v", got)
	}
	if got.SupersededBy != "a-newer" {
		t.Fatalf("取代链被抹掉: 期望 a-newer 实得 %q", got.SupersededBy)
	}
	if !got.ValidTo.Equal(base.Add(time.Hour)) {
		t.Fatalf("有效期被陈旧回写清掉: %+v", got.ValidTo)
	}

	// 并发取代的确定性模拟：第二个晋升者拿着自己对旧卡（读到时还是活跃态）
	// 的取代决定来写——先提交者的取代链必须保留（退役态只许单向置位）。
	second := *a
	second.SupersededBy = "a-second"
	second.UpdatedAt = base.Add(3 * time.Hour)
	if err := s.SaveAtom(bg(), nsA, &second); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAtom(bg(), nsA, "a-revive")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != "a-newer" {
		t.Fatalf("后到晋升者抹掉了先到者的取代链: 期望 a-newer 实得 %q", got.SupersededBy)
	}

	// 人工作废形态同样不得复活。
	if err := s.SaveAtom(bg(), nsA, atom("a-deprecate", "e1", "另一条事实", "ev2", types.ImportanceMed)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeprecateAtom(bg(), nsA, "a-deprecate", base); err != nil {
		t.Fatal(err)
	}
	staleDep := atom("a-deprecate", "e1", "另一条事实", "ev2", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, staleDep); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAtom(bg(), nsA, "a-deprecate")
	if err != nil {
		t.Fatal(err)
	}
	if got.Active() {
		t.Fatalf("人工作废的卡被陈旧回写复活: %+v", got)
	}
}

// created_at 首建即定格：晚到的写者带着不同的创建时刻回写，不得挪动它。
//
// FindAtomBySignature 的 ORDER BY created_at DESC 靠它选「最新卡」——
// created_at 被回写覆写的话，同一批记忆在两个后端（此前只有 PG 覆写）
// 会选出不同的卡，签名去重结果随部署形态变。
func tAtomCreatedAtStable(t *testing.T, s store.Store) {
	a := atom("a-created", "e1", "事实卡 created_at 定格", "ev1", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatal(err)
	}
	b := *a
	b.CreatedAt = base.Add(time.Hour)
	b.UpdatedAt = base.Add(time.Hour)
	if err := s.SaveAtom(bg(), nsA, &b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAtom(bg(), nsA, "a-created")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(base) {
		t.Fatalf("created_at 应保持首建值: 期望 %v 实得 %v", base, got.CreatedAt)
	}
}

// ── 列表的全序兜底键（#26）──

// 同一时刻创建的多个候选：ListCandidates 的返回顺序必须确定
// （created_at DESC, id DESC），两次查询逐字相同——否则带 LIMIT 的翻页
// 重行/漏行，导出的包逐次不同。没有兜底键时并列先后不定，两后端口径分叉。
func tCandidateListOrderDeterministic(t *testing.T, s store.Store) {
	ids := []string{"c-mid", "c-late", "c-early", "c-beta", "c-alpha"}
	for _, id := range ids {
		c := &types.Candidate{
			ID: id, RawEventIDs: []string{"ev1"}, Type: types.CandidateFact,
			Status: types.CandidatePending, Assertion: "并列候选 " + id,
			SubjectName: "机器甲", Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
			CreatedAt: base, // **全部同一时刻**：并列是这条断言的前提
		}
		if err := s.SaveCandidate(bg(), nsA, c); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"c-mid", "c-late", "c-early"} // id 降序的前三个（全并列 ⇒ 只剩 id 决胜）
	for round := range 2 {
		got, err := s.ListCandidates(bg(), nsA, store.CandidateFilter{Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("LIMIT 3 应得 3 条，实得 %d", len(got))
		}
		for i, id := range want {
			if got[i].ID != id {
				t.Fatalf("第 %d 轮并列候选的顺序不确定（期望 %v 实得 %v）",
					round+1, want, []string{got[0].ID, got[1].ID, got[2].ID})
			}
		}
	}
}

// 同一时刻标脏的多个页面：ListDirtyEntityPages 的返回顺序必须确定
// （updated_at ASC, entity_id ASC, scope ASC）——重算循环按它消费 LIMIT，
// 并列时顺序不定的话一部分脏页长时间轮不到（饿死）。
func tDirtyPageOrderDeterministic(t *testing.T, s store.Store) {
	for _, id := range []string{"p-charlie", "p-alpha", "p-bravo"} {
		if err := s.MarkEntityPageDirty(bg(), nsA, id, "", base); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"p-alpha", "p-bravo"} // updated_at 全并列 ⇒ entity_id 升序
	for round := range 2 {
		got, err := s.ListDirtyEntityPages(bg(), nsA, "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("LIMIT 2 应得 2 条，实得 %d", len(got))
		}
		if got[0].EntityID != want[0] || got[1].EntityID != want[1] {
			t.Fatalf("第 %d 轮脏页顺序不确定（期望 %v 实得 [%s %s]）",
				round+1, want, got[0].EntityID, got[1].EntityID)
		}
	}
}

// ── 活跃实体栈：淘汰复核 pinned（#22）──

// 淘汰与并发落钉同时进行：全程钉住的实体一次都不许被淘汰。
//
// TOCTOU 的窗口在 EvictActiveEntities 内部（SELECT 选受害者与逐条 DELETE
// 之间），黑盒造不出确定性的注入点，故本条断言的是**并发下的契约**：
// 该实体从第一笔写起就钉住、钉住者只写 pinned=true——任意时刻它都处于
// 钉住态，无论淘汰者跑到哪一步（SELECT 也好 DELETE 也好）都不得把它删掉
// 或上报为受害者。「DELETE 时复核 pinned」这个精确语义由各后端的白盒用例
// 钉住（storage/sqlite、storage/postgres 的 deleteUnpinnedActiveEntity 用例）：
// 它构造「SELECT 之后、DELETE 之前被钉上」的确定形态，断言行不被删。
func tActiveEntityEvictPinRace(t *testing.T, s store.Store) {
	const rounds = 100
	if err := s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
		ThreadID: "th-race", EntityID: "e-pin", CanonicalName: "被钉的",
		LastSeen: base.Add(-time.Hour), Pinned: true, PinTime: base,
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	pinErrs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range rounds {
			err := s.UpsertActiveEntity(bg(), nsA, &types.ActiveEntity{
				ThreadID: "th-race", EntityID: "e-pin", CanonicalName: "被钉的",
				LastSeen: base, Pinned: true, PinTime: base,
			})
			if err != nil {
				select {
				case pinErrs <- err:
				default:
				}
				return
			}
		}
	}()
	for range rounds {
		victims, err := s.EvictActiveEntities(bg(), nsA, "th-race", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range victims {
			if v.EntityID == "e-pin" {
				t.Fatalf("钉住实体出现在受害者里（淘汰复核失效）: %+v", victims)
			}
		}
	}
	wg.Wait()
	select {
	case err := <-pinErrs:
		t.Fatalf("并发落钉失败: %v", err)
	default:
	}
	left, err := s.ListActiveEntities(bg(), nsA, "th-race", 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range left {
		if a.EntityID == "e-pin" && a.Pinned {
			found = true
		}
	}
	if !found {
		t.Fatalf("全程钉住的实体在淘汰后必须仍在栈里（且保持钉住态）: %+v", left)
	}
}
