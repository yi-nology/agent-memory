package promotion_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/promotion"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("t")

func now() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

// fakeStore 记下每次调用，用于断言「五道检查各自碰了哪些表」。
type fakeStore struct {
	raws     map[string]*types.RawEvent
	atoms    []*types.AtomCard
	ents     map[string]*types.Entity
	savedA   []*types.AtomCard
	merged   []*types.AtomCard
	status   map[string]types.CandidateStatus
	journal  []*types.JournalEntry
	counts   map[string]int
	failGet  bool
	failSave bool
}

func newFake() *fakeStore {
	return &fakeStore{
		raws:   map[string]*types.RawEvent{},
		ents:   map[string]*types.Entity{},
		status: map[string]types.CandidateStatus{},
		counts: map[string]int{},
	}
}

func (f *fakeStore) GetRaw(_ context.Context, _ store.Namespace, id string) (*types.RawEvent, error) {
	if f.failGet {
		return nil, errors.New("存储抖动")
	}
	e, ok := f.raws[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return e, nil
}

func (f *fakeStore) FindAtomBySignature(_ context.Context, _ store.Namespace, sig string) (*types.AtomCard, error) {
	for _, a := range f.atoms {
		if a.Signature() == sig {
			return a, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListAtoms(_ context.Context, _ store.Namespace, flt store.AtomFilter) ([]*types.AtomCard, error) {
	var out []*types.AtomCard
	for _, a := range f.atoms {
		if flt.ActiveOnly && !a.Active() {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeStore) FindEntityByName(_ context.Context, _ store.Namespace, name string) (*types.Entity, error) {
	for _, e := range f.ents {
		if e.CanonicalName == name {
			return e, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) SaveEntity(_ context.Context, _ store.Namespace, e *types.Entity) error {
	f.ents[e.ID] = e
	return nil
}

// ListEntities 供升级通道消歧用。
func (f *fakeStore) ListEntities(_ context.Context, _ store.Namespace, _ types.EntityType, limit int) ([]*types.Entity, error) {
	out := make([]*types.Entity, 0, len(f.ents))
	for _, e := range f.ents {
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Transaction 窄事务：把**自己**交给回调。
//
// 真实后端给的是「事务内的那一份」存储；测试里给同一份即可——
// 「回调拿到的存储在不在事务里」是存储层的责任，
// 晋升层只负责**用回调给的那份**而不是外层那份。
func (f *fakeStore) Transaction(ctx context.Context, fn func(context.Context, promotion.Store) error) error {
	return fn(ctx, f)
}

func (f *fakeStore) SaveAlias(context.Context, store.Namespace, *types.Alias) error { return nil }

func (f *fakeStore) SaveAtom(_ context.Context, _ store.Namespace, a *types.AtomCard) error {
	if f.failSave {
		return errors.New("写事实卡失败：磁盘满")
	}
	f.savedA = append(f.savedA, a)
	f.atoms = append(f.atoms, a)
	return nil
}

func (f *fakeStore) BumpEntityAtomCount(_ context.Context, _ store.Namespace, entityID string, delta int) error {
	f.counts[entityID] += delta
	return nil
}

func (f *fakeStore) UpdateCandidateStatus(_ context.Context, _ store.Namespace, id string,
	_, next types.CandidateStatus, _ types.DecidedBy, _ string) error {
	f.status[id] = next
	return nil
}

func (f *fakeStore) AppendJournal(_ context.Context, _ store.Namespace, e *types.JournalEntry) error {
	f.journal = append(f.journal, e)
	return nil
}

func ctx2() context.Context { return context.Background() }

func cand(assertion, eventID, subject string) *types.Candidate {
	return &types.Candidate{
		ID: "c1", RawEventIDs: []string{eventID}, Type: types.CandidateFact,
		Status: types.CandidatePending, Assertion: assertion,
		QuoteEventID: eventID, SubjectName: subject,
		Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		CreatedAt: now(),
	}
}

func newPromoter(t *testing.T, f *fakeStore, esc promotion.Escalation) *promotion.Promoter {
	t.Helper()
	p, err := promotion.New(f, esc, promotion.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ── 检查 1 价值 ──

func TestValueCheckDropsTrivial(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "是的"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, cand("好的", "e1", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionDrop || v.Check != "value" {
		t.Fatalf("过短的断言应丢弃: %+v", v)
	}
}

// 超长断言多半是整段被误当成断言。
func TestValueCheckDropsOverlong(t *testing.T) {
	f := newFake()
	long := make([]rune, 600)
	for i := range long {
		long[i] = '字'
	}
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "某段很长的会话内容"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	p := newPromoter(t, f, nil)

	v, _ := p.Promote(ctx2(), ns, cand(string(long), "e1", "机器甲"))
	if v.Decision != promotion.DecisionDrop || v.Check != "value" {
		t.Fatalf("超长断言应丢弃: %+v", v)
	}
}

// ── 检查 2 证据 ──

// 出处事件已被保留期清掉 → 丢弃。
// 不降级成「无出处但先记着」：没有出处的事实既不能审计也不能推翻。
func TestEvidenceCheckDropsOrphan(t *testing.T) {
	f := newFake() // raws 为空 → 查不到出处
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, cand("磁盘长期偏小", "已被清理", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionDrop || v.Check != "evidence" {
		t.Fatalf("出处不存在的候选应丢弃: %+v", v)
	}
}

// 存储故障**不是**「判不了」：不能把一次抖动记成「需人工判断」，
// 那会让待办队列被噪音淹没。
func TestStorageFailurePropagates(t *testing.T) {
	f := newFake()
	f.failGet = true
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	p := newPromoter(t, f, nil)

	if _, err := p.Promote(ctx2(), ns, cand("磁盘长期偏小", "e1", "机器甲")); err == nil {
		t.Fatal("存储故障必须上抛：把它记成「需人工」会让待办队列被噪音淹没")
	}
}

// ── 检查 3 实体 ──

// 归一判不出时送人工，**不新建实体**——归一错了会把两条不相干的事实
// 挂到同一张卡上，那种错误在召回时表现为「总是答非所问」。
func TestUnresolvableEntityGoesToReview(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘长期偏小"}
	p := newPromoter(t, f, nil) // 无升级通道

	v, err := p.Promote(ctx2(), ns, cand("磁盘长期偏小", "e1", "从未见过的主体"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionReview || v.Check != "entity" {
		t.Fatalf("归一判不出应送人工: %+v", v)
	}
}

func TestNoSubjectNameGoesToReview(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘长期偏小"}
	p := newPromoter(t, f, nil)

	v, _ := p.Promote(ctx2(), ns, cand("磁盘长期偏小", "e1", ""))
	if v.Decision != promotion.DecisionReview {
		t.Fatalf("无主体名应送人工: %+v", v)
	}
}

type fixedEscalation struct{ id string }

func (e fixedEscalation) ResolveEntity(context.Context, store.Namespace,
	*types.Candidate, []types.Entity) (string, error) {
	return e.id, nil
}

func TestEscalationResolvesEntity(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘长期偏小"}
	p := newPromoter(t, f, fixedEscalation{"e-new"})

	v, err := p.Promote(ctx2(), ns, cand("磁盘长期偏小", "e1", "新对象"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionPromote || v.EntityID != "e-new" {
		t.Fatalf("升级通道判得出就该晋升: %+v", v)
	}
}

// ── 检查 4 重复 ──

// 同签名 → 并入，不新增。并入时补证据不补断言。
func TestDuplicateMergesEvidence(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘长期偏小"}
	f.raws["e2"] = &types.RawEvent{ID: "e2", Content: "磁盘还是偏小"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	f.atoms = []*types.AtomCard{{
		ID: "a1", EntityID: "e1", Assertion: "磁盘长期偏小", QuoteEventID: "e1",
		RawEventIDs: []string{"e1"}, Importance: types.ImportanceMed,
		Confidence: types.ConfidenceMed, OccurredAt: now(), CreatedAt: now(),
	}}
	p := newPromoter(t, f, nil)

	// 首尾空白折叠（内部空白折叠不合并「有空格」与「没空格」，那不是同一条断言）
	c := cand("  磁盘长期偏小  ", "e2", "机器甲")
	v, err := p.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionMerge {
		t.Fatalf("同签名应并入: %+v", v)
	}
	if f.status["c1"] != types.CandidateMerged {
		t.Fatalf("并入后候选状态必须落定，实得 %q", f.status["c1"])
	}
	if len(f.atoms[0].RawEventIDs) != 2 {
		t.Fatalf("并入应补证据：%+v", f.atoms[0].RawEventIDs)
	}
}

// ── 检查 5 矛盾 ──

// 「已修复」与「未修复」是矛盾，**不自动裁决**——自动判「哪个对」就是在猜。
func TestConflictGoesToReview(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "内存泄漏已修复"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	f.atoms = []*types.AtomCard{{
		ID: "a1", EntityID: "e1", Assertion: "内存泄漏未修复", QuoteEventID: "e1",
		Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
		OccurredAt: now(), CreatedAt: now(),
	}}
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, cand("内存泄漏已修复", "e1", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionReview || v.Check != "conflict" {
		t.Fatalf("矛盾必须送人工而不是自动裁决: %+v", v)
	}
	if len(f.savedA) != 0 {
		t.Fatal("送人工时不得写入事实")
	}
}

// 相似但否定方向相同 → 不是矛盾，正常晋升。
func TestSameNegationIsNotConflict(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "内存泄漏仍未修复，且持续扩大"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	f.atoms = []*types.AtomCard{{
		ID: "a1", EntityID: "e1", Assertion: "内存泄漏仍未修复", QuoteEventID: "e1",
		Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
		OccurredAt: now(), CreatedAt: now(),
	}}
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, cand("内存泄漏仍未修复，且持续扩大", "e1", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionPromote {
		t.Fatalf("否定方向相同不构成矛盾: %+v", v)
	}
}

// 相似的另一件事不构成矛盾：三条判据要同时成立才算。
func TestUnrelatedSimilarIsNotConflict(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘空间不足需要清理"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	f.atoms = []*types.AtomCard{{
		ID: "a1", EntityID: "e1", Assertion: "内存占用偏高需要清理", QuoteEventID: "e1",
		Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
		OccurredAt: now(), CreatedAt: now(),
	}}
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, cand("磁盘空间不足需要清理", "e1", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionPromote {
		t.Fatalf("不相干的两件事不该判成矛盾: %+v", v)
	}
}

// ── 构造闸 ──

func TestNewRejectsNilStore(t *testing.T) {
	if _, err := promotion.New(nil, nil, promotion.Config{}); err == nil {
		t.Fatal("未装配存储必须让构造失败")
	}
}

// ── 归属维度 ──

// 归属要**从候选继承到事实卡**：归属在捕获时定下，一路带过来。
//
// 断在中间那一站的后果是「这一组的事实变成了全局的」——
// 那是**泄漏**，而症状只是「别的组也看得见这条事实」。
func TestAtomInheritsCandidateScope(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "机器甲的磁盘长期偏小"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	p := newPromoter(t, f, nil)

	c := cand("机器甲的磁盘长期偏小需要清理", "e1", "机器甲")
	c.Scope = "scope-a"
	v, err := p.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Atom == nil {
		t.Fatalf("该晋升为事实: %+v", v)
	}
	if v.Atom.Scope != "scope-a" {
		t.Fatalf("事实卡没继承候选的归属: %q", v.Atom.Scope)
	}
}

// 跨归属**不许并入**：签名含归属，两组的同一条断言是两张不同的卡。
//
// 不含归属的话，一组的候选会被并进另一组的卡片——那是内容泄漏，不是去重。
func TestNoMergeAcrossScopes(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "机器甲的磁盘长期偏小"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	// 既有事实卡属于另一组。
	f.atoms = append(f.atoms, &types.AtomCard{
		ID: "atom-old", EntityID: "e1", Scope: "scope-b",
		Assertion: "机器甲的磁盘长期偏小需要清理",
	})
	p := newPromoter(t, f, nil)

	c := cand("机器甲的磁盘长期偏小需要清理", "e1", "机器甲")
	c.Scope = "scope-a"
	v, err := p.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision == promotion.DecisionMerge {
		t.Fatalf("跨归属不许并入（那是泄漏）: %+v", v)
	}
	if v.Atom != nil && v.Atom.Scope != "scope-a" {
		t.Fatalf("新卡的归属应是本组: %q", v.Atom.Scope)
	}
}
