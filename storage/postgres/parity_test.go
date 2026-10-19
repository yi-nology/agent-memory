package postgres_test

import (
	"testing"

	"github.com/yi-nology/agent-memory/storage/postgres"
)

// TestParity PG 后端的行为断言接线位。
//
// # 这里的跳过是**故意可见**的，而且是两层
//
// 一层是没配真实测试库（postgres.DsnEnv）。另一层是**后端压根没实现**。
//
// ⚠️ **跳过的用例不证明兼容。** CI 里看到这条 skip 时，
// 「SQLite 与 PG 行为一致」这句话就还没有被验证过——别当已验证读。
// 上游 octop-memory 对同一件事的措辞是「skipped PG cases do not prove compatibility」。
//
// # 落地时怎么改（顺序不能反）
//
//  1. 先实现后端，让它满足 store.Store；
//  2. 再把下面的 t.Skip 换成 parity.Run(t, openPG)——**opener 要连真库、
//     跑 schema、每个用例一份干净库**；
//  3. 最后才在 CI 里配 DsnEnv。
//
// ⚠️ **不要先放一个「能编译的空实现」把门禁糊过去。** 那会让 parity 套件在
// 编译期就通过、在运行期的某个用例上炸，而「后端没实现」与「后端行为不对」
// 这两种病因的修法完全不同——混在一起时，人会先去改后端逻辑，而真正该做的是
// 先把后端写出来。
func TestParity(t *testing.T) {
	switch {
	case !postgres.Configured():
		t.Skipf("未配置 %s：PG 行为一致性**未验证**（skipped 用例不证明兼容）", postgres.DsnEnv)
	default:
		t.Skip("postgres 后端尚未实现（M6）：**跳过**。落地顺序见本文件注释——先实现，再接 parity，最后配 DSN")
	}
}
