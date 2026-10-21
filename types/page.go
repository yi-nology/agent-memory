package types

import "time"

// EntityPage 实体页面：某个实体的长文画像，由其事实卡**生成**出来。
//
// # 它是派生物，不是真源
//
// 页面正文可以整段重写、可以随时丢弃重建，事实卡不受影响。
// 判断某条信息是否可信，永远回 AtomCard，不看页面。
//
// # 脏标记而不是同步重生成
//
// 每次晋升都重写页面，等于让「写一条记忆」的延迟里多一次模型调用——
// 而晋升发生在会话刚结束时，正是宿主最忙的时候。所以写入侧只标脏，
// 由运行时/CLI/宿主择机触发重生成。
//
// # 失败必须留痕
//
// RegenFailure 记录最后一次重生成失败的时刻与原因。没有它，一个持续失败的
// 页面会安静地一直脏着，而调用方看到的是「页面存在且是旧的」——
// 这两者在看板上长得一模一样。
type EntityPage struct {
	// EntityID 归属实体标识。
	EntityID string
	// Scope 页面视角（归属）。**逻辑主键的一部分**。
	//
	// # 页面按视角分页，不按实体分页
	//
	// 页面的逻辑主键是 (namespace, entity_id, scope)：
	// 同一个实体在不同归属下**各有一页**，因为「这个对象在我的视角下是什么样」
	// 与「这个对象在别人视角下是什么样」是两件事——
	// 事实卡已经按归属隔离了，而页面是事实的摘要：
	// 摘要若不分视角，等于把别家的事实揉进一张全局画像里，
	// 而画像是人读的一整段话，读者分不清哪句属于自己。
	//
	// # 这一页汇总哪些事实
	//
	// 「全局事实 + 同归属事实」（判据 types.Scope.VisibleTo）：
	// 归属为空的事实是全局的，人人都该看到；
	// 归属非空且不等于本页视角的事实**一条都不进来**。
	//
	// ⚠️ 本页 scope 为空 = **全局视角**，这一页只汇总全局事实。
	// 这与 store.AtomFilter.Scope 的空值语义（空=不隔离）**不同**，
	// 且必须不同：全局页若也收别人的事实，它就是一张所有人都能看的
	// 汇总了别人事实的页面——那是**内容泄漏**，不是「少取」。
	// 两种空值语义分属两件东西（过滤面 vs 页面视角），故各自写明，
	// 不共用一个参数名下的同一套含义。
	Scope Scope
	// Headline 一句话画像（召回时可作为低成本段落注入）。
	Headline string
	// Body 长文正文。
	Body string
	// AtomIDs 页面覆盖的事实卡集合。
	AtomIDs []string
	// Dirty 是否待重生成。
	Dirty bool
	// UpdatedAt 页面最后生成时刻。
	UpdatedAt time.Time
	// LastRegenAt 最后一次**尝试**重生成的时刻（含失败）。
	LastRegenAt time.Time
	// RegenFailure 最后一次失败原因（空串=无失败）。
	RegenFailure string
}

// RegenerationDue 页面是否该重生成（脏且不在冷却期内）。
//
// StaleAfter 给重生成留冷却：宿主连续晋升十条事实时，只该重生成一次。
func (p *EntityPage) RegenerationDue(now time.Time, staleAfter time.Duration) bool {
	if !p.Dirty {
		return false
	}
	return p.LastRegenAt.IsZero() || now.Sub(p.LastRegenAt) >= staleAfter
}

// Pending 重生成失败但仍脏——调用方需要区别于「没试过」。
func (p *EntityPage) Pending() bool { return p.Dirty }
