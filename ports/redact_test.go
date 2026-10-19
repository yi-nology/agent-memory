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
