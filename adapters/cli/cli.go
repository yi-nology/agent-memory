// Package cli 命令行面：给宿主与库管理者一个不进代码就能操作记忆库的口子。
//
// # 形态
//
// `Run(args, stdout, stderr) int` 而不是 `main()`：命令逻辑因此可被单测
// 直接调用，不用起子进程。`cmd/memory` 只是它的一层薄壳。
//
// # 命令面
//
//	capture   捕获一条事件（落库前已打码）
//	recall    召回并渲染
//	search    检索事实卡
//	stats     计数
//	export    导出成 .hmpkg
//	import    导入 .hmpkg
//	doctor    检查 .hmpkg 完整性
//
// 库管理面（见 admin.go）：
//
//	gc           按龄回收原始事件（并顺带退役证据全没的事实）
//	consolidate  合并同签名事实
//	retention    退役证据全来自运行期错误的事实
//	vacuum       物理整理（后端专属）
//	entity       实体归一面的修正：list / merge / alias
//	episode      列出情节
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/pipeline/capture"
	"github.com/yi-nology/agent-memory/pipeline/recall"
	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// Options 全局选项。
type Options struct {
	// DB 记忆库路径（默认 ./memory.sqlite）。
	DB string
	// Namespace 记忆库名。
	Namespace string
	// Host 宿主标识。
	Host string
}

// Run 执行一条命令，返回进程退出码。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd := args[0]
	rest := args[1:]

	// 子命令：entity 的三个子动作。
	//
	// 必须在建 FlagSet **之前**摘出来——flag 包遇到第一个非 flag 参数就停，
	// 「entity merge --to X」里的 merge 会被留在 fs.Args() 里，
	// 后面的 --to/--from 就再也解析不到了。
	sub := ""
	if cmd == "entity" {
		if len(rest) == 0 {
			fmt.Fprintln(stderr, "entity 需要一个子命令：list / merge / alias")
			return 2
		}
		sub = rest[0]
		rest = rest[1:]
	}

	fs := flag.NewFlagSet("memory "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		db        = fs.String("db", "memory.sqlite", "记忆库路径")
		ns        = fs.String("namespace", "default", "记忆库名")
		host      = fs.String("host", "cli", "宿主标识")
		content   = fs.String("content", "", "事件正文")
		eventID   = fs.String("event-id", "", "事件标识（留空自动生成）")
		sessionID = fs.String("session", "", "会话标识")
		threadID  = fs.String("thread", "", "线程标识")
		user      = fs.String("user", "", "用户标识")
		evType    = fs.String("type", string(types.EventManual), "事件类型")
		explicit  = fs.Bool("explicit", false, "显式写入（不受自动捕获判据约束）")
		limit     = fs.Int("limit", 10, "条数上限")
		out       = fs.String("out", "", "导出包路径")
		pkg       = fs.String("pkg", "", "迁移包路径")
		before    = fs.Duration("before", 30*24*time.Hour, "回收保留期（早于此刻多久的原始事件）")
		toID      = fs.String("to", "", "合并目标实体 ID")
		fromID    = fs.String("from", "", "合并来源实体 ID")
		entityID  = fs.String("entity", "", "实体 ID 或规范名")
		alias     = fs.String("alias", "", "要登记的别名")
		entType   = fs.String("entity-type", "", "实体类型（宿主给定）")
	)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	opt := Options{DB: *db, Namespace: *ns, Host: *host}

	switch cmd {
	case "capture":
		return cmdCapture(ctx, opt, stdout, stderr, *eventID, *sessionID, *threadID, *user,
			*evType, *content, *explicit)
	case "recall":
		query := fs.Arg(0)
		if query == "" {
			fmt.Fprintln(stderr, "recall 需要一个查询词")
			return 2
		}
		return cmdRecall(ctx, opt, stdout, stderr, query, *sessionID, *threadID, *limit)
	case "search":
		query := fs.Arg(0)
		if query == "" {
			fmt.Fprintln(stderr, "search 需要一个查询词")
			return 2
		}
		return cmdSearch(ctx, opt, stdout, stderr, query, *limit)
	case "stats":
		return cmdStats(ctx, opt, stdout, stderr)
	case "export":
		return cmdExport(ctx, opt, stdout, stderr, *out)
	case "import":
		return cmdImport(ctx, opt, stdout, stderr, *pkg)
	case "doctor":
		if *pkg == "" {
			fmt.Fprintln(stderr, "doctor 需要 --pkg")
			return 2
		}
		return cmdDoctor(stdout, stderr, *pkg)

	// ── 库管理面 ──
	case "gc":
		return cmdGC(ctx, opt, stdout, stderr, *before)
	case "consolidate":
		return cmdConsolidate(ctx, opt, stdout, stderr)
	case "retention":
		return cmdRetention(ctx, opt, stdout, stderr)
	case "vacuum":
		return cmdVacuum(ctx, opt, stdout, stderr)
	case "entity":
		switch sub {
		case "list":
			return cmdEntityList(ctx, opt, stdout, stderr, *limit)
		case "merge":
			return cmdEntityMerge(ctx, opt, stdout, stderr, *toID, *fromID)
		case "alias":
			return cmdEntityAlias(ctx, opt, stdout, stderr, *entityID, *alias, *entType)
		default:
			fmt.Fprintf(stderr, "entity 未知子命令: %s\n", sub)
			return 2
		}
	case "episode":
		return cmdEpisodeList(ctx, opt, stdout, stderr, *sessionID, *limit)

	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "未知命令: %s\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `memory — 长期记忆内核的命令行面

用法:
  memory capture --content "…" [--session S] [--thread T] [--user U] [--type T] [--explicit]
  memory recall  <查询词> [--session S] [--thread T] [--limit N]
  memory search  <查询词> [--limit N]
  memory stats
  memory export  --out <包路径>
  memory import  --pkg <包路径>
  memory doctor  --pkg <包路径>

库管理面:
  memory gc [--before 720h]              按龄回收原始事件（默认保留 30 天）
  memory consolidate                     合并同签名事实
  memory retention                       退役证据全来自运行期错误的事实
  memory vacuum                          物理整理（后端专属）
  memory entity list [--limit N]         列出实体
  memory entity merge --to ID --from ID  合并两个实体
  memory entity alias --entity ID --alias A [--entity-type T]
  memory episode list [--session S]      列出情节

全局选项:
  --db <路径>        记忆库文件（默认 memory.sqlite）
  --namespace <名>   记忆库名（默认 default）
  --host <标识>      宿主标识（写进事件的来源）
`)
}

// open 打开记忆库并装配服务。
//
// 装配面**不注入模型**：CLI 是看与搬的口子，而记忆的提炼需要模型。
// CLI 的定位是看与搬（捕获/召回/检索/导出导入），不是生成。
func open(ctx context.Context, opt Options) (*application.Service, *sqlite.Store, error) {
	st, err := sqlite.OpenFile(opt.DB)
	if err != nil {
		return nil, nil, err
	}
	svc, err := application.New(application.Config{
		Namespace: store.Namespace(opt.Namespace),
		Host:      opt.Host,
		Recall:    recall.DefaultConfig(),
	}, application.Deps{
		Store:    st,
		Redactor: ports.PatternRedactor{},
		Bander:   ports.MemoryBand{},
	})
	if err != nil {
		_ = st.Close()
		return nil, nil, err
	}
	return svc, st, nil
}

func cmdCapture(ctx context.Context, opt Options, stdout, stderr io.Writer,
	eventID, sessionID, threadID, user, evType, content string, explicit bool) int {

	if content == "" {
		fmt.Fprintln(stderr, "capture 需要 --content")
		return 2
	}
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	if eventID == "" {
		eventID = fmt.Sprintf("cli-%d", time.Now().UnixNano())
	}
	out, err := svc.Capture(ctx, capture.Input{
		Namespace: store.Namespace(opt.Namespace), EventID: eventID,
		Host: opt.Host, SessionID: sessionID, ThreadID: threadID, User: user,
		Type: types.RawEventType(evType), Content: content,
		Provenance: types.Provenance{Origin: types.OriginOperator},
		Explicit:   explicit,
	})
	if err != nil {
		fmt.Fprintln(stderr, "捕获失败:", err)
		return 1
	}
	if !out.Accepted {
		fmt.Fprintf(stdout, "未收录：%s\n", out.SkipReason)
		return 0
	}
	fmt.Fprintf(stdout, "已收录 %s（脱敏=%v）\n", out.Event.ID, out.Redacted)
	return 0
}

func cmdRecall(ctx context.Context, opt Options, stdout, stderr io.Writer,
	query, sessionID, threadID string, limit int) int {

	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	res, err := svc.Recall(ctx, recall.Request{
		Namespace: store.Namespace(opt.Namespace), Query: query,
		SessionID: sessionID, ThreadID: threadID, Limit: limit,
	})
	if err != nil {
		fmt.Fprintln(stderr, "召回失败:", err)
		return 1
	}
	if res.Empty() {
		fmt.Fprintln(stdout, "（无召回）")
		return 0
	}
	fmt.Fprintln(stdout, res.Render())
	fmt.Fprintf(stderr, "命中 %d 条，丢弃 %d 条（预算 %d / 多样性 %d / 重复 %d）\n",
		len(res.Snippets), res.Dropped,
		res.DroppedByBudget, res.DroppedByDiversity, res.DroppedByDuplicate)
	return 0
}

func cmdSearch(ctx context.Context, opt Options, stdout, stderr io.Writer, query string, limit int) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	atoms, err := svc.Search(ctx, store.Namespace(opt.Namespace), query, limit)
	if err != nil {
		fmt.Fprintln(stderr, "检索失败:", err)
		return 1
	}
	for _, a := range atoms {
		fmt.Fprintf(stdout, "- [%s] %s\n", a.ID, a.Assertion)
	}
	fmt.Fprintf(stderr, "命中 %d 条\n", len(atoms))
	return 0
}

func cmdStats(ctx context.Context, opt Options, stdout, stderr io.Writer) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	s, err := svc.Stats(ctx, store.Namespace(opt.Namespace))
	if err != nil {
		fmt.Fprintln(stderr, "取计数失败:", err)
		return 1
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	fmt.Fprintln(stdout, string(b))
	return 0
}

func cmdExport(ctx context.Context, opt Options, stdout, stderr io.Writer, out string) int {
	if out == "" {
		out = portable.FileName(opt.Namespace, time.Now())
	}
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	man, err := portable.ExportFile(ctx, svc.Store(), store.Namespace(opt.Namespace), out,
		portable.ExportOptions{Host: opt.Host})
	if err != nil {
		fmt.Fprintln(stderr, "导出失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已导出 %s：%v\n", out, man.Counts)
	return 0
}

func cmdImport(ctx context.Context, opt Options, stdout, stderr io.Writer, pkgPath string) int {
	if pkgPath == "" {
		fmt.Fprintln(stderr, "import 需要 --pkg")
		return 2
	}
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	rep, err := portable.Import(ctx, pkgPath, svc.Store(), store.Namespace(opt.Namespace))
	if err != nil {
		fmt.Fprintln(stderr, "导入失败:", err)
		return 1
	}
	fmt.Fprintf(stdout, "已导入：%v\n", rep.Counts)
	return 0
}

func cmdDoctor(stdout, stderr io.Writer, pkgPath string) int {
	rep, err := portable.Doctor(pkgPath)
	if err != nil {
		fmt.Fprintln(stderr, "检查失败:", err)
		return 1
	}
	if !rep.OK {
		for _, p := range rep.Problems {
			fmt.Fprintln(stderr, "- "+p)
		}
		return 1
	}
	fmt.Fprintf(stdout, "包完整：%v\n", rep.Manifest.Counts)
	return 0
}

// Main 供 cmd/memory 调用的薄壳。
func Main() {
	os.Exit(Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
