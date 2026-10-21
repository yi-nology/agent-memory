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
//	extract   从原始事件提炼候选（**要模型**）
//	promote   判定候选并落成事实（**要模型**）
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

	"github.com/yi-nology/agent-memory/adapters/llm"
	"github.com/yi-nology/agent-memory/adapters/mcp"
	"github.com/yi-nology/agent-memory/application"
	"github.com/yi-nology/agent-memory/operations/portable"
	"github.com/yi-nology/agent-memory/pipeline/capture"
	"github.com/yi-nology/agent-memory/pipeline/query"
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
	// Scope 记录级归属（空=全局）。
	//
	// 捕获时定下（事件带着它走，候选与事实卡从它继承），
	// 召回与检索时是读者归属（只看得见「全局 + 同归属」）。
	// 一条命令一个含义：**写**的时候是「这条属于谁」，
	// **读**的时候是「我要看谁的」。
	Scope types.Scope
	// Model 模型接入配置（**按需**：只有要模型的命令才用得上）。
	Model ModelOptions
}

// ModelOptions 模型接入配置。
//
// 从环境变量读，故命令行不必带密钥（密钥进命令行会进 shell 历史与进程表）。
type ModelOptions struct {
	// BaseURL 端点根地址（环境变量 MEMORY_LLM_BASE_URL）。
	BaseURL string
	// APIKey 密钥（环境变量 MEMORY_LLM_API_KEY）。
	APIKey string
	// Model 模型名（环境变量 MEMORY_LLM_MODEL）。
	Model string
	// Timeout 单次请求超时（环境变量 MEMORY_LLM_TIMEOUT）。
	Timeout time.Duration
	// TimeoutError 时长配置的原值与解析失败原因。
	//
	// 为什么必须保留这个字段：**解析失败不能静默回落**。
	// 用户写 MEMORY_LLM_TIMEOUT=300（想 300 秒，漏了单位），
	// 静默回落成 60 秒的后果是「请求莫名其妙卡 60 秒就断」，
	// 而配置里写着 300——**配错的值不报错，返回一个看起来完全正常的结果**。
	// 这类静默失效正是本仓反复强调要消灭的形态。
	TimeoutError string
}

// Configured 报告模型配置是否齐备。
func (m ModelOptions) Configured() bool {
	return m.BaseURL != "" && m.Model != ""
}

// modelEnv 模型配置的环境变量名。
const (
	envLLMBaseURL = "MEMORY_LLM_BASE_URL"
	envLLMAPIKey  = "MEMORY_LLM_API_KEY"
	envLLMModel   = "MEMORY_LLM_MODEL"
	envLLMTimeout = "MEMORY_LLM_TIMEOUT"
)

// readModelEnv 从环境变量读模型配置。
//
// 时长解析失败**保留原值与原因**（记在 TimeoutError），由调用方显式报出——
// 不静默回落。理由见 ModelOptions.TimeoutError 的说明。
func readModelEnv() ModelOptions {
	m := ModelOptions{
		BaseURL: os.Getenv(envLLMBaseURL),
		APIKey:  os.Getenv(envLLMAPIKey),
		Model:   os.Getenv(envLLMModel),
	}
	if t := os.Getenv(envLLMTimeout); t != "" {
		d, err := time.ParseDuration(t)
		switch {
		case err != nil:
			m.TimeoutError = fmt.Sprintf("%s=%q 不是合法的时长（如 30s / 5m）：%v",
				envLLMTimeout, t, err)
		case d <= 0:
			m.TimeoutError = fmt.Sprintf("%s=%q 必须为正时长", envLLMTimeout, t)
		default:
			m.Timeout = d
		}
	}
	return m
}

// Run 执行一条命令，返回进程退出码。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunWithIO(ctx, args, os.Stdin, stdout, stderr)
}

// RunWithIO 与 Run 相同，但**显式**给入参（含 stdin）。
//
// 为什么要多一个：MCP over stdio 是「宿主拉起一个子进程、逐行对话」，
// 所以它得读 stdin —— 而 Run 的签名里没有它 ✓。
// 把 stdin 做成参数而不是直接用 os.Stdin，是为了**能单测** ✓
// （起一个真进程做协议往返的测试，既慢又脆 ✓）。
func RunWithIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
		eventDays = fs.Int("days", 0, "事件类事实的保留期（天；0=缺省 30 天）")
		toID      = fs.String("to", "", "合并目标实体 ID")
		fromID    = fs.String("from", "", "合并来源实体 ID")
		entityID  = fs.String("entity", "", "实体 ID 或规范名")
		alias     = fs.String("alias", "", "要登记的别名")
		entType   = fs.String("entity-type", "", "实体类型（宿主给定）")
		scope     = fs.String("scope", "", "记录级归属（空=全局；写=这条属于谁，读=我要看谁的）")
	)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	opt := Options{DB: *db, Namespace: *ns, Host: *host, Scope: types.Scope(*scope),
		Model: readModelEnv()}

	switch cmd {
	case "capture":
		return cmdCapture(ctx, opt, stdout, stderr, *eventID, *sessionID, *threadID, *user,
			*evType, *content, *explicit)
	case "extract":
		return cmdExtract(ctx, opt, stdout, stderr, *sessionID, *limit)
	case "promote":
		return cmdPromote(ctx, opt, stdout, stderr, *entityID, *limit)
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
	case "mcp":
		// MCP over stdio：拉起后**一直跑着**（与其它命令跑完就退不同 ✓）。
		return cmdMCP(ctx, opt, stdin, stdout, stderr)
	case "query":
		// 查询理解的自查口：把一句问句翻成「可执行的检索条件」并打印。
		//
		// 它不进库也不召回，纯诊断——宿主配了查询理解之后，
		// 「为什么召回少了」的第一个问题就是「这句被理解成了什么」，
		// 而答案不该靠猜。
		q := fs.Arg(0)
		if q == "" {
			fmt.Fprintln(stderr, "query 需要一句问句")
			return 2
		}
		return cmdQuery(ctx, stdout, stderr, q)
	case "stats":
		return cmdStats(ctx, opt, stdout, stderr)
	case "reindex":
		// 向量索引补算：存量事实没有向量时，语义轨等于失灵
		//（索引里没有它们 → 语义路一条都捞不到）。
		return cmdReindex(ctx, opt, stdout, stderr, *limit)
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
	case "expire":
		// 按龄过期**事件类**事实（保留期缺省 30 天）。
		return cmdExpire(ctx, opt, stdout, stderr, time.Duration(*eventDays)*24*time.Hour)
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
  memory capture  --content "…" [--session S] [--thread T] [--user U] [--type T] [--explicit]
  memory extract  [--session S] [--limit N]        从原始事件提炼候选（要模型）
  memory promote  [--entity ID] [--limit N]        判定候选并落成事实（要模型）
  memory recall   <查询词> [--session S] [--thread T] [--limit N]
  memory search   <查询词> [--limit N]
  memory query    <问句>                        把一句问句翻成检索条件（诊断用，不入库）
  memory reindex  [--limit N]                   给存量事实补算向量（语义轨缺它就失灵）
  memory stats
  memory export   --out <包路径>
  memory import   --pkg <包路径>
  memory doctor   --pkg <包路径>

库管理面:
  memory gc [--before 720h]              按龄回收原始事件（默认保留 30 天）
  memory consolidate                     合并同签名事实
  memory retention                       退役证据全来自运行期错误的事实
  memory expire [--days N]                按龄退役事件类事实（缺省 30 天）
  memory vacuum                          物理整理（后端专属）
  memory entity list [--limit N]         列出实体
  memory entity merge --to ID --from ID  合并两个实体
  memory entity alias --entity ID --alias A [--entity-type T]
  memory episode list [--session S]      列出情节

全局选项:
  --db <路径>        记忆库文件（默认 memory.sqlite）
  --namespace <名>   记忆库名（默认 default）
  --host <标识>      宿主标识（写进事件的来源）

模型接入（extract / promote 需要；从环境变量读，密钥不进命令行）:
  MEMORY_LLM_BASE_URL   端点根地址，如 http://127.0.0.1:8000/v1
  MEMORY_LLM_API_KEY    密钥（可留空，表示端点不校验）
  MEMORY_LLM_MODEL      模型名
  MEMORY_LLM_TIMEOUT    单次请求超时（默认 60s）
`)
}

// cmdExtract 从原始事件提炼候选。
//
// 提炼**不落库**（见 extractor.Extract 的契约）：候选先给调用方看，
// 落库与否由调用方决定。这条纪律在 CLI 上也遵守——
// 「提取出来的东西直接进库」会把模型的幻觉直接写进长期记忆。
func cmdExtract(ctx context.Context, opt Options, stdout, stderr io.Writer,
	sessionID string, limit int) int {

	if !opt.Model.Configured() {
		fmt.Fprintf(stderr, "extract 需要模型：请设置 %s 与 %s\n", envLLMBaseURL, envLLMModel)
		return 2
	}
	if opt.Model.TimeoutError != "" {
		// 配置写错了就说清楚，不让它静默回落成默认值。
		fmt.Fprintln(stderr, "模型配置有误:", opt.Model.TimeoutError)
		return 2
	}
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	ns := store.Namespace(opt.Namespace)
	// 归属不隔离（传空值）：提炼要覆盖库里全部事件，
	// 每条事件带自己的归属，候选与事实卡从它继承。
	evs, err := svc.Store().ListRaw(ctx, ns, "", time.Time{}, store.Page{Limit: limit})
	if err != nil {
		fmt.Fprintln(stderr, "读原始事件失败:", err)
		return 1
	}
	if len(evs) == 0 {
		fmt.Fprintln(stdout, "（没有原始事件，先 capture）")
		return 0
	}
	cands, err := svc.Extract(ctx, ns, sessionID, evs)
	if err != nil {
		fmt.Fprintln(stderr, "提炼失败:", err)
		return 1
	}
	// 候选要落库才能晋升（promotion.Promote 的契约：候选必须已在库里，
	// 因为「候选落定」那一步要改候选状态，候选不在库里时整笔晋升会回滚）。
	//
	// 注意这与「提炼不落库」并不矛盾：那条纪律说的是**内核的提炼器**
	// 不替调用方决定落库与否；而 CLI 是调用方，它在这里**看过结果后**
	// 主动落库——落的是候选，不是事实。事实仍要经过 promote 的五道检查。
	saved := 0
	for _, c := range cands {
		if err := svc.Store().SaveCandidate(ctx, ns, c); err != nil {
			fmt.Fprintf(stderr, "存候选 %s 失败: %v\n", c.ID, err)
			continue
		}
		saved++
		fmt.Fprintf(stdout, "- [%s] %s\n", c.ID, c.Assertion)
	}
	// 报**实际存成功数**而不是提炼数：两者不等时（如存库失败），
	// 报提炼数会让「库里存了几个」看不出来——而那正是下一步要用的事实。
	fmt.Fprintf(stderr, "从 %d 条事件提炼出 %d 条候选，已存库 %d 条（用 promote 判定）\n",
		len(evs), len(cands), saved)
	return 0
}

// cmdPromote 判定候选并落成事实。
func cmdPromote(ctx context.Context, opt Options, stdout, stderr io.Writer,
	candidateID string, limit int) int {

	if !opt.Model.Configured() {
		fmt.Fprintf(stderr, "promote 需要模型：请设置 %s 与 %s\n", envLLMBaseURL, envLLMModel)
		return 2
	}
	if opt.Model.TimeoutError != "" {
		fmt.Fprintln(stderr, "模型配置有误:", opt.Model.TimeoutError)
		return 2
	}
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	ns := store.Namespace(opt.Namespace)
	// 只判待判定的候选：已判过的再判一次会撞乐观锁
	//（UpdateCandidateStatus 带 expected 防并发双判）。
	cands, err := svc.Store().ListCandidates(ctx, ns, store.CandidateFilter{
		Statuses: []types.CandidateStatus{types.CandidatePending},
		Limit:    limit,
	})
	if err != nil {
		fmt.Fprintln(stderr, "读候选失败:", err)
		return 1
	}
	if len(cands) == 0 {
		fmt.Fprintln(stdout, "（没有候选，先 extract）")
		return 0
	}
	promoted := 0
	for _, c := range cands {
		if candidateID != "" && c.ID != candidateID {
			continue
		}
		verdict, err := svc.Promote(ctx, ns, c)
		if err != nil {
			fmt.Fprintf(stderr, "判定 %s 失败: %v\n", c.ID, err)
			continue
		}
		promoted++
		fmt.Fprintf(stdout, "- [%s] %s → %s\n", c.ID, c.Assertion, verdict.Decision)
	}
	if promoted == 0 {
		fmt.Fprintln(stderr, "没有候选被判定")
		return 1
	}
	fmt.Fprintf(stderr, "判定 %d 条候选\n", promoted)
	return 0
}

// open 打开记忆库并装配服务。
//
// 模型是**按需注入**的：未配置模型时服务照常装配（未配置模型的端口是
// ports.NoopLLM），只是要模型的命令会显式失败并说清原因。
//
// # 为什么模型从「一律不注入」改成「按需注入」
//
// 原先 CLI 一律不注入模型，理由是「CLI 是看与搬的口子，不是生成口」。
// 这条理由在**搬**的那半边成立，在**看**的那半边不成立：
//
//   - 记忆的完整链路是「捕获 → 提炼 → 晋升 → 召回」，四步里前两步要模型。
//     没有模型时，CLI 只能捕获、只能召回，而库里**永远不会长出事实卡**——
//     recall 永远召回不到东西，而用户看到的是「（无召回）」，
//     没有任何迹象表明「是缺模型」而不是「是没东西可记」。
//     这是最贵的一种易用性缺口：**能力实现了却没有入口，且失败形态是静默的**。
//   - 提早在装配期暴露：模型没配好时，第一次 extract 就报「未配置端点」，
//     比在生产里发现「记了三个月什么都没记住」便宜三个数量级。
//
// 故改为按需注入：需要模型的动作显式要求配置，其余动作零影响。
func open(ctx context.Context, opt Options) (*application.Service, *sqlite.Store, error) {
	st, err := sqlite.OpenFile(opt.DB)
	if err != nil {
		return nil, nil, err
	}
	var model ports.LLM
	if opt.Model.Configured() {
		mc, err := llm.New(llm.Config{
			BaseURL: opt.Model.BaseURL,
			APIKey:  opt.Model.APIKey,
			Model:   opt.Model.Model,
			Timeout: opt.Model.Timeout,
		})
		if err != nil {
			_ = st.Close()
			return nil, nil, err
		}
		model = mc
	}
	svc, err := application.New(application.Config{
		Namespace: store.Namespace(opt.Namespace),
		Host:      opt.Host,
		Recall:    recall.DefaultConfig(),
	}, application.Deps{
		Store: st,
		// 脱敏与防伪是**必需**依赖（缺了就是事故），故这里给的是真实现。
		Redactor: ports.PatternRedactor{},
		Bander:   ports.MemoryBand{},
		LLM:      model,
		// 向量轨：CLI 用**零依赖**的哈希嵌入（不用模型），
		// 索引由存储自己提供（跟着记忆库一起活）。
		//
		// 它捕捉的是词面软匹配，**不是语义相似度**——
		// 命令行这一档要的是「能跑、零外部依赖」，不是「效果最好」。
		Embedder: ports.HashEmbedder{},
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
		Scope:      opt.Scope,
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
		Scope: opt.Scope,
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

	atoms, err := svc.Search(ctx, store.Namespace(opt.Namespace), query, store.AtomFilter{
		Limit: limit,
		Scope: opt.Scope,
	})
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

// cmdReindex 给存量事实补算向量。
//
// # 为什么需要它
//
// 向量是**写路径**产生的：晋升成事实时顺手写一条。
// 于是**存量**事实（写路径接上之前入库的）没有向量——
// 而语义召回靠的是索引，索引里没有它们，就一条都捞不到。
// 症状是「语义轨像是没生效」，而查不出原因（因为它确实在跑，只是索引是空的）。
//
// # 它是幂等的
//
// 按事实卡 ID 覆盖写：跑第二遍结果一样。
func cmdReindex(ctx context.Context, opt Options, stdout, stderr io.Writer, limit int) int {
	_, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	// 经接口断言取能力：后端**可能**没有向量索引面（那是可选能力，不是必需依赖）。
	var cap any = st
	vi, ok := cap.(ports.VectorIndex)
	if !ok {
		fmt.Fprintln(stderr, "这个后端没有向量索引面，补算不了")
		return 1
	}
	emb := ports.HashEmbedder{}
	atoms, err := st.ListAtoms(ctx, store.Namespace(opt.Namespace), store.AtomFilter{Limit: limit})
	if err != nil {
		fmt.Fprintln(stderr, "取事实卡失败:", err)
		return 1
	}
	n := 0
	for _, a := range atoms {
		vec, err := emb.Embed(ctx, a.Assertion)
		if err != nil {
			fmt.Fprintln(stderr, "向量化失败:", err)
			return 1
		}
		if err := vi.Upsert(ctx, string(store.Namespace(opt.Namespace)), a.ID, string(a.Scope), vec); err != nil {
			fmt.Fprintln(stderr, "写向量失败:", err)
			return 1
		}
		n++
	}
	fmt.Fprintf(stdout, "已补算 %d 条事实的向量\n", n)
	return 0
}

// cmdMCP 以 stdio 跑 MCP 服务器。
//
// # 为什么要有这个入口
//
// MCP over stdio 的形态是「宿主拉起一个子进程、逐行说 JSON-RPC」，
// 所以它需要的是**一个能一直跑着的进程**——与其它命令（跑一次就退）形态不同 ✓。
//
// # 谁会用它
//
// OpenClaw 这类宿主**直接认 MCP** ✓：配置里声明一个命令即可 ✓，
// 不需要写 Node 插件 ✓（那是别人的运行时形状决定的，不是协议要求 ✓）。
func cmdMCP(ctx context.Context, opt Options, stdin io.Reader, stdout, stderr io.Writer) int {
	svc, st, err := open(ctx, opt)
	if err != nil {
		fmt.Fprintln(stderr, "打开失败:", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	srv, err := mcp.New(svc)
	if err != nil {
		fmt.Fprintln(stderr, "装配 MCP 服务器失败:", err)
		return 1
	}
	if err := srv.Run(ctx, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "MCP 服务器退出:", err)
		return 1
	}
	return 0
}

// cmdQuery 解析一句问句，打印出「可执行的检索条件」。
//
// # 它为什么值得单开一个命令
//
// 查询理解是**静默**的：宿主配了它之后，召回结果就变了，
// 而「变了」这件事没有任何输出。
// 于是「召回怎么变了的」只能靠对比召回结果去猜——
// 而召回结果的变化有太多可能的原因（时间窗？实体？词面？预算？）。
// 这个命令把那一步**显式化**：一句问句进去，翻译结果出来。
func cmdQuery(ctx context.Context, stdout, stderr io.Writer, text string) int {
	analyzer := query.New(query.Config{})
	plan := analyzer.Analyze(ctx, query.Input{Query: text})
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "序列化失败:", err)
		return 1
	}
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
