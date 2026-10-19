// Package postgres 记忆存储的 PostgreSQL 后端。
//
// # 现状：契约位就位，实现未落
//
// 本包目前只提供**接线方式**与**不可跳过的约定**，实现随 M6 落地。原因写在
// 《方案》§7.3：SQLite 后端先跑通机制面，PG 实现与它共享 storage/parity 那套
// 断言——而 PG 用例必须对着**真实服务器**跑（见下方 Dsn 环境变量）。
//
// # 与 SQLite 后端的关系
//
// 两者跑**同一份** storage/parity 断言。这不是形式主义：记忆的行为一致性是产品
// 承诺（「同一批记忆换个部署形态，召回结果不该变」），而两个实现各自的单测都绿
// 并不代表它们行为一致——软退役的过滤条件写漏一列、分页游标用 `<=` 还是 `<`、
// 时间窗闭开区间，这类不一致在各自的单测里全都看不出来。
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
// 最后一行是最大的实现差异：**FTS5 的 MATCH 语法与 PG 的 tsquery 语法不同**，
// 因此 storage.Store 的 Search* 方法把「查询串原样透传」这件事定为**调用方的
// 责任**（见 sqlite.searchFTS 注释）。跨后端要保证行为一致，就得在召回层做一次
// 方言归一，而不是指望两个后端对同一串文本有同样的理解。
package postgres

import (
	"errors"
	"os"
)

// DsnEnv 环境变量名：指向真实 PostgreSQL 测试库。
const DsnEnv = "BQ_MEMORY_TEST_POSTGRES_DSN"

// ErrNotImplemented 后端未实现时的哨兵。
//
// 用哨兵而不是返回 nil：调用方能 errors.Is 判出来并给出明确信息，
// 而「返回 nil 加一个空 store」会让它在第一次真正调用时才炸，且报错点离病因很远。
var ErrNotImplemented = errors.New("postgres: 后端尚未实现")

// Dsn 返回配置好的测试 DSN，未配置时返回空串。
func Dsn() string { return os.Getenv(DsnEnv) }

// Configured 是否配了真实测试库。
func Configured() bool { return Dsn() != "" }
