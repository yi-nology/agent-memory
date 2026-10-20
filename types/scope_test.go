package types

import "testing"

// 本文件：归属维度（记录级隔离）的用例。

// TestScopeVisibility 可见性判据只有一条。
//
// 空归属（全局）⇒ 谁都看得见；非空归属 ⇒ 只有同归属看得见。
func TestScopeVisibility(t *testing.T) {
	cases := []struct {
		name   string
		card   Scope
		reader Scope
		want   bool
	}{
		{"全局事实对任何读者可见", "", "scope-a", true},
		{"全局事实对无归属读者可见", "", "", true},
		{"本组事实对本组读者可见", "scope-a", "scope-a", true},
		{"本组事实对别组读者不可见", "scope-a", "scope-b", false},
		{"本组事实对无归属读者不可见", "scope-a", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.card.VisibleTo(tc.reader); got != tc.want {
				t.Fatalf("VisibleTo()=%v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestSignatureSeparatesScopes 同一条断言在两个归属下是两张不同的卡。
//
// 这条钉的是**归属参与签名**：签名不含归属的话，跨归属的重复检测会把
// 一组的卡片并进另一组的卡片——那是内容泄漏，不是去重。
func TestSignatureSeparatesScopes(t *testing.T) {
	global := &AtomCard{EntityID: "e1", Assertion: "内核版本为 5.15"}
	mine := &AtomCard{EntityID: "e1", Scope: "scope-a", Assertion: "内核版本为 5.15"}
	other := &AtomCard{EntityID: "e1", Scope: "scope-b", Assertion: "内核版本为 5.15"}

	if global.Signature() == mine.Signature() {
		t.Fatal("全局卡与本组卡的签名不该相同：相同则并入会把两边的卡混成一张")
	}
	if mine.Signature() == other.Signature() {
		t.Fatal("不同归属的卡签名不该相同")
	}
	// 同归属同断言 ⇒ 同一张卡（去重仍然有效）。
	same := &AtomCard{EntityID: "e1", Scope: "scope-a", Assertion: "内核版本为 5.15"}
	if mine.Signature() != same.Signature() {
		t.Fatal("同归属同断言的签名应相同")
	}
}

// TestSignatureOfGlobalCardsIsByteIdentical 全局卡的签名与加归属之前**逐字相同**。
//
// 理由：签名是**存起来的列**。加了归属维度之后若无条件插一段，
// 已落库事实卡的签名就全变了——旧卡再也查不到，
// 于是同一件事被重新记一次时会多出一张重复卡。
// 故归属为空时必须保持字节级一致。
func TestSignatureOfGlobalCardsIsByteIdentical(t *testing.T) {
	// 旧规则：实体 + 分隔符 + 归一断言。
	want := "e1" + SignatureSep + NormalizeAssertion("内核版本为 5.15")
	got := AtomSignature("e1", "", "内核版本为 5.15")
	if got != want {
		t.Fatalf("全局卡的签名变了：期望 %q 实得 %q", want, got)
	}
	// 卡片自己算出来的也要一致（两处实现不许漂）。
	card := &AtomCard{EntityID: "e1", Assertion: "内核版本为 5.15"}
	if card.Signature() != want {
		t.Fatalf("卡片签名与包级函数不一致: %q vs %q", card.Signature(), want)
	}
}
