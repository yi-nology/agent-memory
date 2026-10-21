package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/store"

	// 纯 Go SQLite 驱动（CGO_ENABLED=0 可用；FTS5 已编入，SQLite 3.50.4）。
	_ "modernc.org/sqlite"
)

// Store SQLite 后端。
//
// tx 非空时本实例绑定在事务里——所有方法经 exec/query 路由，于是同一份实现
// 同时服务「直接调用」与「事务内调用」两条路径，不会出现「事务里那条忘了用 tx
// 于是写在事务外」的经典漏洞。
type Store struct {
	// 每张表一个内嵌窄结构。
	//
	// 为什么拆：门禁的 max_methods_per_type=60 不是风格偏好。九个接口平铺在
	// 一个类型上是 73 个方法——而单开后没有一个子结构超过 12 个。扁鹊那边
	// 撞过同一个判决（Scheduler 166 → 49），结论一致：**真正的降数是改接收者
	// 与结构形状**，光把代码搬进不同文件不算。
	//
	// 各自内嵌 *Store 是为了复用 exec/query/queryRow 这三个路由方法；不做这层
	// 内嵌的话每个方法都得写 `s.s.exec(...)`，那才叫噪音。
	rawStore
	atomStore
	candidateStore
	entityStore
	pageStore
	edgeStore
	episodeStore
	journalStore
	metaStore
	vectorStore

	db *sql.DB
	tx *sql.Tx
}

// 各表子结构。方法体与拆之前逐字相同，只是接收者从 Store 换成了对应的表结构，
// 并通过内嵌的 *Store 拿到 exec/query 路由。
type (
	rawStore       struct{ *Store }
	atomStore      struct{ *Store }
	candidateStore struct{ *Store }
	entityStore    struct{ *Store }
	pageStore      struct{ *Store }
	episodeStore   struct{ *Store }
	journalStore   struct{ *Store }
	metaStore      struct{ *Store }
)

var _ store.Store = (*Store)(nil)

// var _ ports.VectorIndex 编译期钉住「本后端直接实现向量索引端口」。
//
// 刻意**不写适配层**：存储层直接实现那个端口，装配面按类型断言取用，
// 省掉一层胶水（见 architecture-policy.yaml 里 storage-sqlite 的说明）。
var _ ports.VectorIndex = (*Store)(nil)

// newStore 造一个全部子结构都回指自身的 Store。
//
// 漏回指一个的后果是该表的方法在调用时 panic（nil 指针）——所以这里用一个
// 循环而不是九行赋值：加表时忘了改这里，编译不过。
func newStore(db *sql.DB) *Store {
	s := &Store{db: db}
	s.rawStore.Store = s
	s.atomStore.Store = s
	s.candidateStore.Store = s
	s.entityStore.Store = s
	s.pageStore.Store = s
	s.edgeStore.Store = s
	s.episodeStore.Store = s
	s.journalStore.Store = s
	s.metaStore.Store = s
	s.vectorStore.Store = s
	return s
}

// Open 按 DSN 打开。
//
// 默认参数是本仓能跑起来的最小集：busy_timeout 让并发写等锁而不是立刻报
// SQLITE_BUSY；foreign_keys 打开（本仓的引用关系靠应用层维护，但打开它能让
// 「引用了不存在的实体」这种写错在测试里立刻暴露）。
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite 失败: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("设置 busy_timeout 失败: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("打开外键失败: %w", err)
	}
	if err := ensureSchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return newStore(db), nil
}

// OpenFile 打开磁盘库（父目录须已存在）。
func OpenFile(path string) (*Store, error) {
	return Open("file:" + path + "?_pragma=journal_mode(WAL)")
}

// OpenMemory 打开内存库，供测试与临时库使用。
//
// 用 shared-cache 的具名内存库并**把连接池压到 1**：`:memory:` 的库随连接生灭，
// 连接池一旦放开就可能拿到一个空库，于是「表不存在」这种错误只在并发下偶发。
// 压到 1 是把那个偶发变成确定——测试要的是确定。
func OpenMemory(name string) (*Store, error) {
	s, err := Open("file:" + name + "?mode=memory&cache=shared")
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	return s, nil
}

// ensureSchema 建表、跑迁移、记版本。
//
// 顺序不能换：先建表（老库的表已存在，IF NOT EXISTS 让它变成空操作），
// 再跑迁移（迁移要 ALTER 老库的表），最后记版本号。
//
// schema_version 记在全局行（namespace=”）上：它描述的是**库**的形状，
// 不是某个记忆库的内容，故不按 namespace 分行。
func ensureSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	cur, err := schemaVersionOf(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= cur {
			continue
		}
		if err := m.apply(ctx, db); err != nil {
			return fmt.Errorf("迁移到 schema v%d 失败: %w", m.version, err)
		}
		if err := setSchemaVersion(ctx, db, m.version); err != nil {
			return err
		}
	}
	return setSchemaVersion(ctx, db, schemaVersion)
}

// Vacuum 物理整理（VACUUM）。
//
// 刻意**不在 store.Store 接口里**：VACUUM 是 SQLite 的概念，PostgreSQL 没有
// 对应物。把它做成可选接口（lifecycle.Vacuumer）而不是塞进 Store，
// 是为了让「本仓支持多个后端」这件事保持为真。
func (s *Store) Vacuum(ctx context.Context) error {
	if _, err := s.exec(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("VACUUM 失败: %w", err)
	}
	return nil
}

// Close 关闭底层连接。
func (s *Store) Close() error {
	if s.tx != nil {
		_ = s.tx.Rollback()
	}
	return s.db.Close()
}

// Transaction 事务边界。
//
// 晋升是「候选落定 + 事实入库 + 实体计数 + 审计条目」四步，中途不回滚会留下
// 「候选已判但事实没进」的空洞——而它在读取侧完全看不出来（只是那条事实凭空
// 消失）。这正是 store.Store 把 Transaction 作为一等公民的原因。
//
// 嵌套调用直接复用外层事务（SQLite 不支持嵌套 BEGIN）。**代价是内层出错不会
// 自动回滚外层**——所以内层 fn 返回错误后，外层 fn 必须立刻返回它。
func (s *Store) Transaction(ctx context.Context, fn func(context.Context, store.Store) error) error {
	if s.tx != nil {
		return fn(ctx, s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开事务失败: %w", err)
	}
	txStore := newStore(s.db)
	txStore.tx = tx
	if err := fn(ctx, txStore); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// ── 路由：事务内外走同一份实现 ──

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if s.tx != nil {
		return s.tx.ExecContext(ctx, q, args...)
	}
	return s.db.ExecContext(ctx, q, args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if s.tx != nil {
		return s.tx.QueryContext(ctx, q, args...)
	}
	return s.db.QueryContext(ctx, q, args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	if s.tx != nil {
		return s.tx.QueryRowContext(ctx, q, args...)
	}
	return s.db.QueryRowContext(ctx, q, args...)
}

// ── 辅助：类型 ↔ 行 ──

func nsOf(ns store.Namespace) string { return string(ns) }

// ts / tsPtr 把时刻存成 Unix 纳秒整数。
//
// 用纳秒而不是秒：ListRaw 的分页游标与 ORDER BY 都按 ts 排，同一秒内写入的多条
// 事件在秒精度下顺序不确定——那会让「倒序取最后 N 条」每次跑出不同结果，
// 而这种不稳定在测试里表现为偶发失败，极难定位。
func ts(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func tsPtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

func fromTS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v).UTC()
}

func fromTSPtr(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromTS(v.Int64)
}

func strPtr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func fromStrPtr(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// stringsJSON 列表列的存取。nil 与空切片都落 "[]"：读回来是 nil 也没关系，
// 存成 "null" 才会让下游的 JSON 解析多一个分支。
func stringsJSON(v []string) string {
	if v == nil {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func parseStrings(s string) []string {
	var out []string
	if s == "" || s == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func mapJSON(v map[string]any) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func parseMap(s string) map[string]any {
	if s == "" || s == "null" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func provenanceJSON(p provenanceFields) string {
	return mapJSON(map[string]any{
		"origin":        string(p.Origin),
		"host":          p.Host,
		"runtime_error": p.RuntimeError,
		"redacted":      p.Redacted,
		"tool":          p.Tool,
	})
}

// provenanceFields 是 provenance 的中间形态（避免辅助函数 import types 造成
// 循环——虽然本仓不存在该循环，但少一个 import 就少一处耦合）。
type provenanceFields = struct {
	Origin       string
	Host         string
	RuntimeError bool
	Redacted     bool
	Tool         string
}

// ftsBody 把多段文本拼成 FTS 单行可索引的正文。
//
// 折叠空白不是为了好看：FTS5 的 unicode61 分词器按空白切，保留换行与连续空格
// 会让词边界错位，中文长句尤其明显。
// ftsBody 造 FTS 索引正文。
//
// 切词走 store.LexicalTokens：**汉字逐字成词元**，英文按空白切。
// 这是中文子串检索能用的关键——unicode61 会把连续汉字算作一个词元，
// 而真实中文文本里没有空格分词，用户查的就是子串。
// 详见 store.LexicalTokens 的注释。
func ftsBody(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(store.LexicalTokens(strings.Join(out, " ")), " ")
}

// ── 辅助：向量的字节形态 ──

// encodeVec 把向量编码成小端 float32 字节序列。
//
// 为什么是二进制而不是 JSON 串：文本往返会丢精度
// （十进制 → float64 → float32，两次舍入），
// 而向量要**逐位可复现**——同一批记忆在不同进程里算出的向量必须逐位相同，
// 否则「召回结果可复现」这条承诺（评测面靠它成立）就破了。
//
// 小端：两个后端统一用它，故跨后端搬库时字节序也一致。
func encodeVec(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(v))
	}
	return buf
}

// decodeVec 解回向量（decodeVec(encodeVec(v)) 与 v 逐位相同）。
func decodeVec(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// vecNorm 向量的模长。
func vecNorm(v []float32) float64 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum)
}

// cosineScore 两个向量的余弦相似度。
//
// 第二个返回值是「能不能比」：零向量没有方向（0/0），
// 比出来的数没有意义，故不能比时返回 false，调用方跳过这一条。
//
// 累加用 float64：float32 累加在长向量上会明显丢精度，
// 而分数是要拿来排序的——排序被噪声带偏，召回次序就跟着漂。
func cosineScore(a, b []float32) (float64, bool) {
	an, bn := vecNorm(a), vecNorm(b)
	if an == 0 || bn == 0 {
		return 0, false
	}
	var dot float64
	for i := range a {
		if i >= len(b) {
			break
		}
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (an * bn), true
}

// ftsQuery 把查询串拼成 FTS5 的 MATCH 串。
//
// 走上游 textutil.FTS5Query（经 store.FTS5Query 钉子）：它把查询串切成词元后
// **逐个加引号**再空格连接。
//
// 加引号是**安全边界**，不是装饰：FTS5 的 AND / OR / NOT / NEAR 是裸词才生效的
// 运算符，用户输入里的 NOT 会被当成「排除后面那个词」而不是「查 NOT 这个词」；
// 引号把它降回字面量，引号同样能让 `"`、`*`、`(` 之类不构成语法错误。
//
// 切不出词元时返回空串——空 MATCH 是 FTS5 的语法错误，调用方据此跳过查询
// 而不是把空串拼上去。
//
// 走 store.FTS5Query 而非直接调上游：查询侧与索引侧（ftsBody → store.LexicalTokens）
// 必须在**同一处**吸收上游切法变化，否则两侧口径分叉 → 静默漏命中。
// 详见 store.FTS5Query 的注释。
func ftsQuery(query string) string {
	return store.FTS5Query(query)
}

// refCol 取 FTS 虚表的引用列名。
func refCol(table string) string {
	switch table {
	case "atom_fts":
		return "atom_id"
	case "raw_fts":
		return "event_id"
	case "candidate_fts":
		return "candidate_id"
	default:
		return "ref"
	}
}

// indexUpsert 覆盖一条 FTS 索引。
//
// 先删后插而不是 UPSERT：FTS5 虚表没有主键约束，UPSERT 需要显式 rowid，
// 而这些表建的时候没给。删+插在同事务里是原子的，效果与 UPSERT 相同。
func (s *Store) indexUpsert(ctx context.Context, table, namespace, ref, body string) error {
	if _, err := s.exec(ctx,
		`DELETE FROM `+table+` WHERE namespace = ? AND `+refCol(table)+` = ?`,
		namespace, ref); err != nil {
		return err
	}
	_, err := s.exec(ctx,
		`INSERT INTO `+table+` (namespace, `+refCol(table)+`, body) VALUES (?, ?, ?)`,
		namespace, ref, body)
	return err
}

func (s *Store) indexDelete(ctx context.Context, table, namespace, ref string) error {
	_, err := s.exec(ctx, `DELETE FROM `+table+` WHERE namespace = ? AND `+refCol(table)+` = ?`,
		namespace, ref)
	return err
}

// searchFTS 走 FTS5 检索，返回按相关度排序的标识。
//
// 查询串**不透传** FTS5 的 MATCH 语法（不支持 "精确短语" / AND / OR / 前缀 *）：
// 用户原文直接拼进 MATCH 就是一处注入面——查询里的引号与运算符会被当成语法。
// 本层无法区分「用户想按语法检索」与「用户输入恰好含语法」，故一律按字面量
// 处理：切词 + 逐词加引号（见 ftsQuery）。
//
// # extra 是附加条件（可选）
//
// 形如 "atom_id IN (SELECT ...)" 的 SQL 片段与它的参数，供**必须在取数层**
// 生效的过滤用（归属过滤，见 atom.go 的 SearchAtoms）：
// 后置过滤会让被滤掉的条目先占掉 LIMIT 配额，于是「该给我的那条」根本没进候选池。
//
// extra 为空串时行为与从前一字不差（这是绝大多数调用点的路径）。
//
// # loose 是**匹配方式**的开关，不是可见性的开关
//
// loose=false：隐式 AND（每个词都要在正文里出现）——严格路。
// loose=true ：命中**任一**词就算（OR）——宽松路。
//
// 两条路的差别**只在词的匹配方式**：namespace 限定、extra 条件（归属 / 活跃 /
// 时序 / 事件窗口 / 实体集合）、排序与 TopK 都是同一套。
// 放宽的是「词的匹配」，**不是「可见性」**——过滤条件少一条的后果是泄漏。
//
// 宽松路的 MATCH 串走 store.FTS5QueryAny（切词口径与索引侧、严格路同源）：
// 自己拼 OR 串的话，两侧切法一旦分叉就是**静默漏命中**。
func (s *Store) searchFTS(ctx context.Context, table, namespace, query string, limit int,
	extra string, loose bool, extraArgs ...any) ([]string, error) {
	match := ftsQuery(query)
	if loose {
		match = store.FTS5QueryAny(query)
	}
	if match == "" {
		// 查询里没有可检索的词元（全是标点/空白）：不是错误，是「没得可查」。
		return nil, nil
	}
	where := "namespace = ? AND " + table + " MATCH ?"
	args := []any{namespace, match}
	if extra != "" {
		where += " AND " + extra
		args = append(args, extraArgs...)
	}
	rows, err := s.query(ctx,
		`SELECT `+refCol(table)+` FROM `+table+`
		 WHERE `+where+` ORDER BY rank LIMIT ?`,
		append(args, limitOf(limit, 10))...)
	if err != nil {
		// MATCH 语法错误不该炸掉整条召回路径——退回空结果，让上层决定要不要兜底。
		//
		// 词元已加引号（见 ftsQuery），这条分支实际上很难再触发；
		// 留着它是纵深防御：FTS5 语法若再扩，这里不会让整条检索路径崩掉。
		if isFTSSyntaxErr(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// isFTSSyntaxErr 判定「是查询语法错」而不是「后端坏了」。
//
// 这是一处**有意的文本匹配**，与本仓「别拿文本做判定」的纪律不冲突，理由是：
// 驱动没有为 FTS5 语法错提供哨兵错误类型，而误判方向是「把后端故障当成用户
// 输出了怪东西」，后果是静默返回空结果——记忆「忘了」某条事实，用户无从察觉。
// 故取保守侧：只认 FTS5 特征串，其他一律当真错误往上抛。
func isFTSSyntaxErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "fts5") || strings.Contains(s, "malformed match")
}

func (s *Store) upsertMeta(ctx context.Context, namespace, key, value string) error {
	_, err := s.exec(ctx,
		`INSERT INTO meta (namespace, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(namespace, key) DO UPDATE SET value = excluded.value`,
		namespace, key, value)
	return err
}

// notFound 统一把 sql.ErrNoRows 翻成 store 哨兵。
//
// 消费方一律 errors.Is 判 store.ErrNotFound——让 sql.ErrNoRows 漏到上层，
// 就等于逼着每个调用点各写一次判断。
// absentOr 区分「查询出错」与「查到了 0 行」。
//
// 这条区分是必须的：`rows.Err()` 在 0 行时是 nil，直接 notFound(nil) 会把
// 「不存在」翻成「成功 + nil 对象」，调用方一解引用就 panic，且报错点在离真正
// 病因十万八千里的地方。
func absentOr(err error) error {
	if err != nil {
		return err
	}
	return store.ErrNotFound
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

func limitOf(limit, def int) int {
	if limit <= 0 {
		return def
	}
	return limit
}
