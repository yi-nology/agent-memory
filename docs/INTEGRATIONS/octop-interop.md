# 与 octop-memory 的 `.hmpkg` 互通（对账说明）

本文是给 **octop-memory 侧**的对账说明：怎么用本仓产出的黄金样本验「两边真的能互通」。

> ⚠️ **两侧没有共享 CI** ✗ ⇒ 「互通」没法靠「两边都跑一遍」证明 ✓。
> 可行的形态是：**本仓产出一个固定的包** ✓，对方拿它跑一次 ✓，
> 断言条数与关键字段 ✓，把结果回报回来 ✓ —— 就是这份文档要做的事 ✓。

## 黄金样本

- 文件：`operations/portable/testdata/octop-golden.hmpkg`（**逐字节固定** ✓）
- 布局：**octop 的布局** ✓ —— `manifest.json` + `data.jsonl.gz` ✓
- 记录行：`{"v":1,"table":"<表名>","data":{…}}` ✓
- 内容：7 张表各 1 条 ✓（raw_events / candidates / entities / aliases / atoms / entity_pages / journal ✓）
- 时刻全部固定为 `2026-10-21T12:00:00Z` ✓ ⇒ 重新生成不会变 ✓

⚠️ **样本必须逐字节固定** ✓ —— 不然对账时对方拿到的就是另一份输入 ✓，
「对方读到几条」这种结论也就没有意义了 ✓。

重新生成（改了种子数据或导出形状时）：

```bash
go test ./operations/portable/ -run Golden -update
```

## 对方侧怎么跑

对方的入口是 `octop-memory portable adopt` ✓（照对方 `adapters/cli/portable_cmd.py` ✓）：

```bash
# 1. 先干跑：不写目标库，只看会读到什么
octop-memory portable adopt octop-golden.hmpkg --as <host>:<namespace> --dry-run

# 2. 真导入
octop-memory portable adopt octop-golden.hmpkg --as <host>:<namespace>
```

⚠️ `--as` 的格式是 `host` 或 `host:namespace` ✓（对方 help 里写明 ✓），
不写 namespace 时会取 `~/.openclaw/openclaw.json` 里配的那个 ✓。

跑完请回报这三样 ✓：

1. **applied 的条数** ✓ —— 应为 **7** ✓（每表 1 条 ✓）；
2. **errors 列表** ✓ —— 应为空 ✓；
3. **每张表的 applied_by_table** ✓ —— 应与样本的 `row_counts` 一致 ✓。

⚠️ **先看 `--dry-run`** ✓：dry-run 的结果里 applied=0 而 errors 非空 ✓
⇒ 那是「键名对不上」这类问题 ✓，而**不 dry-run 直接导入**的话，
对方会先备份目标库再导 ✓、导完说「成功」✓，而实际是 0 条 ✓ ⇒ 难查 ✓。

## 契约（照对方源码抄下来的，可直接对照）

### 对方**必填**的字段

缺任何一个 ⇒ **那一整表都导不进去** ✓。
而对方的导入器把这类错误收进 `summary.errors` 后 `continue` ✓
⇒ 宿主看到的是「导入成功、0 条」✓ ⇒ **静默** ✓。

| 表 | 必填字段 |
|---|---|
| `raw_events` | `id` `host` `timestamp` `event_type` |
| `candidates` | `id` `candidate_type` `created_at` |
| `atoms` | `id` `entity_id` `candidate_id` `assertion` `verbatim_quote` `quote_event_id` `occurred_at` `created_at` |
| `entities` | `id` `entity_type` `canonical_name` `created_at` |
| `aliases` | `alias` `entity_id` `entity_type` `created_at` |
| `entity_pages` | `id` `entity_id` `created_at` `updated_at` |
| `journal` | `id` `timestamp` `action` |

### 两边**名字不一样**的那几处（本仓已按对方那套写 ✓）

| 表 | 本仓的叫法 | octop 的叫法 |
|---|---|---|
| `raw_events` | `Type` | **`event_type`** |
| `candidates` | `Type` | **`candidate_type`** |
| `entity_pages` | `Body` | **`summary_markdown`** |
| `entity_pages` | `AtomIDs` | **`topics`** |
| `journal` | `At` | **`timestamp`** |
| `journal` | `Kind` | **`action`** |
| `journal` | `Summary` | **`note`** |

⚠️ 这批字段**用本仓的叫法就完了** ✓ —— 对方按名字取 ✓，
取不到就抛 `missing required field` ✓ ⇒ 整表跳过 ✓ ⇒ 静默 ✓。

### 取值域

对方的枚举是**闭集** ✓（`types.py` 里的 `Literal` ✓）。
本仓导出时按对照表翻译 ✓，翻译不出、且对方域内没有的 ⇒ 落到兜底值 ✓
（事件类型 → `manual` ✓、候选类型 → `Fact` ✓、候选状态 → `pending` ✓、
判定者 → `auto` ✓）。

| 概念 | 对方的取值域 |
|---|---|
| 事件类型 | `user_message` `assistant_message` `tool_call` `tool_result` `host_memory_write` `session_start` `session_end` `compaction` `manual` |
| 候选类型 | `Fact` `Decision` `Task` `Preference` `ConflictCandidate` |
| 候选状态 | `pending` `promoted` `rejected` `needs_review` `conflict` `extractor_failed` |
| 判定者 | `auto` `user` `rule` `portable` |
| 置信/重要度 | `low` `medium` `high` |

⚠️ **候选类型的大小写不一样** ✓（本仓 `fact` ↔ 对方 `Fact` ✓）——
那不是笔误，是两边的词表不同 ✓ ⇒ 本仓显式翻译 ✓。

## 已知的有损之处（先说清，免得对账时当成 bug）

| 东西 | 怎么处理 | 为什么 |
|---|---|---|
| **记录级归属**（`scope` ✓） | **不导出** ✓（带归属的记录整条跳过 ✓，并在清单的 `skipped_scoped` 里记账 ✓） | 对方那边**没有归属字段** ✓ ⇒ 带过去就变成**全局可见** ✓ ⇒ 那是**内容泄漏** ✓ ⇒ 宁可少，不可漏 ✓ |
| 事实的**时效类别**（`kind` ✓） | 不带过去 ✓ | 对方没有这一栏 ✓ ⇒ 落回缺省（**长期经验、不淘汰**那一类 ✓）——落的是安全的一侧 ✓ |
| 生效区间（`valid_from`/`valid_to` ✓） | 不带过去 ✓ | 同上 ✓（零值 = 无区间 = 不因时效被排除 ✓） |
| 画像的**归属视角** | 不导出 ✓ | 同「归属」那条 ✓ |
| 画像的 `topics` | 对方的主题词 ↔ 本仓的事实卡标识 ✓ **不是一回事** ✓ ⇒ 读入时不互相映射 ✓ | 硬映射会把主题词当事实卡标识存进去 ✓ |
| 审计的**对象** | 本仓是「事实卡/候选/实体」三合一 ✓，对方拆成三个 `target_*` ✓ ⇒ 本仓按**动作类型**分流 ✓ | 动作本身才说明对象是哪一类 ✓ ⇒ 不是猜 ✓ |
| 审计的 `action` 词表 | 两边不是一套 ✓ ⇒ 只翻确实对应的 ✓，翻不准的**原样带过去** ✓ | 硬翻成「看起来对」的词是造假 ✓ |
| 实体的**类型** | 原样带过去 ✓ | 本仓的实体类型是**宿主给定的自由字符串** ✓，没有可对照的词表 ✓ |

## 时间格式

本仓写的是 RFC3339（`2026-10-21T12:00:00Z` ✓，零值时刻是 `0001-01-01T00:00:00Z` ✓）。
对方的 `datetime.fromisoformat` 要 **Python ≥ 3.11** 才认 `Z` 后缀 ✓
（对方 `pyproject.toml` 写的是 `requires-python = ">=3.12"` ✓）⇒ 没问题 ✓。

## 反向：对方产出一个包给本仓

本仓的 `memory import` **能自动识别两种布局** ✓（本仓原生 ✓ 与 octop 布局 ✓）。
对方产出的包可以直接喂给本仓 ✓：

```bash
memory import --db /tmp/memory.sqlite --namespace imported --pkg <对方产的包>.hmpkg
```

⚠️ 读入方向的对称问题（键名/取值）已按同一份对照表处理 ✓，
但**跨仓那一半仍然要对方跑一次** ✓ —— 因为本仓这边验不到「对方产的东西长什么样」✓。
