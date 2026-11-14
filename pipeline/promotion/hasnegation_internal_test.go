package promotion

import "testing"

// 英文否定判定按**词元**而不按子串（issue #56）：
// 裸子串会让 un/no 命中 function/known/north 的词内子串，
// 无否定的英文断言被判含否定，进 XOR 矛盾判定后产生假矛盾。
// 每条用例都对应一个修复前会误判的词。
func TestHasNegationEnglishWordBoundary(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
		note string
	}{
		{"The function returns the count", false, "function 词内含 un，不是否定"},
		{"This is a well-known issue", false, "known 词内含 no，不是否定"},
		{"Head north for two kilometers", false, "north 词内含 no，不是否定"},
		{"Notice the unit number", false, "notice/unit/number 都只是含否定字面，不是否定"},
		{"The disk is not mounted", true, "not 整词"},
		{"Reboot never happened", true, "never 整词"},
		{"No further action required", true, "no 整词"},
		{"The service is unhealthy", false, "un 连写形态不判（unit/under/unless 全以 un 开头，前缀判正是本单要修的病）；un- 连字形态切词后整词命中"},
		{"un-healthy deploy", true, "连字形态切词后 un 独立成词元"},
		{"nonempty result set", true, "non 是否定前缀：nonempty 是否定"},
		{"non-blocking IO", true, "连字符切词后 non 独立成词元"},
		{"under pressure until noon", false, "under/until 是 un 开头的非否定词——un 不做前缀判"},
		{"磁盘未挂载", true, "中文族按子串判，不受影响"},
		{"没 有 问题", true, "中文族按子串判（无词边界）"},
	} {
		if got := hasNegation(tc.text); got != tc.want {
			t.Errorf("hasNegation(%q)=%v，期望 %v（%s）", tc.text, got, tc.want, tc.note)
		}
	}
}

// 修复前的假矛盾形态（issue #56 的触发场景）：两条都无否定的英文断言
// 不得判出「否定方向相反」。
func TestNegationDiffersNoFalsePositive(t *testing.T) {
	if negationDiffers("The function returns the count", "The function returns a number") {
		t.Fatal("两条无否定的断言被误判为否定方向相反（词内子串假阳性）")
	}
	if negationDiffers("The service is healthy", "The service is unhealthy") {
		t.Fatal("unhealthy 的 un 连写形态不判（见 hasNegation 的取舍说明）")
	}
	if !negationDiffers("The result is empty", "The result is non-empty") {
		t.Fatal("non- 否定形态必须判出")
	}
	if !negationDiffers("磁盘已挂载", "磁盘未挂载") {
		t.Fatal("中文否定方向相反必须判出（既有语义不变）")
	}
}
