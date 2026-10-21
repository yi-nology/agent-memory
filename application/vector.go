package application

import (
	"context"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：向量轨的装配与一致性。
//
// # 谁写向量、谁删向量
//
//   - **写**：晋升成事实之后（事实卡的唯一产生口）。向量是事实的**派生物**——
//     它可以按事实重算，而事实卡丢了向量救不回来。
//   - **删**：事实退役时（软退役 / 人工作废）。
//
// # 为什么放装配面
//
// 生命周期面与晋升面都只声明自己需要的窄接口；
// 为了一次向量清理把索引塞进它们的依赖是本末倒置。
// 装配面本来就看得见全量存储，故这一步落在它这里
// （与「晋升后标脏画像」同一形状：派生动作由装配面做）。
//
// # 一致性为什么重要
//
// 事实退役了而它的向量还在索引里 —— 于是语义召回会先捞到一条**死事实**
// （再在「取回事实卡」那一步被过滤掉）。
// 症状是「语义路的命中率莫名偏低」，而那很难归因：
// 索引看起来是好的，召回看起来也在跑。
//
// 同理，晋升后的向量写失败也不阻断晋升：
// 事实卡是真源，让一条派生标记的失败把真源记忆挡在门外是本末倒置；
// 但必须**留痕**（漏掉一条向量的症状是「语义召回少了一条」，看不出来）。

// vectorStore 包一层存储：事实退役时同步删掉它的向量。
//
// 刻意只覆写两个方法（其余经内嵌的全量存储转发）——
// 这与 promoStore 同一形状：窄接口的适配落在装配面，
// 而全量接口在这里只是被转发，不被展开。
type vectorStore struct {
	store.Store
	idx ports.VectorIndex
}

// SupersedeAtom 软退役，并把它的向量删掉。
func (v vectorStore) SupersedeAtom(ctx context.Context, ns store.Namespace, id, supersededBy string) error {
	if err := v.Store.SupersedeAtom(ctx, ns, id, supersededBy); err != nil {
		return err
	}
	_ = v.idx.Delete(ctx, string(ns), id)
	return nil
}

// DeprecateAtom 作废，并把它的向量删掉。
func (v vectorStore) DeprecateAtom(ctx context.Context, ns store.Namespace, id string, at time.Time) error {
	if err := v.Store.DeprecateAtom(ctx, ns, id, at); err != nil {
		return err
	}
	_ = v.idx.Delete(ctx, string(ns), id)
	return nil
}

// indexVector 把事实卡的向量写进索引。
//
// # 向量化的是**断言正文**
//
// 不是整张卡：卡上的 ID、来源凭证那些字段与「说的是什么」无关，
// 混进去只会让「两条说同一件事但来源不同的卡」离得更近——
// 而那恰恰是去重该做的事，不是相似度该做的。
func (s *Service) indexVector(ctx context.Context, ns store.Namespace, a *types.AtomCard) {
	if s.embedder == nil || s.vecIdx == nil || a == nil {
		return
	}
	vec, err := s.embedder.Embed(ctx, a.Assertion)
	if err != nil {
		s.noteVectorFailure(ctx, ns, a, "向量化失败", err)
		return
	}
	if err := s.vecIdx.Upsert(ctx, string(ns), a.ID, string(a.Scope), vec); err != nil {
		s.noteVectorFailure(ctx, ns, a, "写向量索引失败", err)
	}
}

// unindexVector 把一条事实的向量从索引里删掉。
func (s *Service) unindexVector(ctx context.Context, ns store.Namespace, id string) {
	if s.vecIdx == nil || id == "" {
		return
	}
	_ = s.vecIdx.Delete(ctx, string(ns), id)
}

// noteVectorFailure 记一次向量同步失败。
//
// 留痕的必要性：向量化失败的症状是「语义召回少了一条」，
// 而那在结果里完全看不出来（内容一条不少，只是少了一条相关的）。
// 没有留痕就没人知道该去补算。
func (s *Service) noteVectorFailure(ctx context.Context, ns store.Namespace, a *types.AtomCard,
	what string, err error) {
	_ = s.store.AppendJournal(ctx, ns, &types.JournalEntry{
		ID: "journal-vector-" + a.ID, At: s.now(),
		Kind: types.JournalVectorSync, Subject: a.ID, Actor: types.DecidedRule,
		Summary: what + "（事实已入库，语义召回会少这一条）：" + err.Error(),
	})
}
