package parity

import (
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：事实卡的**宽松**词法检索（命中任一词就算，见 store.AtomStore.SearchAtomsLoose）。
//
// # 为什么它要单独成文件
//
// 宽松路与严格路（SearchAtoms）只差**词的匹配方式**：
// 严格路是隐式 AND（每个词都要在），宽松路是 OR（命中任一词就算）。
//
// # 这一组断言在守什么
//
// 宽松路最危险的失效形态是「**过滤条件被漏掉**」：
// 它与「命中范围变宽」长得很像，而后者的后果只是「多召回几条可能相关的」
// （由排序与融合兜着），前者的后果是**泄漏**（别家的卡进了候选池）。
//
// 故断言要同时钉住两件事：
//   ① 宽松路确实更宽（命中任一词就算）；
//   ② 放宽的是「词的匹配」，**不是「可见性」**——过滤条件一条也不许少。

// tAtomSearchLooseMatchesAnyWord 宽松路：命中任一词就算，且过滤条件一条不少。
func tAtomSearchLooseMatchesAnyWord(t *testing.T, s store.Store) {
	// 四张卡：两条命中查询里的**全部**词，一条只命中**一个**词，
	// 一条谁都不命中（它用来守住「别把不相关的也捞进来」）。
	both := kindAtom("lo-both", "磁盘与内存都要看", types.FactLesson, base)
	one := kindAtom("lo-one", "磁盘余量偏低", types.FactLesson, base)
	none := kindAtom("lo-none", "与本查询毫无关系的一条", types.FactLesson, base)
	// 太老的事件类事实：它**只命中一个词**，宽松路本该命中它——
	// 事件窗口若被漏掉，它就会混进结果（那正是本用例要守的失效）。
	old := kindAtom("lo-old", "磁盘余量更低", types.FactEvent, base.Add(-48*time.Hour))
	// 已退役的事实：活跃过滤若被漏掉，它会混进结果。
	retired := kindAtom("lo-retired", "磁盘余量已作废", types.FactLesson, base)
	retired.SupersededBy = "lo-both"

	for _, a := range []*types.AtomCard{both, one, none, old, retired} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatalf("写入失败(%s): %v", a.ID, err)
		}
	}

	// 严格路：每个词都要在，故只命中「全部词」那一张。
	strict, err := s.SearchAtoms(bg(), nsA, "磁盘 内存", store.AtomFilter{ActiveOnly: true, Limit: 100})
	if err != nil {
		t.Fatalf("严格检索失败: %v", err)
	}
	if ids := atomIDs(strict); !sameIDs(ids, []string{"lo-both"}) {
		t.Fatalf("严格路应只命中全部词的那张，实得 %v", ids)
	}

	// 宽松路：命中任一词就算，故「只命中一个词」的那张也要进来；
	// 但谁都不命中的那张**不该**进来（放宽的是「词」，不是「什么都能进」）。
	loose, err := s.SearchAtomsLoose(bg(), nsA, "磁盘 内存", store.AtomFilter{ActiveOnly: true, Limit: 100})
	if err != nil {
		t.Fatalf("宽松检索失败: %v", err)
	}
	if ids := atomIDs(loose); !sameIDs(ids, []string{"lo-both", "lo-one", "lo-old"}) {
		t.Fatalf("宽松路应命中任一词的那几张，实得 %v", ids)
	}

	// 事件窗口在宽松路**同样生效**：太老的事件类事实该被挡掉。
	// 漏掉它的失效形态是「泄漏」而不是「少取」——因为它挡的是别的事实。
	windowed, err := s.SearchAtomsLoose(bg(), nsA, "磁盘 内存",
		store.AtomFilter{ActiveOnly: true, EventSince: base, Limit: 100})
	if err != nil {
		t.Fatalf("带事件窗口的宽松检索失败: %v", err)
	}
	if ids := atomIDs(windowed); !sameIDs(ids, []string{"lo-both", "lo-one"}) {
		t.Fatalf("宽松路漏掉了事件窗口，应得 %v，实得 %v",
			[]string{"lo-both", "lo-one"}, ids)
	}
}
