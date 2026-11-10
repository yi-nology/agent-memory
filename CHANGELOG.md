# 更新日志

本仓的版本号语义：纯修复 patch、有新面（新参数/新行为口径）minor。
每段对应一个 tag，条目按「改了什么 / 为什么」成对写——只写改动的
话，半年后没人记得当初为什么改。

## v0.12.0（2026-11-11）——issue 清零第四轮（#29-#44）

行为修正 + 少量新面（`store.AtomFilter.ScopeExact`、导入幂等探测面），
按本仓版本语义取 minor。

### 存储（sqlite / postgres / parity）

- SQLite 侧 `SaveRaw` / `SaveCandidate` 补 `ON CONFLICT ... DO UPDATE`，
  与 PG 的 upsert 对齐：事件与候选的重放/重试（at-least-once 捕获、崩溃恢复
  补写、迁移包重导）不再在本地后端撞唯一键。parity 补 raw / candidate
  两条「重复保存是更新」断言（此前只有事实卡那一条）。(#29)
- `ListActiveEntities` 排序回归契约「按最近提及倒序」：`pinned DESC`
  从两后端的 ORDER BY 里摘掉（钉住语义只属于淘汰，不属于读取序——
  它会让代词「那个」恒定指到三天前的钉住项），并列时刻补
  `entity_id` 兜底键。召回层再显式按 `LastSeen` 排一次（纵深防御），
  parity 补「钉住项不改变读取顺序」断言。(#33)
- 检索路（FTS）与列表路的缺省条数归一为同一个常量（50）：
  `AtomFilter.Limit` 的「0=后端默认」在两条路上不再是 50 对 10 的
  5 倍差距，parity 补同源断言。(#37)
- `store.AtomFilter` 新增 `ScopeExact`（归属严格等于，连全局的也不要）：
  矛盾/取代检测的「同归属比较」语义此前表达不出来，只能在 Go 里后置
  过滤——被滤掉的条目已占掉 LIMIT 配额，矛盾/取代静默漏判。
  `checkConflict` / `checkUpdate` 改走取数层过滤，两后端各加一条
  `scope = ?` 条件，parity 补「配额不被全局/别家吃掉」断言。(#32)
- （核实项）PG 侧 `ListCandidates` / `ListDirtyEntityPages` 的全序兜底键
  在 v0.11.0（#26，commit c661fbd）已落地并带 parity 断言，
  issue #44 描述的是 v0.11.0 之前的旧代码（issue 引用的 commit
  6ab60cb 不存在于本仓）。本轮核实后无需改动。(#44)

### 召回（pipeline/recall）

- 语义路（混合召回）的过滤条件与两条词面路**单源**（`atomFilter`）：
  补齐 `EventSince`（过期事件不再从语义路绕回候选池）与实体限定
  （`RestrictAtomsToEntities` 不再只在词面路生效，显式限定为空时语义路
  同样一条不带），`Limit` 对齐 `len(ids)`（候选池不再按后端默认 50
  静默截断），并补与宽松路同款的纵深防御复查。(#31 #36)
- `fuse` 片段计量失败改为**上抛**：此前 `return lexical` 把整条融合结果
  （含宽松与语义两路候选）静默丢弃、退回纯词面且返回 nil 错误——
  注释写的与代码做的相反。(#35)

### 晋升（pipeline/promotion）

- `classifyUpdate` 解析失败与「关系串不认识」同款处置：送人工
  （conflict），不再上抛——上抛会回滚整笔晋升、候选永远停在 pending，
  反复重试同一输出永远失败。error 只留给「模型调用失败」。(#42)

### 装配面（application）

- `markPageDirty` 补认 `DecisionUpdate`：取代（值变了）改的是断言正文，
  画像不标脏就永久停在旧值——与同函数内向量轨的口径一致。(#43)

### 查询理解（pipeline/query）

- 时间词表删掉今/明/昨的**单字备选**（「说明/声明/证明/如今/至今」
  不再被抽成时间窗、吞掉词面），「大前天」成为独立规则（-3 天，
  不再被子串命中错成 -2 天）。(#38)
- 代词表拆成两条规则：多字形态（这个/那个/这些/那些）任意位置可认；
  单字形态（它/他/她/此/该/本）只认**词首**（句首或前一字符不是汉字）
  ——「其他/脚本/应该/因此/日本/版本」不再被当代词误消解、撕坏词面。(#39)

### 生命周期（lifecycle）

- 合并审计条目的签名截断改 rune 安全、分隔符替换目标从 `\x00` 改为
  `\x1f`（`types.SignatureSep`）：中文断言不再被切成非法 UTF-8
  （PG 侧整条审计写失败），0x1F 控制符不再漏进人读的 Summary；
  四个动作（GC/合并/保留期/过期）的审计写失败从 `_ =` 改为**上抛**——
  「退役/合并必须留审计条目」的承诺不再静默失效。(#40)

### 提炼三面（extractor / episode / page）

- LLM JSON 解析统一收编 `oekit/pkg/jsonrepair`（与事实更新判定同款）：
  尾逗号、全角冒号、散文包裹不再让候选/情节/画像**整批作废**；
  各面的语义判定（枚举拒绝、summary/headline 缺失即失败）保持不变。(#41)

### 契约件（ports）

- `NewBudget(total<=0, shares)` 不再把桶上限乘成 0：总量不限时
  登记桶同样不设上限（与 `NewBudget(0, nil)` 一致），「只想用分桶配额」
  的宿主不再一条都收不下、且丢弃原因误报 DropQuota。(#34)

### 迁移包（operations/portable）

- `Import` 兑现幂等承诺：新增可选探测面 `ExistingProbe`（store.Store
  天然满足），已存在的记录跳过并计数——`ImportReport.Skipped` 从死字段
  变成可观测量；目标实现了事务面时**整包导入包在一个事务里**，
  「宁可一句都不导」从注释变成行为。Target 写面保持冻结不加方法。(#30)

### 依赖

- 全量升级（lib/pq v1.12.3、modernc.org/sqlite v1.62.0、oekit/pkg v0.42.0、
  textx v0.10.7 等），全量测试绿。
- guardx 钉回 v0.8.2：v0.24.1 的 Masker 输出形态变了（掩码括号不再
  配对，脱敏断言红）——脱敏是安全面，升级必须有测试背书。

## v0.11.0（2026-11-09）

见 tag 说明与 PR #28（第三轮 issue 清零 #21-#27）。
