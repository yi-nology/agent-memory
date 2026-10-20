package parity

import (
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// ── 事实卡 ──

func tAtomRetiredExcluded(t *testing.T, s store.Store) {
	live := atom("a-live", "e1", "活跃事实", "ev1", types.ImportanceHigh)
	sup := atom("a-sup", "e1", "被推翻的事实", "ev1", types.ImportanceMed)
	dep := atom("a-dep", "e1", "被作废的事实", "ev1", types.ImportanceLow)
	for _, a := range []*types.AtomCard{live, sup, dep} {
		if err := s.SaveAtom(bg(), nsA, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SupersedeAtom(bg(), nsA, "a-sup", "a-live"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeprecateAtom(bg(), nsA, "a-dep", base); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListAtoms(bg(), nsA, store.AtomFilter{ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a-live" {
		var ids []string
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		t.Fatalf("只该召回活跃事实，实得 %v", ids)
	}
	// 不过滤时两条退役事实都还在（软退役不删）。
	all, err := s.ListAtoms(bg(), nsA, store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("不过滤时应得 3 条，实得 %d", len(all))
	}
}

func tAtomSignature(t *testing.T, s store.Store) {
	_ = s.SaveAtom(bg(), nsA, atom("a1", "e1", "磁盘剩余 2G", "ev1", types.ImportanceMed))
	dup, err := s.FindAtomBySignature(bg(), nsA, atom("tmp", "e1", "  磁盘剩余 2G  ", "ev2", types.ImportanceMed).Signature())
	if err != nil {
		t.Fatalf("空白差异应折叠成同一签名: %v", err)
	}
	if dup.ID != "a1" {
		t.Fatalf("应命中 a1，实得 %s", dup.ID)
	}
	// 数量不同的事实必须分开——它们互相矛盾，该送审不是合并。
	if _, err := s.FindAtomBySignature(bg(), nsA,
		atom("tmp2", "e1", "磁盘剩余 3G", "ev3", types.ImportanceMed).Signature()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("数量不同的事实不该撞成同一签名，实得 %v", err)
	}
}

// 中文子串检索必须命中**真实形态**的中文正文。
//
// 这条曾经被违反：FTS5 的 unicode61 分词器把连续汉字算作**一个词元**，
// 于是索引里的「磁盘长期偏小」与查询「磁盘」永不相等 → 0 命中。
//
// 而夹具若写成「磁盘 剩余 2G」（汉字之间有空格），unicode61 正好切成两个词元，
// 查询照样命中——**夹具替实现蒙混过关**，缺陷在 parity 套件下全绿。
// 故这里的夹具一律用真实中文形态（句子内部没有空格分词）。
func tAtomSearch(t *testing.T, s store.Store) {
	_ = s.SaveAtom(bg(), nsA, atom("m1", "e1", "磁盘剩余 2G", "ev1", types.ImportanceHigh))
	_ = s.SaveAtom(bg(), nsA, atom("m2", "e1", "内存占用偏高", "ev1", types.ImportanceHigh))
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "m1" {
		t.Fatalf("应只命中 m1，实得 %+v", hits)
	}
	// 子串不在词首：中文查询常是词中的一部分（「剩余」「占用」）。
	hits2, err := s.SearchAtoms(bg(), nsA, "剩余", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits2) != 1 || hits2[0].ID != "m1" {
		t.Fatalf("词中子串应命中 m1，实得 %+v", hits2)
	}
	// 英文侧不能被中文归一化带坏。
	_ = s.SaveAtom(bg(), nsA, atom("m3", "e1", "the disk is almost full", "ev1", types.ImportanceHigh))
	en, err := s.SearchAtoms(bg(), nsA, "disk", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(en) != 1 || en[0].ID != "m3" {
		t.Fatalf("英文查询应只命中 m3，实得 %+v", en)
	}
}

// 真实形态的中文正文必须能被中文子串检索命中，且不同子串互不串味。
//
// 这条是「夹具替实现背书」的教训本身：夹具按真实形态写，
// 实现若退回字典序/整词匹配，这条会红。
func tAtomSearchChineseSubstring(t *testing.T, s store.Store) {
	_ = s.SaveAtom(bg(), nsA, atom("c1", "e1", "磁盘长期偏小需要清理旧文件", "ev1", types.ImportanceHigh))
	for _, q := range []string{"磁盘", "长期", "清理", "旧文件"} {
		hits, err := s.SearchAtoms(bg(), nsA, q, store.AtomFilter{Limit: 5})
		if err != nil {
			t.Fatalf("检索 %q 失败: %v", q, err)
		}
		if len(hits) != 1 || hits[0].ID != "c1" {
			t.Fatalf("中文子串 %q 应只命中 c1，实得 %+v", q, hits)
		}
	}
	// 不在正文里的子串不得命中。
	hits, err := s.SearchAtoms(bg(), nsA, "扩容", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("不相关的查询不该命中，实得 %+v", hits)
	}
}

// 没有出处的记忆既不能审计也不能推翻——入库前就该拦下。
func tAtomValidation(t *testing.T, s store.Store) {
	bad := &types.AtomCard{
		ID: "a1", Assertion: "无出处", Confidence: types.ConfidenceHigh,
		Importance: types.ImportanceMed, OccurredAt: base, CreatedAt: base,
	}
	if err := s.SaveAtom(bg(), nsA, bad); !errors.Is(err, types.ErrAtomNoEvidence) {
		t.Fatalf("无出处的事实卡必须被拒，实得 %v", err)
	}
	// 冲突的退役两态同样在入库前拦。
	bad2 := atom("a2", "e1", "x", "ev1", types.ImportanceMed)
	bad2.SupersededBy = "a3"
	bad2.DeprecatedAt = base
	if err := s.SaveAtom(bg(), nsA, bad2); !errors.Is(err, types.ErrAtomDoubleRetired) {
		t.Fatalf("冲突退役态必须被拒，实得 %v", err)
	}
	// 空继任者会让事实永远查不到自己被谁推翻。
	if err := s.SupersedeAtom(bg(), nsA, "a2", ""); err == nil {
		t.Fatal("空继任者必须被拒")
	}
}

// SaveAtom 是 upsert：事实卡会被更新（并入补证据、退役改状态、合并迁归属）。
// 用纯 INSERT 的话，第二次保存就撞唯一键——而这个 bug 潜伏了很久，
// 只在「真的发生并入」时暴露。
// SaveAtom 是 upsert：事实卡会被更新（并入补证据、退役改状态、合并迁归属）。
func tAtomSaveIsUpsert(t *testing.T, s store.Store) {
	a := atom("a-up", "e1", "某条事实", "ev1", types.ImportanceMed)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatal(err)
	}
	a.RawEventIDs = []string{"ev1", "ev2"}
	a.UpdatedAt = base.Add(time.Hour)
	if err := s.SaveAtom(bg(), nsA, a); err != nil {
		t.Fatalf("重复保存应是更新而不是唯一键冲突: %v", err)
	}
	got, err := s.GetAtom(bg(), nsA, "a-up")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RawEventIDs) != 2 {
		t.Fatalf("重复保存未落更新: %+v", got.RawEventIDs)
	}
}

// 并列时的返回顺序必须确定。
//
// 重要度与发生时刻都相同的两张卡，若没有稳定序，「同签名合并时保留哪一张」
// 就随遍历顺序变——同一批数据合并两次得到不同结果。
func tAtomOrderIsDeterministic(t *testing.T, s store.Store) {
	for _, id := range []string{"d3", "d1", "d2"} {
		if err := s.SaveAtom(bg(), nsA, atom(id, "e1", "并列事实 "+id, "ev1", types.ImportanceMed)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatal("同一次查询返回条数不该变")
		}
		for j := range again {
			if again[j].ID != first[j].ID {
				t.Fatalf("并列时的返回顺序不确定: 第 %d 位 %s vs %s", j, first[j].ID, again[j].ID)
			}
		}
	}
}

// 带向量的事实卡必须被拒。
//
// 本仓的定案是「向量轨不接」：两个后端都不落向量。
// 静默丢掉的话，症状是「相似度检索永远空」——
// 而它与「存错了地方」毫无相似之处，极难归因。
// 故带向量的卡入库时**必须报错**。
func tAtomVectorNotStored(t *testing.T, s store.Store) {
	a := atom("v1", "e1", "磁盘长期偏小", "ev1", types.ImportanceHigh)
	a.Vector = []float32{0.1, 0.2, 0.3}
	if err := s.SaveAtom(bg(), nsA, a); err == nil {
		t.Fatal("带向量的卡必须被拒：后端不落向量，静默丢掉比报错更坏")
	}
}

// 按实体取事实卡：必须**包含已退役的**。
//
// 实体合并要把该实体名下所有卡（含已退役的）一起迁到新归属——
// 漏掉已退役的，那些卡会永远指向一个已经不存在的实体，
// 而它们仍在库里、仍可被搜到。
func tAtomListByEntity(t *testing.T, s store.Store) {
	live := atom("e-live", "e1", "磁盘长期偏小", "ev1", types.ImportanceHigh)
	dead := atom("e-dead", "e1", "内存泄漏已修复", "ev1", types.ImportanceHigh)
	dead.SupersededBy = "e-live"
	_ = s.SaveAtom(bg(), nsA, live)
	_ = s.SaveAtom(bg(), nsA, dead)
	_ = s.SaveAtom(bg(), nsA, atom("other", "e2", "别的事实", "ev1", types.ImportanceMed))

	got, err := s.ListAtomsByEntity(bg(), nsA, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应取回 e1 名下全部 2 张（含已退役），实得 %d 张", len(got))
	}
	other, err := s.ListAtomsByEntity(bg(), nsA, "e2")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].ID != "other" {
		t.Fatalf("应只取回 e2 名下的那一张，实得 %+v", other)
	}
}

// 重要度排序必须是**语义序**（high → med → low），不是字典序。
//
// 这条曾经被违反：importance 列是 TEXT，`ORDER BY importance DESC`
// 的字典序是 'med' > 'low' > 'high'——**最重要的排在最后**。
// 后果：召回的候选池是 ListAtoms 的前 N 条（N 默认 50），
// 事实卡一多，最重要的那批最先被裁掉，而它们恰恰是最该进 prompt 的。
// 晋升的矛盾检测、生命周期三件套、迁移包导出也都走 ListAtoms，
// 于是「按字典序取前 N 条」会渗进所有依赖它的动作。
//
// 跨后端断言：PG 侧若用同样的字典序排序，这条会红。
func tAtomImportanceOrder(t *testing.T, s store.Store) {
	// 同一发生时刻，只让重要度不同——把其他变量钉死。
	for _, tc := range []struct {
		id  string
		imp types.Importance
	}{
		{"i-high", types.ImportanceHigh},
		{"i-med", types.ImportanceMed},
		{"i-low", types.ImportanceLow},
	} {
		if err := s.SaveAtom(bg(), nsA, atom(tc.id, "e1", "同刻事实 "+tc.id, "ev1", tc.imp)); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListAtoms(bg(), nsA, store.AtomFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range list {
		got = append(got, a.ID)
	}
	want := []string{"i-high", "i-med", "i-low"}
	if len(got) != len(want) {
		t.Fatalf("本用例的三张卡应全部返回，实得 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("重要度排序应按语义（high→med→low），实得 %v", got)
		}
	}
}

func tAtomRetiredStillSearchable(t *testing.T, s store.Store) {
	_ = s.SaveRaw(bg(), nsA, &types.RawEvent{
		ID: "ev1", Timestamp: base, Type: types.EventManual, Content: "磁盘 曾经很紧张",
	})
	_ = s.SaveAtom(bg(), nsA, atom("a1", "e1", "磁盘 曾经很紧张", "ev1", types.ImportanceHigh))
	if err := s.DeprecateAtom(bg(), nsA, "a1", base); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchAtoms(bg(), nsA, "磁盘", store.AtomFilter{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !hits[0].SoftRetired() {
		t.Fatalf("退役事实必须仍可搜到（用于回答「当初为何这么判」）: %+v", hits)
	}
}
