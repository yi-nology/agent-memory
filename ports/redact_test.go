package ports

import (
	"context"
	"strings"
	"testing"
)

// 兵底脱敏的覆盖面用例。
//
// # 为什么单独一个文件
//
// 本仓自研的 5 条形态规则与上游 guardx/logredact 的规则表**互补而非包含**
// （对照表见 redact.go 的类型注释）。这套用例守两件事：
//
//  1. **上游独有的四类形态确实被处置了**——Authorization 头、裸 Bearer、
//     裸 JWT、截断在中段的私钥块。它们在本仓自研兵底里是漏的，而凭据一旦
//     落库即不可逆（收录标准第三条：漏报代价不可逆）。
//  2. **处置顺序没被"顺手优化"掉**——顺序是「本仓在前、上游在后」，
//     倒过来的后果是已知前缀族（ghp_ / sk- / AKIA …）的定位线索被上游的
//     `****` 抹掉。这条最容易被后人当成冗余而改掉，故钉住。

func redact(t *testing.T, in string) string {
	t.Helper()
	out, err := PatternRedactor{}.Redact(context.Background(), RedactRequest{
		Surface: SurfaceBody,
		Text:    in,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// 上游规则表覆盖、本仓自研兵底原本漏掉的四类形态。
func TestRedactorCoversUpstreamOnlyCredentialForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// 明文残片：处置后正文里不许再出现它。
		leak string
	}{
		{"Authorization 头",
			"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789",
			"abcdefghijklmnopqrstuvwxyz0123456789"},
		{"裸 Bearer",
			"带上 Bearer sk-abcdefghijklmnopqrstuvwxyz0123456789 再来",
			"sk-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"裸 JWT",
			"令牌是 eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
			"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"},
		{"截断在中段的私钥块",
			"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n（此处日志被截断）",
			"MIIEowIBAAKCAQEA"},
		{"上游独有的键名 passphrase",
			"passphrase: hunter2hunter2",
			"hunter2hunter2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redact(t, tc.in)
			if strings.Contains(out, tc.leak) {
				t.Fatalf("凭据明文不许残留在处置结果里:\n输入: %s\n输出: %s", tc.in, out)
			}
			if out == tc.in {
				t.Fatalf("该形态必须被处置（原文一字未改）: %s", tc.in)
			}
		})
	}
}

// 本仓自研兵底独有的形态不能被上游那套吃掉：长十六进制与任意 scheme 的
// URL 内嵌凭据，上游都没有对应规则。
func TestRedactorKeepsOwnOnlyCredentialForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		leak string
	}{
		{"长十六进制", "会话句柄 0123456789abcdef0123456789abcdef01234567 用一下", "0123456789abcdef0123456789abcdef01234567"},
		// 口令用不会与掩码类型名撞词的字面量：类型名里含 secret 时，
		// 断言「输出不含 secret」会与类型标签本身冲突。
		{"非白名单 scheme 的 URL 凭据", "连接 s3://AKIAIOSFODNN7EXAMPLE:hunter2xyz@host/path", "hunter2xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redact(t, tc.in)
			if strings.Contains(out, tc.leak) {
				t.Fatalf("凭据明文不许残留在处置结果里:\n输入: %s\n输出: %s", tc.in, out)
			}
		})
	}
}

// 掩码必须**惰性**：处置结果里不许出现嵌套掩码。
//
// 形态形如 `«kind:****»`，而 kind 后面跟着冒号——若 kind 里含敏感键名表
// 里的词（credential / token / secret …），后续那条规则会把自己的掩码
// 再打码一次，产出 `«url-«secret-assignment:****»»`。
//
// 后果不是泄漏，是**审计失真**：读掩码的人看到的形态标签和事实不是一回事。
// 这条用例让「类型名不许撞键名表」这条约束变成**可执行**的，而不是
// 靠人记得。首个撞车的是 `url-credential`（键名表里有 credential）。
func TestRedactorOutputHasNoNestedMask(t *testing.T) {
	inputs := []string{
		"连接 s3://AKIAIOSFODNN7EXAMPLE:hunter2xyz@host/path",
		"token=abcdef123456",
		"句柄 0123456789abcdef0123456789abcdef01234567",
		"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----",
		"部署用的访问凭据是 ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz0123456789",
	}
	for _, in := range inputs {
		out := redact(t, in)
		if strings.Count(out, "«") != strings.Count(out, "»") {
			t.Fatalf("掩码括号必须配对: %q", out)
		}
		if strings.Count(out, "«") > 1 {
			t.Fatalf("处置结果里出现嵌套掩码（类型名撞了敏感键名表）:\n输入: %s\n输出: %s", in, out)
		}
	}
}

// 顺序钉住：已知前缀族由**本仓**的规则处置，因此掩码里带着形态标签
// （`«known-token-prefix:…»`）而不是上游那种一律 `****`。
//
// 定位线索是刻意保留的：全量打码会让处置后的文本失去定位能力——
// 用户问「我那条凭据呢」时，没人能指出是哪一条。而掩码必须不可复原，
// 所以保留位数一律封顶 4，不随原值长度变化。
//
// 这条用例存在的意义是**防顺序被"顺手优化"**：若把上游那套挪到前面，
// ghp_ 这几族会先被抹成 `****`，定位线索就此丢失——而这个变化在内容上
// 看不出来，只在有人回头找凭据时才暴露。
func TestRedactorOrderKeepsLocatingClue(t *testing.T) {
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := redact(t, "部署用的访问凭据是 "+secret+"，请勿外传。")
	if !strings.Contains(out, "known-token-prefix") {
		t.Fatalf("已知前缀族应由本仓规则处置（掩码带形态标签）: %q", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("凭据明文不许残留: %q", out)
	}
}

// 本仓掩码保留首尾少量字符作定位线索；上游一律 `****`。
// 两者同时出现在一段文本里是**已知且刻意**的取舍（见 redact.go 的注释）：
// 漏报代价不可逆，而定位线索只是「少几个字符就能补」的那一档。
func TestRedactorKeepsClueOnLongHex(t *testing.T) {
	const hex = "0123456789abcdef0123456789abcdef01234567"
	out := redact(t, "句柄 "+hex+" 记一下")
	if !strings.Contains(out, "long-hex") {
		t.Fatalf("长十六进制应由本仓规则处置（掩码带形态标签）: %q", out)
	}
	if !strings.Contains(out, "0123") {
		t.Fatalf("掩码应保留首部定位线索: %q", out)
	}
}

// 掩码**长度不得泄露原值长度**。
//
// 这条是被自己的实现撞出来的：自研那版把「被掩掉多少字符」直接写成等长的
// 星号串，于是掩码长度 = 原值长度——凭据 16 位是 16 个星，36 位是 36 个星。
// 攻击者据此知道原值长度，而注释里「不可复原」的承诺只兑现了一半
// （复原不了，但长度泄了）。
//
// 判据用**不同长度的同形态输入产出等长掩码**来表达，而不是数星号个数：
// 数星号会把「掩码主体固定」与「定位线索按原值长度变化」两件事混在一起。
func TestRedactorMaskLengthDoesNotLeak(t *testing.T) {
	short := redact(t, "token=abcdefghij")
	long := redact(t, "token=abcdefghijklmnopqrstuvwxyz0123456789")
	if len([]rune(short)) != len([]rune(long)) {
		t.Fatalf("同形态不同长度的原值必须产出等长掩码（否则长度泄露）:\n短: %q\n长: %q", short, long)
	}
	if strings.Contains(short, "abcdefghij") || strings.Contains(long, "abcdefghij") {
		t.Fatalf("凭据明文不许残留:\n短: %q\n长: %q", short, long)
	}
}

// 带保留位的规则必须有**命中长度地板**，否则短凭据会被完整暴露。
//
// 上游 stringx.Masker 的 Keep 档在「匹配长度 ≤ 2*Keep」时，头尾两段保留位
// 恰好拼回完整原文（掩码主体只是插在中间的装饰）——这是上游已知缺陷，
// 本仓的规避手段是：凡 keep>0 的规则必须声明 minLen ≥ 4*keep，构造期校验。
//
// 这条用例钉住「声明的 minLen 与正则的真实下限一致」：改正则的量词而忘了
// 改 minLen，或反过来，都会在这里露出来。
func TestRedactorKeepRuleLengthFloor(t *testing.T) {
	// long-hex 的正则是 {32,}：31 位不命中，32 位命中。
	short31 := strings.Repeat("a", 31)
	if out := redact(t, "句柄 "+short31+" 记一下"); out != "句柄 "+short31+" 记一下" {
		t.Fatalf("31 位十六进制不该被命中（低于声明的长度地板 32）: %q", out)
	}
	long32 := strings.Repeat("a", 32)
	out := redact(t, "句柄 "+long32+" 记一下")
	if !strings.Contains(out, "long-hex") {
		t.Fatalf("32 位十六进制应被命中: %q", out)
	}
	// 命中之后，原值不得整体出现在结果里（保留位只留首尾各 4）。
	if strings.Contains(out, long32) {
		t.Fatalf("掩码后原值整体残留: %q", out)
	}
	// 保留位合计必须显著少于原值：留 8 藏 1 等于没藏。
	if kept := len(out) - len("句柄  记一下"); kept >= len(long32) {
		t.Fatalf("保留位过多，等于没藏: %q", out)
	}
}

// 长度地板必须在**运行期按实际命中长度**生效（2026-10-20 实弹补）。
//
// # 这条用例钉的是什么
//
// 原先的「声明 minLen + 构造期校验」是**拿声明比声明**：
//   - minLen 从未传进下游（上游 stringx.Rule 只有 Name/Re/Keep 三个字段，
//     没有 MinLen），运行期零作用；
//   - 构造期校验的只是「声明值够不够」，不是「正则的真实下限」。
//
// 真正保护秘密的物理量是**实际命中长度**——它只有运行期按实际匹配值才知道。
//
// 失效形态（一个人「为了多抓一点」的常见小改）：
// 把 long-hex 的量词从 {32,} 改成 {8,}，声明的 minLen 仍是 32（构造期不炸），
// 但 8 位密钥会被掩成 `0123«long-hex:****»4567`——**完整可复原**。
//
// # 断言必须挑「两种情形结果不同」的输入
//
// 不能拿「查不到东西」当断言——那分不清「有地板」与「没地板」。
// 这里断言的是**可复原性**：把掩码里的形态标签剥掉、把保留位拼回去，
// 若能还原出原值，就是泄露。这条断言在「有地板」时红（短值被全量打码，
// 拼不回），在「没地板」时绿（拼得回）——**能区分**。
func TestRedactorShortHitIsFullyMasked(t *testing.T) {
	// 8 位十六进制：低于 long-hex 当前量词 {32,}，故不命中——
	// 但它正是「量词被改短」之后的形态。用例直接构造这个场景。
	short := "01234567"

	// 直接验证运行期地板的行为：构造一条 keep>0、量词放宽的规则，
	// 走同一条 maskStructural 路径。
	out := redactWithShortHit(t, short)
	if strings.Contains(out, short) {
		t.Fatalf("短命中被完整暴露: %q", out)
	}
	// 复原检测：把形态标签与分隔符剥掉，看能不能拼回原值。
	reconstructed := stripMask(out)
	if reconstructed == short {
		t.Fatalf("短命中可被复原（长度地板未在运行期生效）: 掩码=%q 复原=%q", out, reconstructed)
	}
	// 地板生效时，短命中应被**全量**打码（无保留位）——
	// 掩码里不应出现形态标签（标签会暴露是哪条规则命中）。
	if strings.Contains(out, "long-hex") {
		t.Fatalf("短命中应走全量打码（无形态标签），实际: %q", out)
	}
}

// 命中长度**够**时，仍应保留定位线索（地板不能把有用信息也抹掉）。
func TestRedactorLongHitKeepsLocator(t *testing.T) {
	long := strings.Repeat("a", 32)
	out := redact(t, "句柄 "+long+" 记一下")
	if !strings.Contains(out, "long-hex") {
		t.Fatalf("长命中应带形态标签: %q", out)
	}
	if strings.Contains(out, long) {
		t.Fatalf("长命中原值整体残留: %q", out)
	}
}
