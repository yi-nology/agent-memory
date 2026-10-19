package application

import (
	"context"

	"github.com/yi-nology/agent-memory/pipeline/promotion"
	"github.com/yi-nology/agent-memory/store"
)

// promoStore 把全量存储适配成晋升面需要的窄存储。
//
// # 存在的理由
//
// 晋升要在一个**事务**里做四步（候选落定 + 事实入库 + 实体计数 + 审计条目），
// 而事务回调收到的那份存储必须是**事务内**的那一份——
// 用外层那份写会落到事务外（原子性没了且不报任何错），
// 连接池只有一条连接时还会直接挂起（事务持着唯一连接，外层语句永远等不到）。
//
// # 为什么适配落在装配面
//
// 窄事务的回调类型定义在 pipeline/promotion 里：回调给的是晋升面那一小片，
// 而不是全量 store.Store（全量接口在测试里要实现 70 多个方法，
// 那正是「阶段只声明自己需要的子接口」这条纪律要躲开的东西）。
//
// 而 storage 层不能反向依赖 pipeline——依赖方向红线是
// types/ports → store → pipeline → application → adapters，
// 存储层一旦 import 流水线，这条线就反了。故适配落在装配面：
// 它是唯一同时看得见两边的层。
//
// # 注意：本类型不再满足 store.Store
//
// 它自己的 Transaction 是窄版，会遮蔽内嵌的全量版。这是有意的——
// 晋升面只该看见窄接口，别的面（导出、生命周期）走的是别的适配。
type promoStore struct{ store.Store }

// Transaction 开事务，把事务内的那份存储交给回调。
func (p promoStore) Transaction(ctx context.Context, fn func(context.Context, promotion.Store) error) error {
	return p.Store.Transaction(ctx, func(ctx context.Context, s store.Store) error {
		return fn(ctx, promoStore{s})
	})
}
