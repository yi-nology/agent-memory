package evaluation

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/yi-nology/agent-memory/types"
)

// 指标的手算校验。
//
// 期望值一律**手写常量**并在注释里给出推导：
// 拿实现再算一遍当期望值，那恒等于真——测试会一直绿，而口径漂了也没人知道。
func TestMetricsHandComputed(t *testing.T) {
	ds := Dataset{
		Name: "hand",
		Cases: []Case{{
			ID:       "c1",
			Query:    "alpha beta gamma",
			Relevant: []string{"r1", "r2", "r3"},
		}},
	}
	// 召回结果：4 条，第 2、4 位相关（相关集合共 3 条，漏了 r3）。
	r := fixedRetriever{results: map[string][]string{
		"c1": {"n0", "r1", "n1", "r2"},
	}}

	rep, err := Run(context.Background(), ds, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	m := rep.Metrics

	// 手算：
	//   k=1  前 1 条 = [n0]        命中 0 → Recall 0/3 = 0，Precision 0/1 = 0
	//   k=3  前 3 条 = [n0,r1,n1]  命中 1 → Recall 1/3，Precision 1/3
	//   k=5  前 5 条 = 全部 4 条    命中 2 → Recall 2/3，Precision 2/5
	//   k=10 只有 4 条，与 k=5 同样命中 2 → Recall 2/3，Precision 2/10
	wantRecall := map[int]float64{1: 0, 3: 1.0 / 3.0, 5: 2.0 / 3.0, 10: 2.0 / 3.0}
	wantPrecision := map[int]float64{1: 0, 3: 1.0 / 3.0, 5: 2.0 / 5.0, 10: 2.0 / 10.0}
	for k, want := range wantRecall {
		if got := m.RecallAtK[k]; !near(got, want) {
			t.Errorf("Recall@%d = %v，期望 %v", k, got, want)
		}
	}
	for k, want := range wantPrecision {
		if got := m.PrecisionAtK[k]; !near(got, want) {
			t.Errorf("Precision@%d = %v，期望 %v", k, got, want)
		}
	}

	// nDCG 手算：
	//   DCG@3 = 第 2 位相关 → 1/log2(3) = 0.63092975357145742
	//   IDCG@3 = 1/log2(2) + 1/log2(3) + 1/log2(4) = 2.1309297535714573
	//   nDCG@3 = 0.63092975357145742 / 2.1309297535714573 = 0.29608191096586528
	//   DCG@5 = 第 2、4 位相关 → 1/log2(3) + 1/log2(5) = 1.0616063116448504
	//   nDCG@5 = 1.0616063116448504 / 2.1309297535714573 = 0.49818925746641285
	//   k=10 与 k=5 同（只有 4 条结果）。
	wantNDCG := map[int]float64{
		1:  0,
		3:  0.29608191096586528,
		5:  0.49818925746641285,
		10: 0.49818925746641285,
	}
	for k, want := range wantNDCG {
		if got := m.NDCGAtK[k]; !near(got, want) {
			t.Errorf("nDCG@%d = %v，期望 %v", k, got, want)
		}
	}

	// MRR：第一条相关在第 2 位 → 1/2 = 0.5；HitRate：命中过 → 1。
	if !near(m.MRR, 0.5) {
		t.Errorf("MRR = %v，期望 0.5", m.MRR)
	}
	if !near(m.HitRate, 1) {
		t.Errorf("HitRate = %v，期望 1", m.HitRate)
	}
	if m.Cases != 1 {
		t.Errorf("Cases = %d，期望 1", m.Cases)
	}
}

// IDCG 的截断：相关条数多于 k 时，理想排序按 k 截断。
//
// 不截断的话，相关条数越多 IDCG 越大，nDCG 天然更低——
// 那是分母的过错，不是排序的过错。
func TestNDCGTruncatesIDCG(t *testing.T) {
	ds := Dataset{
		Name: "trunc",
		Cases: []Case{{
			ID:       "c1",
			Query:    "alpha beta gamma",
			Relevant: []string{"a", "b", "c"},
		}},
	}
	// 第一条就命中，其余不相关。
	r := fixedRetriever{results: map[string][]string{"c1": {"a", "x"}}}

	rep, err := Run(context.Background(), ds, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	m := rep.Metrics

	// nDCG@1：DCG = 1/log2(2) = 1；IDCG@1 = min(3,1) 项 = 1 → 比值 1。
	// 若 IDCG 不按 k 截断，这里会变成 1/2.1309297535714573 ≈ 0.469——
	// 那正是「相关条数多的用例被无端压低」的形态。
	if !near(m.NDCGAtK[1], 1) {
		t.Errorf("nDCG@1 = %v，期望 1（IDCG 应按 k 截断）", m.NDCGAtK[1])
	}
	// nDCG@3：DCG = 1（只有第 1 位相关）；IDCG@3 = 1 + 1/log2(3) + 1/2
	//        = 2.1309297535714573 → 1/2.1309297535714573 = 0.46927872602275655。
	if !near(m.NDCGAtK[3], 0.46927872602275655) {
		t.Errorf("nDCG@3 = %v，期望 0.46927872602275655", m.NDCGAtK[3])
	}
}

// IDCG = 0 的边界：相关条数为 0 时比值无定义，须记 0 而不是 NaN。
//
// NaN 会顺着聚合传播，把整份报告的数字全废。
func TestNDCGZeroIDCG(t *testing.T) {
	if got := ndcgAtK(0, []bool{true, true}, 3); got != 0 {
		t.Errorf("相关条数为 0 时 nDCG = %v，期望 0", got)
	}
	if got := ndcgAtK(0, nil, 1); got != 0 {
		t.Errorf("相关条数为 0 时 nDCG = %v，期望 0", got)
	}

	// 相关集合为空的用例走一遍打分：指标全零，且不是 NaN。
	m := Score(Case{ID: "c1", Query: "q"}, []string{"a", "b"}, nil)
	for k, v := range m.NDCGAtK {
		if v != 0 {
			t.Errorf("NDCG@%d = %v，期望 0", k, v)
		}
	}
	for k, v := range m.RecallAtK {
		if v != 0 {
			t.Errorf("Recall@%d = %v，期望 0", k, v)
		}
	}
}

// 相关集合为空的用例被跳过，不进任何分母。
func TestEmptyRelevantCaseSkipped(t *testing.T) {
	ds := Dataset{
		Name: "skip",
		Cases: []Case{
			{ID: "c1", Query: "alpha", Relevant: []string{"a"}},
			{ID: "c2", Query: "beta"}, // 无标注 → 无从判分
		},
	}
	r := fixedRetriever{results: map[string][]string{
		"c1": {"a"},
		"c2": {"x"},
	}}

	rep, err := Run(context.Background(), ds, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if rep.Metrics.Cases != 1 {
		t.Errorf("Cases = %d，期望 1（跳过的用例不计入）", rep.Metrics.Cases)
	}
	// 分母只算参与评分的用例：命中 1/1，而不是 1/2。
	if !near(rep.Metrics.HitRate, 1) {
		t.Errorf("HitRate = %v，期望 1（跳过的用例不进分母）", rep.Metrics.HitRate)
	}
	if len(rep.PerCase) != 2 {
		t.Fatalf("PerCase 条数 = %d，期望 2（明细仍要逐条给出）", len(rep.PerCase))
	}
	if !rep.PerCase[1].Skipped {
		t.Error("相关集合为空的用例应标记为跳过")
	}
	if rep.PerCase[1].Metrics.Cases != 0 {
		t.Errorf("跳过的用例 Cases = %d，期望 0", rep.PerCase[1].Metrics.Cases)
	}

	// 全部用例都被跳过时，各指标记 0（不是 NaN）。
	all := Dataset{Name: "all-skip", Cases: []Case{{ID: "c1", Query: "alpha"}}}
	rep2, err := Run(context.Background(), all, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if rep2.Metrics.Cases != 0 {
		t.Errorf("Cases = %d，期望 0", rep2.Metrics.Cases)
	}
	for k, v := range rep2.Metrics.RecallAtK {
		if v != 0 {
			t.Errorf("Recall@%d = %v，期望 0", k, v)
		}
	}
	if rep2.Metrics.HitRate != 0 {
		t.Errorf("HitRate = %v，期望 0", rep2.Metrics.HitRate)
	}
}

// 逐用例明细要能看出漏了哪些相关的。
func TestPerCaseShowsMissed(t *testing.T) {
	ds := Dataset{
		Name: "missed",
		Cases: []Case{{
			ID:       "c1",
			Query:    "alpha beta",
			Relevant: []string{"a", "b", "c"},
		}},
	}
	r := fixedRetriever{results: map[string][]string{"c1": {"a", "z"}}}

	rep, err := Run(context.Background(), ds, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	got := rep.PerCase[0]
	if !reflect.DeepEqual(got.Hits, []string{"a"}) {
		t.Errorf("Hits = %v，期望 [a]", got.Hits)
	}
	if !reflect.DeepEqual(got.Missed, []string{"b", "c"}) {
		t.Errorf("Missed = %v，期望 [b c]（漏掉的相关事实必须看得见）", got.Missed)
	}
	if got.FirstHitRank != 1 {
		t.Errorf("FirstHitRank = %d，期望 1", got.FirstHitRank)
	}
	if !reflect.DeepEqual(got.Retrieved, []string{"a", "z"}) {
		t.Errorf("Retrieved = %v，期望 [a z]", got.Retrieved)
	}
}

// 召回器报错必须上抛：静默 0 分会让「召回器炸了」与「召回很烂」长得一样。
func TestRunPropagatesRetrieverError(t *testing.T) {
	boom := errors.New("boom")
	ds := Dataset{
		Name:  "boom",
		Cases: []Case{{ID: "c1", Query: "alpha", Relevant: []string{"a"}}},
	}

	rep, err := Run(context.Background(), ds, fixedRetriever{err: boom})
	if err == nil {
		t.Fatal("召回器报错必须上抛，不许静默当 0 分")
	}
	if rep != nil {
		t.Error("失败时不该给出报告")
	}
	if !errors.Is(err, boom) {
		t.Errorf("错误链断了: %v", err)
	}
}

// 可复现：同一份数据 + 同一个召回器 → 同一份报告。
func TestRunIsReproducible(t *testing.T) {
	set := NewSynthetic(11, 6)
	r := fakeRetriever{facts: set.Facts(), limit: 10}

	rep1, err := Run(context.Background(), set.Dataset, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	rep2, err := Run(context.Background(), set.Dataset, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if !reflect.DeepEqual(rep1, rep2) {
		t.Error("同一份数据 + 同一个召回器必须给出同一份报告")
	}
}

// 合成数据集的可复现：同 seed 必得同结果，不同 seed 必得不同结果。
func TestSyntheticDeterministic(t *testing.T) {
	a := NewSynthetic(7, 5)
	b := NewSynthetic(7, 5)
	if !reflect.DeepEqual(a.Dataset, b.Dataset) {
		t.Error("同 seed 的两次生成必须逐条相等")
	}
	if !reflect.DeepEqual(a.Facts(), b.Facts()) {
		t.Error("同 seed 的两次生成必须给出同一批事实卡")
	}
	if !reflect.DeepEqual(Synthetic(7, 5), a.Dataset) {
		t.Error("Synthetic 与 NewSynthetic 必须同源")
	}

	c := NewSynthetic(8, 5)
	if reflect.DeepEqual(a.Dataset, c.Dataset) {
		t.Error("不同 seed 必须给出不同的数据集")
	}
}

// 合成数据集要能区分好坏召回：指标不能全是 1.0。
func TestSyntheticEvaluationSpread(t *testing.T) {
	set := NewSynthetic(42, 8)
	r := fakeRetriever{facts: set.Facts(), limit: 10}

	rep, err := Run(context.Background(), set.Dataset, r)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	m := rep.Metrics

	if m.Cases != 8 {
		t.Errorf("Cases = %d，期望 8", m.Cases)
	}
	if !near(m.HitRate, 1) {
		t.Errorf("HitRate = %v，期望 1（词面召回至少该命中一条相关）", m.HitRate)
	}
	if !(m.RecallAtK[10] > m.RecallAtK[1]) {
		t.Errorf("Recall@10 = %v，Recall@1 = %v：合成的近义干扰必须让召回率随 k 拉开差距",
			m.RecallAtK[10], m.RecallAtK[1])
	}
	if !(m.PrecisionAtK[10] < 1) {
		t.Errorf("Precision@10 = %v，期望 < 1（近义干扰占了召回位）", m.PrecisionAtK[10])
	}
	if m.NDCGAtK[5] <= 0 {
		t.Errorf("nDCG@5 = %v，期望 > 0", m.NDCGAtK[5])
	}

	// 归属隔离要真的被覆盖：语料里得有「词面完美但归属不对」的卡，
	// 而一个守隔离的召回器不许把它捞回来。
	trap := 0
	for _, card := range set.Facts() {
		for _, c := range set.Dataset.Cases {
			if card.Scope.VisibleTo(c.Scope) {
				continue
			}
			if sharedTerms(card.SearchTerms, c.Query) == len(strings.Fields(c.Query)) {
				trap++
			}
		}
	}
	if trap == 0 {
		t.Error("语料里没有「词面完美但归属不对」的卡，归属隔离没被覆盖")
	}
	for i, pc := range rep.PerCase {
		c := set.Dataset.Cases[i]
		for _, id := range pc.Retrieved {
			card := findCard(set.Facts(), id)
			if card == nil {
				t.Fatalf("召回结果里有语料中不存在的卡: %s", id)
			}
			if !card.Scope.VisibleTo(c.Scope) {
				t.Errorf("用例 %s 召回了读者不可见的卡 %s（归属隔离失效）", c.ID, id)
			}
		}
	}
}

// fixedRetriever 按用例标识返回固定结果（测试夹具）。
type fixedRetriever struct {
	results map[string][]string
	err     error
}

func (f fixedRetriever) Recall(_ context.Context, c Case) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.results[c.ID], nil
}

// fakeRetriever 词面假召回器：按与查询共享的词数排序，取前 limit 条。
//
// 它只做词面匹配与归属过滤：评测面要评的是「结果像不像样」，
// 不是某条召回链的实现细节，故这个替身刻意保持简单。
type fakeRetriever struct {
	facts []*types.AtomCard
	limit int
}

func (f fakeRetriever) Recall(_ context.Context, c Case) ([]string, error) {
	type hit struct {
		id    string
		score int
	}
	var hits []hit
	for _, card := range f.facts {
		if !card.Scope.VisibleTo(c.Scope) {
			continue
		}
		if card.SoftRetired() {
			continue
		}
		score := sharedTerms(card.SearchTerms, c.Query)
		if score == 0 {
			continue
		}
		hits = append(hits, hit{id: card.ID, score: score})
	}
	// 同分按 ID 排：同分时的次序不能靠遍历顺序决定，
	// 否则同一份语料两次召回可能给出不同顺序，报告就不再可复现。
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].id < hits[j].id
	})
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.id)
		if f.limit > 0 && len(ids) >= f.limit {
			break
		}
	}
	return ids, nil
}

// sharedTerms 卡与查询共享的词数。
func sharedTerms(tokens []string, query string) int {
	n := 0
	for _, t := range strings.Fields(query) {
		for _, x := range tokens {
			if x == t {
				n++
				break
			}
		}
	}
	return n
}

// findCard 按 ID 找卡。
func findCard(cards []*types.AtomCard, id string) *types.AtomCard {
	for _, c := range cards {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// near 浮点比较（评测指标是比值，末位误差不该让用例红）。
func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-12
}
