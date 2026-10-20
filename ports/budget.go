package ports

// BucketID 配额桶标识。
//
// 刻意是**字符串**而不是枚举：桶的含义由**宿主**定——
// 本仓的「事实 / 引用 / 画像」只是本仓的来源分类，
// 自造召回的宿主有自己的分法（按它们自己的对象维度）。
// 内核替宿主枚举桶，等于把宿主的语义写进本仓，而那正是本仓不认领域的那条纪律。
//
// 也不做成 struct：新 ID 就该是「多一个字符串」的事，
// 做成结构体只会逼着每个宿主再实现一遍字符串的语义。
type BucketID string

// DropReason 丢弃原因。
//
// 丢弃**可归因**是召回质量的基本要求：只给一个总数时，
// 排查「为什么少了」只能靠猜——是桶配额满了、总量满了、还是内容重复？
// 三者的处置完全不同（改分桶 / 改总量 / 改提炼层）。
type DropReason string

const (
	// DropBudget 超**总**预算而丢弃。
	DropBudget DropReason = "budget"
	// DropQuota 超**桶**配额而丢弃。
	//
	// 刻意与 DropBudget 分开：前者说明总量不够（该调大总预算），
	// 后者说明**这一类**内容太多（该调分桶占比，或少产出这一类）。
	// 合成一个数就看不出是哪一种——而这两者的处置完全不同。
	DropQuota DropReason = "quota"
	// DropLowScore 分数过低而丢弃。
	DropLowScore DropReason = "low_score"
	// DropExpired 内容过期而丢弃。
	DropExpired DropReason = "expired"
	// DropDuplicate 内容重复而丢弃。
	DropDuplicate DropReason = "duplicate"
	// DropDiversity 单一来源超额而丢弃（多样性裁剪）。
	DropDiversity DropReason = "diversity"
)

// Budget 分桶配额与丢弃计数。
//
// # 它是**契约件**，不是召回实现的一部分
//
// 「按来源分桶的配额」与「丢弃原因计数」这两件事本身与「怎么取数、
// 怎么排序」无关：自造召回的宿主要的正是这两件（它们是最贵的那两件：
// 没有它们，召回质量出问题时能观测到的只有「结果少了」，看不到「为什么少」），
// 但它不可能整体替换成本仓的召回链——那会连带走晋升判据、召回排序、band 强制。
//
// 故把这两件抽到这里：**取数与排序策略仍由宿主自定，桶与丢弃这套契约可复用。**
//
// # 用法
//
//	b := ports.NewBudget(1500, map[ports.BucketID]float64{
//		"fact": 0.7, "quote": 0.2, "profile": 0.1,
//	})
//	for _, item := range items {          // 宿主自己的取数与排序
//		if b.Admit(item.bucket, item.tokens) {
//			kept = append(kept, item)
//		}
//	}
//	// 宿主在自己那里丢弃的（低分、过期、去重…）按原因上报：
//	b.Drop(ports.DropLowScore, 3)
//	drops := b.Dropped()
//
// # 零值与 nil
//
// nil *Budget 是**不启用配额**（全收、不计数）——零成本，不配置就不启用。
// 这不是「宽松」，是「宿主没要求配额」；宿主要配额时自己造一个。
type Budget struct {
	total     int
	shares    map[BucketID]float64
	caps      map[BucketID]int
	used      map[BucketID]int
	kept      map[BucketID]int
	usedTotal int
	dropped   map[DropReason]int
}

// NewBudget 造配额件。
//
// total 是总预算（token）。**total <= 0 表示不限总量**——
// 宿主可能只想用分桶配额而不管总量。
//
// shares 是各桶的**相对权重**（按总和归一）。规则三条：
//
//   - nil 或总和 <= 0 → **不分桶**（每桶都不设上限），只守总量；
//   - 总和 > 0 → 归一（各除以总和），故宿主按「80/15/5」这类整数配也对；
//   - 权重 0 的桶**上限为 0**（一条都收不下）——「这一类不要」的自然读法。
//
// 「相对权重」的直接推论：**只配一个桶时它独占全部预算**。
// 这与本仓既有的口径一致（recall.Config.normalize 干的是同一件事）——
// 两处口径若不同，同一个宿主在两处会拿到不同的配额。
//
// 桶没出现在 shares 里 → 该桶**不设上限**（只受总量约束）。
//
// NewBudget 不会因为参数不合法而失败：配额是「装不装得下」的度量，
// 不是一个需要响亮失败的契约（与本仓「缺失的默认值不是宽容，是事故」那条
// 纪律不冲突——那条说的是**必需的外部能力**，而这里是可选的度量件）。
func NewBudget(total int, shares map[BucketID]float64) *Budget {
	b := &Budget{
		total:   total,
		shares:  map[BucketID]float64{},
		caps:    map[BucketID]int{},
		used:    map[BucketID]int{},
		kept:    map[BucketID]int{},
		dropped: map[DropReason]int{},
	}
	sum := 0.0
	for _, v := range shares {
		sum += v
	}
	if sum <= 0 {
		return b
	}
	for id, v := range shares {
		norm := v / sum
		b.shares[id] = norm
		b.caps[id] = int(float64(total) * norm)
	}
	return b
}

// Admit 试着接收一条内容。
//
// 返回 true = 收下（已计入用量与桶计数）；false = 收不下。
// 收不下时**按原因计数**：桶配额不足记 DropQuota，总量不足记 DropBudget。
//
// 判定顺序是**桶先、总量后**：一条内容两条都超限时，归给桶——
// 因为处置不同（桶满该调分桶，总量满该调总量），而「桶满」是更具体的那个原因。
//
// 收不下**不意味着后面收不下**：本方法只判定这一条，
// 后面可能有更小的内容塞得进去——调用方不要因为一次 false 就 break。
func (b *Budget) Admit(bucket BucketID, tokens int) bool {
	if b == nil {
		return true // 未启用配额：全收
	}
	if tokens < 0 {
		tokens = 0
	}
	if cap, ok := b.caps[bucket]; ok && tokens > cap-b.used[bucket] {
		b.dropped[DropQuota]++
		return false
	}
	if b.total > 0 && b.usedTotal+tokens > b.total {
		b.dropped[DropBudget]++
		return false
	}
	b.used[bucket] += tokens
	b.usedTotal += tokens
	b.kept[bucket]++
	return true
}

// Drop 按原因记一次丢弃。
//
// 供召回器上报**它自己**的丢弃（低分、过期、去重……）——
// 那些不是配额判定的结果，件里无从得知。
func (b *Budget) Drop(reason DropReason, n int) {
	if b == nil || n <= 0 {
		return
	}
	b.dropped[reason] += n
}

// Dropped 按原因分路的丢弃计数（副本）。
//
// 返回副本而不是内部 map：调用方拿到的东西不该能改坏计数。
func (b *Budget) Dropped() map[DropReason]int {
	if b == nil {
		return nil
	}
	out := make(map[DropReason]int, len(b.dropped))
	for k, v := range b.dropped {
		out[k] = v
	}
	return out
}

// DroppedTotal 丢弃总条数。
func (b *Budget) DroppedTotal() int {
	if b == nil {
		return 0
	}
	n := 0
	for _, v := range b.dropped {
		n += v
	}
	return n
}

// Kept 各桶收到的**条数**（副本）。
func (b *Budget) Kept() map[BucketID]int {
	if b == nil {
		return nil
	}
	out := make(map[BucketID]int, len(b.kept))
	for k, v := range b.kept {
		out[k] = v
	}
	return out
}

// Used 各桶已用的 token（副本）。
func (b *Budget) Used() map[BucketID]int {
	if b == nil {
		return nil
	}
	out := make(map[BucketID]int, len(b.used))
	for k, v := range b.used {
		out[k] = v
	}
	return out
}

// UsedTotal 已用的总 token。
func (b *Budget) UsedTotal() int {
	if b == nil {
		return 0
	}
	return b.usedTotal
}

// Total 总预算（token；<= 0 表示不限总量）。
func (b *Budget) Total() int {
	if b == nil {
		return 0
	}
	return b.total
}

// Share 某桶的占比（归一后；未登记的桶返回 0）。
func (b *Budget) Share(bucket BucketID) float64 {
	if b == nil {
		return 0
	}
	return b.shares[bucket]
}
