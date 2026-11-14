package parity

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：2026-11-14 issue 清零批（#48-#59）补的跨后端断言。

// ── 事实卡：retire 本体的退役态单向守卫（#48）──

// 专职退役 API（SupersedeAtom / DeprecateAtom）与 SaveAtom 受同一份单向约束：
// 已退役的卡不得被二次退役改写退役态。修法前两后端同病——retire 是无守卫的
// 读改写 UPDATE，DeprecateAtom 的 mutate 无条件清空 superseded_by，
// 对已被取代的卡调人工作废会把既有取代链静默抹掉；两个并发 retire 同卡时
// 后提交者覆盖先提交者（SaveAtom 注释点名的失效在这条路径原样存在）。
func tAtomRetireOneWay(t *testing.T, s store.Store) {
	// 已被取代的卡再调人工作废：取代链必须原样保留。
	a := atom("a-ow-sup", "e1", "退役单向：取代后再作废", "ev1", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeAtom(bg(), nsA, "a-ow-sup", "a-ow-newer"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeprecateAtom(bg(), nsA, "a-ow-sup", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAtom(bg(), nsA, "a-ow-sup")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != "a-ow-newer" {
		t.Fatalf("二次退役（作废）抹掉了既有取代链: 期望 a-ow-newer 实得 %q", got.SupersededBy)
	}
	if !got.DeprecatedAt.IsZero() {
		t.Fatalf("已被取代的卡被二次退役写上作废时刻，互斥态被改写: %+v", got.DeprecatedAt)
	}

	// 反方向同样：人工作废的卡再调取代，不得写下新的取代链。
	b := atom("a-ow-dep", "e1", "退役单向：作废后再取代", "ev2", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, b); err != nil {
		t.Fatal(err)
	}
	if err := s.DeprecateAtom(bg(), nsA, "a-ow-dep", base); err != nil {
		t.Fatal(err)
	}
	if err := s.SupersedeAtom(bg(), nsA, "a-ow-dep", "a-ow-late"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAtom(bg(), nsA, "a-ow-dep")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != "" {
		t.Fatalf("二次退役（取代）在人工作废的卡上写下了取代链: %q", got.SupersededBy)
	}
	if !got.DeprecatedAt.Equal(base) {
		t.Fatalf("二次退役挪动了既有的作废时刻: 期望 %v 实得 %v", base, got.DeprecatedAt)
	}

	// 并发取代同一张活跃卡：先提交者的取代链不得被后提交者抹掉。
	// 谁赢取决于后端的事务串行化方式（SQLite 单连接排队、PG 靠行上的
	// CASE 仲裁），所以只断言「赢家稳定」：终值是两方之一，且第三次
	// retire（读到的已是退役态）不得改写它。
	c := atom("a-ow-race", "e1", "退役单向：并发取代", "ev3", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, c); err != nil {
		t.Fatal(err)
	}
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		retErr error
	)
	for _, winner := range []string{"a-ow-w1", "a-ow-w2"} {
		wg.Add(1)
		go func(w string) {
			defer wg.Done()
			if err := s.SupersedeAtom(bg(), nsA, "a-ow-race", w); err != nil {
				mu.Lock()
				retErr = err
				mu.Unlock()
			}
		}(winner)
	}
	wg.Wait()
	if retErr != nil {
		t.Fatalf("并发取代失败: %v", retErr)
	}
	got, err = s.GetAtom(bg(), nsA, "a-ow-race")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != "a-ow-w1" && got.SupersededBy != "a-ow-w2" {
		t.Fatalf("并发取代后取代链既不是 w1 也不是 w2: %q", got.SupersededBy)
	}
	winner := got.SupersededBy
	if err := s.SupersedeAtom(bg(), nsA, "a-ow-race", "a-ow-w3"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAtom(bg(), nsA, "a-ow-race")
	if err != nil {
		t.Fatal(err)
	}
	if got.SupersededBy != winner {
		t.Fatalf("第三次取代抹掉了先到者的取代链: 期望 %s 实得 %s", winner, got.SupersededBy)
	}
}

// ── 实体：SaveEntity 的整实体语义（#51）──

// Merge 的失败恢复故事押在「最后一步 SaveEntity 把 from 计数清 0」上：
// SaveEntity 的 ON CONFLICT 只更新 entity_type/canonical_name 的话，
// 清零是 no-op，中途失败的 Bump 差量留下的幽灵计数永久留在库里
// （ListEntities 还按 atom_count DESC 把它排最前）。
// 本用例在**真后端**上走「迁移半途失败（一次 Bump 差量丢失）→ 重跑同一条
// Merge」的完整序列，断言终态 from 计数为 0——钉的是 SQL 语义，
// 不是 fake 的语义（整实体覆盖的 fake 恰好会替坏实现背书）。
func tEntitySaveEntityWholeRow(t *testing.T, s store.Store) {
	const total = 5
	_ = s.SaveEntity(bg(), nsA, &types.Entity{
		ID: "e-to", EntityType: "机器", CanonicalName: "目标机器", CreatedAt: base,
	})
	_ = s.SaveEntity(bg(), nsA, &types.Entity{
		ID: "e-from", EntityType: "机器", CanonicalName: "来源机器",
		CreatedAt: base, AtomCount: total, LastPromotedAt: base,
	})
	// 5 张卡挂在 from 名下（计数与卡数一致）。
	for i := range total {
		a := atom(fmt.Sprintf("atom-mv-%d", i), "e-from",
			fmt.Sprintf("迁移用例事实 %d 号", i), fmt.Sprintf("ev-mv-%d", i), types.ImportanceMed)
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}
	// 迁移一步 = 卡改归属 + 双向 Bump（entity.Merge 的机械迁移）。
	move := func(id string) {
		t.Helper()
		a, err := s.GetAtom(bg(), nsA, id)
		if err != nil {
			t.Fatal(err)
		}
		a.EntityID = "e-to"
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
		if err := s.BumpEntityAtomCount(bg(), nsA, "e-to", 1); err != nil {
			t.Fatal(err)
		}
		if err := s.BumpEntityAtomCount(bg(), nsA, "e-from", -1); err != nil {
			t.Fatal(err)
		}
	}
	moveAtomOnly := func(id string) {
		// 只搬卡、不记账：Merge 在「SaveAtom 与 Bump 之间」失败的样子——
		// 这正是它声称被终态抹平的「中途失败的 Bump 差量」。
		t.Helper()
		a, err := s.GetAtom(bg(), nsA, id)
		if err != nil {
			t.Fatal(err)
		}
		a.EntityID = "e-to"
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}

	// 第一轮：完整迁走 atom-0，迁 atom-1 时**卡搬了、计数没记**就失败，
	// 终态的 SaveEntity(from, 0) 也没跑。
	move("atom-mv-0")
	moveAtomOnly("atom-mv-1")

	// 重跑同一条 Merge：ListAtomsByEntity(from) 只剩没搬走的 3 张，
	// 逐张迁移，最后 from 退役（计数清 0、晋升时刻清空）。
	move("atom-mv-2")
	move("atom-mv-3")
	move("atom-mv-4")
	from, err := s.GetEntity(bg(), nsA, "e-from")
	if err != nil {
		t.Fatal(err)
	}
	from.AtomCount = 0
	from.LastPromotedAt = time.Time{}
	if err := s.SaveEntity(bg(), nsA, from); err != nil {
		t.Fatal(err)
	}

	gotFrom, err := s.GetEntity(bg(), nsA, "e-from")
	if err != nil {
		t.Fatal(err)
	}
	if gotFrom.AtomCount != 0 {
		t.Fatalf("重跑收敛后 from 计数应为 0（脏差量被终态抹平），实得 %d", gotFrom.AtomCount)
	}
	if !gotFrom.LastPromotedAt.IsZero() {
		t.Fatalf("重跑收敛后 from 的 LastPromotedAt 应已清空: %v", gotFrom.LastPromotedAt)
	}
	// to 计数带一次永久丢失的 Bump 差量（4 而非 5）：契约只承诺
	// 「from 的计数误差被终态抹平」，to 侧那次丢失的差量不在承诺内。
	// 没有这条断言的话，move 换成纯 Bump 也能凑出 from=0（假绿）。
	gotTo, err := s.GetEntity(bg(), nsA, "e-to")
	if err != nil {
		t.Fatal(err)
	}
	if gotTo.AtomCount != total-1 {
		t.Fatalf("to 计数应为 %d（含一次丢失的 Bump 差量），实得 %d", total-1, gotTo.AtomCount)
	}
}
