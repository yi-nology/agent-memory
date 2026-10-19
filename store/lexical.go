package store

import (
	"strings"
	"unicode"
)

// LexicalTokens 把查询串与可检索正文切成同一套词元。
//
// # 存在的理由：中文子串检索
//
// FTS5 的 unicode61 分词器把连续汉字算作**一个词元**。于是索引里的
// 「磁盘长期偏小」是一个词元，查询「磁盘」是另一个词元，两者永不相等 →
// 0 命中。而真实中文文本里没有空格分词，用户查的就是**子串**——
// 结果是检索面对中文完全失效，且**静默**返回空集（用户看到的是「库里没有这条」）。
//
// 汉字逐字成词元后，「磁盘」→「磁 盘」，FTS5 的空格分词语义（隐式 AND）
// 把它变成「同时含这两个字」的检索。实测（SQLite 3.50.4）2 字中文查询
// 「磁盘」「清理」「占用」均正常命中。
//
// # 为什么放在 store 而不是各后端
//
// 两个后端必须用**同一套**切法，否则同一串查询在两个后端给出不同结果——
// 而「同一批记忆换个部署形态，召回结果不该变」是本仓的产品承诺
// （见 storage/parity 的存在理由）。规则落在这里，两个后端都依赖它，漂移无处发生。
//
// # 语义边界（写清楚，免得被当成连续子串匹配）
//
//   - 切出来的是**词的合取**，不是连续子串：
//     「长期偏」能命中「磁盘长期偏小」，即便三者不连成一片。
//   - 英文按空白与标点切词，原样保留大小写
//     （英文分词本就是 FTS5 的强项，不要动它）。
//   - 只产出**裸词元**：字母、数字、下划线与汉字。标点、运算符、引号一律丢弃，
//     故任意用户输入都不会造成检索语法错误，更不会被当成 NOT 条件。
//     代价是**不支持短语检索**（`"disk is"` 会被当成两个词的合取）。
func LexicalTokens(s string) []string {
	var out []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			out = append(out, word.String())
			word.Reset()
		}
	}
	for _, r := range s {
		switch {
		case isCJKRune(r):
			// 汉字逐字成词元。
			flush()
			out = append(out, string(r))
		case isWordRune(r):
			word.WriteRune(r)
		default:
			// 标点与空白是切分点，也顺带把运算符与引号清掉了。
			flush()
		}
	}
	flush()
	return out
}

// isCJKRune 判定是否为需要逐字切分的表意文字。
//
// 范围覆盖：CJK 统一表意文字、扩展 A、兼容表意文字、假名、谚文。
// 不含日文假名与谚文之外的其它文字（如西里尔、阿拉伯）——
// 那些文字本身以空格分词，交给 FTS5 的 unicode61 处理即可。
func isCJKRune(r rune) bool {
	switch {
	case r >= 0x3400 && r <= 0x4DBF: // CJK 扩展 A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK 统一表意文字
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK 兼容表意文字
		return true
	case r >= 0x3040 && r <= 0x30FF: // 假名
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // 谚文音节
		return true
	}
	return false
}

// isWordRune 判定是否属于「词内字符」。
//
// 用 unicode 包而不是 ASCII 判定：拉丁字母带重音的形态、
// 西里尔文、希腊文都该算词内字符，它们按空格分词，FTS5 处理得很好。
func isWordRune(r rune) bool {
	if r == '_' {
		return true
	}
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
