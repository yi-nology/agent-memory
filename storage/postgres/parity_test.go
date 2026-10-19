package postgres_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/storage/parity"
	"github.com/yi-nology/agent-memory/storage/postgres"
	"github.com/yi-nology/agent-memory/store"
)

// TestParity PG 后端的行为断言。
//
// # 为什么必须对着**真实服务器**跑
//
// 内存实现测不出「约束没建」「索引没建」「类型映射错」这类问题，
// 而它们恰恰是换后端时最常见的故障：SQLite 的 TEXT 装什么都行，
// PG 的 BOOLEAN/TIMESTAMPTZ 装错会当场报错，但「列没建」要到第一次读写
// 那个表时才炸——而那时候失败点在离病因很远的地方。
//
// 「skipped 的 PG 用例不证明兼容」：上面这行 skip 是**故意可见**的，
// CI 里看到它时，「SQLite 与 PG 行为一致」这句话就还没有被验证过。
func TestParity(t *testing.T) {
	if !postgres.Configured() {
		t.Skipf("未配置 %s：PG 行为一致性**未验证**（skipped 用例不证明兼容）", postgres.DsnEnv)
	}
	parity.Run(t, openPG)
}

// openPG 每个用例开一份**干净库**。
//
// 为什么不是「同一个库清空表」：清表要按外键顺序删，漏一张就会留下残数据，
// 而残数据会让后一个用例的断言莫名其妙地红——
// 「测试之间互相污染」在测试报告里表现为「随机失败」，最难查。
// 建新库的成本是一次 CREATE DATABASE，换来的是彻底隔离。
func openPG(t *testing.T) store.Store {
	t.Helper()
	dsn := postgres.Dsn()
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("连不上 PG: %v", err)
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Fatalf("PG 不通: %v", err)
	}

	name := "am_parity_" + sanitizeIdent(t.Name()) + "_" + fmt.Sprint(time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		_ = admin.Close()
		t.Fatalf("建测试库失败: %v", err)
	}

	testDSN, err := postgres.WithDatabase(dsn, name)
	if err != nil {
		t.Fatalf("拼测试库 DSN 失败: %v", err)
	}
	s, err := postgres.Open(testDSN)
	if err != nil {
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
		_ = admin.Close()
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		// WITH (FORCE) 断开残留连接：PG 13+ 才支持。
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS "` + name + `" WITH (FORCE)`)
		_ = admin.Close()
	})
	return s
}

// sanitizeIdent 把测试名变成合法的库名片段。
//
// PG 的标识符最长 63 字节，而测试名里可能有中文与斜杠——
// 直接拿它当库名会失败，而失败信息（"invalid identifier"）与病因毫无相似之处。
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
