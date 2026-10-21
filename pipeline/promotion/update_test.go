package promotion_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yi-nology/agent-memory/pipeline/promotion"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：事实更新判定（取代）的用例。
//
// # 三条要钉住的
//
//  1. **同一主体、值变了 → 取代**（旧事实到期、版本号 +1）。
//  2. **矛盾仍然送人工**——取代这一支不能吞掉矛盾，
//     否则「已修复/未修复」会被自动裁决（那是被明令禁止的）。
//  3. **模型只做路由**：「作废」与「矛盾」永远落人工。

// fakeLLM 按用途返回固定的 JSON。
type fakeLLM struct {
	out string
	err error
	// prompts 记下收到过的提示词。
	prompts []string
}

func (f *fakeLLM) Complete(_ context.Context, req ports.LLMRequest) (string, error) {
	f.prompts = append(f.prompts, req.Prompt)
	if f.err != nil {
		return "", f.err
	}
	return f.out, nil
}

// 一个「同一件事、值变了」的夹具：旧事实说 4G，新候选说 8G。
func updateFixture() (*fakeStore, *types.Candidate) {
	f := newFake()
	f.raws["e1"] = &types.RawEvent{ID: "e1", Content: "机器甲的内存容量是 8G"}
	f.ents["e1"] = &types.Entity{ID: "e1", CanonicalName: "机器甲"}
	f.atoms = []*types.AtomCard{{
		ID: "a1", EntityID: "e1", Assertion: "机器甲的内存容量是 4G", QuoteEventID: "e0",
		Importance: types.ImportanceMed, Confidence: types.ConfidenceMed,
		OccurredAt: now(), CreatedAt: now(), Version: 1,
	}}
	return f, cand("机器甲的内存容量是 8G", "e1", "机器甲")
}

// 规则判出「差异落在值上」→ 直接取代。
func TestUpdateSupersedesOldFact(t *testing.T) {
	f, c := updateFixture()
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionUpdate {
		t.Fatalf("同一主体且值变了应取代: %+v", v)
	}
	if v.Superseded == nil || v.Superseded.ID != "a1" {
		t.Fatalf("判决里应带上被取代的那条: %+v", v)
	}
	// 旧事实到期（不是删除）。
	if v.Superseded.ValidTo.IsZero() {
		t.Fatal("旧事实的有效期应被划上终点：它「到这一刻为止有效」")
	}
	if v.Superseded.SupersededBy != v.Atom.ID {
		t.Fatalf("旧事实应记着被谁取代: %+v", v.Superseded)
	}
	// 新事实版本号 +1。
	if v.Atom.Version != 2 {
		t.Fatalf("新事实的版本号应是 2: %d", v.Atom.Version)
	}
	// 审计留痕。
	found := false
	for _, j := range f.journal {
		if j.Kind == types.JournalSupersede {
			found = true
		}
	}
	if !found {
		t.Fatalf("取代必须留痕: %+v", f.journal)
	}
}

// 矛盾**仍然**送人工：取代这一支不许吞掉矛盾。
//
// 这是最要紧的一条：「已修复」与「未修复」不是「值变了」，
// 自动裁决它就是在猜（本仓明令禁止）。
func TestConflictStillGoesToReview(t *testing.T) {
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
	if v.Decision != promotion.DecisionReview {
		t.Fatalf("矛盾必须送人工: %+v", v)
	}
	if len(f.savedA) != 0 {
		t.Fatal("送人工时不得写入事实")
	}
}

// 模型说「同一个东西换了值」→ 取代（哪怕规则看不出来）。
func TestModelDrivenUpdate(t *testing.T) {
	f, c := updateFixture()
	// 把旧事实改成规则判不出的形态（词面差得多、但确实是同一件事）。
	f.atoms[0].Assertion = "机器甲使用版本 1.32"
	c.Assertion = "机器甲已升级到版本 1.36"
	llm := &fakeLLM{out: `{"relation":"update","reason":"版本变了"}`}
	pr, err := promotion.New(f, nil, promotion.Config{LLM: llm})
	if err != nil {
		t.Fatal(err)
	}

	v, err := pr.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionUpdate {
		t.Fatalf("模型判为 update 时应取代: %+v", v)
	}
	if len(llm.prompts) == 0 {
		t.Fatal("应把两条断言问给模型")
	}
}

// 模型说「矛盾」→ 送人工（模型不能裁决矛盾）。
func TestModelConflictGoesToReview(t *testing.T) {
	f, c := updateFixture()
	f.atoms[0].Assertion = "机器甲使用版本 1.32"
	c.Assertion = "机器甲已升级到版本 1.36"
	llm := &fakeLLM{out: `{"relation":"conflict","reason":"说不清"}`}
	pr, err := promotion.New(f, nil, promotion.Config{LLM: llm})
	if err != nil {
		t.Fatal(err)
	}

	v, err := pr.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionReview {
		t.Fatalf("模型判为矛盾时必须送人工: %+v", v)
	}
	if len(f.savedA) != 0 {
		t.Fatal("送人工时不得写入事实")
	}
}

// 模型说「作废」→ **仍然**送人工（绝不由模型一句话作废一条已存的事实）。
func TestModelDeleteGoesToReview(t *testing.T) {
	f, c := updateFixture()
	f.atoms[0].Assertion = "机器甲使用版本 1.32"
	c.Assertion = "机器甲已升级到版本 1.36"
	llm := &fakeLLM{out: `{"relation":"delete","reason":"不该存在了"}`}
	pr, err := promotion.New(f, nil, promotion.Config{LLM: llm})
	if err != nil {
		t.Fatal(err)
	}

	v, err := pr.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionReview {
		t.Fatalf("模型判为 delete 时必须送人工: %+v", v)
	}
}

// 模型失败 → **上抛**（不静默退化成「不取代」）。
func TestModelFailurePropagates(t *testing.T) {
	f, c := updateFixture()
	f.atoms[0].Assertion = "机器甲使用版本 1.32"
	c.Assertion = "机器甲已升级到版本 1.36"
	llm := &fakeLLM{err: errors.New("模型超时")}
	pr, err := promotion.New(f, nil, promotion.Config{LLM: llm})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pr.Promote(ctx2(), ns, c); err == nil {
		t.Fatal("模型失败必须上抛：静默退化的症状是「事实更新悄悄停了」")
	}
}

// 没装模型 + 判不清 → 按普通晋升处理（**不**取代、**不**送人工）。
//
// 这条钉的是降级路径：没配模型的宿主不该突然多出一堆待人处置的候选。
func TestNoModelKeepsPromotePath(t *testing.T) {
	f, c := updateFixture()
	f.atoms[0].Assertion = "机器甲使用版本 1.32"
	c.Assertion = "机器甲已升级到版本 1.36"
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionPromote {
		t.Fatalf("没装模型时判不清应按普通晋升处理: %+v", v)
	}
	if v.Superseded != nil {
		t.Fatal("没装模型时不得取代任何事实")
	}
}

// 归属不同的既有事实**不可比**：那不是「同一个东西换了值」。
func TestUpdateIgnoresOtherScope(t *testing.T) {
	f, c := updateFixture()
	f.atoms[0].Scope = "team-b"
	c.Scope = "team-a"
	p := newPromoter(t, f, nil)

	v, err := p.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionPromote {
		t.Fatalf("归属不同的不可比，不得取代: %+v", v)
	}
}

// 模型输出带围栏也能解析（被要求输出 JSON 时模型常连围栏一起给）。
func TestModelOutputWithFence(t *testing.T) {
	f, c := updateFixture()
	f.atoms[0].Assertion = "机器甲使用版本 1.32"
	c.Assertion = "机器甲已升级到版本 1.36"
	llm := &fakeLLM{out: "```json\n{\"relation\":\"update\",\"reason\":\"版本变了\"}\n```"}
	pr, err := promotion.New(f, nil, promotion.Config{LLM: llm})
	if err != nil {
		t.Fatal(err)
	}
	v, err := pr.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != promotion.DecisionUpdate {
		t.Fatalf("带围栏的合法输出不该判失败: %+v", v)
	}
}

// 时间相关：取代发生时旧事实的到期时刻不早于新事实的生效时刻。
func TestUpdateKeepsTimeOrder(t *testing.T) {
	f, c := updateFixture()
	p := newPromoter(t, f, nil)
	v, err := p.Promote(ctx2(), ns, c)
	if err != nil {
		t.Fatal(err)
	}
	if v.Superseded.ValidTo.Before(v.Atom.ValidFrom) {
		t.Fatalf("旧事实的到期不该早于新事实的生效: %v vs %v",
			v.Superseded.ValidTo, v.Atom.ValidFrom)
	}
}
