package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// FTS5 的查询串是**注入面**：MATCH 的参数直接进 FTS5 的查询解析器，
// 而 AND / OR / NOT / NEAR 是裸词才生效的运算符，`(`、`)`、`*` 是语法字符。
//
// 本层的处置是「切词 + 逐词加引号」：查询串不透传 FTS5 语法，用户的原文
// 一律当字面量。
//
// # 这条用例怎么才能真的测到东西
//
// 两个坑叠在一起：
//
//  1. 后端对 MATCH 语法错是**吞掉**的（退回空结果，见 searchFTS 的注释
//     「MATCH 语法错误不该炸掉整条召回路径」）；
//  2. 于是「加引号」与「没加引号但碰巧语法错」两种情形的可观测结果
//     **都是 0 条**——用「查不到东西」当断言，等于没断言。
//
// 故这里只用**能区分两种情形**的查询：库里存着字面量 "and" / "not" / "or"
// 的那几条。加引号时它们是字面量、正常命中；不加引号时它们是运算符、
// 整条查询语法错、被吞成 0 条。
//
// 这不是造出来的人工场景：用户查「and」这种词本身是常事，
// 而「明明有却查不到」是记忆系统最难受的失效形态。
func TestFTS5QueryTreatsUserInputAsLiteral(t *testing.T) {
	s := openFresh(t)
	ns := store.Namespace("ns-op")

	if err := s.SaveAtom(context.Background(), ns, ftsAtom("a-and", "disk and memory")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(context.Background(), ns, ftsAtom("a-not", "disk not memory")); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAtom(context.Background(), ns, ftsAtom("a-or", "disk or memory")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		query string
		// wantCount 期望命中的条数。
		wantCount int
		// wantID 期望命中的那一条的标识（wantCount==1 时有意义）。
		wantID string
	}{
		// 保留字当字面量：加引号时是三个词元，命中对应那一条；
		// 不加引号时是 FTS5 的运算符，整条查询语法错 → 0 条。
		{"AND 当字面量", "AND disk memory", 1, "a-and"},
		{"NOT 当字面量", "NOT disk memory", 1, "a-not"},
		{"OR 当字面量", "OR disk memory", 1, "a-or"},
		// 完整词元：加引号后照常命中——证明上面三条不是「查什么都查不到」。
		{"完整词元", "disk", 3, ""},
		// 语法字符：不得让查询报错，也不得被当成语法。
		// 括号是**分隔符**（切词时丢掉），于是剩下普通词元照常命中——
		// 命中三条正说明括号没被当成 FTS5 的分组语法。
		{"左括号", "(disk", 3, ""},
		{"右括号", "disk)", 3, ""},
		{"双引号", `"disk"`, 3, ""},
		// 纯符号：切不出词元 → 跳过查询，不是错误。
		{"纯符号", "***", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := s.SearchAtoms(context.Background(), ns, tc.query, 10)
			if err != nil {
				t.Fatalf("查询 %q 不得报错: %v", tc.query, err)
			}
			if len(hits) != tc.wantCount {
				t.Fatalf("查询 %q 应命中 %d 条，实得 %d 条（%v）",
					tc.query, tc.wantCount, len(hits), hitIDs(hits))
			}
			if tc.wantID != "" && hits[0].ID != tc.wantID {
				t.Fatalf("查询 %q 应命中 %s，实得 %v", tc.query, tc.wantID, hitIDs(hits))
			}
		})
	}
}

// 中文子串检索必须命中**真实形态**的中文正文。
//
// 索引侧的切词走 store.LexicalTokens（汉字逐字成词元），查询侧走
// textutil.FTS5Query（逐词加引号）——两侧口径必须一致，否则中文查询
// 静默返回空集。这条用例钉住「两侧都在场」这件事。
func TestFTS5CJKSubstringSearch(t *testing.T) {
	s := openFresh(t)
	ns := store.Namespace("ns-cjk")

	if err := s.SaveAtom(context.Background(), ns, ftsAtom("c1", "磁盘长期偏小")); err != nil {
		t.Fatal(err)
	}
	// 子串不在词首：中文查询常是词中的一部分（「剩余」「占用」）。
	if err := s.SaveAtom(context.Background(), ns, ftsAtom("c2", "内存剩余很多")); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchAtoms(context.Background(), ns, "剩余", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "c2" {
		t.Fatalf("「剩余」应命中 c2，实得 %v", hitIDs(hits))
	}
	hits, err = s.SearchAtoms(context.Background(), ns, "磁盘", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "c1" {
		t.Fatalf("「磁盘」应命中 c1，实得 %v", hitIDs(hits))
	}
}

// ftsAtom 造一张最小事实卡。
//
// 检索词面刻意只放**首个词元**：若放整句派生词面，多张卡的正文会互相命中，
// 「只命中相关事实」这条断言就测不出东西了。
func ftsAtom(id, assertion string) *types.AtomCard {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	toks := store.LexicalTokens(assertion)
	terms := []string{}
	if len(toks) > 0 {
		terms = []string{toks[0]}
	}
	return &types.AtomCard{
		ID:            id,
		EntityID:      "e1",
		CandidateID:   "c-" + id,
		RawEventIDs:   []string{"ev-" + id},
		Assertion:     assertion,
		VerbatimQuote: "「" + assertion + "」",
		QuoteEventID:  "ev-" + id,
		SearchTerms:   terms,
		OccurredAt:    at,
		Confidence:    types.ConfidenceHigh,
		Importance:    types.ImportanceHigh,
		CreatedAt:     at,
		UpdatedAt:     at,
		Provenance:    types.Provenance{Origin: types.OriginOperator},
	}
}

func hitIDs(hits []*types.AtomCard) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

// 兜底：openFresh 已在 parity_test.go 里定义，这里引用它以免重复开库逻辑。
var _ = sqlite.OpenMemory
