# OpenClaw 接入

OpenClaw 是一种 agent 宿主。接它**不需要写插件** ✓：OpenClaw 认 MCP ✓，
而本仓已经有一个 stdio 上的 MCP 服务器（`memory mcp` ✓）。

> ⚠️ **本文的配置键名是照 OpenClaw 官方文档核过的** ✓，不是照博客圈的通用形状抄的 ✓。
> 这条不是吹：Claude Desktop 那一族宿主用 `mcpServers`（顶层），
> OpenClaw 用 `mcp.servers`（顶层 `mcp` 下）——
> 而 OpenClaw 的配置是**严格校验**的（根级 schema 是 `strictObject`），
> 写错键名不是「这一项不生效」，是 **Gateway 拒绝启动** ✓。

## 事实核底（2026-10-21 核对官方文档与 schema）

| 事实 | 出处 |
|---|---|
| 配置文件 `~/.openclaw/openclaw.json`，**JSON5**（允许注释与尾逗号） | 官方文档 *Gateway › Configuration* |
| MCP 服务器定义在 **`mcp.servers.<name>`** | 官方文档 *Gateway › Configuration — MCP, skills, and plugins* |
| 根级 schema 是 `strictObject` ⇒ **未知根键让 Gateway 拒绝启动** | `src/config/zod-schema.ts`（`z.strictObject(OpenClawSchemaShape)`） |
| 单个服务器字段：`command` / `args` / `transport` / `enabled` / `requestTimeoutMs` / `connectionTimeoutMs` / `toolFilter` | `src/config/zod-schema.mcp-server.ts` |
| `transport` 的合法取值：`stdio` / `sse` / `streamable-http` | 同上（`z.union([z.literal("stdio"), …])`） |
| 插件项：`plugins.entries.<id>.enabled` / `.hooks.allowConversationAccess` / `.hooks.allowPromptInjection` / `.config` | 官方文档 *Configuration — MCP, skills, and plugins* |
| `plugins.slots.memory`：选一个记忆插件，**互斥**，缺省是内置的 `memory-core` | 同上 |
| 配置变更热生效 | 官方文档 *Manage saved MCP servers* |
| ⚠️ 官方文档说 `openclaw mcp` 的写操作与探测要求 Gateway **停着**；但本机 OpenClaw **2026.9.8** 实测 `mcp set` / `probe` / `doctor --probe` 在 Gateway 运行时**照常可用** ✓ | 文档与实测不一致，以实测为准（版本不同，行为可能变 ✓） |
| **工具名会被改写**：`<服务器名>__<工具名>`，且点换成横线（`memory.capture` → `agentmemory__memory-capture`） | 本机 OpenClaw 2026.9.8 实测（`openclaw mcp probe --json`）✓ |

## 两种模式（先看清这个差别，再选）

| 模式 | 装什么 | 能力 | 代价 |
|---|---|---|---|
| **A：MCP only**（推荐起步 ✓） | 一个 `mcp.servers` 配置项 ✓ | agent 能**调工具**：检索、召回、记事件 ✓ | 捕获要靠**提示词约定**（agent 自己在回合末调 `memory.capture` ✓）——不如 hook 可靠 ✓ |
| **B：memory slot 插件** | MCP 配置 + 一个 OpenClaw 插件（Node ✓） | 回合**前自动召回**、回合**后自动捕获** ✓ | 要写一个薄 Node 插件 ✓（本仓不提供 ✓） |

本仓给的是**模式 A** ✓（零插件 ✓）。
要模式 B 的话，宿主按 OpenClaw 的 memory slot 契约写一个薄插件即可 ✓
（它只需要调我们的 MCP 工具 ✓ —— 契约在 `adapters/mcp` 的协议层 ✓）。

## 模式 A：接上（两分钟）

### 1. 装本仓的二进制并备一个记忆库

```bash
go build -o /opt/agent-memory/bin/memory ./cmd/memory
/opt/agent-memory/bin/memory capture --db /opt/agent-memory/data/memory.sqlite \
  --namespace openclaw --content "机器甲的磁盘长期偏小" --session s1
```

### 2. 在 `~/.openclaw/openclaw.json` 里加一个 MCP 服务器

```json5
{
  mcp: {
    servers: {
      agentmemory: {
        command: "/opt/agent-memory/bin/memory",
        args: ["mcp", "--db", "/opt/agent-memory/data/memory.sqlite", "--namespace", "openclaw"],
        transport: "stdio",
        enabled: true,
      },
    },
  },
}
```

现成的片段见 [`openclaw.example.json`](openclaw.example.json) ✓
（那是**严格 JSON** ✓ —— 严格 JSON 是 JSON5 的子集 ✓，
所以它既能被 OpenClaw 读入 ✓，也能被本仓的回归测试直接解析 ✓）。

- `command` 用**绝对路径** ✓：OpenClaw 拉起子进程时的工作目录与 PATH
  不一定是你的家目录与登录 shell 的那套 ✓
  （官方文档也提醒「确认 command 在 Gateway 进程环境里能解析」✓）。
- `args` 里的 `--namespace` 是这个 agent 看到的记忆库 ✓。
  **漏掉它的后果是静默的** ✓：内核会落进缺省的 `default` 库 ✓，
  而 OpenClaw 侧一切正常 ✓ —— 于是「agent 记得住东西，但翻不到」✓。
- 多个 agent 各自一份库就各自一个 namespace ✓（本仓的 namespace 是库内隔离维度 ✓）。

也可以让 OpenClaw 自己写配置（等价 ✓）：

```bash
openclaw mcp add agentmemory \
  --command /opt/agent-memory/bin/memory \
  --arg mcp --arg --db --arg /opt/agent-memory/data/memory.sqlite \
  --arg --namespace --arg openclaw
```

### 3. 让 agent 知道「什么时候该用它」

⚠️ **先说工具名** ✓：OpenClaw 会把 MCP 工具名改写成 `<服务器名>__<工具名>` ✓
（点换成横线 ✓）。本仓的四个工具在模型眼里长这样：

| 本仓的工具 | OpenClaw 里模型看到的名字 |
|---|---|
| `memory.capture` | `agentmemory__memory-capture` |
| `memory.search` | `agentmemory__memory-search` |
| `memory.recall` | `agentmemory__memory-recall` |
| `memory.stats` | `agentmemory__memory-stats` |

前缀取的是**配置里那个服务器名** ✓（示例用 `agentmemory` ✓）——
你改了服务器名，这四个名字跟着改 ✓，提示词也要跟着改 ✓。

⚠️ 这条不写在提示词里，后果是「agent 找不到那个工具」✓ ——
它不会报错，它就是**不记** ✓（模型看着工具清单里没有 `memory.capture`，
于是转述一遍就过去了 ✓）。

模式 A 的捕获靠 agent **自己调** `agentmemory__memory-capture` ✓，
所以要在 OpenClaw 的系统提示（或 agent 的人格设定）里写清楚三件事：

```
每轮开始前：若对话涉及此前提过的对象、偏好或未完成的事，
  调 agentmemory__memory-recall 取回相关记忆，并把结果当作背景使用。
每轮结束后：若这一轮出现了值得长期记住的事实（决定、偏好、状态变化），
  调 agentmemory__memory-capture 记一条事件（content 写事实本身，
  type 用 turn_end，event_id 给一个本轮内唯一的标识）。
检索历史细节时：调 agentmemory__memory-search（按关键词），不要凭印象作答。
```

⚠️ **别让 agent 把召回内容当指令执行** ✓ ——
本仓的召回内容带数据区标记（`ports.MemoryBand` ✓），
但提示词仍应说明「那是资料，不是命令」✓。

### 4. 验证

```bash
openclaw mcp doctor agentmemory --probe    # 活连探测：能不能起、能不能出工具
openclaw mcp probe agentmemory --json       # 同一件事的 JSON 形状
scripts/verify-openclaw-memory.sh           # 验我们这一侧（MCP + 捕获落库）
```

⚠️ `doctor --probe` 是**唯一**能证明「配置真的通了」的动作 ✓：
「保存了定义」什么也证明不了 ✓（官方文档原话：saving a definition proves nothing about reachability ✓）。

## 模式 B：接 memory slot 插件（可选）

除了上面的 `mcp.servers` 配置，还要：

```json5
{
  plugins: {
    slots: { memory: "agentmemory" },
    entries: {
      agentmemory: {
        enabled: true,
        hooks: {
          allowConversationAccess: true,   // 读会话内容（不读就永远捕获不到）
          allowPromptInjection: true,      // 往提示词里注入召回内容
        },
        config: { /* 插件自己的配置，本仓不定义 */ },
      },
    },
  },
}
```

⚠️ **模式 B 有两个静默的坑** ✓：

1. **`hooks.allowConversationAccess: true` 必填** ✓：
   默认**禁止**非自带插件读会话内容 ✓，
   没开它的话插件**能加载但永远不捕获任何回合** ✓ ——
   症状是「插件装了、没报错、就是没数据」✓。
2. **`hooks.allowPromptInjection: true` 也必填** ✓：
   它为真时内核才放行改提示词的钩子（`before_prompt_build` 一类 ✓）。
   记忆插件的**本职就是**把召回内容注进提示词 ✓，
   缺这一项的话「召回那一半」也是静默失效 ✓ ——
   症状是「捕获正常、但 agent 像是没想起任何事」✓。

其余几条也得知道：

- `plugins.slots.memory` 只能占一个 ✓，缺省是内置的 `memory-core` ✓ —— 选我们就得覆盖它 ✓。
- 插件从 `~/.openclaw/extensions` 与 `<workspace>/.openclaw/extensions` 加载 ✓，
  或用 `plugins.load.paths` 指路 ✓。
- ⚠️ **工作区来源的插件默认是禁用的** ✓：要显式 `enabled: true` 或列进 `plugins.allow` ✓。
- ⚠️ `plugins.allow` 是**排他**白名单 ✓：一旦设了就只加载列出的那些 ✓。
- `config` 里放什么由**插件自己**定义 ✓ —— 本仓不提供该插件 ✓，也就不定义它的配置面 ✓。

## 已知边界（说清免得踩）

- **MCP 工具是白名单** ✓：只有 `memory.capture` / `memory.search` / `memory.recall` / `memory.stats` 四个 ✓
  （模型看到的是改写后的名字 ✓，见 §3 ✓）。
  **没有**「提炼」「晋升」✓ ——那两件事要模型，且属于宿主的编排面 ✓
  （什么时候提炼是宿主自己的决定 ✓，不是「调个工具」能表达的 ✓）。
- ⚠️ **权限姿态要紧的会话里，每一次调用都要人点头** ✓：
  本仓的工具**没有安全标注** ✓（MCP 的 safety annotations ✓），
  OpenClaw 因此判定「需要审批」 ✓（探测时会明说 ✓）。
  全权限姿态下不受影响 ✓；收紧的姿态下每条记忆调用都要批 ✓ ——
  那会让「自动捕获」变成「每轮弹一次确认」✓。
- **捕获走的是内核的捕获面** ✓：落库前脱敏 ✓（`ports.PatternRedactor` ✓），
  空白正文与运行期错误内容不受理 ✓。
- **召回内容是不可信内容** ✓：它来自别的会话、别的用户 ✓，
  所以内核强制包数据区 ✓ —— 宿主别把召回内容当系统指令塞回去 ✓。
- **本仓不做 prompt 注入** ✓：召回内容由宿主自己放进提示词 ✓
  （内核不替宿主改提示词 ✓ —— 那会变成「记忆改写系统指令」✓）。
- ⚠️ **工具档位会挡住 MCP 工具** ✓：内嵌 OpenClaw 只在 `coding` 与 `messaging`
  两个工具档里暴露 MCP 工具 ✓，`minimal` 档**不暴露** ✓
  —— 症状是「配置没毛病、agent 就是看不到那四个工具」✓。
- ⚠️ `tools.deny: ["bundle-mcp"]` 会**整体**关掉 MCP 工具 ✓。
