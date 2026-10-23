package store_test

import (
	"context"
	"testing"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：可选能力的**探测语义**。
//
// # 为什么探测要有独立用例
//
// 探测是「可选能力」这件事的**唯一执行面**：接口声明只是文档，
// 而「调用方到底问没问」才是行为。
//
// 三种失效形态：
//
//   - 不探测 ⇒ 编译期打红（issue #12 的原形）；
//   - 探测了但**没降级** ⇒ 宿主装个半套就崩；
//   - 探测了、降级了、但**没观测** ⇒ 静默降级（最阴的一种）。
//
// 观测那一面在 pipeline/recall 的用例里（Result.LooseSearchUnavailable + 指标），
// 本文件只守「探测本身」。

// looseImpl 实现了宽松检索的假实现。
type looseImpl struct{}

func (looseImpl) SearchAtomsLoose(context.Context, store.Namespace, string,
	store.AtomFilter) ([]*types.AtomCard, error) {
	return nil, nil
}

// plainImpl 什么都不实现的假实现（它连 recall.Reader 都不是，只用来验探测）。
type plainImpl struct{}

// 实现了就有，没实现就没有——探测的语义就这一条。
func TestLooseSearcherDetectsCapability(t *testing.T) {
	if _, ok := store.LooseSearcher(looseImpl{}); !ok {
		t.Fatal("实现了宽松检索的实现必须被认出来")
	}
	if _, ok := store.LooseSearcher(plainImpl{}); ok {
		t.Fatal("没实现的必须被认成没有（否则调用方会把不存在的能力当真）")
	}
}

// nil 不得 panic。
//
// 理由：探测的输入常常是「可能为 nil 的接口值」
// （宿主没注入、装配面漏传……）。探测函数 panic ⇒ 故障从「少召回」
// 变成「整个进程崩」——而前者是降级，后者是事故。
func TestLooseSearcherHandlesNil(t *testing.T) {
	if _, ok := store.LooseSearcher(nil); ok {
		t.Fatal("nil 必须被认成没有")
	}
	var l store.AtomLooseSearcher
	if _, ok := store.LooseSearcher(l); ok {
		t.Fatal("nil 接口值必须被认成没有")
	}
}

// 探测出来的能力**能真的用**——不是「断言说是就是」。
//
// 这条守的是「探测函数返回的对象与被探测对象是同一个实现」：
// 若探测里包了一层空壳，调用方会拿到一个永远返回空集的能力，
// 而症状是「宽松路永远捞不到东西」——看着像排序问题。
func TestLooseSearcherReturnsUsableCapability(t *testing.T) {
	want := []*types.AtomCard{{ID: "a-1"}}
	impl := looseWithCards{want: want}
	ls, ok := store.LooseSearcher(impl)
	if !ok {
		t.Fatal("实现了就必须被认出来")
	}
	got, err := ls.SearchAtomsLoose(context.Background(), "ns", "q", store.AtomFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a-1" {
		t.Fatalf("探测到的能力必须真的能用，实得 %+v", got)
	}
}

// looseWithCards 返回固定事实的假实现。
type looseWithCards struct {
	want []*types.AtomCard
}

func (l looseWithCards) SearchAtomsLoose(context.Context, store.Namespace, string,
	store.AtomFilter) ([]*types.AtomCard, error) {
	return l.want, nil
}
