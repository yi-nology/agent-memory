package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：多进程并发开库（#24）的用例。
//
// WAL 模式下服务 + CLI 工具同开一份库是设计内场景。ensureSchema 此前是
// 「读版本 → 判迁移 → ALTER → 写版本」逐条 autocommit：两个进程同时读到
// hasColumn=false，后者的 ALTER 撞 duplicate column name → Open 失败。
// 修复是单条连接上的 BEGIN IMMEDIATE（先拿写锁、再读版本、整段一个事务）。

// TestOpenConcurrentMigrations 多个进程并发打开**旧形状**的库：都要真实跑迁移，
// 任何一方都不得因竞态失败（修复前：check-then-ALTER 撞 duplicate column）。
func TestOpenConcurrentMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.sqlite")

	// 先建出当前形状的库，再把它拆回 v6 形状（拆掉 kind 列与其索引、版本回拨），
	// 让并发打开的每一方都真实跑 migrateV7——竞态要发生在「跑迁移」上才有意义。
	s0, err := sqlite.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	seed := &types.AtomCard{
		ID: "a-seed", EntityID: "e1", Assertion: "种子事实", VerbatimQuote: "「种子事实」",
		QuoteEventID: "ev1", SearchTerms: []string{"种子"},
		OccurredAt: time.Now().UTC(), Confidence: types.ConfidenceMed,
		Importance: types.ImportanceMed, CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(), Provenance: types.Provenance{Origin: types.OriginOperator},
	}
	if err := s0.SaveAtom(context.Background(), store.Namespace("ns-race"), seed); err != nil {
		t.Fatal(err)
	}
	if err := s0.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS atoms_kind`,
		`ALTER TABLE atoms DROP COLUMN kind`,
		`UPDATE meta SET value = '6' WHERE namespace = '' AND key = 'schema_version'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("拆回 v6 形状失败（%s）: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 同一时刻起跑：把竞态窗口顶满
			s, err := sqlite.OpenFile(path)
			if err != nil {
				errs[i] = err
				return
			}
			_ = s.Close()
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发打开失败（ensureSchema 竞态）: %v", i, err)
		}
	}

	// 迁移真的发生过：形状与版本都到位。
	db, err = sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var kindCols int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('atoms') WHERE name = 'kind'`).Scan(&kindCols); err != nil {
		t.Fatal(err)
	}
	if kindCols != 1 {
		t.Fatal("并发打开后 atoms.kind 列应已被迁移补上")
	}
	var v string
	if err := db.QueryRow(
		`SELECT value FROM meta WHERE namespace = '' AND key = 'schema_version'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "7" {
		t.Fatalf("schema_version 应为 7，实得 %s", v)
	}
}

// TestOpenWaitsForExternalWriteLock 另一进程持写锁时 Open 不得报错退场：
// 要在锁上等到锁释放（busy_timeout），然后成功打开——
// 「双进程同时启动必有一方 Open 失败」正是本条修复要消灭的失效。
func TestOpenWaitsForExternalWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.sqlite")
	s0, err := sqlite.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s0.Close(); err != nil {
		t.Fatal(err)
	}

	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	tx, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO meta (namespace, key, value) VALUES ('', 'probe', 'x')`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		s, err := sqlite.OpenFile(path)
		if err == nil {
			err = s.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("写锁被占用时 Open 不该完成（ensureSchema 应在 BEGIN IMMEDIATE 上等锁）: %v", err)
	case <-time.After(200 * time.Millisecond):
		// 还没返回 = 在等锁，正确。
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("锁释放后 Open 应成功: %v", err)
	}
}
