package portable

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：与 octop-memory（Python）的 .hmpkg **互通**。
//
// # 前提核过一遍（2026-10-21）：两侧的包格式**并不相同**
//
// 同的是扩展名（.hmpkg）与版本号（1），**布局完全不同**：
//
//	octop（Python）  manifest.json + data.jsonl.gz
//	                 记录行 {"v":1,"table":"atoms","data":{…}}
//	本仓（Go）       manifest.json + 10 个分类 .jsonl
//	                 记录体直接铺平（无信封）
//
// 而两侧的**记录体字段几乎逐字段一致**（id / entity_id / candidate_id /
// raw_event_ids / assertion / verbatim_quote / quote_event_id / search_terms /
// occurred_at / confidence / importance …）——同一设计的两个实现，
// 本仓是超集（多 scope / kind / valid_from / valid_to / version）。
//
// ⇒ 互通要补的有三样：**信封**、**表名**、**字段名与取值**。
//
// # 字段映射表（契约）——**逐字段对照**，不是机械翻译
//
// ⚠️ **原先这里是机械翻译**（Go 字段名 → snake_case ✓），
// 看上去两侧就通了 ✓——**但那是假的通** ✓。
// 两侧的模型是「同一个设计的两个实现」✓，**不是同一个模型** ✓，
// 有一批字段**两边的名字本来就不一样** ✓。
//
// 对照的来源是对方仓库的**源码** ✓（不是文档、不是推测 ✓）：
//   `src/octop_memory/types.py`（dataclass 字段名 = 对方的导出面 ✓）
//   `src/octop_memory/operations/migration/import_.py`（`_build_*` = 对方的读入面 ✓）
//
// 名字对不上的那几处：
//
//	本仓 Type     ↔ octop event_type       （raw_events）
//	本仓 Type     ↔ octop candidate_type   （candidates）
//	本仓 Body     ↔ octop summary_markdown （entity_pages）
//	本仓 AtomIDs  ↔ octop topics           （entity_pages）
//	本仓 At       ↔ octop timestamp        （journal）
//	本仓 Kind     ↔ octop action           （journal）
//	本仓 Summary  ↔ octop note             （journal）
//
// ⚠️ 而对方的读端是**按名字取**的 ✓（`d["event_type"]` ✓），
// 取不到就抛 `missing required field` ✓ ⇒ **那一整张表都导不进去** ✓。
//
// ⚠️ 更糟的是它**不响** ✓：对方的导入器把这类错误收进 summary.errors ✓
// 然后 `continue` ✓ ⇒ 宿主看到的是「导入成功，0 条」✓。
//
// ⇒ 所以这一层是**逐字段对照出来的** ✓（见 octopmap.go ✓），
// 并且导出时**自检对方必填的字段** ✓（缺一个就在本仓这边先红 ✓）。
//
// **表名**对照（octop 8 张 ↔ 本仓 9 类）：
//
//	raw_events ↔ raw_events        candidates ↔ candidates
//	entities   ↔ entities          atoms      ↔ atoms
//	aliases    ↔ aliases           entity_pages ↔ pages
//	journal    ↔ journal
//	（本仓多出且 octop 没有的：episodes 情节、digests 周期摘要
//	  ⇒ 导出时跳过，读入时为空 ✓；反向 octop 多出的
//	  thread_active_entities（活跃实体栈）本仓没有对应表
//	  ⇒ 读入时跳过 ✓，导出时本仓也不写它 ✓）
//
// ⚠️ **本仓多出的字段**（scope / kind / valid_from / valid_to / version）在
// octop 那边**没有** ⇒ 导出过去时会被丢弃（octop 的模型里没有它们）✓；
// 反过来从 octop 读入时，这些字段取零值 ✓（= 全局 / 默认类别 / 无区间）✓
// —— 即「老数据语义不变」那条纪律在跨仓时同样成立 ✓。
//
// ⚠️⚠️ **带归属的记录不导出**（scope ≠ 空 ⇒ 整条跳过 ✓）：
// 归属在对方那边**没有对应字段** ✓ ⇒ 导过去就变成**全局可见** ✓
// ⇒ 那是**内容泄漏** ✓，不是「少了个字段」✓ ⇒ 宁可少，不可漏 ✓。
//
// ⚠️ **键名对不上的失效形态**：Go 的 json.Unmarshal 找不到对应字段时
// **静默丢掉**那个键 ⇒ 「导入成功但内容全空」✓，极难归因 ✓。
// 故这里做**显式对照** ✓，而不是指望对方迁就本仓的 Go 字段名 ✓。
//
// # 键名为什么也要补
//
// 本仓的领域类型**没有 json tag** ⇒ 序列化出来是 Go 字段名（EntityID），
// 而 octop 那边是 snake_case（entity_id）。
// 键名对不上时 Go 的 json.Unmarshal **不会**报错（它按字段名匹配，
// 找不到就**丢掉**那个键）——于是「导入成功」但内容是空的，
// 而症状是「导入后库里什么都没有」，极难归因。
//
// 故这里做**显式翻译**（机械规则，不是逐字段手抄）：
//
//	Go 字段名 → snake_case：按大写边界切词，连续大写算一个词（ID / URL…）
//	snake_case → Go 字段名：按 "_" 切 + 首字母大写 + 缩写还原（id → ID）
//
// # 为什么本仓侧要写这个（而不是让对方适配）
//
// octop 那边**已经是那个格式的作者** ✓：它的读端现成 ✓。
// 本仓侧多写一个布局 = 对方零改动 ✓。
// 反过来（让对方适配本仓的布局）要改的是**已经能用的代码** ✗ —— 方向不对。

// octop 侧的格式常量。
const (
	// OctopPkgVersion octop 侧 manifest 的 pkg_version。
	OctopPkgVersion = 1
	// OctopExportVersion octop 侧记录行的 v（导出版本）。
	OctopExportVersion = 1
	// octopDataFile octop 侧的数据文件名（gzip 压缩的 JSONL 流）。
	octopDataFile = "data.jsonl.gz"
	// octopManifestFile 两边同名的清单文件。
	octopManifestFile = "manifest.json"
)

// OctopManifest octop 侧的 manifest 形状。
//
// 字段名与 octop-memory 的 packer.py 一一对应（那边写什么，这里读什么）✓。
type OctopManifest struct {
	PkgVersion      int            `json:"pkg_version"`
	ExportVersion   int            `json:"export_version"`
	SourceHostKind  string         `json:"source_host_kind"`
	SourceNamespace string         `json:"source_namespace"`
	SourceDBPath    string         `json:"source_db_path"`
	AgentName       string         `json:"agent_name"`
	PackedAt        string         `json:"packed_at"`
	ToolVersion     string         `json:"tool_version"`
	SchemaVersion   string         `json:"schema_version"`
	RowCounts       map[string]int `json:"row_counts"`
	TotalRows       int            `json:"total_rows"`
	// SkippedScoped 因带归属而**没有导出**的记录条数。
	//
	// 为什么要有这一栏：带归属的记录在 octop 那边**没有对应字段** ✓
	// ⇒ 导过去就变成全局可见 ✓ ⇒ 那是**内容泄漏** ✓ ⇒ 所以不导 ✓。
	//
	// ⚠️ 但「不导」必须**看得见** ✓ —— 不然宿主看到的是
	// 「导出成功、少了几条」✓，而它并不知道少的是哪几条、为什么少 ✓。
	SkippedScoped int `json:"skipped_scoped,omitempty"`
}

// octopTableNames 表名映射：本仓的每类记录 → octop 侧的表名。
//
// octop 有 8 张表，本仓有 9 类 —— 本仓多的两类（episodes 情节、digests 周期摘要）
// 在 octop 那边**没有对应表** ⇒ 导出时跳过 ✓（并在文档里写明 ✓），
// 反过来读 octop 的包时这两类自然为空 ✓。
var octopTableNames = []struct {
	Go     string
	Octop  string
	Export func(ctx context.Context, src Source, ns store.Namespace) ([]any, error)
}{
	{"raw_events", "raw_events", func(ctx context.Context, src Source, ns store.Namespace) ([]any, error) {
		evs, err := src.ListRaw(ctx, ns, "", time.Time{}, store.Page{Limit: 1000000})
		if err != nil {
			return nil, err
		}
		return toAny(evs), nil
	}},
	{"candidates", "candidates", func(ctx context.Context, src Source, ns store.Namespace) ([]any, error) {
		cs, err := src.ListCandidates(ctx, ns, store.CandidateFilter{Limit: 1000000})
		if err != nil {
			return nil, err
		}
		return toAny(cs), nil
	}},
	{"entities", "entities", func(ctx context.Context, src Source, ns store.Namespace) ([]any, error) {
		es, err := src.ListEntities(ctx, ns, "", 1000000)
		if err != nil {
			return nil, err
		}
		return toAny(es), nil
	}},
	{"atoms", "atoms", func(ctx context.Context, src Source, ns store.Namespace) ([]any, error) {
		as, err := src.ListAtoms(ctx, ns, store.AtomFilter{Limit: 1000000})
		if err != nil {
			return nil, err
		}
		return toAny(as), nil
	}},
	{"aliases", "aliases", func(ctx context.Context, src Source, ns store.Namespace) ([]any, error) {
		es, err := src.ListEntities(ctx, ns, "", 1000000)
		if err != nil {
			return nil, err
		}
		var out []any
		for _, e := range es {
			as, err := src.ListAliases(ctx, ns, e.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, toAny(as)...)
		}
		return out, nil
	}},
	{"entity_pages", "entity_pages", func(ctx context.Context, src Source, ns store.Namespace) ([]any, error) {
		ps, err := src.ListEntityPages(ctx, ns, 1000000)
		if err != nil {
			return nil, err
		}
		return toAny(ps), nil
	}},
	{"journal", "journal", func(ctx context.Context, src Source, ns store.Namespace) ([]any, error) {
		js, err := src.ListJournal(ctx, ns, "", 1000000)
		if err != nil {
			return nil, err
		}
		return toAny(js), nil
	}},
}

// toAny 把一组指针转成 []any（便于统一处理）。
func toAny[T any](items []*T) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	return out
}

// ExportOctop 按 **octop 的布局**导出一个包（对方零改动即可读）。
//
// 返回的是 octop 形状的清单 ✓。
func ExportOctop(ctx context.Context, src Source, ns store.Namespace, w io.Writer,
	opt ExportOptions) (*OctopManifest, error) {
	if src == nil {
		return nil, fmt.Errorf("portable: 源为空")
	}
	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	gz, err := zw.Create(octopDataFile)
	if err != nil {
		return nil, fmt.Errorf("portable: 建 %s 失败: %w", octopDataFile, err)
	}
	gzw := gzip.NewWriter(gz)

	counts := map[string]int{}
	total := 0
	skippedScoped := 0
	for _, t := range octopTableNames {
		rows, err := t.Export(ctx, src, ns)
		if err != nil {
			return nil, err
		}
		spec, ok := octopTables[t.Octop]
		if !ok {
			// 契约里没有的表 ⇒ 不导出（而不是原样塞过去 ✓）。
			// 原样塞过去的后果是「对方按名字取、取不到 ⇒ 整表跳过」✓，
			// 而宿主看到的是「导入成功、0 条」✓ ⇒ 静默 ✓。
			continue
		}
		for _, row := range rows {
			body, err := json.Marshal(row)
			if err != nil {
				return nil, fmt.Errorf("portable: 序列化 %s 记录失败: %w", t.Go, err)
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, fmt.Errorf("portable: 解析 %s 记录失败: %w", t.Go, err)
			}
			// 键名与取值：走**逐字段对照**出来的契约 ✓（octopmap.go ✓），
			// 不是机械翻译 ✓ —— 两边的模型不是同一个模型 ✓。
			data, keep := spec.To(m)
			if !keep {
				// 带归属的记录不导出 ✓（导出去会变成全局可见 = 泄漏 ✓）。
				// 但要**记账** ✓ —— 不然宿主不知道少了几条、也不知道为什么 ✓。
				skippedScoped++
				continue
			}
			// 自检：对方必填的字段缺一个 ⇒ 那一整表在对方那边都导不进去 ✓。
			// ⇒ 在**本仓这边**先红 ✓，而不是等对方那边「导入成功但 0 条」✓。
			if missing := checkOctopRequired(t.Octop, data); len(missing) > 0 {
				return nil, fmt.Errorf("portable: %s 记录缺对方必填字段 %v（导出会在对方那边整表失败）",
					t.Octop, missing)
			}
			env := map[string]any{
				"v":     OctopExportVersion,
				"table": t.Octop,
				"data":  data,
			}
			line, err := json.Marshal(env)
			if err != nil {
				return nil, fmt.Errorf("portable: 序列化 %s 记录失败: %w", t.Go, err)
			}
			if _, err := gzw.Write(append(line, '\n')); err != nil {
				return nil, fmt.Errorf("portable: 写 %s 失败: %w", t.Go, err)
			}
			counts[t.Octop]++
			total++
		}
	}
	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("portable: 关闭 gzip 失败: %w", err)
	}

	// 打包时刻：优先用注入的时钟 ✓。
	//
	// 为什么必须可注入：黄金样本（跨仓对账用的那个固定包 ✓）
	// 要求**逐字节可复现** ✓ —— 而清单里带着真实时钟的话，
	// 每导一次就差一行 ⇒ 那个包每次都在变 ⇒ 「固定样本」名不副实 ✓。
	//
	// ⚠️ 这里原先直接用 time.Now()，而原生布局的导出（Export ✓）
	// 用的是注入时钟（opt.Now ✓）—— 同一条链上两套口径，
	// 于是「导出可复现」这条性质只对一半的布局成立 ✗。
	packedAt := opt.Now
	if packedAt.IsZero() {
		packedAt = time.Now()
	}

	man := &OctopManifest{
		PkgVersion:      OctopPkgVersion,
		ExportVersion:   OctopExportVersion,
		SourceHostKind:  "agent-memory",
		SourceNamespace: string(ns),
		AgentName:       opt.Host,
		PackedAt:        packedAt.UTC().Format(time.RFC3339),
		ToolVersion:     "agent-memory/operations/portable",
		RowCounts:       counts,
		TotalRows:       total,
		SkippedScoped:   skippedScoped,
	}
	blob, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("portable: 序列化清单失败: %w", err)
	}
	f, err := zw.Create(octopManifestFile)
	if err != nil {
		return nil, fmt.Errorf("portable: 建 %s 失败: %w", octopManifestFile, err)
	}
	if _, err := f.Write(blob); err != nil {
		return nil, fmt.Errorf("portable: 写 %s 失败: %w", octopManifestFile, err)
	}
	return man, nil
}

// ExportOctopFile 把 octop 布局的包写到文件。
func ExportOctopFile(ctx context.Context, src Source, ns store.Namespace, filePath string,
	opt ExportOptions) (*OctopManifest, error) {
	f, err := os.Create(filePath)
	if err != nil {
		return nil, fmt.Errorf("portable: 建包失败: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ExportOctop(ctx, src, ns, f, opt)
}

// IsOctopPackage 判断一个包是不是 octop 布局。
//
// 判据：**有没有 data.jsonl.gz** ✓。
// （不是看 manifest 的键 —— 那需要先解压清单；
// 而 zip 的中央目录可以直接列 ✓。）
func IsOctopPackage(pkgPath string) (bool, error) {
	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		return false, fmt.Errorf("portable: 打开包失败: %w", err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if f.Name == octopDataFile {
			return true, nil
		}
	}
	return false, nil
}

// ImportOctop 导入一个 **octop 布局**的包。
//
// 记录体的键名是 snake_case ⇒ 先翻译回 Go 字段名再反序列化 ✓
// （不翻译的后果是「导入成功但内容全空」✓ —— Go 的 json 会**静默丢掉**对不上的键 ✓）。
func ImportOctop(ctx context.Context, pkgPath string, tgt Target, ns store.Namespace) (*ImportReport, error) {
	if tgt == nil {
		return nil, fmt.Errorf("portable: 目标未装配")
	}
	zr, err := zip.OpenReader(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("portable: 打开包失败: %w", err)
	}
	defer func() { _ = zr.Close() }()

	var dataFile *zip.File
	for _, f := range zr.File {
		if f.Name == octopDataFile {
			dataFile = f
			break
		}
	}
	if dataFile == nil {
		return nil, fmt.Errorf("portable: 包里没有 %s", octopDataFile)
	}
	rc, err := dataFile.Open()
	if err != nil {
		return nil, fmt.Errorf("portable: 打开 %s 失败: %w", octopDataFile, err)
	}
	defer func() { _ = rc.Close() }()
	gzr, err := gzip.NewReader(rc)
	if err != nil {
		return nil, fmt.Errorf("portable: 解压 %s 失败: %w", octopDataFile, err)
	}
	defer func() { _ = gzr.Close() }()

	rep := &ImportReport{Counts: map[string]int{}}
	dec := json.NewDecoder(gzr)
	for {
		var env struct {
			V     int             `json:"v"`
			Table string          `json:"table"`
			Data  json.RawMessage `json:"data"`
		}
		if err := dec.Decode(&env); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("portable: 解析记录失败: %w", err)
		}
		if env.V != OctopExportVersion {
			return nil, fmt.Errorf("portable: 记录版本 %d 不认识（本仓只认 %d）", env.V, OctopExportVersion)
		}
		n, err := importOctopRow(ctx, env.Table, env.Data, tgt, ns)
		if err != nil {
			return nil, err
		}
		rep.Counts[env.Table] += n
	}
	return rep, nil
}

// importOctopRow 导入一条记录。
//
// 表名 → 本仓的类型；键名 snake → Go 字段名。
func importOctopRow(ctx context.Context, table string, data json.RawMessage, tgt Target,
	ns store.Namespace) (int, error) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, fmt.Errorf("portable: 解析 %s 记录失败: %w", table, err)
	}
	// 键名与取值：走**逐字段对照**出来的契约 ✓（octopmap.go ✓）。
	//
	// ⚠️ 原先这里是机械翻译（snake_case → PascalCase ✓），
	// 而两边的模型**不是同一个模型** ✓ ⇒ 有一批字段翻译过去就找不到 ✓
	// （`event_type` → `EventType` ≠ 本仓的 `Type` ✓）⇒ 那条记录的内容就丢了 ✓，
	// 而症状是「导入成功、内容为空」✓。
	spec, ok := octopTables[table]
	if !ok {
		// 不认识的表：跳过，不报错。
		//
		// 理由：本仓比 octop 多的两类（episodes / digests）、
		// 以及未来对方新增的表，都不该让**整包导入失败**——
		// 丢一表好过丢一整包。
		return 0, nil
	}
	body, err := json.Marshal(spec.From(m))
	if err != nil {
		return 0, fmt.Errorf("portable: 序列化 %s 记录失败: %w", table, err)
	}
	switch table {
	case "raw_events":
		var v types.RawEvent
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, fmt.Errorf("portable: 解析 raw_events 失败: %w", err)
		}
		return 1, tgt.SaveRaw(ctx, ns, &v)
	case "candidates":
		var v types.Candidate
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, fmt.Errorf("portable: 解析 candidates 失败: %w", err)
		}
		return 1, tgt.SaveCandidate(ctx, ns, &v)
	case "entities":
		var v types.Entity
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, fmt.Errorf("portable: 解析 entities 失败: %w", err)
		}
		return 1, tgt.SaveEntity(ctx, ns, &v)
	case "atoms":
		var v types.AtomCard
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, fmt.Errorf("portable: 解析 atoms 失败: %w", err)
		}
		return 1, tgt.SaveAtom(ctx, ns, &v)
	case "aliases":
		var v types.Alias
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, fmt.Errorf("portable: 解析 aliases 失败: %w", err)
		}
		return 1, tgt.SaveAlias(ctx, ns, &v)
	case "entity_pages":
		var v types.EntityPage
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, fmt.Errorf("portable: 解析 entity_pages 失败: %w", err)
		}
		return 1, tgt.UpsertEntityPage(ctx, ns, &v)
	case "journal":
		var v types.JournalEntry
		if err := json.Unmarshal(body, &v); err != nil {
			return 0, fmt.Errorf("portable: 解析 journal 失败: %w", err)
		}
		return 1, tgt.AppendJournal(ctx, ns, &v)
	default:
		// 上面已按契约表拦掉了不认识的表 ✓ ⇒ 这里到不了 ✓。
		// 保留分支是为了「将来加了新表但忘了写转换器」时不至于崩 ✓。
		return 0, nil
	}
}
