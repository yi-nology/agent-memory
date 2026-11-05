package entity_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/entity"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("ent")

func now() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

// fakeStore 记写操作。
type fakeStore struct {
	ents    map[string]*types.Entity
	aliases map[string]*types.Alias
	atoms   []*types.AtomCard
}

func newFake() *fakeStore {
	return &fakeStore{ents: map[string]*types.Entity{}, aliases: map[string]*types.Alias{}}
}

func (f *fakeStore) FindEntityByName(_ context.Context, _ store.Namespace, name string) (*types.Entity, error) {
	for _, e := range f.ents {
		if e.CanonicalName == name {
			return e, nil
		}
	}
	for _, a := range f.aliases {
		if a.Alias == name {
			return f.ents[a.EntityID], nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) GetEntity(_ context.Context, _ store.Namespace, id string) (*types.Entity, error) {
	if e, ok := f.ents[id]; ok {
		return e, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListEntities(context.Context, store.Namespace, types.EntityType, int) ([]*types.Entity, error) {
	var out []*types.Entity
	for _, e := range f.ents {
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeStore) SaveEntity(_ context.Context, _ store.Namespace, e *types.Entity) error {
	f.ents[e.ID] = e
	return nil
}

func (f *fakeStore) SaveAlias(_ context.Context, _ store.Namespace, a *types.Alias) error {
	f.aliases[a.Alias] = a
	return nil
}

func (f *fakeStore) ListAliases(_ context.Context, _ store.Namespace, entityID string) ([]*types.Alias, error) {
	var out []*types.Alias
	for _, a := range f.aliases {
		if a.EntityID == entityID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) BumpEntityAtomCount(_ context.Context, _ store.Namespace, id string, d int) error {
	if e, ok := f.ents[id]; ok {
		e.AtomCount += d
	}
	return nil
}

func (f *fakeStore) ListAtomsByEntity(_ context.Context, _ store.Namespace, entityID string) ([]*types.AtomCard, error) {
	var out []*types.AtomCard
	for _, a := range f.atoms {
		if a.EntityID == entityID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) SaveAtom(_ context.Context, _ store.Namespace, a *types.AtomCard) error {
	f.atoms = append(f.atoms, a)
	return nil
}

func ctx2() context.Context { return context.Background() }

// 登记实体与别名，重复登记幂等。
func TestRegisterIsIdempotent(t *testing.T) {
	f := newFake()
	r, _ := entity.New(f)
	e1, err := r.Register(ctx2(), ns, "机器甲", "机器", "那台开发机", types.DecidedUser)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := r.Register(ctx2(), ns, "机器甲", "机器", "那台开发机", types.DecidedUser)
	if err != nil {
		t.Fatalf("重复登记应幂等: %v", err)
	}
	if e1.ID != e2.ID {
		t.Fatalf("重复登记不该产生第二个实体: %s vs %s", e1.ID, e2.ID)
	}
	// 别名可查
	got, err := r.Resolve(ctx2(), ns, "那台开发机")
	if err != nil {
		t.Fatalf("按别名查不到: %v", err)
	}
	if got.ID != e1.ID {
		t.Fatalf("别名指向了别的实体: %s", got.ID)
	}
}

// 别名撞上别的实体 → 冲突，不默默覆盖。
func TestAliasConflictIsReported(t *testing.T) {
	f := newFake()
	r, _ := entity.New(f)
	_, _ = r.Register(ctx2(), ns, "机器甲", "机器", "", types.DecidedUser)
	_, _ = r.Register(ctx2(), ns, "机器乙", "机器", "", types.DecidedUser)

	// 把「机器甲」登记成机器乙的别名
	_, err := r.Register(ctx2(), ns, "机器乙", "机器", "机器甲", types.DecidedUser)
	if err == nil {
		t.Fatal("别名撞上别的实体必须报冲突")
	}
	var ce *entity.ConflictError
	if !asConflict(err, &ce) {
		t.Fatalf("应为 ConflictError，实得 %T: %v", err, err)
	}
}

// Resolve 查不到时不新建。
func TestResolveDoesNotCreate(t *testing.T) {
	f := newFake()
	r, _ := entity.New(f)
	if _, err := r.Resolve(ctx2(), ns, "从未见过"); err == nil {
		t.Fatal("查不到应报 not found")
	}
	if len(f.ents) != 0 {
		t.Fatal("Resolve 不得新建实体：归一错了会把一条事实劈成两半")
	}
}

// 合并：事实迁归属、别名迁指向、计数相加、被并方退役。
func TestMergeMovesFactsAndAliases(t *testing.T) {
	f := newFake()
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲", AtomCount: 1, CreatedAt: now()}
	f.ents["e2"] = &types.Entity{ID: "e2", CanonicalName: "机器乙", AtomCount: 1, CreatedAt: now()}
	f.aliases["别名"] = &types.Alias{Alias: "别名", EntityID: "e2", CreatedAt: now()}
	f.atoms = []*types.AtomCard{{ID: "a1", EntityID: "e2", Assertion: "某条事实"}}

	r, _ := entity.New(f)
	res, err := r.Merge(ctx2(), ns, "e1", "e2")
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedAtoms != 1 {
		t.Fatalf("应迁移 1 条事实，实得 %d", res.MovedAtoms)
	}
	if res.MovedAliases != 1 {
		t.Fatalf("应迁移 1 条别名，实得 %d", res.MovedAliases)
	}
	if f.atoms[0].EntityID != "e1" {
		t.Fatalf("事实没迁到目标实体: %s", f.atoms[0].EntityID)
	}
	if f.aliases["别名"].EntityID != "e1" {
		t.Fatalf("别名没迁到目标实体: %s", f.aliases["别名"].EntityID)
	}
	if f.ents["e2"].AtomCount != 0 {
		t.Fatalf("被并方应清零计数，实得 %d", f.ents["e2"].AtomCount)
	}
}

func asConflict(err error, out **entity.ConflictError) bool {
	ce, ok := err.(*entity.ConflictError)
	if ok {
		*out = ce
	}
	return ok
}

// flakyStore 在第 N 次 SaveAtom 前注入一次失败——模拟 Merge 中途失败。
type flakyStore struct {
	fakeStore
	failBefore int
	calls      int
}

func (f *flakyStore) SaveAtom(ctx context.Context, ns store.Namespace, a *types.AtomCard) error {
	f.calls++
	if f.calls == f.failBefore {
		return context.DeadlineExceeded // 中途失败形状
	}
	return f.fakeStore.SaveAtom(ctx, ns, a)
}

// TestMergeIdempotentConvergence 中途失败后重跑同一条 Merge 必须收敛：
// 已迁的卡不在 from 侧列表里（不会二次搬）；from 的脏计数被退役清 0 抹平。
// 这是 Merge doc「非原子但可安全重跑」承诺的钉子——宿主的中途失败处置
// 就是「重跑」，这条不成立 doc 就是假的。
func TestMergeIdempotentConvergence(t *testing.T) {
	// 第一轮：3 张卡，第 2 张迁移时失败。
	f := &flakyStore{fakeStore: *newFake(), failBefore: 2}
	r, err := entity.New(f)
	if err != nil {
		t.Fatal(err)
	}
	to, err := r.Register(ctx2(), ns, "机器甲", "机器", "", types.DecidedUser)
	if err != nil {
		t.Fatal(err)
	}
	from, err := r.Register(ctx2(), ns, "机器乙", "机器", "", types.DecidedUser)
	if err != nil {
		t.Fatal(err)
	}
	_ = to
	var cards []*types.AtomCard
	for _, id := range []string{"a1", "a2", "a3"} {
		cards = append(cards, &types.AtomCard{
			ID: id, EntityID: from.ID, CandidateID: "c" + id, RawEventIDs: []string{"ev" + id},
			Assertion: "断言" + id, VerbatimQuote: "「断言" + id + "」",
			QuoteEventID: "ev" + id, Importance: types.ImportanceLow, Confidence: types.ConfidenceHigh,
			CreatedAt: now(), UpdatedAt: now(),
		})
	}
	f.atoms = cards

	_, err = r.Merge(ctx2(), ns, to.ID, from.ID)
	if err == nil {
		t.Fatal("注入的失败应让第一轮 Merge 失败")
	}
	// 模拟真实「落库失败」语义：失败的那张（a2）在库里仍是旧值。
	// （fake 与卡共享指针，Merge 先改入参再落库，失败时对象已被原地改——
	// 真存储是值语义，库里不会跟着改；这里手工恢复旧值对齐真实形态。）
	for _, a := range f.atoms {
		if a.ID == "a2" {
			a.EntityID = from.ID
		}
	}
	// 此时库是部分迁移态：a1 已迁、a2/a3 还在 from。清掉注入。
	f.failBefore = -1
	f.calls = 0

	res, err := r.Merge(ctx2(), ns, to.ID, from.ID)
	if err != nil {
		t.Fatalf("重跑应收敛而不是二次失败: %v", err)
	}
	if res.MovedAtoms != 2 {
		t.Fatalf("重跑只应搬剩余的 2 张（不是 3 张——已迁的不重复搬），实得 %d", res.MovedAtoms)
	}
	// 全部卡最终都归 to。
	for _, id := range []string{"a1", "a2", "a3"} {
		var got *types.AtomCard
		for _, a := range f.atoms {
			if a.ID == id {
				got = a
			}
		}
		if got == nil || got.EntityID != to.ID {
			t.Fatalf("卡 %s 应已归 to", id)
		}
	}
	// from 退役：计数清 0（中途失败的 Bump 差量被终态抹平）。
	if f.ents[from.ID].AtomCount != 0 {
		t.Fatalf("from 退役后计数应为 0（脏差量被终态抹平），实得 %d", f.ents[from.ID].AtomCount)
	}
	// 第二次重跑（已收敛态）：幂等空操作。
	res2, err := r.Merge(ctx2(), ns, to.ID, from.ID)
	if err != nil {
		t.Fatalf("收敛后再跑应无错: %v", err)
	}
	if res2.MovedAtoms != 0 || res2.MovedAliases != 0 {
		t.Fatalf("收敛后重跑应是空操作，实得 atoms=%d aliases=%d", res2.MovedAtoms, res2.MovedAliases)
	}
}
