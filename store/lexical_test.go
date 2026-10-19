package store_test

import (
	"strings"
	"testing"

	"github.com/yi-nology/agent-memory/store"
)

// 词元切分是**跨后端契约**：两个后端必须用同一套切法，否则同一串查询在两个
// 后端给出不同结果——而「同一批记忆换个部署形态，召回结果不该变」是本仓的
// 产品承诺。故这层值得有自己的用例。
func TestLexicalTokensSplitsCJKPerRune(t *testing.T) {
	got := store.LexicalTokens("磁盘长期偏小")
	want := []string{"磁", "盘", "长", "期", "偏", "小"}
	if len(got) != len(want) {
		t.Fatalf("汉字应逐字成词元，实得 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个词元应为 %q，实得 %q（全量 %v）", i, want[i], got[i], got)
		}
	}
}

// 下划线是**分隔符**，与 FTS5 的 unicode61 实际分词一致。
//
// 这条曾经不一致：本仓早先的一版把下划线当词内字符（`api_key` 切成一个词元），
// 而 FTS5 的 unicode61 只把字母与数字当词元字符，索引里 `api_key` 实际存成
// `api` + `key` 两个词元——两侧口径对不上。
//
// 之所以一直没出事：FTS5 对加引号的查询串会重新分词，`"api_key"` 在匹配阶段
// 又被切成 `api` + `key`，于是照样命中。那是「碰巧对」，不是「设计对」。
func TestLexicalTokensTreatsUnderscoreAsSeparator(t *testing.T) {
	got := store.LexicalTokens("api_key")
	want := []string{"api", "key"}
	if len(got) != len(want) {
		t.Fatalf("下划线应作分隔符（与 FTS5 unicode61 一致），实得 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个词元应为 %q，实得 %q（全量 %v）", i, want[i], got[i], got)
		}
	}
}

// 切出来的必须是**裸词元**：标点、运算符、引号一律丢弃。
//
// 这是安全边界而非洁癖：FTS5 的 AND / OR / NOT / NEAR 是裸词才生效的运算符，
// 而引号、括号、星号这些字符一旦留在查询串里就是**语法**——要么查询报错，
// 要么语义被改写。裸词元保证查询串里除字母数字汉字外没有别的东西。
//
// 注意区分两件事：
//   - 「AND」这种**运算符词**本身是合法词元，留在词元里**不危险**——
//     因为查询侧给每个词元都加了引号（见 textutil.FTS5Query），
//     加了引号的 AND 就是字面量。安全性由**加引号那一层**保证，
//     不由词元层保证；端到端的那条断言在 storage/sqlite 的用例里。
//   - 真正的语法字符（引号、括号、星号）必须从词元里消失。
func TestLexicalTokensDropSyntaxChars(t *testing.T) {
	cases := []string{
		`"disk is"`,
		`disk*`,
		`(disk`,
		`disk)`,
		`NEAR(disk memory)`,
		`disk AND memory`,
		`NOT disk`,
	}
	for _, in := range cases {
		for _, tok := range store.LexicalTokens(in) {
			for _, r := range tok {
				switch r {
				case '"', '*', '(', ')':
					t.Fatalf("词元里不许留语法字符: 输入 %q 切出 %q", in, tok)
				}
			}
		}
	}
}

// 空串与纯标点切不出词元——空 MATCH 是 FTS5 的语法错误，调用方据此跳过查询。
func TestLexicalTokensEmptyOnNoWords(t *testing.T) {
	for _, in := range []string{"", "   ", "!!!", "???"} {
		if got := store.LexicalTokens(in); len(got) != 0 {
			t.Fatalf("%q 应切不出词元，实得 %v", in, got)
		}
	}
}

// 查询侧与索引侧的词元必须**同一套**——这是「同一批记忆换个部署形态，
// 召回结果不该变」这条产品承诺的机械保证。
//
// 这条断言是 2026-10-20 实弹补的：SQLite 查询侧曾直接调上游 textutil.FTS5Query，
// 绕过了 LexicalTokens 这个钉子。当时两者行为一致（不是当下的错），
// 但上游一旦改 FTS5Query 的切法，就会出现「索引侧走 Tokens、查询侧走新口径」
// → 两侧分叉 → **静默漏命中**（用户看到「明明记过却查不到」）。
//
// 测试层面此前完全看不见这个破口：没有一条断言写「查询侧词元 == 索引侧词元」。
func TestFTS5QueryUsesSameTokensAsIndexSide(t *testing.T) {
	cases := []string{
		"磁盘长期偏小",
		"api_key",
		"disk AND memory",
		"NOT disk",
		"磁盘 清理",
	}
	for _, in := range cases {
		query := store.FTS5Query(in)
		tokens := store.LexicalTokens(in)
		if len(tokens) == 0 {
			if query != "" {
				t.Fatalf("%q 切不出词元时查询串应为空，实得 %q", in, query)
			}
			continue
		}
		// 查询串里的每个词元都应带引号，且词元集合与索引侧一致。
		for _, tok := range tokens {
			quoted := `"` + tok + `"`
			if !strings.Contains(query, quoted) {
				t.Fatalf("查询串应含带引号的词元 %s，实际 %q（输入 %q）", quoted, query, in)
			}
		}
		// 反向：查询串里不该出现索引侧没有的词元
		for _, field := range strings.Fields(query) {
			trimmed := strings.Trim(field, `"`)
			if trimmed == "" {
				continue
			}
			found := false
			for _, tok := range tokens {
				if tok == trimmed {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("查询串出现了索引侧没有的词元 %q（输入 %q，索引侧 %v）", trimmed, in, tokens)
			}
		}
	}
}
