package ports_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/ports"
)

// 本文件：排序端口的用例。

// TestLexicalRankerMatchesLegacyFormula 默认打分器与打分器这个端口出现之前的
// 公式**逐条等价**。
//
// 这条钉的是「默认语义零漂移」：注入缝是给宿主换口径用的，
// 宿主**不注入**时的行为必须与从前一字不差。
//
// 故这里把旧公式在测试里**重新写一遍**当期望值：
// 复制的那份若与实现漂了，这条用例就红——
// 而它漂了意味着「默认排序悄悄变了」，那是本仓最不该发生的一类变化。
func TestLexicalRankerMatchesLegacyFormula(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cands := []ports.RankCandidate{
		{RefID: "a1", Body: "磁盘长期偏小需要清理旧文件", ImportanceRank: 3,
			OccurredAt: now.Add(-time.Hour)},
		{RefID: "a2", Body: "内存占用偏高", ImportanceRank: 2,
			OccurredAt: now.Add(-72 * time.Hour)},
		{RefID: "a3", Body: "磁盘占用很高", ImportanceRank: 1,
			OccurredAt: now.Add(-40 * 24 * time.Hour)},
		{RefID: "a4", Body: "", ImportanceRank: 0, OccurredAt: time.Time{}},
	}
	query := "磁盘 内存"

	got, err := ports.LexicalRanker{}.Score(context.Background(), query, cands, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cands) {
		t.Fatalf("分数条数应与候选数一致，实得 %d vs %d", len(got), len(cands))
	}
	for i, c := range cands {
		want := 0.6*legacyLexical(c.Body, legacyTokenize(query)) +
			0.25*float64(c.ImportanceRank)/3 +
			0.15*legacyRecency(c.OccurredAt, now)
		if math.Abs(got[i]-want) > 1e-9 {
			t.Fatalf("候选 %s 的分数漂了：期望 %v 实得 %v", c.RefID, want, got[i])
		}
	}
}

// TestLexicalRankerWeightsAreConfigurable 权重可调，且「三项全 0」不是有意义的配置。
//
// 全零时用缺省权重；给了非零的权重就按给的算（其中的 0 是字面的 0）——
// 否则「不要新鲜度」这种最常见的调法没法表达。
func TestLexicalRankerWeightsAreConfigurable(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cands := []ports.RankCandidate{
		{RefID: "a1", Body: "磁盘偏小", ImportanceRank: 3, OccurredAt: now},
		{RefID: "a2", Body: "内存偏高", ImportanceRank: 1, OccurredAt: now},
	}
	// 只要词面：重要度与新鲜度的权重都压成 0。
	got, err := ports.LexicalRanker{
		Weights: ports.NewRankWeights(1, 0, 0),
	}.Score(context.Background(), "磁盘", cands, now)
	if err != nil {
		t.Fatal(err)
	}
	// a1 命中查询词、a2 没命中 → 只看词面时 a1 必须在前。
	if got[0] <= got[1] {
		t.Fatalf("只按词面打分时应是 a1 在前，实得 %v", got)
	}
	// 全零权重 = 缺省权重（不是「全部不算」）。
	def, err := ports.LexicalRanker{}.Score(context.Background(), "磁盘", cands, now)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := ports.LexicalRanker{
		Weights: ports.NewRankWeights(0, 0, 0),
	}.Score(context.Background(), "磁盘", cands, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range def {
		if math.Abs(def[i]-zero[i]) > 1e-9 {
			t.Fatalf("全零权重应等于缺省权重: %v vs %v", def, zero)
		}
	}
}

// ── 旧公式（当期望值用）──

func legacyLexical(body string, terms []string) float64 {
	if len(terms) == 0 || body == "" {
		return 0
	}
	hit := 0
	for _, t := range terms {
		if strings.Contains(body, t) {
			hit++
		}
	}
	return float64(hit) / float64(len(terms))
}

func legacyRecency(at, now time.Time) float64 {
	if at.IsZero() {
		return 0
	}
	age := now.Sub(at)
	switch {
	case age <= 0:
		return 1
	case age >= 30*24*time.Hour:
		return 0
	default:
		return 1 - age.Seconds()/(30*24*3600)
	}
}

func legacyTokenize(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ',' || r == '，' ||
			r == '?' || r == '？' || r == '!' || r == '！' || r == '。' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
