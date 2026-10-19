package ports

import (
	"regexp"
	"strings"
	"testing"

	"github.com/oekit/pkg/stringx"
)

// stripMask 剥掉掩码的形态标签与定界符，只留可见字符。
//
// 用途：检测「短命中是否可复原」——把 `0123«long-hex:****»4567` 剥成
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

// redactWithShortHit 走「量词被改短」场景下的掩码路径。
//
// 构造一条 keep=4、量词放宽到 {4,} 的规则（模拟有人把 long-hex 的
// {32,} 改成 {8,} 这类「为了多抓一点」的小改），然后调同一条
// maskStructural 路径。
//
// 关键：它走的是**生产代码同一条路径**（maskStructuralWith），
// 不是另写一份模拟逻辑——模拟逻辑的测试钉不住生产代码。
func redactWithShortHit(t *testing.T, value string) string {
	t.Helper()
	patterns := []structuralPattern{
		{
			name:   "long-hex",
			re:     regexp.MustCompile(`\b[0-9a-fA-F]{4,}\b`),
			keep:   4,
			minLen: 32, // 声明仍写 32 —— 构造期校验照样过（这正是那个洞）
		},
	}
	rules := make([]stringx.Rule, 0, len(patterns))
	for _, p := range patterns {
		rules = append(rules, stringx.Rule{Name: p.name, Re: p.re, Keep: p.keep})
	}
	m, err := stringx.NewMasker(rules)
	if err != nil {
		t.Fatal(err)
	}
	return maskStructuralWith("句柄 "+value+" 记一下", patterns, m)
}
