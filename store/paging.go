package store

import (
	"context"

	"github.com/yi-nology/agent-memory/types"
)

// 分页取全的单页大小。
//
// 值取「一次往返搬多少行不心疼」的常规档：太小则全量扫描的往返数
// 随库规模线性涨，太大则单页内存占用失去意义。调用方一般不该配它——
// 它是本函数的实现参数，不是契约。
const defaultScanPage = 500

// AtomLister 分页取全所需的取数面（消费者按需实现/裁剪）。
//
// 故意不是 AtomStore：生命周期面、召回装配面各自声明自己的窄接口，
// 本函数不该逼它们长出全量事实卡存取面。
type AtomLister interface {
	ListAtoms(ctx context.Context, ns Namespace, f AtomFilter) ([]*types.AtomCard, error)
}

// ListAllAtoms 分页取尽事实卡：f 的条件原样生效，Limit/Offset 由本函数接管。
//
// # 为什么必须有它
//
// 「全量扫描」类动作（向量补算、生命周期四件套、迁移导出）不能拿一个
// 写死的大 Limit（100000 之类）冒充全量：单 namespace 超过那个数时，
// ListAtoms 按取数序（重要度、发生时刻）截断，排在尾部的卡静默落选——
// 而落选的恰恰是最不重要的那批，GC/合并/过期**永不**处理它们
// （issue #59）。Reindex 也一样：limit<=0 声称「不限量」，实际被存储层
// 兜成缺省 50，宿主拿到成功回执但只补了前 50 条（issue #49）。
//
// 本函数按 defaultScanPage 翻页，直到某页不满一页（取尽）。
// 翻页不重不漏的前提是**取数期间数据不动**：调用方应先取尽、后处理
// （处理中途的写入不影响已取出的那份快照）。
func ListAllAtoms(ctx context.Context, s AtomLister, ns Namespace, f AtomFilter, pageSize int) ([]*types.AtomCard, error) {
	if pageSize <= 0 {
		pageSize = defaultScanPage
	}
	out := make([]*types.AtomCard, 0, pageSize)
	for offset := 0; ; offset += pageSize {
		page := f // 不动调用方的 filter：Limit/Offset 是本函数的翻页参数
		page.Limit = pageSize
		page.Offset = offset
		batch, err := s.ListAtoms(ctx, ns, page)
		if err != nil {
			return out, err
		}
		out = append(out, batch...)
		if len(batch) < pageSize {
			return out, nil
		}
	}
}
