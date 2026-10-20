package ports

import "testing"

// 分桶配额与丢弃计数这套**契约件**的用例。
//
// 它住在 ports 而不是 pipeline/recall，因为「桶 + 配额 + 丢弃计数」与
// 「怎么取数、怎么排序」无关——宿主自造召回时也要它（见 ports/budget.go）。
// 故这里测的是**件本身**的行为，不经过任何召回实现。

// 桶配额不足与总量不足必须**分开记**。
//
// 这正是这套件存在的理由：两者处置完全不同（桶满该调分桶占比，
// 总量满该调总预算），合成一个数就看不出是哪一种——
// 而「看不出是哪一种」正是下游自造召回时最缺的那件东西。
func TestBudgetDistinguishesQuotaFromBudget(t *testing.T) {
	// 总预算 1000；「a」桶的权重是 0.1（与「rest」一起归一后占 10%，上限 100）。
	// 另有一个**不在表里**的桶「free」：未登记的桶不设桶上限，只受总量约束。
	b := NewBudget(1000, map[BucketID]float64{"a": 0.1, "rest": 0.9})

	// 一条 150 token 的内容：桶装不下（100），但总量还够 → 桶不足。
	if b.Admit("a", 150) {
		t.Fatal("超出桶配额的内容不该被收下")
	}
	if got := b.Dropped()[DropQuota]; got != 1 {
		t.Fatalf("桶不足应记 DropQuota=1，实得 %d", got)
	}
	if got := b.Dropped()[DropBudget]; got != 0 {
		t.Fatalf("总量没满，不该记 DropBudget，实得 %d", got)
	}

	// 一个**不限桶**的内容：桶不设上限，只有总量能挡它。
	// 收 900 → 收下（总量 1000 够）；再来 200 → 总量不足。
	if !b.Admit("free", 900) {
		t.Fatal("不限桶的内容在总量够时应收下")
	}
	if b.Admit("free", 200) {
		t.Fatal("总量已满，不该再收")
	}
	if got := b.Dropped()[DropBudget]; got != 1 {
		t.Fatalf("总量不足应记 DropBudget=1，实得 %d", got)
	}
	if got := b.Dropped()[DropQuota]; got != 1 {
		t.Fatalf("桶不足那条不该被改写，实得 %d", got)
	}
}

// 桶之间**互不占用**额度：事实桶满了不占画像桶的额度。
//
// 这条是「画像桶恒被挤空」那个症状的对立面——
// 分桶是上限，不是先到先得。
func TestBudgetBucketsAreIndependent(t *testing.T) {
	b := NewBudget(1000, map[BucketID]float64{"fact": 0.7, "profile": 0.1, "other": 0.2})
	if !b.Admit("fact", 700) {
		t.Fatal("事实桶在自己的额度内应收下")
	}
	if b.Admit("fact", 1) {
		t.Fatal("事实桶超额后不该再收")
	}
	if !b.Admit("profile", 100) {
		t.Fatal("画像桶的额度不该被事实桶占用")
	}
	kept := b.Kept()
	if kept["fact"] != 1 || kept["profile"] != 1 {
		t.Fatalf("各桶计数不对: %v", kept)
	}
}

// 一次收不下**不意味着**后面收不下。
//
// 配额是逐条判定的，调用方不该因为一次 false 就 break——
// 后面可能有更小的内容塞得进去。漏掉这条的后果是「预算明明还有，
// 结果丢了一堆」。
func TestBudgetSmallerItemStillFitsAfterRejection(t *testing.T) {
	b := NewBudget(1000, map[BucketID]float64{"a": 0.1, "rest": 0.9}) // 桶上限 100
	if b.Admit("a", 150) {
		t.Fatal("150 超过桶上限 100，不该收下")
	}
	if !b.Admit("a", 80) {
		t.Fatal("前面那次拒绝后，80 仍应塞得进去（逐条判定，不是累计拒绝）")
	}
}

// 占比是**相对权重**（按总和归一）：宿主按「80/15/5」这类整数配也对。
//
// 这与本仓既有的口径一致（recall.Config.normalize 干的是同一件事）——
// 两处口径若不同，同一个宿主在两处会得到不同的配额。
func TestBudgetNormalizesShares(t *testing.T) {
	b := NewBudget(1000, map[BucketID]float64{"a": 80, "b": 20})
	if got := b.Share("a"); got < 0.79 || got > 0.81 {
		t.Fatalf("占比应归一到 0.8，实得 %v", got)
	}
	// 归一后「a」的上限是 800（不是 80000）。
	if b.Admit("a", 801) {
		t.Fatal("归一后的桶上限应是 800")
	}
	// 只配一个桶时它**独占**全部预算（相对权重的直接推论）。
	one := NewBudget(1000, map[BucketID]float64{"only": 0.1})
	if !one.Admit("only", 1000) {
		t.Fatal("只配一个桶时它应独占全部预算")
	}
}

// 占比为 0 的桶**一条都收不下**（「这一类不要」的自然读法）。
func TestBudgetZeroShareBucketIsOff(t *testing.T) {
	b := NewBudget(1000, map[BucketID]float64{"a": 0.5, "off": 0})
	if b.Admit("off", 1) {
		t.Fatal("占比 0 的桶不该收下任何内容")
	}
	if got := b.Dropped()[DropQuota]; got != 1 {
		t.Fatalf("应记桶不足，实得 %d", got)
	}
}

// shares 为空（或总和为 0）→ **不分桶**，只守总量。
func TestBudgetWithoutSharesOnlyCapsTotal(t *testing.T) {
	b := NewBudget(100, nil)
	if !b.Admit("anything", 100) {
		t.Fatal("不分桶时总量够就该收下")
	}
	if b.Admit("anything", 1) {
		t.Fatal("总量已满，不该再收")
	}
	if got := b.Dropped()[DropBudget]; got != 1 {
		t.Fatalf("应记总量不足，实得 %d", got)
	}
}

// total <= 0 表示**不限总量**（宿主可能只想用分桶配额）。
func TestBudgetNonPositiveTotalMeansUnlimited(t *testing.T) {
	b := NewBudget(0, map[BucketID]float64{"a": 0.5})
	if !b.Admit("free", 100000) {
		t.Fatal("总量不限时不该因总量拒收")
	}
}

// 召回器自己丢的（低分、过期、去重）按原因上报。
func TestBudgetDropReportsByReason(t *testing.T) {
	b := NewBudget(1000, nil)
	b.Drop(DropLowScore, 3)
	b.Drop(DropExpired, 1)
	b.Drop(DropDuplicate, 2)
	drops := b.Dropped()
	if drops[DropLowScore] != 3 || drops[DropExpired] != 1 || drops[DropDuplicate] != 2 {
		t.Fatalf("按原因计数不对: %v", drops)
	}
	if b.DroppedTotal() != 6 {
		t.Fatalf("丢弃总数不对: %d", b.DroppedTotal())
	}
	// 计数为 0 与不出现是同一件事——宿主读 map 时不必判存在性。
	if _, ok := drops[DropQuota]; ok {
		t.Fatal("没发生过的原因不该出现在表里")
	}
}

// 取出来的表是**副本**：调用方改它不改坏计数。
func TestBudgetCountersAreCopies(t *testing.T) {
	b := NewBudget(1000, nil)
	b.Drop(DropLowScore, 1)
	d := b.Dropped()
	d[DropLowScore] = 99
	if b.Dropped()[DropLowScore] != 1 {
		t.Fatal("计数表必须是副本：调用方改了不该影响内部")
	}
	k := b.Kept()
	k["x"] = 99
	if len(b.Kept()) != 0 {
		t.Fatal("桶计数表必须是副本")
	}
}

// nil 件 = **不启用配额**（全收、不计数）。
//
// 零成本是这条的要点：宿主没要求配额时，不该为它建一堆状态。
func TestBudgetNilIsNoop(t *testing.T) {
	var b *Budget
	if !b.Admit("a", 1000) {
		t.Fatal("nil 件应全收")
	}
	b.Drop(DropLowScore, 5)
	if b.Dropped() != nil || b.DroppedTotal() != 0 || b.Kept() != nil {
		t.Fatal("nil 件不该有计数")
	}
	if b.Total() != 0 || b.UsedTotal() != 0 || b.Share("a") != 0 {
		t.Fatal("nil 件的读口应返回零值")
	}
}
