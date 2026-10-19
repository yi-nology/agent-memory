package promotion_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yi-nology/agent-memory/pipeline/promotion"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件钉住实体归一面的一条纪律：**「查不动」与「查不到」是两件事。**
//
// 存储故障（连接重置、超时）不是「判不了」，它是基础设施出问题了。
// 混在一起的后果比「送审队列被噪音淹没」更重：归一会「成功」但实体为空，
// 于是产出一条**谁都不属于的事实**——空 EntityID 一路流到签名、矛盾检测、
// 召回配额，最后作为一条无归属事实进 prompt，而数据上完全看不出异常。
//
// 这条纪律在同文件的 GetRaw 那处一直是对的（TestStorageFailurePropagates），
// 唯独实体归一那处写反了：注释说「往上抛」，代码却 return "", nil。
// 那处没有 error 返回位可抛——本文件的同时也钉住了「给它一个 error 返回位」
// 这个改法本身。

// errStore 是一份只在 FindEntityByName 上报存储故障的假存储。
//
// 内嵌 *fakeStore 复用其余方法的实现；本类型自己定义 FindEntityByName，
// 内嵌的同名方法被遮蔽（Go 的方法集规则：外层定义优先）。
type errStore struct {
	*fakeStore
}

func (e *errStore) FindEntityByName(context.Context, store.Namespace, string) (*types.Entity, error) {
	return nil, errors.New("存储抖动：数据库连接被重置")
}

// Transaction 必须交出**自己**，不能交出内嵌的 fakeStore。
//
// 这一条是窄事务契约的核心：回调拿到的存储要是「本次事务的那一份」。
// 内嵌的 fakeStore.Transaction 交出的是内层那个——
// 于是 errStore 的 FindEntityByName 覆盖被悄悄丢掉，
// 故障注入失效、断言看起来「不红」，而真实后端不会有这个坑。
func (e *errStore) Transaction(ctx context.Context, fn func(context.Context, promotion.Store) error) error {
	return fn(ctx, e)
}

// newPromoterOn 造一个晋升器，接受任意实现 promotion.Store 的存储。
func newPromoterOn(t *testing.T, s promotion.Store) *promotion.Promoter {
	t.Helper()
	p, err := promotion.New(s, nil, promotion.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// 存储故障必须以错误结束，不得伪装成判定结果。
func TestEntityLookupStoreFailurePropagates(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘剩余空间为 2G"}
	p := newPromoterOn(t, &errStore{fakeStore: f})

	v, err := p.Promote(context.Background(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲"))
	if err == nil {
		t.Fatalf("存储故障必须上抛（注释写的是「往上抛」），实得判决 %+v", v)
	}
	if v != nil && v.Decision == promotion.DecisionPromote && v.EntityID == "" {
		t.Fatalf("存储故障下不得产出无归属的事实卡: %+v", v)
	}
}

// 反向断言：真正「查不到」（ErrNotFound）时走的是送人工路径，不是错误路径。
// 这条钉住修复的边界——别把「查不到」也改成上抛。
func TestEntityNotFoundGoesToReview(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘剩余空间为 2G"}
	p := newPromoter(t, f, nil)

	v, err := p.Promote(context.Background(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲"))
	if err != nil {
		t.Fatalf("查不到实体不是存储故障，不该上抛: %v", err)
	}
	if v == nil || v.Decision != promotion.DecisionReview {
		t.Fatalf("查不到实体且无升级通道时应送人工，实得 %+v", v)
	}
}

// ── 升级通道的契约 ──

// errEscalation 是一份总是失败的升级通道。
type errEscalation struct{}

func (errEscalation) ResolveEntity(context.Context, store.Namespace,
	*types.Candidate, []types.Entity) (string, error) {
	return "", errors.New("升级通道超时")
}

// 升级通道的 error 是**通道故障**，不是「判不了」。
//
// 把它记成「上下文不足」会让送审理由指向一个错误的原因，
// 而送审队列的积压恰恰会掩盖真正的故障。
func TestEscalationFailureIsNotReview(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘剩余空间为 2G"}
	p := newPromoter(t, f, errEscalation{})

	v, err := p.Promote(context.Background(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲"))
	if err == nil {
		t.Fatalf("升级通道故障必须上抛，实得判决 %+v", v)
	}
	if v != nil && v.Decision == promotion.DecisionReview {
		t.Fatalf("通道故障不得记成「需人工判断」: %+v", v)
	}
}

// 空串才是「判不了」——接口约定如此，故走送人工。
func TestEscalationEmptyIDGoesToReview(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘剩余空间为 2G"}
	p := newPromoter(t, f, fixedEscalation{}) // fixedEscalation 零值返回空串

	v, err := p.Promote(context.Background(), ns, cand("磁盘剩余空间为 2G", "e1", "机器甲"))
	if err != nil {
		t.Fatalf("升级通道返回空串是「判不了」，不该上抛: %v", err)
	}
	if v == nil || v.Decision != promotion.DecisionReview {
		t.Fatalf("升级通道判不出时应送人工，实得 %+v", v)
	}
}

// spyEscalation 记下升级通道看到了什么。
type spyEscalation struct {
	seen []types.Entity
	id   string
}

func (s *spyEscalation) ResolveEntity(_ context.Context, _ store.Namespace,
	_ *types.Candidate, existing []types.Entity) (string, error) {
	s.seen = existing
	return s.id, nil
}

// 升级通道要判「它指谁」就得看见既有实体——传空列表等于让它盲判。
//
// 这条曾经被违反：升级通道的签名带着 existing 参数，调用方却一直传 nil。
func TestEscalationSeesExistingEntities(t *testing.T) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "磁盘剩余空间为 2G"}
	// 库里已有一个别的实体：升级通道正是靠它来消歧。
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	esc := &spyEscalation{id: "e1"}
	p := newPromoter(t, f, esc)

	// 主体名**不能**命中既有实体——命中就不走升级通道了。
	if _, err := p.Promote(context.Background(), ns, cand("磁盘剩余空间为 2G", "e1", "从没登记过的主体")); err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) == 0 {
		t.Fatal("升级通道必须能看到既有实体才能消歧：传空列表等于让它盲判")
	}
}
