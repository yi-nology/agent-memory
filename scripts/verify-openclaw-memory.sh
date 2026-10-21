#!/usr/bin/env bash
# verify-openclaw-memory.sh —— 验「OpenClaw 接本仓」这件事。
#
# # 两段验证，分开报
#
# A. **本仓侧**（任何机器都能跑 ✓）：
#    按 docs/INTEGRATIONS/openclaw.example.json 里那份**参数向量**起 memory mcp，
#    跑一遍真协议往返（initialize → tools/list → memory.capture → memory.stats），
#    确认两件事：
#      ① 一次捕获之后，库里多了一条原始事件；
#      ② 那条事件落在**配置指定的那个库**里，且缺省库里一条都没有。
#
#    ② 是这段验证里最要紧的一条：`--namespace` 传错或没传，症状**不是报错**——
#    内核落进缺省库，一切看起来都正常，于是「agent 看着能记，实际翻不到」。
#    只断言「事件数 +1」的话，那种失效形式照样绿。
#
# B. **宿主侧**（装了 OpenClaw 的机器才有 ✓）：
#    `openclaw mcp doctor <name> --probe` —— 官方文档说这是**唯一**能证明
#    「配置真的通了」的动作：「保存了定义」什么也证明不了。
#
#    默认**只读** ✓：配置里已经有 agentmemory 就探它 ✓；
#    没有就跳过并告诉人怎么配 ✓。
#    传 --install-probe 则临时装一个定义、探完**恢复原状** ✓
#    （临时二进制 + 临时库 ✓，探完就删 ✓，不留痕 ✓）。
#
# # 为什么 A 段要读示例配置而不是自己写一套参数
#
# 因为示例配置是**用户真会照抄的那份**。脚本自己写一套参数，
# 验的就是「我自己写的那套能用」，而用户照抄的是另一份 ⇒ 两份会漂。
# 读同一份，脚本的结论才落在文档上。
#
# # 退出码
#
#   0  本仓侧通过（宿主侧要么通过、要么被明确标注为「未验证」）
#   1  本仓侧不通过
#   2  传了 --require-openclaw，而宿主侧过不了
#
# ⚠️ **未验证不等于通过** ✓：宿主侧没跑就是没跑，
# 脚本会把它单独列出来，而不是混进「通过」里 ✓。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
EXAMPLE="$ROOT/docs/INTEGRATIONS/openclaw.example.json"

REQUIRE_OPENCLAW=0
INSTALL_PROBE=0
for arg in "$@"; do
	case "$arg" in
	--require-openclaw) REQUIRE_OPENCLAW=1 ;;
	--install-probe) INSTALL_PROBE=1 ;;
	*)
		printf '用法: %s [--require-openclaw] [--install-probe]\n' "$0" >&2
		exit 2
		;;
	esac
done

say() { printf '%s\n' "$*"; }
ok() { printf '  ✓ %s\n' "$*"; }
bad() { printf '  ✗ %s\n' "$*"; }
skip() { printf '  ○ %s\n' "$*"; }

TMP="$(mktemp -d)"
# 装了临时定义就一定要清掉 —— 脚本不该在用户环境里留东西。
INSTALLED=""
cleanup() {
	if [ -n "$INSTALLED" ]; then
		openclaw mcp unset "$INSTALLED" >/dev/null 2>&1 || true
	fi
	rm -rf "$TMP"
}
trap cleanup EXIT

# ── 前置 ──
for tool in go python3; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		say "verify: 缺 ${tool}，跑不了"
		exit 1
	fi
done
if [ ! -f "$EXAMPLE" ]; then
	say "verify: 找不到示例配置 $EXAMPLE"
	exit 1
fi

say "verify-openclaw-memory: 本仓侧"

# ── 构建 ──
BIN="$TMP/bin/memory"
if ! (cd "$ROOT" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/memory) 2>"$TMP/build.log"; then
	say "verify: 构建失败"
	cat "$TMP/build.log"
	exit 1
fi
ok "构建了 $BIN"

# ── 取示例配置（只把 --db 的**值**换成临时库）──
#
# 换的是**值**，不是**参数名**：参数名换了，脚本要跟着发现（那正是要验的）。
DB="$TMP/db.sqlite"
eval "$(python3 - "$EXAMPLE" "$DB" <<'PY'
import json
import shlex
import sys

example, db = sys.argv[1], sys.argv[2]
with open(example, encoding="utf-8") as fh:
    cfg = json.load(fh)

servers = cfg.get("mcp", {}).get("servers", {})
if not servers:
    sys.exit("示例配置里没有 mcp.servers")
name = "agentmemory" if "agentmemory" in servers else sorted(servers)[0]
srv = servers[name]

args = list(srv.get("args", []))
if "--db" not in args:
    sys.exit("示例配置的 args 里没有 --db")
args[args.index("--db") + 1] = db

# 直接 emit shell 赋值：值都过一遍 shlex.quote，
# 于是带空格的路径也不会把 shell 的词法拆坏。
print("SERVER_NAME=%s" % shlex.quote(name))
print("EXAMPLE_COMMAND=%s" % shlex.quote(srv.get("command", "")))
print("ARGS=(%s)" % " ".join(shlex.quote(a) for a in args))
PY
)"

if [ -z "${SERVER_NAME:-}" ] || [ "${#ARGS[@]}" -eq 0 ]; then
	say "verify: 没从示例配置里取到参数"
	exit 1
fi
NS=""
for i in "${!ARGS[@]}"; do
	if [ "${ARGS[$i]}" = "--namespace" ] && [ $((i + 1)) -lt "${#ARGS[@]}" ]; then
		NS="${ARGS[$((i + 1))]}"
	fi
done
if [ -z "$NS" ]; then
	say "verify: 示例配置里没有 --namespace"
	exit 1
fi
ok "示例参数向量（服务器名 ${SERVER_NAME}）：${ARGS[*]}"

# ── 协议往返 ──
REQ_INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
REQ_LIST='{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
REQ_CAP='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory.capture","arguments":{"event_id":"ev-verify-1","content":"验证脚本记下的一条事实：磁盘长期偏小","type":"turn_end"}}}'
REQ_STAT='{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory.stats","arguments":{}}}'

RESP="$(printf '%s\n' "$REQ_INIT" "$REQ_LIST" "$REQ_CAP" "$REQ_STAT" | "$BIN" "${ARGS[@]}")"
printf '%s\n' "$RESP" >"$TMP/resp.jsonl"

# 响应是 JSON-RPC，**工具结果里的文本又是一层 JSON**（被转义过的那层）。
#
# 所以这里用解析而不是 grep：grep 匹配的是**转义后的字节**，
# 判据写成 '"accepted":true' 会匹配不上（实际字节是 \"accepted\":true），
# 而「判据匹配不上」与「功能没成」在终端上长得一模一样 ——
# 那正是把实现改坏去迎合判据的起点。
#
# ⚠️ 响应走**文件**而不是管道给 python：脚本本体用 heredoc 喂给 python，
# heredoc 会占住 stdin，管道进来的内容就被顶掉了（读到 0 行）。
#
# 解析出来的结论以 shell 变量交给后面用（键值对，值已做 shell 转义）。
eval "$(python3 - "$TMP/resp.jsonl" <<'PY'
import json
import shlex
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    lines = [l for l in fh.read().splitlines() if l.strip()]
if len(lines) != 4:
    sys.exit("预期 4 条响应，实际 %d 条" % len(lines))

init = json.loads(lines[0])
tools = json.loads(lines[1])
cap = json.loads(lines[2])
stat = json.loads(lines[3])

for name, resp in (("initialize", init), ("tools/list", tools),
                   ("memory.capture", cap), ("memory.stats", stat)):
    if resp.get("error"):
        sys.exit("%s 报错: %s" % (name, resp["error"]))

cap_text = json.loads(cap["result"]["content"][0]["text"])
stat_text = json.loads(stat["result"]["content"][0]["text"])
tool_names = [t["name"] for t in tools["result"]["tools"]]

print("HAS_PROTO=%s" % shlex.quote("1" if init["result"].get("protocolVersion") else "0"))
print("TOOLS=%s" % shlex.quote(" ".join(tool_names)))
print("ACCEPTED=%s" % shlex.quote("1" if cap_text.get("accepted") else "0"))
print("RAW=%s" % shlex.quote(str(stat_text.get("raw_events", -1))))
PY
)"

if [ "${HAS_PROTO:-0}" != "1" ]; then
	bad "initialize 没有 protocolVersion"
	printf '%s\n' "$RESP"
	exit 1
fi
ok "initialize 回了 protocolVersion"

for t in memory.capture memory.search memory.recall memory.stats; do
	case " ${TOOLS:-} " in
	*" $t "*) ;;
	*)
		bad "tools/list 里没有 ${t}（实际：${TOOLS:-}）"
		printf '%s\n' "$RESP"
		exit 1
		;;
	esac
done
ok "tools/list 给了四个工具"

if [ "${ACCEPTED:-0}" != "1" ]; then
	bad "捕获未被受理"
	printf '%s\n' "$RESP"
	exit 1
fi
ok "memory.capture 被受理"

if [ "${RAW:-x}" != "1" ]; then
	bad "统计里的原始事件数不是 1（实际 ${RAW:-x}）"
	printf '%s\n' "$RESP"
	exit 1
fi
ok "memory.stats 报 raw_events=1"

# ── 直接查库：事件落在配置指定的那个库里，且缺省库是空的 ──
COUNT_NS="$("$BIN" stats --db "$DB" --namespace "$NS" | tr -d ' \n' | grep -o '"RawEvents":[0-9]*' | cut -d: -f2)"
COUNT_DEF="$("$BIN" stats --db "$DB" --namespace default | tr -d ' \n' | grep -o '"RawEvents":[0-9]*' | cut -d: -f2)"
if [ "${COUNT_NS:-0}" != "1" ]; then
	bad "库 $NS 里的原始事件数应为 1，实际 ${COUNT_NS:-0}"
	exit 1
fi
ok "库 $NS 里有 1 条原始事件"
if [ "${COUNT_DEF:-0}" != "0" ]; then
	bad "缺省库里不该有事件（说明 --namespace 没生效），实际 ${COUNT_DEF:-0}"
	exit 1
fi
ok "缺省库是空的（--namespace 生效了）"

# ── B 段：宿主侧 ──
say ""
say "verify-openclaw-memory: 宿主侧"
HOST_STATUS=0
if ! command -v openclaw >/dev/null 2>&1; then
	skip "本机没装 openclaw —— 宿主侧未验证（不是通过）"
	skip "装好之后照 docs/INTEGRATIONS/openclaw.md 配一段，再跑："
	skip "  openclaw mcp doctor $SERVER_NAME --probe"
	HOST_STATUS=2
else
	if ! openclaw mcp list 2>/dev/null | grep -q "$SERVER_NAME"; then
		if [ "$INSTALL_PROBE" -eq 1 ]; then
			# 临时装一个定义：探完就撤 ✓（cleanup 里撤 ✓）。
			#
			# 装的是**临时二进制 + 临时库** ✓：所以探测的是「刚构建出来的那份」✓，
			# 而不是用户机器上某个可能早就过期的安装 ✓。
			DEF="$(python3 - "$BIN" "$DB" "$NS" <<'PY'
import json
import sys

print(json.dumps({
    "command": sys.argv[1],
    "args": ["mcp", "--db", sys.argv[2], "--namespace", sys.argv[3]],
    "transport": "stdio",
    "enabled": True,
}))
PY
)"
			if openclaw mcp set "$SERVER_NAME" "$DEF" >"$TMP/set.log" 2>&1; then
				INSTALLED="$SERVER_NAME"
				ok "临时装了 $SERVER_NAME 这个定义（探完撤销）"
			else
				bad "临时安装失败"
				cat "$TMP/set.log"
				HOST_STATUS=1
			fi
		else
			skip "openclaw 里还没有 $SERVER_NAME 这个定义（照文档配置后重跑本脚本；"
			skip "  想让本脚本自己装一个临时定义并探完撤销，加 --install-probe）"
			HOST_STATUS=2
		fi
	fi

	if [ "$HOST_STATUS" -eq 0 ]; then
		if openclaw mcp probe "$SERVER_NAME" --json >"$TMP/probe.json" 2>"$TMP/probe.log"; then
			PROBE_TOOLS="$(python3 - "$TMP/probe.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    data = json.load(fh)
print(" ".join(data.get("tools", [])))
PY
)"
			ok "openclaw mcp probe 通过（能起、能出工具）"
			# 工具名会被改写成 <服务器名>__<工具名> ✓，
			# 而**模型只认改写后的名字** ✓ ⇒ 这里必须按改写后的形状断言 ✓。
			for t in capture search recall stats; do
				case " $PROBE_TOOLS " in
				*"${SERVER_NAME}__memory-$t"*) ;;
				*)
					bad "探测到的工具名里没有 ${SERVER_NAME}__memory-${t}（实际：${PROBE_TOOLS}）"
					HOST_STATUS=1
					;;
				esac
			done
			if [ "$HOST_STATUS" -eq 0 ]; then
				ok "四个工具都按 <服务器名>__memory-<名字> 的形状注册了"
			fi
		else
			bad "openclaw mcp probe 没通过"
			cat "$TMP/probe.log" 2>/dev/null || true
			HOST_STATUS=1
		fi
	fi
fi

say ""
if [ "$HOST_STATUS" -eq 0 ]; then
	say "verify-openclaw-memory: 全绿（本仓侧 + 宿主侧）"
	exit 0
fi
if [ "$HOST_STATUS" -eq 2 ] && [ "$REQUIRE_OPENCLAW" -eq 1 ]; then
	say "verify-openclaw-memory: 宿主侧未验证（--require-openclaw 要求它必须过）"
	exit 2
fi
if [ "$HOST_STATUS" -eq 2 ]; then
	say "verify-openclaw-memory: 本仓侧全绿；宿主侧未验证"
	exit 0
fi
say "verify-openclaw-memory: 有项目没过"
exit 1
