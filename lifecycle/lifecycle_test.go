package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/lifecycle"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

var ns = store.Namespace("lc")

func now() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }

func seed(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.OpenMemory("lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func atom(id, entity, assertion, evidence string, runtimeErr bool) *types.AtomCard {
	return &types.AtomCard{
		ID: id, EntityID: entity, Assertion: assertion, QuoteEventID: evidence,
		RawEventIDs: []string{evidence}, Importance: types.ImportanceMed,
		Confidence: types.ConfidenceMed, OccurredAt: now(), CreatedAt: now(), UpdatedAt: now(),
		Provenance: types.Provenance{Origin: types.OriginModel, RuntimeError: runtimeErr},
	}
}

// 回收：原始事件按龄删除。
func TestGCDeletesOldRaw(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "old", Content: "很久以前", Timestamp: now().Add(-48 * time.Hour), Type: types.EventManual})
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "new", Content: "刚刚", Timestamp: now(), Type: types.EventManual})

	res, err := lifecycle.GC(ctx, s, ns, now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.RawDeleted != 1 {
		t.Fatalf("应删 1 条，实得 %d", res.RawDeleted)
	}
	if _, err := s.GetRaw(ctx, ns, "old"); err == nil {
		t.Fatal("旧事件不该还在")
	}
	if _, err := s.GetRaw(ctx, ns, "new"); err != nil {
		t.Fatalf("窗口外的事件不该被删: %v", err)
	}
}

// 回收：证据全被清掉的事实要退役（它成了一条无法回溯的断言）。
func TestGCRetiresAtomsWhoseEvidenceVanished(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Content: "旧素材", Timestamp: now().Add(-48 * time.Hour), Type: types.EventManual})
	_ = s.SaveAtom(ctx, ns, atom("a1", "ent1", "某条事实", "e1", false))

	res, err := lifecycle.GC(ctx, s, ns, now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.AtomsRetired != 1 {
		t.Fatalf("证据全没了的事实应退役，实得 %d", res.AtomsRetired)
	}
	got, err := s.GetAtom(ctx, ns, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Active() {
		t.Fatal("退役的事实不该再参与召回")
	}
}

// 证据还在的事实**不能**被退役。
func TestGCKeepsAtomsWithLiveEvidence(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveRaw(ctx, ns, &types.RawEvent{
		ID: "e1", Content: "素材", Timestamp: now(), Type: types.EventManual})
	_ = s.SaveAtom(ctx, ns, atom("a1", "ent1", "某条事实", "e1", false))

	if _, err := lifecycle.GC(ctx, s, ns, now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAtom(ctx, ns, "a1")
	if !got.Active() {
		t.Fatal("证据还在的事实不该被退役：误判「查不了」的代价比多查几次大得多")
	}
}

// 合并：同签名事实归到一张，其余软退役（不硬删）。
func TestConsolidateMergesSameSignature(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, atom("a1", "ent1", "磁盘长期偏小", "e1", false))
	_ = s.SaveAtom(ctx, ns, atom("a2", "ent1", "磁盘长期偏小", "e2", false))

	res, err := lifecycle.Consolidate(ctx, s, ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Merged != 1 {
		t.Fatalf("应合并掉 1 张，实得 %d", res.Merged)
	}
	// 断言「恰好一张活跃、另一张指向它」，而不断言具体是哪张：
	// 保留哪一张由「创建更早者胜、同刻按 ID 决胜」决定，
	// 但**测试不该把那个决胜规则复述一遍**——它一变，测试就该变，
	// 而它没变时测试也不该因为遍历顺序而红。
	var live, retired *types.AtomCard
	for _, id := range []string{"a1", "a2"} {
		got, err := s.GetAtom(ctx, ns, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Active() {
			live = got
		} else {
			retired = got
		}
	}
	if live == nil || retired == nil {
		t.Fatalf("应恰好一张活跃一张退役，实得 live=%v retired=%v", live, retired)
	}
	if retired.SupersededBy != live.ID {
		t.Fatalf("退役必须指向继任者: retired=%s live=%s", retired.SupersededBy, live.ID)
	}
	if len(live.RawEventIDs) != 2 {
		t.Fatalf("证据应合并到存活的那张: %+v", live.RawEventIDs)
	}
}

// 保留期：退役「证据全部来自运行期错误」的事实。
func TestRetentionRetiresRuntimeErrorAtoms(t *testing.T) {
	s := seed(t)
	ctx := context.Background()
	_ = s.SaveAtom(ctx, ns, atom("a-bad", "ent1", "合成出来的错误", "e1", true))
	_ = s.SaveAtom(ctx, ns, atom("a-good", "ent1", "真实发生的事", "e2", false))

	res, err := lifecycle.Retention(ctx, s, ns)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retired != 1 {
		t.Fatalf("应退役 1 条，实得 %d", res.Retired)
	}
	bad, _ := s.GetAtom(ctx, ns, "a-bad")
	if bad.Active() {
		t.Fatal("运行期错误来源的事实应退役")
	}
	good, _ := s.GetAtom(ctx, ns, "a-good")
	if !good.Active() {
		t.Fatal("正常来源的事实不该被退役")
	}
}

// 压缩：后端专属能力，不支持的必须明说而不是静默跳过。
func TestVacuumOnUnsupportedBackend(t *testing.T) {
	err := lifecycle.Vacuum(context.Background(), struct{}{})
	if err == nil {
		t.Fatal("不支持的后端必须明确报错：静默跳过会让人以为压过了")
	}
}

// SQLite 后端支持压缩。
func TestVacuumOnSQLite(t *testing.T) {
	s := seed(t)
	if err := lifecycle.Vacuum(context.Background(), s); err != nil {
		t.Fatalf("SQLite 应支持 VACUUM: %v", err)
	}
}
