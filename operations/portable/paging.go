package portable

import (
	"context"
	"fmt"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：octop 导出面的**分页取全**能力（issue #61）。
//
// # 为什么需要它
//
// octop 导出（octop.go）曾把每张表的取数写死 Limit: 1000000 冒充全量——
// 与 #59（生命周期四件套的 100000 冒充全量）同形：单 namespace 超过那个数时，
// 排在取数序尾部的行被静默截断，导出的包残缺且零信号。
// 而迁移恰恰是最容不得「少了几条」的环节：包缺一半，导入「成功」了，
// 宿主从此拥有一段残缺的记忆且不知道缺了什么。
//
// 修法与 #59 同口径——**分页取尽**。其中 raw_events 与 atoms 两张表
// 走既有面就够了（ListRaw 的 Page 自带 Offset；atoms 直接复用
// store.ListAllAtoms）；candidates / entities / entity_pages / journal 四张表
// 的既有取数签名只收 limit——它们在 store.* **冻结面**上（仓外宿主实现着），
// 加 offset 参数 = 隐式 breaking（AGENTS.md「已发布的契约面是冻结的」），
// 翻页参数没处放。故能力面落在 store/optional.go（可选能力面的家）：
// store.PagedLister——独立小接口 + 运行时探测 + 明确的降级路径。
//
// # 为什么缺能力时是**报错**而不是降级
//
// 可选能力的缺省降级方向是「少给一点，不报错」——但那只适用于**增强**。
// 导出是库管理动作，**完整性是前提**：能走的降级形态只剩「单次大 Limit 硬取」，
// 而那正是本单要撤的冒充全量——把它留着当降级路径，等于把静默截断
// 换个名字留下。故没有分页能力 ⇒ 拿不出完整导出 ⇒ 在入口响亮失败
// （与 recall.New 对缺失捕获面的处理同一档：前提缺失不静默假装成功）。
// in-repo 两个后端（sqlite / postgres）都实现了这一面；
// 仓外自造存储的宿主补 4 个方法即可，错误信息里写清了为什么。
//
// # 翻页不重不漏的前提
//
// 每张表的取数序是**全序**（各排序键都带 id 兜底），先取尽、后写包。
// 导出是只读动作，取数期间的写入不影响已取出的那份快照——
// 快照之后落库的行不在本包里，那是快照语义，不是丢数据。

// octopScanPage 分页取全的单页大小。
//
// 与 store 的 defaultScanPage 同档：太小则全量扫描的往返数随库规模线性涨，
// 太大则单页内存占用失去意义。调用方一般不该配它——它是实现参数，
// 不是契约（ExportOptions.PageSize 只是给测试与特殊库用的口子）。
const octopScanPage = 500

// PagedSource octop 导出面的分页取全能力——即 store.PagedLister。
//
// 单独起名只给导出面的文档一个落点（接口本体与探测语义都在 store
// 那一份上，见 store/optional.go 的「契约演进规约」段）。
// 四个方法的过滤条件与 Source 里对应方法逐字一致（导出是库管理动作：
// 归属传空值取全量），翻页循环由本包的 pageAll 接管。
type PagedSource = store.PagedLister

// errNoPagedSource 存储没有分页取全能力时的报错。
//
// 为什么单独一个哨兵：错误信息要**教宿主怎么做**（实现 store.PagedLister），
// 而不是只说「不行」。四张表的翻页参数在冻结面上放不下，
// 这是可选能力面存在的原因——看到这条报错的宿主照着做就是了。
func errNoPagedSource(src any) error {
	return fmt.Errorf("portable: octop 导出要求存储实现 PagedSource 分页取全（%T 未实现）："+errNoPagedSourceHint, src)
}

const errNoPagedSourceHint = "candidates/entities/entity_pages/journal 四张表的取数签名" +
	"只收 limit（store.* 冻结面，翻页参数放不下），没有分页取全时导出拿不出完整集合；" +
	"实现 store.PagedLister 的 4 个方法（offset+limit 翻页）后重导。" +
	"⚠️ 若存储被包装（如 application 的向量同步装饰器），包装层须逐方法转发该能力"

// pageAll 分页取尽：按页翻到某页不满一页为止。
//
// fetch 收 (offset, limit)，返回**恰好这一页**。与 store.ListAllAtoms 同一条
// 取尽判据：len(batch) < limit 即取尽（而非 >0 判空——恰好整页的尾页
// 还得再翻一次空页才能确认，多一次往返换「不漏」，值得）。
func pageAll[T any](pageSize int, fetch func(offset, limit int) ([]T, error)) ([]T, error) {
	if pageSize <= 0 {
		pageSize = octopScanPage
	}
	out := make([]T, 0, pageSize)
	for offset := 0; ; offset += pageSize {
		batch, err := fetch(offset, pageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < pageSize {
			return out, nil
		}
	}
}

// listAllEntities 分页取尽全部实体（entities 表与 aliases 表共用：
// 别名没有「全部列出」的口子，要按实体逐个取）。
func listAllEntities(ctx context.Context, paged PagedSource, ns store.Namespace,
	pageSize int) ([]*types.Entity, error) {
	return pageAll(pageSize, func(offset, limit int) ([]*types.Entity, error) {
		return paged.ListEntitiesPaged(ctx, ns, "", offset, limit)
	})
}
