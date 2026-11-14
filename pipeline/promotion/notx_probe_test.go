package promotion_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/pipeline/promotion"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件钉住 issue #57 的两段拆分：判定（五道检查 + 模型 + 宿主升级通道）
// 在**事务外**，事务回调里只有四步写——事务的持有时长不得等于模型时长。

// probeStore 记下「当前是否在事务回调里」的探针存储。
type probeStore struct {
	inTx    bool
	txCount int
	// callsInTx 记事务回调内发生过的存储方法名（事务里只该有四步写）。
	callsInTx []string
	raws      map[string]*types.RawEvent
	ents      map[string]*types.Entity
	atoms     map[string]*types.AtomCard
	status    map[string]types.CandidateStatus
}

func (s *probeStore) Transaction(ctx context.Context, fn func(context.Context, promotion.Store) error) error {
	s.txCount++
	was := s.inTx
	s.inTx = true
	defer func() { s.inTx = was }()
	return fn(ctx, s)
}

func (s *probeStore) mark(what string) {
	if s.inTx {
		s.callsInTx = append(s.callsInTx, what)
	}
}

func (s *probeStore) GetRaw(_ context.Context, _ store.Namespace, id string) (*types.RawEvent, error) {
	s.mark("GetRaw")
	if ev, ok := s.raws[id]; ok {
		return ev, nil
	}
	return nil, store.ErrNotFound
}

func (s *probeStore) FindAtomBySignature(_ context.Context, _ store.Namespace, sig string) (*types.AtomCard, error) {
	s.mark("FindAtomBySignature")
	for _, a := range s.atoms {
		if a.Signature() == sig {
			return a, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *probeStore) ListAtoms(_ context.Context, _ store.Namespace, f store.AtomFilter) ([]*types.AtomCard, error) {
	s.mark("ListAtoms")
	var out []*types.AtomCard
	for _, a := range s.atoms {
		out = append(out, a)
	}
	return out, nil
}

func (s *probeStore) FindEntityByName(_ context.Context, _ store.Namespace, name string) (*types.Entity, error) {
	s.mark("FindEntityByName")
	if e, ok := s.ents[name]; ok {
		return e, nil
	}
	return nil, store.ErrNotFound
}

func (s *probeStore) ListEntities(context.Context, store.Namespace, types.EntityType, int) ([]*types.Entity, error) {
	s.mark("ListEntities")
	return nil, nil
}

func (s *probeStore) SaveAtom(_ context.Context, _ store.Namespace, a *types.AtomCard) error {
	s.mark("SaveAtom")
	s.atoms[a.ID] = a
	return nil
}

func (s *probeStore) BumpEntityAtomCount(_ context.Context, _ store.Namespace, _ string, _ int) error {
	s.mark("BumpEntityAtomCount")
	return nil
}

func (s *probeStore) UpdateCandidateStatus(_ context.Context, _ store.Namespace, id string,
	_, next types.CandidateStatus, _ types.DecidedBy, _ string) error {
	s.mark("UpdateCandidateStatus")
	s.status[id] = next
	return nil
}

func (s *probeStore) AppendJournal(context.Context, store.Namespace, *types.JournalEntry) error {
	s.mark("AppendJournal")
	return nil
}

// probeEscalation 记下宿主升级通道被调用时是否在事务里。
type probeEscalation struct {
	st       *probeStore
	sawInTx  bool
	called   bool
	entityID string
}

func (e *probeEscalation) ResolveEntity(context.Context, store.Namespace, *types.Candidate, []types.Entity) (string, error) {
	e.called = true
	e.sawInTx = e.st.inTx
	return e.entityID, nil
}

// probeLLM 记下模型被调用时是否在事务里。
type probeLLM struct {
	st      *probeStore
	sawInTx bool
	called  bool
	reply   string
}

func (l *probeLLM) Complete(context.Context, ports.LLMRequest) (string, error) {
	l.called = true
	l.sawInTx = l.st.inTx
	return l.reply, nil
}

func probeCandidate(assertion, subject string) *types.Candidate {
	return &types.Candidate{
		ID: "c-probe", RawEventIDs: []string{"e1"}, Type: types.CandidateFact,
		Status: types.CandidatePending, Assertion: assertion, SubjectName: subject,
		QuoteEventID: "e1", VerbatimQuote: assertion,
		Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		CreatedAt: now(), Scope: "",
	}
}

func probeFixture() (*probeStore, *probeEscalation, *promotion.Promoter, error) {
	st := &probeStore{raws: map[string]*types.RawEvent{}, ents: map[string]*types.Entity{},
		atoms: map[string]*types.AtomCard{}, status: map[string]types.CandidateStatus{}}
	st.raws["e1"] = &types.RawEvent{
		ID: "e1", Content: "磁盘剩余 2G", Timestamp: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC),
		Provenance: types.Provenance{Origin: types.OriginTool, Host: "h1", Tool: "df"},
	}
	esc := &probeEscalation{st: st, entityID: "e1"}
	p, err := promotion.New(st, esc, promotion.Config{})
	if err != nil {
		return nil, nil, nil, err
	}
	return st, esc, p, nil
}

// 实体归一走升级通道（宿主回调）时，回调必须发生在**事务外**：
// ResolveEntity 是宿主的逻辑（可能慢、可能超时），它持着事务的话
// 事务时长=宿主回调时长（issue #57）。
func TestResolveEntityRunsOutsideTransaction(t *testing.T) {
	st, esc, p, err := probeFixture()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Promote(context.Background(), ns, probeCandidate("磁盘剩余空间只有 2G 了", "机器乙")); err != nil {
		t.Fatal(err)
	}
	if !esc.called {
		t.Fatal("用例没走到升级通道（夹具失效）")
	}
	if esc.sawInTx {
		t.Fatal("宿主升级通道（ResolveEntity）在事务回调里执行了")
	}
	if st.txCount != 1 {
		t.Fatalf("落库应恰好开一次事务，实得 %d 次", st.txCount)
	}
	// 事务回调里只有四步写，不得再出现判定期的读。
	for _, c := range st.callsInTx {
		switch c {
		case "SaveAtom", "BumpEntityAtomCount", "UpdateCandidateStatus", "AppendJournal":
		default:
			t.Fatalf("事务回调里出现了写以外的调用: %s（全部: %v）", c, st.callsInTx)
		}
	}
}

// 「判不清要问模型」那一支：模型调用必须发生在**事务外**——
// 一次 LLM 调用超时可达数十秒，事务攥在手里等模型的话，
// PG 小池会被并发晋升占满、SQLite 的 WAL 会被读快照顶住。
func TestAskModelRunsOutsideTransaction(t *testing.T) {
	st, _, _, err := probeFixture()
	if err != nil {
		t.Fatal(err)
	}
	// 既有事实与候选相似度落在 [UpdateHintSimilarity, UpdateSimilarity)
	// 且差异不含数值 ⇒ 规则判不清 → 问模型。模型答 noop → 并入。
	st.ents["机器甲"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	old := &types.AtomCard{
		ID: "atom-old", EntityID: "e1", CandidateID: "c-old",
		RawEventIDs: []string{"e1"}, Assertion: "机器甲的维护窗口在周二凌晨",
		VerbatimQuote: "「机器甲的维护窗口在周二凌晨」", QuoteEventID: "e1",
		Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		CreatedAt: now(), UpdatedAt: now(),
		Provenance: types.Provenance{Origin: types.OriginTool},
	}
	st.atoms[old.ID] = old

	llm := &probeLLM{st: st, reply: `{"relation":"noop","reason":"同一件事"}`}
	p2, err := promotion.New(st, &probeEscalation{st: st, entityID: "e1"},
		promotion.Config{LLM: llm})
	if err != nil {
		t.Fatal(err)
	}
	v, err := p2.Promote(context.Background(), ns, probeCandidate("机器甲的例行维护窗口安排在周二夜里", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if !llm.called {
		t.Fatal("用例没走到模型判定（夹具失效）")
	}
	if llm.sawInTx {
		t.Fatal("模型调用（askModel）在事务回调里执行了")
	}
	if v.Decision != promotion.DecisionMerge {
		t.Fatalf("模型答 noop 应回落为并入，实得 %+v", v)
	}
	if st.txCount != 1 {
		t.Fatalf("落库应恰好开一次事务，实得 %d 次", st.txCount)
	}
}

// 并入路径的写前复核（issue #57）：判定读到的卡与落库隔了一次模型调用的
// 时长——期间卡被退役的话，并入没有对象，回落为普通晋升，
// 不把陈旧读视图写回去。
func TestMergeRecheckFallsBackToPromoteWhenRetired(t *testing.T) {
	st, _, _, err := probeFixture()
	if err != nil {
		t.Fatal(err)
	}
	st.ents["机器甲"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	old := &types.AtomCard{
		ID: "atom-old", EntityID: "e1", CandidateID: "c-old",
		RawEventIDs: []string{"e1"}, Assertion: "机器甲的维护窗口在周二凌晨",
		VerbatimQuote: "「机器甲的维护窗口在周二凌晨」", QuoteEventID: "e1",
		Confidence: types.ConfidenceMed, Importance: types.ImportanceMed,
		CreatedAt: now(), UpdatedAt: now(),
		Provenance: types.Provenance{Origin: types.OriginTool},
	}
	st.atoms[old.ID] = old

	// Transaction 期间把既有卡退役：模拟「判定之后、落库之前」的并发取代。
	inner := func(ctx context.Context, s promotion.Store) error {
		old.SupersededBy = "atom-other"
		old.UpdatedAt = now()
		return s.SaveAtom(ctx, ns, old)
	}
	// 先包一层：把退役动作塞进事务回调的最前面。
	p2, err := promotion.New(&preTxStore{probeStore: st, pre: inner}, &probeEscalation{st: st, entityID: "e1"},
		promotion.Config{LLM: &probeLLM{st: st, reply: `{"relation":"noop","reason":"同一件事"}`}})
	if err != nil {
		t.Fatal(err)
	}
	v, err := p2.Promote(context.Background(), ns, probeCandidate("机器甲的例行维护窗口安排在周二夜里", "机器甲"))
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionPromote {
		t.Fatalf("既有卡已在判定与落库之间退役 ⇒ 应回落为普通晋升，实得 %+v", v)
	}
}

// preTxStore 在事务回调开头先跑一段注入动作（模拟并发写）。
type preTxStore struct {
	*probeStore
	pre func(context.Context, promotion.Store) error
}

func (s *preTxStore) Transaction(ctx context.Context, fn func(context.Context, promotion.Store) error) error {
	return s.probeStore.Transaction(ctx, func(ctx context.Context, inner promotion.Store) error {
		if err := s.pre(ctx, inner); err != nil {
			return err
		}
		return fn(ctx, inner)
	})
}
