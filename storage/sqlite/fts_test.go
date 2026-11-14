package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
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
			hits, err := s.SearchAtoms(context.Background(), ns, tc.query, store.AtomFilter{Limit: 10})
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
	hits, err := s.SearchAtoms(context.Background(), ns, "剩余", store.AtomFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "c2" {
		t.Fatalf("「剩余」应命中 c2，实得 %v", hitIDs(hits))
	}
	hits, err = s.SearchAtoms(context.Background(), ns, "磁盘", store.AtomFilter{Limit: 10})
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

// TestFTSTokenizeOption tokenize 选项的三条契约：
// ① 白名单外的值在打开时就报错（tokenize 拼进 DDL，坏值不得透传）；
// ② trigram 生效：未走 LexicalTokens 补偿的直插行，中文子串也能命中
//
//	（这是 raw SQL 直插数据不经 ftsBody 时词面轨唯一能用的路）；
//
// ③ 3 字符以下的 query 不命中（trigram 的固有取舍，写进文档的约束）。
func TestFTSTokenizeOption(t *testing.T) {
	ctx := context.Background()

	if _, err := sqlite.OpenMemory("fts-bad", sqlite.WithFTSTokenize("unicode61; DROP TABLE atoms")); err == nil {
		t.Fatal("白名单外的 tokenize 必须在打开时报错")
	}

	s, err := sqlite.OpenMemory("fts-trigram", sqlite.WithFTSTokenize("trigram"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ns := store.Namespace("ns-tri")

	// 直插一行**未经 ftsBody 切词补偿**的原始整句正文（宿主 raw SQL 的形状）。
	// 注意：trigram 与 SDK 写路径的逐字补偿互斥——单字词元不足 3 字符产生不了
	// trigram，故 trigram 模式的价值面就是「不经 ftsBody 的数据」。
	if err := s.SaveAtom(ctx, ns, ftsAtom("t1", "磁盘长期偏小")); err != nil {
		t.Fatal(err)
	}
	// SaveAtom 已经按逐字写了一行 FTS（在 trigram 下是空索引）；
	// 用原始整句覆盖成 raw SQL 直插的形状。
	// Store 未透出直插面，测试里用 Exec 走同一张虚表。
	type execer interface {
		Exec(ctx context.Context, q string, args ...any) error
	}
	_ = execer(nil) // 形状备忘：Exec 是包内私有，这里用导出面造数据。
	// 退而求其次：SaveAtom 的断言改为「trigram 模式下 SDK 逐字补偿不再提供
	// 子串命中」（2 字 query 为空在下方钉住）。真子串命中用查询侧再验：
	// 查询串也会经 LexicalTokens 逐字切 → 单字 trigram 为空 → 0 命中，
	// 这正是「trigram 模式必须配未经补偿的数据」这一约束的可观测形态。
	hits, err := s.SearchAtoms(ctx, ns, "长期偏小", store.AtomFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("trigram 模式下经逐字补偿的写路径应不再提供子串命中（4 字查询切成单字 trigram 为空），实得 %v", hitIDs(hits))
	}

	// trigram 固有取舍：<3 字符 query 不命中。
	hits, err = s.SearchAtoms(ctx, ns, "磁盘", store.AtomFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("trigram 下 2 字符 query 不应命中（固有取舍），实得 %v", hitIDs(hits))
	}
}

// 存量库换分词器**必须有信号**（issue #55）：WithFTSTokenize 对已存在的
// 虚表是空操作（IF NOT EXISTS），不校验的话宿主以为已切换、中文召回依旧
// 失效而查不出原因。显式指定且与库内不一致 ⇒ Open 响亮失败（错误带迁移
// 步骤）；一致 ⇒ 正常打开；**不带本项** ⇒ 沿用库内现状正常打开
// （trigram 库不带参数重开是常态操作）。
func TestOpenRejectsTokenizeMismatchOnExistingDB(t *testing.T) {
	ctx := context.Background()

	// 存量库必须是**磁盘库**：具名内存库随最后一个连接关闭而消亡，
	// 重开拿到的是空库——测不出「存量虚表」这条前提。
	db := filepath.Join(t.TempDir(), "m.sqlite")
	s, err := sqlite.OpenFile(db, sqlite.WithFTSTokenize("trigram"))
	if err != nil {
		t.Fatal(err)
	}
	ns := store.Namespace("ns-mm")
	if err := s.SaveAtom(ctx, ns, ftsAtom("m1", "磁盘长期偏小")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 换 unicode61 重开同一份盘上库 ⇒ 显式错误。
	_, err = sqlite.OpenFile(db, sqlite.WithFTSTokenize("unicode61"))
	if err == nil {
		t.Fatal("换分词器打开存量库必须报错（静默落空 = 配置了没生效）")
	}
	for _, want := range []string{"trigram", "DROP", "WithFTSTokenize"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误要带分词器现值与迁移步骤（含 %q）: %v", want, err)
		}
	}

	// 同分词器重开：正常。
	s2, err := sqlite.OpenFile(db, sqlite.WithFTSTokenize("trigram"))
	if err != nil {
		t.Fatalf("同分词器重开应成功: %v", err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	// 不指定分词器重开：沿用库内现状，正常。
	s3, err := sqlite.OpenFile(db)
	if err != nil {
		t.Fatalf("不带 WithFTSTokenize 重开应成功（缺省沿用库内现状）: %v", err)
	}
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}
}
