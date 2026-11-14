package portable_test

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
)

// 本文件：原生布局 Export 的 **Limit=0 分页取尽**用例（agent-memory#66）。
//
// # 要钉住的是什么
//
// ExportOptions.Limit 的 doc 是「0=不限」，旧实现把 0 兜成 100000——
// 超过十万条的行按取数序**静默截断**（迁移最容不得「少了几条」，与 #61
// 撤掉的形态同族）。判据三条：
//
//  1. **跨页取尽**：Limit=0 + 小页宽下，导出集合与库里全量逐条相等，
//     且翻页参数真的按页推进（不是「一次大 Limit 硬取恰好没截断」的假绿）；
//  2. **能力缺失响亮失败**：存储没有 PagedLister 时，Limit=0 导出报错
//     （与 ExportOctop 同档——不降级成大 Limit 硬取）；
//  3. **显式 cap 不变**：Limit>0 仍是单次取的条数上限（调用方主动要的）。
//
// 走真 SQLite 内存库（fake 不替真后端背书，#51 的教训）；探针只是取数
// 调用的计数器，不伪造任何数据（pagedProbe 复用 octop_paged_test.go）。

// TestExportUnlimitedExhaustsAcrossPages 跨页取尽：Limit=0 下 >页宽条数
// 的导出集合与全量逐相等，且翻页序列真的按页推进。
func TestExportUnlimitedExhaustsAcrossPages(t *testing.T) {
	ctx := context.Background()
	ns := store.Namespace("paged")

	src, err := sqlite.OpenMemory("native-unlimited-src")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	pagedSeed(t, src)
	probe := &pagedProbe{Store: src}

	const pageWidth = 5
	var buf bytes.Buffer
	man, err := portable.Export(ctx, probe, ns, &buf,
		portable.ExportOptions{Host: "unlimited-test", Limit: 0, PageSize: pageWidth})
	if err != nil {
		t.Fatal(err)
	}

	// ① seed 的七类各 12 条全部在包里（少一条即截断）；episodes/digests
	//    未播种 = 0（MaxInt 路径不截断也不凭空造数）。
	for _, k := range []string{"raw", "candidates", "entities", "atoms", "aliases", "pages", "journal"} {
		if got := man.Counts[k]; got != 12 {
			t.Fatalf("%s 应导出 12 条（页宽 %d 跨页取尽），实得 %d", k, pageWidth, got)
		}
	}
	if man.Counts["episodes"] != 0 || man.Counts["digests"] != 0 {
		t.Fatalf("未播种的 episodes/digests 应为 0: episodes=%d digests=%d",
			man.Counts["episodes"], man.Counts["digests"])
	}

	// ② 翻页真的按页推进了——12 条 / 页宽 5 ⇒ 每表 3 次取数，
	//    offset 序列 0/5/10（末页 2 条 < 页宽即止）。
	wantSeq := [][2]int{{0, pageWidth}, {pageWidth, pageWidth}, {2 * pageWidth, pageWidth}}
	if !reflect.DeepEqual(probe.candPages, wantSeq) ||
		!reflect.DeepEqual(probe.pagePages, wantSeq) ||
		!reflect.DeepEqual(probe.jrnlPages, wantSeq) {
		t.Fatalf("三张表的翻页序列应为 %v：candidates=%v pages=%v journal=%v",
			wantSeq, probe.candPages, probe.pagePages, probe.jrnlPages)
	}
	if len(probe.rawPages) != 3 {
		t.Fatalf("raw_events 应分 3 页取尽，实得 %d 页: %+v", len(probe.rawPages), probe.rawPages)
	}
	if len(probe.atomPages) != 3 {
		t.Fatalf("atoms 应分 3 页取尽（store.ListAllAtoms），实得 %d 页: %+v", len(probe.atomPages), probe.atomPages)
	}
	// entities 取一遍（aliases 复用已取尽的 ents，不二次翻页——与 octop
	// 导出的「aliases 单独再取一遍」形态不同，native 的别名循环吃 ents）。
	if !reflect.DeepEqual(probe.entPages, wantSeq) {
		t.Fatalf("entities 翻页序列应为 %v，实得 %v", wantSeq, probe.entPages)
	}
}

// TestExportExplicitLimitStillCaps 显式 Limit>0 保持单次取的 cap 语义
// （#66 只撤「0 被兜成 100000」，不改调用方主动要的上限）。
func TestExportExplicitLimitStillCaps(t *testing.T) {
	ctx := context.Background()
	ns := store.Namespace("paged")

	src, err := sqlite.OpenMemory("native-cap-src")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	pagedSeed(t, src)

	var buf bytes.Buffer
	man, err := portable.Export(ctx, src, ns, &buf,
		portable.ExportOptions{Host: "cap-test", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got := man.Counts["raw"]; got != 5 {
		t.Fatalf("显式 Limit=5 应截到 5 条，实得 %d", got)
	}
}

// TestExportUnlimitedRejectsUnpagedSource 能力缺失响亮失败：Limit=0 且
// 存储没有 PagedLister ⇒ 报错点名 PagedSource，不降级成大 Limit 硬取
// （与 ExportOctop 同档——那正是 #66 要撤的冒充全量）。
func TestExportUnlimitedRejectsUnpagedSource(t *testing.T) {
	var buf stubWriter
	_, err := portable.Export(context.Background(), &stubSource{},
		store.Namespace("stub"), &buf, portable.ExportOptions{Limit: 0})
	if err == nil {
		t.Fatal("没有 PagedLister 能力时 Limit=0 导出必须报错")
	}
	if !strings.Contains(err.Error(), "PagedSource") {
		t.Fatalf("报错须点名 PagedSource（教宿主怎么补），实得: %v", err)
	}
}
