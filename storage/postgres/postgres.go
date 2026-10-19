// Package postgres 记忆存储的 PostgreSQL 后端。
//
// # 与 SQLite 后端的关系
//
// 两者跑**同一份** storage/parity 断言。这不是形式主义：记忆的行为一致性是产品
// 承诺（「同一批记忆换个部署形态，召回结果不该变」），而两个实现各自的单测都绿
// 并不代表它们行为一致——软退役的过滤条件写漏一列、分页游标用 `<=` 还是 `<`、
// 时间窗闭开区间，这类不一致在各自的单测里全都看不出来。
//
// # 切词规则在契约层，不在方言里
//
// 检索的切词由 **store.LexicalTokens** 定义（汉字逐字成词元、英文按空白切词），
// 两侧都在 Go 侧切好再落库/查询，PG 的 'simple' 词典只负责按空格切——
// 于是两个后端的检索行为逐条一致（见 store.LexicalTokens 的注释）。
//
// # 已知差异面（实现时必须逐条对齐）
//
//	概念        SQLite                        PostgreSQL
//	----------- ----------------------------- ------------------------------
//	命名空间     表内 namespace 列             同左（不用表前缀，见 sqlite.go 头注）
//	布尔         INTEGER 0/1                  BOOLEAN
//	时刻         INTEGER（Unix 纳秒）          TIMESTAMPTZ
//	列表/载荷    TEXT 存 JSON                  同左（用 JSONB 更优，但会让 parity
//	                                          断言的「往返失真」测不到编码问题）
//	全文检索     FTS5 虚表                     tsvector + GIN（PG 原生全文检索）
//
// # 全文检索为什么用 tsvector + 'simple'
//
// PG 自带的分词器（'simple' 之外的那些）对中文要么不支持、要么依赖额外扩展，
// 而本仓的核心场景是中文。'simple' 只按空白与标点切词——
// 而汉字逐字切词这件事已经在 Go 侧做完了（store.LexicalTokens），
// 所以 'simple' 拿到的就是「磁 盘 长 期」这样的空格分隔串，切出来正好是词元。
//
// 查询侧用 plainto_tsquery：它把词元**合取**（AND）起来，
// 与 FTS5 的空格分词语义一致。
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/store"

	// 纯 Go 的 PostgreSQL 驱动（CGO_ENABLED=0 可用；依赖 database/sql）。
	_ "github.com/lib/pq"
)

// Store PostgreSQL 后端。
//
// tx 非空时本实例绑定在事务里——所有方法经 exec/query 路由，于是同一份实现
// 同时服务「直接调用」与「事务内调用」两条路径，不会出现「事务里那条忘了用 tx
// 于是写在事务外」的经典漏洞。
type Store struct {
	// 每张表一个内嵌窄结构（与 SQLite 后端同款形状）。
	rawStore
	atomStore
	candidateStore
	entityStore
	pageStore
	episodeStore
	journalStore
	metaStore

	db *sql.DB
	tx *sql.Tx
}

// 各表子结构。方法体与 SQLite 后端同构，只是接收者不同，
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

// newStore 造一个全部子结构都回指自身的 Store。
//
// 漏回指一个的后果是该表的方法在调用时 panic（nil 指针）——所以这里用一个
// 循环而不是八行赋值：加表时忘了改这里，编译不过。
func newStore(db *sql.DB) *Store {
	s := &Store{db: db}
	s.rawStore.Store = s
	s.atomStore.Store = s
	s.candidateStore.Store = s
	s.entityStore.Store = s
	s.pageStore.Store = s
	s.episodeStore.Store = s
	s.journalStore.Store = s
	s.metaStore.Store = s
	return s
}

// Open 按 DSN 打开。
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 postgres 失败: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连不上 postgres: %w", err)
	}
	if err := ensureSchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return newStore(db), nil
}

// Transaction 事务边界。
//
// 晋升是「候选落定 + 事实入库 + 实体计数 + 审计条目」四步，中途不回滚会留下
// 「候选已判但事实没进」的空洞——而它在读取侧完全看不出来（只是那条事实凭空
// 消失）。这正是 store.Store 把 Transaction 作为一等公民的原因。
//
// 嵌套调用直接复用外层事务。**代价是内层出错不会自动回滚外层**——
// 所以内层 fn 返回错误后，外层 fn 必须立刻返回它。
func (s *Store) Transaction(ctx context.Context, fn func(ctx context.Context, st store.Store) error) error {
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

// Close 释放底层资源。
func (s *Store) Close() error {
	if s.tx != nil {
		_ = s.tx.Rollback()
	}
	return s.db.Close()
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

// ts 把时刻交给驱动（TIMESTAMPTZ）。
//
// 零值时刻存 NULL 而不是零时间：零时间（0001-01-01）在 PG 里是个合法的
// TIMESTAMPTZ，读回来会变成带时区偏移的一个真实时刻——
// 「从未发生」与「发生在公元 1 年」在数据上必须分得开。
func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func tsPtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func fromTSPtr(v sql.NullTime) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return v.Time.UTC()
}

// placeholders 生成 PG 的占位符（$1, $2, ...）。
//
// PG **不支持** SQLite 的 ? 风格占位符——这是两个后端 SQL 写法不同的地方之一，
// 但它只影响「怎么把参数送进去」，不影响「送进去的是什么」。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "$%d", i)
	}
	return b.String()
}

// stringsJSON / mapJSON 与 SQLite 后端同款：列表与载荷都存 JSON 串。
//
// 刻意用 TEXT 而不是 JSONB：JSONB 更省空间、也能直接建索引，
// 但它会把「往返失真」这类编码问题抹平——而 parity 断言里正好有一条
// 「往返失真」的用例，JSONB 会让那条用例测不到东西。
// mustJSON 序列化成 JSON 串；失败时退到空形态而不是 panic。
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func stringsJSON(v []string) string {
	if v == nil {
		return "[]"
	}
	return mustJSON(v)
}

func mapJSON(v map[string]any) string {
	if v == nil {
		return "{}"
	}
	return mustJSON(v)
}

// provenanceJSON 来源凭证的 JSON 形态。
func provenanceJSON(p provenanceFields) string {
	return mapJSON(map[string]any{
		"origin":        string(p.Origin),
		"host":          p.Host,
		"runtime_error": p.RuntimeError,
		"redacted":      p.Redacted,
		"tool":          p.Tool,
	})
}

// ftsBody 造全文检索正文。
//
// 与 SQLite 后端**同一套切词**（store.LexicalTokens）：汉字逐字成词元，
// 英文按空白切。PG 的 'simple' 词典只按空格切，故两边行为逐条一致。
func ftsBody(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(store.LexicalTokens(strings.Join(out, " ")), " ")
}

// ftsQuery 把词元拼成 tsquery 的输入。
//
// 用 plainto_tsquery：它把词元合取（AND）起来，与 FTS5 的空格分词语义一致。
// 词元里不含引号与运算符（store.LexicalTokens 只产出字母/数字/下划线/汉字），
// 故不需要转义——而这正是「查询串不再原样透传」换来的好处：
// 任意用户输入都不会变成语法。
func ftsQuery(tokens []string) string {
	return strings.Join(tokens, " ")
}

// notFound 统一把 sql.ErrNoRows 翻成 store 哨兵。
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

// absentOr 区分「查询出错」与「查到了 0 行」。
//
// 这条区分是必须的：rows.Err() 在 0 行时是 nil，直接 notFound(nil) 会把
// 「不存在」翻成「成功 + nil 对象」，调用方一解引用就 panic。
func absentOr(err error) error {
	if err != nil {
		return err
	}
	return store.ErrNotFound
}

func limitOf(limit, def int) int {
	if limit <= 0 {
		return def
	}
	return limit
}

// DSNEnv 环境变量名：指向真实 PostgreSQL 测试库。
const DsnEnv = "BQ_MEMORY_TEST_POSTGRES_DSN"

// Dsn 返回配置好的测试 DSN，未配置时返回空串。
func Dsn() string { return os.Getenv(DsnEnv) }

// Configured 是否配了真实测试库。
func Configured() bool { return Dsn() != "" }

// WithDatabase 把 DSN 里的库名换成别的。
//
// 用 URL 解析而不是字符串切割：DSN 的形态多变（有没有路径、有没有端口、
// 有没有查询参数），切割会在某一种形态上悄悄拼错库名——
// 而「连到了别的库」在测试里的表现是「断言莫名其妙地全绿」。
func WithDatabase(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("解析 DSN 失败: %w", err)
	}
	if u.Scheme == "" {
		return "", errors.New("DSN 必须是 URL 形态（如 postgres://user:pass@host:5432/db?sslmode=disable）")
	}
	u.Path = "/" + dbName
	return u.String(), nil
}
