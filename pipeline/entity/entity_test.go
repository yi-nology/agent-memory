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
