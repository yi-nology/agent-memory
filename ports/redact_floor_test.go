package ports

import (
	"regexp"
	"strings"
	"testing"

	"github.com/oekit/pkg/stringx"
)

// stripMask 剥掉掩码的形态标签与定界符，只留可见字符。
//
// 用途：检测「命中是否可复原」——把 `0123«long-hex:****»4567` 剥成
// `01234567`，若与原值相等即说明掩码只是装饰，凭据明文还在。
func stripMask(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch r {
		case '«':
			inTag = true
		case '»':
			inTag = false
		default:
			if !inTag {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// TestUpstreamMaskerShortHitIsFullyMasked 钉住上游 stringx.Masker 的
// 「短值整段全量打码」保证（上游 v0.17.0，issue #31）。
//
// # 这条是**依赖契约测试**，不是本仓实现测试
//
// 本仓的脱敏安全性建立在一条上游保证上：**Keep 档不会把短命中拼回原文**。
// v0.17.0 之前不成立：上游在 n <= 2*Keep 时让头尾两段保留位首尾相接、
// 恰好覆盖整段原文，掩码主体 `****` 只是插在中间的装饰——
// 脱敏原语等于没生效（实测 `Mask("01234567")` → `0123«probe:****»4567`）。
//
// 本仓曾为这个缺陷自带过两层兜底（声明式 minLen + 运行期预扫），
// **现已全部撤除**——上游修好后本仓再抄一份就是双源漂移。
// 于是这条保证就成了**本仓依赖的上游契约**，必须由本仓钉住：
// 上游回退、或有人把依赖降级到 v0.16.x，本用例立刻变红。
//
// # 断言挑「可复原性」而不是「查不到东西」
//
// 剥掉形态标签后拼不回原值才算过。这条断言在「有地板」时绿、
// 在「没地板」时红——**能区分**。反过来，拿「查不到东西」当断言
// 在两端都成立，钉不住任何东西。
func TestUpstreamMaskerShortHitIsFullyMasked(t *testing.T) {
	// keep=4、量词放宽到 {4,}：模拟「有人把 long-hex 的 {32,} 改成 {8,}」
	// 这类为了多抓一点的小改。命中长度落在上游的地板之下（4*4=16）。
	m, err := stringx.NewMasker([]stringx.Rule{
		{Name: "long-hex", Re: regexp.MustCompile(`\b[0-9a-fA-F]{4,}\b`), Keep: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{4, 7, 8, 15} {
		value := strings.Repeat("a", n)
		out := m.Mask("句柄 " + value + " 记一下")
		if strings.Contains(out, value) {
			t.Fatalf("%d 位命中被完整暴露: %q", n, out)
		}
		// 复原检测：把形态标签与定界符剥掉，看能不能拼回原值。
		if reconstructed := stripMask(out); strings.Contains(reconstructed, value) {
			t.Fatalf("%d 位命中可被复原（上游的短值全量打码保证未生效）: 掩码=%q 复原=%q",
				n, out, reconstructed)
		}
	}
}

// TestUpstreamMaskerLongHitKeepsLocator 钉住上游的另一半：命中够长时
// **仍保留定位线索**（短值全掩不能把有用信息也抹掉——
// 抹掉之后「我那条凭据呢」就没人能指出是哪一条了）。
//
// 门槛是 n >= Keep*4：16 位起保留首尾各 4 个字符。
func TestUpstreamMaskerLongHitKeepsLocator(t *testing.T) {
	m, err := stringx.NewMasker([]stringx.Rule{
		{Name: "long-hex", Re: regexp.MustCompile(`\b[0-9a-fA-F]{4,}\b`), Keep: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Repeat("a", 16)
	out := m.Mask("句柄 " + value + " 记一下")
	if !strings.Contains(out, "long-hex") {
		t.Fatalf("够长的命中应带形态标签: %q", out)
	}
	if !strings.Contains(out, "aaaa") {
		t.Fatalf("够长的命中应保留首部定位线索: %q", out)
	}
	// 保留位合计必须显著少于原值：留 8 藏 1 等于没藏。
	if kept := len(stripMask(out)); kept >= 2*len(value) {
		t.Fatalf("保留位过多，等于没藏: %q", out)
	}
}
