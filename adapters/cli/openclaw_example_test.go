package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yi-nology/agent-memory/adapters/cli"
	"github.com/yi-nology/agent-memory/storage/sqlite"
	"github.com/yi-nology/agent-memory/store"
	"github.com/yi-nology/agent-memory/types"
)

// 这组用例钉的是**文档里的 OpenClaw 配置片段**与**本仓 CLI 的真实参数面**一致。
//
// 为什么要有它：OpenClaw 按配置里的 command + args 拉起记忆内核，
// 于是「文档写错了一个参数名」这件事，**要等到用户照抄配置时才会暴露** ——
// 而那时症状是「Gateway 起不来 / agent 没有工具」，离病根十万八千里。
//
// 更糟的是其中有一半是静默的：参数名写错，Go 的 flag 包会报错（响），
// 但**参数值写错**（比如 namespace 拼错、或者某个参数被悄悄忽略）
// 会让内核落进另一个记忆库 —— 一切看起来都正常，只是 agent「记不住」。
//
// 所以这里不只验「配置能跑」，还验「落进了配置指定的那个库」。

// openclawExamplePath 示例配置（相对本包目录）。
const openclawExamplePath = "../../docs/INTEGRATIONS/openclaw.example.json"

// openclawDocPath 接入文档（相对本包目录）。
const openclawDocPath = "../../docs/INTEGRATIONS/openclaw.md"

// openclawConfig 是示例配置的形状。
//
// 只取用得到的字段：**多取一个字段就多一条「文档加了它、测试没跟上」的漂移面**。
type openclawConfig struct {
	MCP struct {
		Servers map[string]struct {
			Command   string   `json:"command"`
			Args      []string `json:"args"`
			Transport string   `json:"transport"`
			Enabled   *bool    `json:"enabled"`
		} `json:"servers"`
	} `json:"mcp"`
}

// loadOpenclawExample 读示例配置。
func loadOpenclawExample(t *testing.T) openclawConfig {
	t.Helper()
	raw, err := os.ReadFile(openclawExamplePath)
	if err != nil {
		t.Fatalf("读示例配置失败（路径变了？）: %v", err)
	}
	var cfg openclawConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("示例配置不是合法 JSON: %v", err)
	}
	return cfg
}

// agentmemoryServer 取示例里给本仓留的那一个服务器定义。
func agentmemoryServer(t *testing.T, cfg openclawConfig) struct {
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Transport string   `json:"transport"`
	Enabled   *bool    `json:"enabled"`
} {
	t.Helper()
	srv, ok := cfg.MCP.Servers["agentmemory"]
	if !ok {
		t.Fatalf("示例配置里应有 mcp.servers.agentmemory，实际有 %v", keysOf(cfg.MCP.Servers))
	}
	return srv
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestOpenclawExampleIsUsableAsIs：示例配置本身**照抄就能用**。
//
// 逐条断言的都是「OpenClaw 会照着做、而本仓必须能接住」的条件：
//   - command 是绝对路径：OpenClaw 拉子进程时的工作目录与 PATH 不受登录 shell 管；
//   - args[0] 是子命令名：少它就是「把本仓当 MCP 服务器跑」的意图没了；
//   - transport 是 stdio：这是本仓唯一实现的传输形态；
//   - enabled 为真：定义留着但不启用 = 工具不出现。
func TestOpenclawExampleIsUsableAsIs(t *testing.T) {
	cfg := loadOpenclawExample(t)
	srv := agentmemoryServer(t, cfg)

	if !filepath.IsAbs(srv.Command) {
		t.Errorf("command 必须是绝对路径（OpenClaw 拉子进程时的工作目录不确定）: %q", srv.Command)
	}
	if len(srv.Args) == 0 {
		t.Fatal("args 不能为空")
	}
	if srv.Args[0] != "mcp" {
		t.Errorf("args[0] 应是 mcp 子命令，实际 %q", srv.Args[0])
	}
	if srv.Transport != "stdio" {
		t.Errorf("transport 应是 stdio，实际 %q", srv.Transport)
	}
	if srv.Enabled == nil || !*srv.Enabled {
		t.Error("enabled 应为 true：定义留着但不启用，症状是「工具不出现」")
	}
	// 库路径与归属都必须显式给。
	//
	// 这两条漏一条的后果都是**静默**的：落进默认库，而 OpenClaw 侧一切正常。
	if !hasFlag(srv.Args, "--db") {
		t.Error("args 里应有 --db：记忆库位置不能靠缺省值")
	}
	if !hasFlag(srv.Args, "--namespace") {
		t.Error("args 里应有 --namespace：落进哪个记忆库不能靠缺省值")
	}
}

// hasFlag 参数表里有没有这个 flag。
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// flagValue 取 flag 后面那个值。
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestOpenclawExampleCapturesIntoItsOwnNamespace：C-2 的验收 ——
// **一次捕获之后，库里多一条原始事件，且落在配置指定的那个库里**。
//
// 这条用例的价值在最后那两行断言：
// 事件数 + 事件所在库。少断言「所在库」的话，
// 「落进了 default 库」这种失效形式照样绿 ——
// 而那正是「agent 看着一切正常、却翻不到自己的记忆」的成因。
func TestOpenclawExampleCapturesIntoItsOwnNamespace(t *testing.T) {
	cfg := loadOpenclawExample(t)
	srv := agentmemoryServer(t, cfg)

	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")

	// 逐字用示例的参数向量，只把 --db 的**值**换成临时库路径。
	//
	// 为什么不整段重写：整段重写就等于「测试自己写了一份参数」，
	// 于是示例里的参数名漂了也测不出来 —— 那样这条用例就在守空气。
	args := replaceFlagValue(srv.Args, "--db", db)
	ns := flagValue(srv.Args, "--namespace")
	if ns == "" {
		t.Fatal("示例里 --namespace 没有值")
	}

	const content = "宿主约定：磁盘长期偏小的那台机器要在周五前清理"
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory.capture","arguments":{"event_id":"ev-openclaw-1","content":"` + content + `","type":"turn_end"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory.stats","arguments":{}}}`,
	}, "\n") + "\n"

	var out, errb bytes.Buffer
	code := cli.RunWithIO(context.Background(), args, strings.NewReader(in), &out, &errb)
	if code != 0 {
		t.Fatalf("按示例参数起 MCP 服务器失败: %d %s", code, errb.String())
	}

	lines := nonEmptyLines(out.String())
	if len(lines) != 3 {
		t.Fatalf("应有 3 条响应（initialize + capture + stats），实际 %d 条: %q", len(lines), out.String())
	}

	// 捕获被受理。
	capture := decodeToolText(t, lines[1])
	if !strings.Contains(capture, `"accepted":true`) {
		t.Fatalf("捕获未被受理: %s", capture)
	}

	// 统计说库里有一条原始事件。
	stats := decodeToolText(t, lines[2])
	if !strings.Contains(stats, `"raw_events":1`) {
		t.Fatalf("统计里的原始事件数应为 1: %s", stats)
	}

	// 落到**配置指定的那个库**里，且默认库里一条都没有。
	st, err := sqlite.OpenFile(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	events, err := st.ListRaw(context.Background(), store.Namespace(ns), types.Scope(""), time.Time{}, store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("库 %q 里应有 1 条原始事件，实际 %d 条", ns, len(events))
	}
	if events[0].Content != content {
		t.Errorf("事件正文与捕获的不一致: %q", events[0].Content)
	}

	// 默认库必须是空的：这条断言专治「参数没生效，落进缺省库」。
	//
	// 它是这条用例里最要紧的一句 —— 没有它，「落错库」这种失效形式照样绿。
	def, err := st.ListRaw(context.Background(), store.Namespace("default"), types.Scope(""), time.Time{}, store.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 0 {
		t.Fatalf("默认库里不该有事件（说明 --namespace 没生效）: %d 条", len(def))
	}
}

// replaceFlagValue 把参数表里某个 flag 后面那个值换掉。
func replaceFlagValue(args []string, flag, value string) []string {
	out := append([]string(nil), args...)
	for i, a := range out {
		if a == flag && i+1 < len(out) {
			out[i+1] = value
			return out
		}
	}
	return out
}

// nonEmptyLines 拆出非空行。
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// decodeToolText 取 tools/call 结果里的文本。
func decodeToolText(t *testing.T, line string) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%q)", err, line)
	}
	if resp.Error != nil {
		t.Fatalf("工具调用报错: %d %s", resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Result.Content) == 0 {
		t.Fatalf("工具结果里没有内容: %q", line)
	}
	return resp.Result.Content[0].Text
}

// TestOpenclawDocToolNamesMatchServer：文档里让 agent 调的工具名，
// 必须是**服务器真的暴露**的那些。
//
// 为什么两个方向都要断言：
//   - 文档写了服务器没有的名字 ⇒ agent 照着调会失败（表现为「捕获时好时坏」）；
//   - 服务器有文档没写的名字 ⇒ 文档的白名单清单过时了（读者以为只有四个）。
//
// ⚠️ 断言的是**两套名字** ✓：协议层的 `memory.capture`，
// 以及 OpenClaw 改写后模型真正看到的 `agentmemory__memory-capture` ✓
// （前缀取配置里的服务器名 ✓，点换成横线 ✓）。
// 只查其中一套 ⇒ 另一套错了照样绿 ✓，
// 而**模型只认改写后的那套** ✓ —— 提示词写错的后果是「agent 找不到那个工具」✓
// （不报错、就是不记 ✓）。
//
// 匹配只认**不在路径里**的 memory.x：像 memory.sqlite 那种是文件名，
// 不是工具名 —— 把它算进来就是一次假红（门禁的头号死法）。
func TestOpenclawDocToolNamesMatchServer(t *testing.T) {
	raw, err := os.ReadFile(openclawDocPath)
	if err != nil {
		t.Fatalf("读接入文档失败（路径变了？）: %v", err)
	}
	doc := string(raw)

	// 三层名字都要能对上服务器真实暴露的工具 ✓：
	//   协议名   memory.recall
	//   注册名   agentmemory__memory-recall      （probe 打印的、claude-cli 用的）
	//   agent 名 MCP.agentmemory.memoryRecall    （**模型真正调的就是这个** ✓）
	//
	// 只查其中一层 ⇒ 另两层错了照样绿 ✓，
	// 而写错的那一层的后果是「agent 找不到那个工具」✓（不报错、就是不记 ✓）。
	plain := map[string]bool{}
	re := regexp.MustCompile(`memory\.[a-z][a-z0-9_]*`)
	for _, loc := range re.FindAllStringIndex(doc, -1) {
		if loc[0] > 0 && isPartOfLargerName(doc[loc[0]-1]) {
			// 这是**更大标识符/路径里的一段** ✓，不是工具名 ✓：
			//   …/memory.sqlite          （路径里的文件名 ✓）
			//   verify-openclaw-memory.sh （脚本名 ✓）
			//   agentmemory.memoryRecall  （工具名的驼峰形 ✓ → 切成 memory.memory ✗）
			//
			// 这条排除是**必须的**：把它们算成工具名就是一次假红 ——
			// 而假红是门禁的头号死法（团队会开始整体绕它，绕过的门禁等于没有）。
			continue
		}
		plain[doc[loc[0]:loc[1]]] = true
	}
	if len(plain) == 0 {
		t.Fatal("文档里一个 memory.x 工具名都没有：文档与代码脱节了")
	}

	prefixes := map[string]bool{}
	aliased := map[string]bool{}

	// 注册名：<服务器名>__memory-<名字>
	nsRe := regexp.MustCompile(`([A-Za-z0-9_]+)__memory-([a-z]+)`)
	for _, m := range nsRe.FindAllStringSubmatch(doc, -1) {
		prefixes[m[1]] = true
		aliased["memory."+m[2]] = true
	}
	// agent 能调的名字：MCP.<服务器名>.memory<驼峰>
	agentRe := regexp.MustCompile(`MCP\.([A-Za-z0-9_]+)\.memory([A-Z][A-Za-z]*)`)
	for _, m := range agentRe.FindAllStringSubmatch(doc, -1) {
		prefixes[m[1]] = true
		aliased["memory."+strings.ToLower(m[2])] = true
	}
	if len(aliased) == 0 {
		t.Fatal("文档里没有改写后的工具名：提示词约定那节多半没了")
	}

	served := servedToolNames(t)
	for name := range plain {
		if !served[name] {
			t.Errorf("文档提到的工具 %q 服务器并不提供（文档过时了？）", name)
		}
	}
	for name := range aliased {
		if !served[name] {
			t.Errorf("文档里改写后的工具名 %q 服务器并不提供（改写规则对不上？）", name)
		}
	}
	for name := range served {
		if !plain[name] {
			t.Errorf("服务器提供了工具 %q，文档里没提（白名单清单过时了？）", name)
		}
	}

	// 改写后的前缀必须等于**示例配置里的服务器名** ✓：
	// 前缀取自配置 ✓，所以示例与文档必须说同一个名字 ✓，
	// 否则提示词里的名字在真机上不存在 ✓。
	cfg := loadOpenclawExample(t)
	srvNames := keysOf(cfg.MCP.Servers)
	for p := range prefixes {
		if !contains(srvNames, p) {
			t.Errorf("文档里的服务器名前缀 %q 与示例配置的 %v 对不上（提示词里的名字在真机上不存在）", p, srvNames)
		}
	}
}

// contains 切片里有没有这个值。
func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// isPartOfLargerName 这个字节是不是「更大标识符/路径」的一部分。
//
// 判据：字母数字、下划线，或路径分隔符（/ - _ .）✓。
// 于是 `agentmemory.memoryRecall` 里的 `memory.memory` 被判为「更大标识符的一段」✓，
// 而 `memory.recall`（前面是反引号或空格 ✓）不被误伤 ✓。
func isPartOfLargerName(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '/', b == '-', b == '.':
		return true
	}
	return false
}

// servedToolNames 起一个 MCP 服务器，把工具清单取回来。
func servedToolNames(t *testing.T) map[string]bool {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "m.sqlite")
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"
	var out, errb bytes.Buffer
	code := cli.RunWithIO(context.Background(),
		[]string{"mcp", "--db", db, "--namespace", "t"},
		strings.NewReader(in), &out, &errb)
	if code != 0 {
		t.Fatalf("起 MCP 服务器失败: %d %s", code, errb.String())
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("tools/list 应回一条，实际 %d 条: %q", len(lines), out.String())
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("tools/list 响应不是合法 JSON: %v (%q)", err, lines[0])
	}
	out2 := map[string]bool{}
	for _, tl := range resp.Result.Tools {
		out2[tl.Name] = true
	}
	if len(out2) == 0 {
		t.Fatal("tools/list 没有工具")
	}
	return out2
}
