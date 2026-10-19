package sqlite_test

import (
	"testing"

	"github.com/yi-nology/agent-memory/storage/parity"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
)

// TestParity 跑跨后端共享行为断言。
//
// 库名用 t.Name()：每个子用例一份独立的具名内存库，互不污染。
// 用具名而不是 ":memory:"，是因为后者随连接生灭，而连接池一旦在测试里被放开
// 就会偶发拿到空库——那种偶发会以「表不存在」的形式出现，与真实 bug 难辨。
func TestParity(t *testing.T) {
	parity.Run(t, openFresh)
}

func openFresh(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.OpenMemory(t.Name())
	if err != nil {
		t.Fatalf("开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestOpenIsIdempotent 打开同一个库两次必须都成功。
//
// 「第二个进程因为表已存在就打不开库」不该是一种失败模式：宿主回本机跑命令、
// 另一个测试进程同时打开，都是常态。
func TestOpenIsIdempotent(t *testing.T) {
	first, err := sqlite.OpenMemory("idem")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := sqlite.OpenMemory("idem")
	if err != nil {
		t.Fatalf("重复打开必须成功: %v", err)
	}
	defer func() { _ = second.Close() }()
}

// TestSchemaVersionRecorded schema 版本必须落库。
//
// 迁移按这个号线性追加。没有它，「这个库的形状是第几版」就只能靠猜，
// 而猜错的代价是让一份旧形状的库被新代码按新形状读。
func TestSchemaVersionRecorded(t *testing.T) {
	s, err := sqlite.OpenMemory("ver")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	v, err := s.GetMeta(t.Context(), "", "schema_version")
	if err != nil {
		t.Fatalf("读 schema_version 失败: %v", err)
	}
	if v == "" {
		t.Fatal("schema_version 不得为空")
	}
	t.Logf("schema_version = %s", v)
}
