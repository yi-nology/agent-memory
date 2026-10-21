package postgres

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/storage/parity"
)

// 本文件：向量索引（持久化那一面）的用例（与 SQLite 后端同一组行为）。
//
// # 三组用例，三种职责
//
// ① **共享行为断言**（parity.RunVector）：两个后端跑的是**同一份**断言，
//
//	见 storage/parity/parity_vector.go。
//
// ② **迁移**：旧形状的库（没有向量表）打开后要能升上来，且迁移跑第二遍不报错。
// ③ **后端专属**：向量的字节形态（逐位相同）、空输入拒收、维度哨兵。
//
// # 为什么字节形态要在后端专属用例里钉
//
// 向量是**逐位可复现**的东西：同一批记忆在不同进程里算出的向量必须逐位相同，
// 否则「召回结果可复现」这条承诺（评测面靠它成立）就破了。
// 而「逐位相同」这件事只有真正读过库里的字节才验得了——
// 检索面拿到的是解出来的 float32，它可以是「差不多」而不是「一模一样」。
func TestVectorParity(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 向量面**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	parity.RunVector(t, func(t *testing.T) parity.VectorIndex {
		s, dbName := openTempDB(t, dsn)
		t.Cleanup(func() { dropTempDB(t, dsn, dbName) })
		// 这一行是**契约钉子**：端口那边改了命中类型或签名，它立刻编译不过。
		return parity.Adapt(s, func(h ports.VectorHit) parity.VectorHit {
			return parity.VectorHit{ID: h.ID, Score: h.Score}
		})
	})
}

// TestMigrationAtomVectors 旧形状的库（没有向量表）打开后要能升上来。
//
// # 旧形状怎么造
//
// **按当前代码建库，再把它改回旧形状**——不抄一份旧 DDL：
// 抄的那份会与真 DDL 漂移，于是用例绿着而真库炸着。
// 改回去的动作（删表、改版本号）都是「当前形状的反面」，故它们不会随真 DDL 失效。
func TestMigrationAtomVectors(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	// 变回「v5 形状」：删掉向量表、版本号改回 5。
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS atom_vectors`,
		`UPDATE meta SET value = '5' WHERE namespace = '' AND key = 'schema_version'`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("造旧形状失败（%s）: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关库失败: %v", err)
	}

	// 重新打开：迁移必须把表建出来，版本号到 6。
	s2, err := Open(tempDSN(t, dsn, dbName))
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	v, err := s2.GetMeta(context.Background(), "", "schema_version")
	if err != nil {
		t.Fatalf("读 schema_version 失败: %v", err)
	}
	// 版本号要比「打开前的形状版本」新：旧库升上来后必须落在**当前**形状版本上。
	//
	// 断言对着 schemaVersion 常量而不是写死某个数字：迁移是线性追加的，
	// 写死的数字会在下一次加迁移时又红一次——而那与向量索引毫无关系。
	if v != strconv.Itoa(schemaVersion) {
		t.Fatalf("版本号应升到 %d，实得 %q", schemaVersion, v)
	}
	if _, err := s2.db.Exec(`SELECT 1 FROM atom_vectors LIMIT 1`); err != nil {
		t.Fatalf("向量表没建出来: %v", err)
	}

	// 迁移后的形状必须与新建的库一样：两条路落在同一个形状上，
	// 否则「有的库能语义召回、有的库不能」。
	fresh, freshName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, freshName)
	defer func() { _ = fresh.Close() }()
	if !slices.Equal(tableShapePG(t, fresh.db, "atom_vectors"), tableShapePG(t, s2.db, "atom_vectors")) {
		t.Fatalf("迁移后的表形状与新建的库不一致:\n新建: %v\n迁移后: %v",
			tableShapePG(t, fresh.db, "atom_vectors"), tableShapePG(t, s2.db, "atom_vectors"))
	}
}

// TestMigrationAtomVectorsIsIdempotent 迁移跑第二遍不报错。
//
// 版本号那一行可能被手工改过、被老版本覆盖过，或者库是从别处拷来的——
// 同一段迁移可能被跑第二次，而迁移跑第二遍而失败的话**库就打不开了**。
//
// 这一条**直接调迁移**（不走打开流程）：走打开流程时建表脚本会先把表建出来，
// 于是「迁移本身幂等」这件事会被脚本的幂等掩盖——用例绿着，而守的不是它。
func TestMigrationAtomVectorsIsIdempotent(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 迁移路径**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)

	if _, err := s.db.Exec(
		`UPDATE meta SET value = '5' WHERE namespace = '' AND key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 打开一次：迁移跑第一遍。
	s2, err := Open(tempDSN(t, dsn, dbName))
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = s2.Close() }()

	// 第二遍：表已经是新形状，迁移必须认出来并跳过。
	if err := migrateV6AtomVectors(context.Background(), s2.db); err != nil {
		t.Fatalf("迁移跑第二遍失败（不幂等）: %v", err)
	}
	// 把表删掉后再跑一遍：它必须**自己**把表建出来
	//（建表脚本每次打开都跑，故这一步只考验迁移本身）。
	if _, err := s2.db.Exec(`DROP TABLE atom_vectors`); err != nil {
		t.Fatal(err)
	}
	if err := migrateV6AtomVectors(context.Background(), s2.db); err != nil {
		t.Fatalf("迁移在缺表时失败: %v", err)
	}
	if _, err := s2.db.Exec(`SELECT 1 FROM atom_vectors LIMIT 1`); err != nil {
		t.Fatalf("迁移没有把表建出来: %v", err)
	}
}

// TestVectorBlobIsBitExact 向量进库必须逐位相同。
//
// 判据：读库里的原始字节，与「按小端 float32 编码的期望字节」逐位比对。
// 若实现改用 JSON 串或文本，这一条立刻红——而那正是它要防的：
// 文本往返会丢精度（十进制 → float64 → float32 两次舍入），
// 于是同一批记忆的向量在不同进程里不再逐位相同。
func TestVectorBlobIsBitExact(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 向量面**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	defer func() { _ = s.Close() }()

	vec := []float32{1.5, -2.25, 0.1}
	if err := s.Upsert(context.Background(), "ns", "v1", "", vec); err != nil {
		t.Fatalf("写向量失败: %v", err)
	}
	var raw []byte
	if err := s.db.QueryRow(
		`SELECT vec FROM atom_vectors WHERE namespace = $1 AND id = $2`, "ns", "v1").Scan(&raw); err != nil {
		t.Fatalf("读原始字节失败: %v", err)
	}
	want := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(want[4*i:], math.Float32bits(v))
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("向量字节失真:\n实得 %v\n期望 %v", raw, want)
	}
}

// TestVectorRejectsEmptyInput 空标识与空向量必须被拒。
//
// 静默收下的话，故障会推迟到检索面：一条没有内容的向量在结果里看不出异常，
// 而调用方只会看到「语义召回像是没生效」。
func TestVectorRejectsEmptyInput(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 向量面**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	defer func() { _ = s.Close() }()

	if err := s.Upsert(context.Background(), "ns", "", "", []float32{1}); err == nil {
		t.Fatal("空标识必须被拒")
	}
	if err := s.Upsert(context.Background(), "ns", "v1", "", nil); err == nil {
		t.Fatal("空向量必须被拒")
	}
	if _, err := s.Search(context.Background(), "ns", "", nil, 10); err == nil {
		t.Fatal("空查询向量必须被拒")
	}
}

// TestVectorDimensionMismatchSentinel 维度不一致必须报那个哨兵。
//
// 共享断言只钉「报错而不是空集」（它不认识 ports），
// 哨兵本身在这里钉：调用方判的是 errors.Is(err, ports.ErrDimensionMismatch)，
// 而哨兵错了的话，调用方会以为「维度对得上，只是没命中」。
func TestVectorDimensionMismatchSentinel(t *testing.T) {
	dsn := Dsn()
	if dsn == "" {
		t.Skipf("未配置 %s：PG 向量面**未验证**（skipped 用例不证明兼容）", DsnEnv)
	}
	s, dbName := openTempDB(t, dsn)
	defer dropTempDB(t, dsn, dbName)
	defer func() { _ = s.Close() }()

	if err := s.Upsert(context.Background(), "ns", "v1", "", []float32{1, 2, 3}); err != nil {
		t.Fatalf("写向量失败: %v", err)
	}
	_, err := s.Search(context.Background(), "ns", "", []float32{1, 2, 3, 4}, 10)
	if !errors.Is(err, ports.ErrDimensionMismatch) {
		t.Fatalf("维度不一致必须判 ports.ErrDimensionMismatch，实得 %v", err)
	}
	// 写入侧的同一判据：同一命名空间不许混维度。
	if err := s.Upsert(context.Background(), "ns", "v2", "", []float32{1, 2}); !errors.Is(err, ports.ErrDimensionMismatch) {
		t.Fatalf("混维度写入必须判 ports.ErrDimensionMismatch，实得 %v", err)
	}
}
