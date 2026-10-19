package types

import (
	"testing"
	"time"
)

// timeFixture 固定时刻的夹具：让与时间相关的判读不受运行时刻影响
// （冷却期、重生成时机这类断言最容易被「跑在整点边界上」搞成假红）。
func timeFixture() time.Time {
	return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
}

func TestProvenanceRuntimeErrorIsTheOnlyCaptureJudge(t *testing.T) {
	// 判据是**来源**，不是「像不像值得记的话」。
	// 这条用例钉住：一句内容明显是报错，只要来源不是运行期错误，就照收；
	// 反之内容像正经结论，只要标了运行期错误，就不收。
	cases := []struct {
		name        string
		content     string
		runtimeErr  bool
		wantCapture bool
	}{
		{"正常结论", "这台机器的磁盘长期偏小", false, true},
		{"像结论的报错文本", "执行失败：磁盘不足，请联系管理员并提供工单号", false, true},
		{"像结论的运行期错误", "磁盘空间不足", true, false},
		{"空的运行期错误", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &RawEvent{Content: tc.content, Provenance: Provenance{RuntimeError: tc.runtimeErr}}
			if got := ev.Capturable(); got != tc.wantCapture {
				t.Fatalf("Capturable()=%v，期望 %v（内容=%q）", got, tc.wantCapture, tc.content)
			}
		})
	}
}

func TestAtomActiveRejectsDoubleRetire(t *testing.T) {
	now := timeFixture()
	live := &AtomCard{ID: "a1"}
	if !live.Active() {
		t.Fatal("未退役的事实卡应参与召回")
	}

	superseded := &AtomCard{ID: "a2", SupersededBy: "a3"}
	if superseded.Active() {
		t.Fatal("被推翻的事实卡不得参与召回")
	}
	if !superseded.SoftRetired() {
		t.Fatal("被推翻应属软退役")
	}

	deprecated := &AtomCard{ID: "a4", DeprecatedAt: now}
	if deprecated.Active() {
		t.Fatal("被作废的事实卡不得参与召回")
	}

	// 出处须先备齐，否则 Valid 会先撞证据校验、轮不到双退役这条断言。
	both := &AtomCard{ID: "a5", QuoteEventID: "e1", Confidence: ConfidenceHigh,
		Importance: ImportanceMed, SupersededBy: "a6", DeprecatedAt: now}
	if err := both.Valid(); err != ErrAtomDoubleRetired {
		t.Fatalf("同时推翻+作废应判 ErrAtomDoubleRetired，实得 %v", err)
	}
}

func TestAtomValidRejectsMissingEvidence(t *testing.T) {
	// 没有出处的记忆既不能审计也不能推翻——形状校验必须在入库前拦住。
	a := &AtomCard{ID: "a1", Confidence: ConfidenceHigh, Importance: ImportanceMed}
	if err := a.Valid(); err != ErrAtomNoEvidence {
		t.Fatalf("缺 QuoteEventID 应判 ErrAtomNoEvidence，实得 %v", err)
	}

	a.QuoteEventID = "e1"
	if err := a.Valid(); err != nil {
		t.Fatalf("补上出处后应合法，实得 %v", err)
	}

	a.Confidence = "0.82" // 浮点置信度诱使人微调，形状校验必须拒
	if err := a.Valid(); err != ErrAtomBadConfidence {
		t.Fatalf("浮点置信应判 ErrAtomBadConfidence，实得 %v", err)
	}
}

// 归一过激会把互相矛盾的事实判成同一条——而该走的是送审不是合并。
func TestSignatureKeepsContradictingFactsApart(t *testing.T) {
	a := &AtomCard{EntityID: "e1", Assertion: "磁盘  剩余 2G"}
	b := &AtomCard{EntityID: "e1", Assertion: "磁盘剩余 3G"}
	if a.Signature() == b.Signature() {
		t.Fatal("数量不同的两条事实不应撞成同一签名")
	}

	c := &AtomCard{EntityID: "e1", Assertion: "  磁盘  剩余 2G  "}
	if a.Signature() != c.Signature() {
		t.Fatal("空白差异应折叠成同一签名")
	}
}

func TestNodeValidRejectsAtomOnWrongLevel(t *testing.T) {
	id, atom := "n1", "a1"

	leaf := &MemoryNode{ID: id, Level: LevelLeaf}
	if err := leaf.Valid(); err != ErrLeafWithoutAtom {
		t.Fatalf("叶节点无事实引用应判 ErrLeafWithoutAtom，实得 %v", err)
	}

	branch := &MemoryNode{ID: id, Level: LevelBranch, AtomID: &atom}
	if err := branch.Valid(); err != ErrNonLeafWithAtom {
		t.Fatalf("非叶持事实应判 ErrNonLeafWithAtom，实得 %v", err)
	}

	root := &MemoryNode{ID: id, Level: LevelRoot}
	if err := root.Valid(); err != nil {
		t.Fatalf("干净根节点应合法，实得 %v", err)
	}

	leaf.AtomID = &atom
	if err := leaf.Valid(); err != nil {
		t.Fatalf("叶节点持事实应合法，实得 %v", err)
	}
}

func TestEntityPageRegenerationCooldown(t *testing.T) {
	now := timeFixture()

	// 从未试过：立即可生成。
	fresh := &EntityPage{EntityID: "e1", Dirty: true}
	if !fresh.RegenerationDue(now, 60) {
		t.Fatal("从未生成过的脏页面应可立即生成")
	}

	// 刚试过：冷却期内不该重复生成（宿主连续晋升十条事实时只生成一次）。
	recent := &EntityPage{EntityID: "e1", Dirty: true, LastRegenAt: now.Add(-1)}
	if recent.RegenerationDue(now, 60) {
		t.Fatal("冷却期内不该重复生成")
	}

	// 干净页面：永不生成。
	clean := &EntityPage{EntityID: "e1", Dirty: false, LastRegenAt: now.Add(-999)}
	if clean.RegenerationDue(now, 60) {
		t.Fatal("干净页面不该进重生成队列")
	}

	// 冷却期过了：可再生成（哪怕上次失败过——失败不豁免重试）。
	stale := &EntityPage{EntityID: "e1", Dirty: true, LastRegenAt: now.Add(-120), RegenFailure: "模型超时"}
	if !stale.RegenerationDue(now, 60) {
		t.Fatal("冷却期后应可再生成")
	}
}

func TestCandidateStatusPredicates(t *testing.T) {
	pending := &Candidate{Status: CandidatePending}
	if pending.Decided() || pending.Settled() {
		t.Fatal("待判定候选既未判定也未落定")
	}

	review := &Candidate{Status: CandidateReview}
	if !review.Decided() {
		t.Fatal("转人工是一个被显式选出的出口，属判定层已表态")
	}
	if review.Settled() {
		t.Fatal("待人工处置的候选不算落定，不得进召回面")
	}

	promoted := &Candidate{Status: CandidatePromoted}
	if !promoted.Decided() || !promoted.Settled() {
		t.Fatal("已晋升候选应既表态又落定")
	}
}

func TestCandidateValidRejectsNoSource(t *testing.T) {
	c := &Candidate{ID: "c1", Status: CandidatePending}
	if err := c.Valid(); err != ErrCandidateNoSource {
		t.Fatalf("无素材来源应判 ErrCandidateNoSource，实得 %v", err)
	}
	c.RawEventIDs = []string{"e1"}
	if err := c.Valid(); err != nil {
		t.Fatalf("补上来源后应合法，实得 %v", err)
	}
	c.Status = "maybe"
	if err := c.Valid(); err != ErrCandidateUnknownStatus {
		t.Fatalf("未知状态应判 ErrCandidateUnknownStatus，实得 %v", err)
	}
}

func TestRankHelpersAreOrdered(t *testing.T) {
	if !(ConfidenceHigh.Rank() > ConfidenceMed.Rank() && ConfidenceMed.Rank() > ConfidenceLow.Rank()) {
		t.Fatal("置信档位排序应高>中>低")
	}
	if ImportanceHigh.Rank() <= ImportanceLow.Rank() {
		t.Fatal("重要度档位排序应高>低")
	}
	if Confidence("").Rank() != 0 || Importance("").Rank() != 0 {
		t.Fatal("未知档位应排在最低")
	}
}
