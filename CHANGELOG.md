# agent-memory CHANGELOG

> 本文件由 `matrix-tools/scripts/gen_changelogs.py` 从 git 历史生成，按 tag 切段、取每段内的 conventional commit 渲染。

## [v0.13.0] — 2026-11-13

- 重构（**BREAKING**）：删 `recall.Result` 三条旧丢弃字段——归因只留 `Drops` 通用表

**下游要做什么：**
- 读 `res.DroppedByBudget`（预算装不下）→ `res.Drops[ports.DropBudget]+res.Drops[ports.DropQuota]`
- 读 `res.DroppedByDiversity`（单实体超额）→ `res.Drops[ports.DropDiversity]`
- 读 `res.DroppedByDuplicate`（内容重复）→ `res.Drops[ports.DropDuplicate]`
- memory.recall 工具（bridge）返回 JSON：`dropped_by_budget` / `dropped_by_diversity` /
  `dropped_by_duplicate` 三键移除，改读 `drops` 表（按 `ports.DropReason` 分路，键为
  budget/quota/low_score/expired/duplicate/diversity）+ `dropped` 合计；`count`/`entities`/
  `rendered` 不变
- CLI `recall` 命令 stderr 统计口径不变（仍打印 预算/多样性/重复 三路）

细节：`DroppedByBudget/DroppedByDiversity/DroppedByDuplicate` 是 v0.12.0 引入
`Drops`（ports.Budget 通用表）时刻意保留的投影兼容面，两张口径并存一个版本周期后
清掉旧面，归因入口收敛到单源。仓内消费方（recall 构造处、cloneResult、bridge、cli）
全部改读 `Drops`；测试保留「合计=通用表之和」不变量并钉住分路可定位。
`Period` / `CaptureInput` 包内简写别名系架构简写非历史兼容，保留不动。
验证=make check 全绿（fmt/vet/test/architecture-check/tidy-check）。

## [v0.12.1] — 2026-11-12

- 杂务：高版本 Go 现代化——sort→slices/cmp、range-over-int、builtin min（零行为变更）

- sort.Strings/sort.Ints/自然序 sort.Slice → slices.Sort；score 降序/多键比较器 → slices.SortFunc/SortStableFunc（cmp.Compare/strings.Compare，零向量与除零已在上游防护，分数恒有限）
- 计数循环 for i := 0; i < N; i++ → for i := range N / for range N（parity/sqlite/postgres/synthetic）
- 复合上界 len(gains)&&i<k → for i := range min(len(gains), k)；手写 if m>k{m=k} → builtin min（evaluation/metrics.go）
- 不动 go.mod、不动生成代码；gofmt/vet/build/test 与 -race 抽查全绿


## [v0.12.0] — 2026-11-11

### 修复：agent-memory 第四轮 issue 清零（#29-#44）

* fix(storage): SQLite SaveRaw/SaveCandidate 补 upsert、活跃栈排序回归契约、
  检索/列表缺省 Limit 单源、AtomFilter.ScopeExact 取数层过滤（#29 #33 #37 #32）
#29 SQLite 侧两张表（raw_events/candidates）是纯 INSERT，第二次写同 ID 撞
唯一键——事件与候选的重放/重试在本地后端直接失败，而 PG 是 upsert，违反
parity 承诺。补 ON CONFLICT(namespace,id) DO UPDATE（字段集与 PG 对齐），
parity 补 raw/candidate 两条「重复保存是更新」断言。
#33 ListActiveEntities 的 ORDER BY 带 pinned DESC，与契约「按最近提及倒序」
相反：只要栈里有钉住项，代词「那个」恒定指到三天前提及的那个。两后端摘掉
pinned、补 entity_id 兜底键；召回层显式按 LastSeen 排序（纵深防御）；parity
补「钉住项不改变读取顺序」断言（与淘汰那条分开）。
#37 AtomFilter.Limit 的「0=后端默认」两条路各写一个数（列表 50、检索 10），
同参数候选池差 5 倍。归一为单源常量 defaultFetchLimit=50，parity 补同源断言。
#32 矛盾/取代检测的归属过滤在 LIMIT 之后做：别家（乃至全局）的事实吃光配额，
矛盾与取代静默漏判。AtomFilter 新增 ScopeExact（归属严格等于，连全局的也不
要），两后端各加 scope = ? 条件，checkConflict/checkUpdate 改走取数层过滤，
parity 补「配额不被全局/别家吃掉」断言。
#44（核实项）PG ListCandidates/ListDirtyEntityPages 的全序兜底键在 v0.11.0
（#26，c661fbd）已落地并带 parity 断言，issue 引用的 commit 6ab60cb 不存在
于本仓——本轮核实无需改动，CHANGELOG 记录核实结论。
* fix(recall): 语义路过滤单源化 + fuse 片段失败上抛（#31 #36 #35）
#31/#36 语义路（hybrid.collectSemantic）独缺 EventSince 与实体限定：过期
事件与别家实体的事实照样进 prompt，且不带 Limit——后端默认 50 小于 ids 数
时融合名次尾部静默截断。三路过滤收敛到 atomFilter 单源构造，语义路补齐
EventSince/EntityIDs/Limit=len(ids) 与宽松路同款的纵深防御复查；显式限定为
空时语义路同样一条不带。
#35 fuse() 片段计量失败 return lexical——注释写「上抛」代码写静默丢弃全部
融合候选且返回 nil 错误。改为上抛（与 collectSemantic 同口径）。
* fix(promotion/application): classifyUpdate 解析失败送人工、取代后画像标脏
  （#42 #43）
#42 解析失败分支返回 error 让整笔晋升回滚、候选永远 pending——与 godoc
「判不出来→conflict→送人工」相反。改为与「关系串不认识」同款送人工，
error 只留给模型调用失败。
#43 markPageDirty 只认 DecisionPromote：取代（值变了）改的是断言正文，不标
脏则画像永久停在旧值。补 DecisionUpdate，与同函数向量轨口径一致。
* fix(query): 时间词/代词单字备选的误命中（#38 #39）
#38 今/明/昨的单字备选把「说明/声明/如今/至今」抽成时间窗并吞词面；删除
单字备选（全形态已列全）。「大前天」成为独立规则（-3 天），不再被子串命中
错成前天（-2 天）。
#39 代词表单字备选（它/他/她/此/该/本）几乎总在词内（其他/脚本/应该/因此/
日本/版本）：误消解到最近实体 + 撕坏词面。拆两条规则——多字任意位置可认，
单字只认词首（句首或前一字符非汉字，RE2 无逆向断言用前导消耗+捕获组表达）。
* fix(lifecycle/pipeline): 审计签名截断 rune 安全 + 审计失败可见 + 三面收编
  jsonrepair（#40 #41）
#40 short() 裸字节截断把中文断言切成非法 UTF-8（PG 侧审计整条写失败又被
`_ =` 吞掉），分隔符替换还在替换 NUL 时代的 \x00。改 rune 安全 + 替换 \x1f；
四个生命周期动作的审计写失败从吞掉改为上抛。
#41 extractor/episode/page 三个手搓解析器（取首个 {/[ 到末个 }/] +
json.Unmarshal）收编 oekit/pkg/jsonrepair，与 update 判定同款口径：尾逗号/
全角冒号/散文不再让候选/情节/画像整批作废；各面语义判定（枚举拒绝、
summary/headline 缺失即失败）不变。
* fix(ports/portable): Budget 总量不限不再拒收 + Import 幂等与原子性（#34 #30）
#34 NewBudget(total<=0) 把每个桶的 cap 乘成 0——「只想用分桶配额」的宿主
一条都收不下且误报 DropQuota。总量不限时桶不设上限。
#30 Import 注释承诺「已存在跳过」实际重导直接报错，ImportReport.Skipped
从未被写。新增可选探测面 ExistingProbe（store.Store 天然满足，Target 写面
保持冻结），已存在跳过并计数；目标实现事务面时整包导入包在一个事务里
（「宁可一句都不导」从注释变成行为）。探测代码独立成 import_probe.go。
* chore(deps): 全量升级 + guardx 钉回
lib/pq v1.12.3、modernc.org/sqlite v1.62.0、oekit/pkg v0.42.0、textx v0.10.7
等，全量测试绿。guardx v0.24.1 的 Masker 输出形态变了（掩码括号不再配对，
脱敏断言红）——钉回 v0.8.2 并在 go.mod 注明。
CHANGELOG.md 首建（v0.12.0 段），门禁 make check + go test -race 全绿。

## [v0.11.0] — 2026-11-09

### 修复：agent-memory 第三轮 issue 清零（#21 #22 #23 #24 #25 #26 #27）

* fix(query): parseCount 中文数字组合分支可达 + Config.WeekStart 三条周规则接活（#25 #27）
#25 单字数字分支对多字输入提前 return，『二十/二十三』两条组合分支不可达：
『最近二十天』抽不出窗口（零值=不约束 ⇒ 时间过滤整体失效）且时间词留在词面。
#27 Config.WeekStart 只被赋值、从不被读，三条周规则写死 time.Monday；
字段改为 *time.Weekday（time.Weekday 零值恰是周日，值类型区分不了
『没配』与『配成周日』——正是死配置静默落回周一的根源）。
⚠️ BREAKING：Config.WeekStart 类型 time.Weekday → *time.Weekday。
该字段此前被证明是死配置（配了也不生效），改动只影响显式赋值过的调用点。
* fix(store): SaveAtom 退役态单向保护与 created_at 定格、淘汰复核 pinned、PG 列表补全序兜底键（#21 #22 #26）
#21 SaveAtom 整卡 upsert 无任何保护：陈旧写者拿退役前的读视图回写可把
已退役卡复活；PG 上两个晋升器并发取代同一张旧卡，后提交者抹掉先提交者的
取代链。两后端统一改为『退役态只许单向置位』（superseded_by/deprecated_at/
valid_to 三列 CASE 保护），并删掉 PG 侧 created_at = excluded.created_at
与 SQLite 对齐（FindAtomBySignature 的取卡口径两后端一致）。
#22 EvictActiveEntities SELECT 排除 pinned 但 DELETE 不复核：并发落钉
落在两步之间时钉住实体照删。条件删除（pinned 进 DELETE 谓词）把复核与
删除原子化，被钉住的行不算被淘汰也不上报。
#26 PG 侧 ListCandidates/ListDirtyEntityPages 补全序兜底键（created_at
DESC+id DESC / updated_at ASC+entity_id ASC+scope ASC），与 SQLite 侧
逐字一致；parity 补并列时刻顺序确定断言。
* fix(storage): 行与 FTS 索引同事务落库并加重建路径、ensureSchema 并发开库串行化（#23 #24）
#23 SaveAtom/SaveRaw/SaveCandidate 的『行 + FTS 索引』此前是三个独立
autocommit：行 INSERT 与索引 DELETE/INSERT 之间崩溃即永久静默漏命中。
统一封装进内部 Transaction（嵌套复用外层事务），DeleteRawBefore 的
行删与孤儿索引清同样收进事务；新增可选能力 store.FTSIndexRebuilder 与
SQLite 实现 RebuildFTSIndex（从真源全量重建三张虚表，修历史失配存量），
CLI 挂 rebuild-fts 命令。PG 词元向量与正文同行落库，无独立索引可失配，
不实现该能力。
#24 ensureSchema 此前逐条 autocommit：多进程并发开库（WAL 下服务 + CLI
同开一份库是设计内场景）双方都读到 hasColumn=false，后者的 ALTER 撞
duplicate column → Open 失败。SQLite 侧改单条连接 BEGIN IMMEDIATE
（先拿写锁再读版本，整段一个事务）；PG 侧单事务 + pg_advisory_xact_lock
串行化。OpenFile 的 DSN 把 busy_timeout 放在 journal_mode 之前，
连接初始化的 WAL pragma 撞锁时等锁而不是立刻 BUSY。
---------
Co-authored-by: Murphy-Yi <zhangyi@Murphy-YideMac-Mini.local>

## [v0.10.0] — 2026-11-06

### 修复：QA 分母口径统一 + Search 文档互斥修正 + Merge 原子性语义声明（issue #17 #18 #19）

#17 QA 分母只计参与评分的用例：
- Cases/Sufficient 分母不再含 Skipped（无 SupportIDs 用例）——与检索口径
  （metrics.go skipped 不进任何分母）同一条规则；Skipped 单列数进
  QAMetrics（藏起来的数字没法看趋势，列出来的数字不污染比率）
- AvgTokens 分母保持全量（skipped 用例也跑了检索，是真实开销）
- 钉测试：1 参与用例 + 1 skipped ⇒ 充分率必须为 1（旧实现得 0.5）
#18 Search 三方文档互斥修正：
- doc 头是旧语义残留（「归属过滤在取回事实卡那一步」），按实现与
  index.go 契约改写：归属过滤在 Index 层（scope 原样透传，索引侧过滤）；
  本层过取只为补失效事实槽位，不为归属——over-fetch 取值的判据归位
#19 Merge 原子性语义声明 + 幂等收敛钉子：
- doc 显式声明：多步写、非原子、失败可安全重跑；两条不变量写明
  （每步只迁 from 侧当前剩余 / from 脏计数被退役终态抹平），
  宿主的中途失败处置只有一句：重跑同一条 Merge
- TestMergeIdempotentConvergence：注入中途失败 → 重跑只搬剩余 2 张 →
  from 计数清 0 → 收敛后再跑是空操作
Closes #17, closes #18, closes #19

### 修复：Reindex 收编向量空间一致化 + 维度拒绝带指引 + FTS tokenize 选项化

application（issue #13）：
- 新增 Service.Reindex(ctx, ns, limit)：补算走 Service 自身 embedder/索引
  装配（与 Capture 写路径同源），CLI 与宿主共用同一实现，维度/向量空间
  天然一致；幂等，失败带已写入条数返回；缺件显式报 ErrNoEmbedder/ErrNoIndex
- CLI Options.Embedder 注入（缺省 HashEmbedder 保留并标注失真警告）；
  cmdReindex 改走 Service.Reindex，适配层不再手写 ListAtoms+Embed+Upsert
storage/sqlite（issue #14）：
- checkVecDim 拒绝路径 wrap 带操作指引：namespace、存量 dim、本次 dim、
  「清 atom_vectors（索引是派生物可安全重建）」标准动作进错误文案；
  errors.Is 判定不破坏
storage/sqlite（issue #15）：
- Open/OpenFile/OpenMemory 变参 Option（现有调用零改动）；
  WithFTSTokenize 白名单（unicode61/trigram）——tokenize 拼进 DDL，
  非白名单值打开即报错防注入；中文宿主切 trigram 的取舍与存量库迁移
  指引写进 doc
- 附注：本仓写路径 ftsBody 已走 LexicalTokens 汉字逐字切词补偿
  （unicode61 之弊对 SDK 路径已被应用层绕开，TestFTS5CJKSubstringSearch
  钉着）；trigram 的价值面是不经 ftsBody 的数据（raw SQL 直插）
Closes #13, closes #14, closes #15

- 文档(plan)：轨道 B 再订正——宿主已到 v0.9.0，契约面冻结后适配层零改动

issue #12 修完并发布 v0.9.0 后，宿主（扁鹊）已升到 v0.9.0
（提交 00a50c1d），且**适配层一行未改**即编译干净。
⇒ 「破坏面清单」补 v0.9.0 一段：宽松检索**移出**契约面成为可选能力
（实现了就启用，没实现则回落严格路并报出来），调用方改用探测；
已发布接口的**方法集从此冻结**。
⇒ B-3 的状态如实改写：🟡 部分跑了（全仓 build 0 + 三包测试绿），
**宿主全量 CI 仍未跑**（不写成「全绿」）。


## [v0.9.0] — 2026-10-24

### 新增(portable)：octop 互通——把「字段名其实对不上」这件事查出来并修掉

做黄金样本时拿对方源码逐字段对了一遍，发现原来的「octop 兼容导出」
只做到了**布局对**，**字段名没对** ⇒ 对方实际只能读进 2 张表。
对照的来源是对方仓库源码（不是文档、不是推测）：
  src/octop_memory/types.py（dataclass 字段名 = 对方的导出面）
  src/octop_memory/operations/migration/import_.py（_build_* = 对方的读入面）
两边的模型是「同一个设计的两个实现」，**不是同一个模型**，有一批字段
名字本来就不一样：
  raw_events   Type    ↔ event_type
  candidates   Type    ↔ candidate_type
  entity_pages Body    ↔ summary_markdown、AtomIDs ↔ topics
  journal      At      ↔ timestamp、Kind ↔ action、Summary ↔ note
而对方的读端是**按名字取**的（d["event_type"]），取不到就抛
missing required field ⇒ **那一整表都导不进去**；更糟的是它**不响**——
对方把这类错误收进 summary.errors 后 continue ⇒ 宿主看到的是
「导入成功、0 条」。
⇒ 机械翻译（Go 字段名 → snake_case）这条思路是错的，换成逐字段对照
  （operations/portable/octopmap.go），并连带删掉了已无用的翻译层。
⇒ 导出时**自检对方必填字段**：缺一个就在本仓这边先红，而不是等对方
  那边「导入成功但 0 条」。
⇒ 取值也要翻：事件类型/候选类型/候选状态/判定者四组枚举词表不同
  （候选类型大小写都不一样：fact ↔ Fact）。翻译不出且不在对方域内的
  落到兜底值——**不许原样送过去**，送一个对方不认的值等于那条记录在
  对方的逻辑里不存在（静默）。
另外两条纪律落进了代码：
- **带归属的记录不导出**：对方没有归属字段，带过去就变成全局可见 ⇒
  内容泄漏。宁可少不可漏，且在清单里记账（skipped_scoped），不然宿主
  只看到「导出成功、少了几条」却不知道为什么少。
- **候选列表排序要有全序**（created_at DESC, id DESC）：否则同刻创建的
  候选先后不定 ⇒ 导出的包逐次不同 ⇒ 黄金样本没法固定。
黄金样本（operations/portable/testdata/octop-golden.hmpkg）：
- 逐字节可复现（为此给 ExportOptions 加了注入时钟的用法——octop 布局的
  导出原先直接用真实时钟，与原生布局两套口径；候选表也补了排序兜底键）。
- 用例钉住：可复现、布局与键名（对方那套叫法 + 不该出现的本仓式叫法）、
  必填字段齐整、读回本仓后逐字段对上。
- .gitignore 加了例外：别处的 .hmpkg 都是本地产物不入库，黄金样本
  恰恰相反——对方拿到的必须是仓库里那一份。
对账说明见 docs/INTEGRATIONS/octop-interop.md：确切命令
（octop-memory portable adopt <pkg> --as host:ns --dry-run）、要回报的
三样、契约表、以及已知的有损之处（归属、时效类别、生效区间、topics
与事实卡标识不是一回事等）。
反向验证：把 event_type 改回 type ⇒ 导出自检当场红；去掉归属拦截 ⇒
记账用例红。

### 新增(integration)：端到端真跑通——工具名有三层，提示词按第三层写

上一版文档里写的 `agentmemory__memory-recall` 是**错的**——那是
`openclaw mcp probe` 打印的注册名（也是 claude-cli 回合用的形状），
不是内嵌 runner 里模型真正能调的名字。是运行中的 agent 自己纠正的：
「工具真名是 MCP.agentmemory.memoryRecall」。
⇒ 工具名有三层，提示词按第三层写：
  协议名   memory.recall
  注册名   agentmemory__memory-recall   （probe 打印 / claude-cli 用）
  agent 名 MCP.agentmemory.memoryRecall （模型真正调的就是这个）
差一层的后果是「agent 找不到那个工具」——不报错、就是不记。
本轮把「问一句 → 召回命中」这一环真跑通了：
- 种一条带标记的记忆 → 让 agent 调 MCP.agentmemory.memoryRecall
  → 它把标记原样说了出来。
- agent 调 MCP.agentmemory.memoryCapture → 库里 RawEvents 从 1 变 2，
  且那条能被 recall 召回。
脚本加了两段：
- --ask：种标记 → 问一句 → 看它说不说得出（会真的调一次模型，显式开关）。
- 开头自查「变量紧跟多字节字符」：bash 会把多字节首字节吞进变量名，
  本轮踩了两次，症状是崩在一个跟问题无关的地方。
测试同步扩到三层名字，并加了前缀一致性断言（文档里的服务器名必须等于
示例配置里的服务器名，否则提示词里的名字在真机上不存在）。
反向验证：把文档里的前缀改成别的 → 变红。
另外两条实测结论也写进文档了：
- 模型凭据在哪决定用哪条命令：`openclaw agent exec` 是本地内嵌回合
  （要本机环境变量），`openclaw agent` 走 Gateway（凭据在 Gateway 那边）。
- memory.search 对只有原始事件、没晋升成事实卡的内容返回 []，那是设计
  使然，不是接线坏了。

### 新增(integration)：OpenClaw 接入——配置片段 + 捕获接线 + 端到端验证

轨道 C 的 C-1/C-2/C-3 落地。接第三方宿主这件事，配置形状必须照它的
schema 源码核，不能照博客圈的通用形状抄——本轮就抄错过一次。
核出来的四件事（都写进了 docs/INTEGRATIONS/openclaw.md）：
1. 配置键是 mcp.servers，不是 mcpServers（后者是 Claude Desktop 那一族的
   形状）。而 OpenClaw 根级 schema 是 strictObject，未知根键让 Gateway
   拒绝启动——写错不是「这项不生效」，是「起不来」。
2. 工具名会被改写成 <服务器名>__<工具名>（点换横线）。所以提示词约定
   必须写改写后的名字：写原名的话 agent 找不到那个工具，不报错、就是不记。
3. 模式 B 有两个静默坑：allowConversationAccess 不开则永远不捕获；
   allowPromptInjection 不开则召回那一半静默失效。
4. 官方文档说 mcp 写操作要求 Gateway 停着，而本机 2026.9.8 实测在
   Gateway 运行时照常可用——以实测为准。
产出：
- docs/INTEGRATIONS/openclaw.example.json：可照抄的配置片段（严格 JSON，
  是 JSON5 的子集，故两边都读得动）。
- adapters/cli/openclaw_example_test.go：三条用例把片段与 CLI 真实参数面
  钉在一起，含「事件必须落在配置指定的库里」这条静默失效的断言。
- scripts/verify-openclaw-memory.sh：本仓侧 + 宿主侧两段验证；--install-probe
  会临时装定义、探完撤销，不在用户环境里留东西。
反向验证（改坏生产代码看用例是否变红）：
- 捕获落进缺省库 → 红；双写（对的库 + 缺省库）→ 「缺省库必须是空的」那条
  断言承重（stats 断言会漏掉这种）。
- 工具改名 → 文档↔服务器双向红。
- 示例里的服务器名改掉 → 前缀一致性红。
- CLI 的 --namespace 改名 → 红。
本机 OpenClaw 2026.9.8 已端到端跑通（openclaw mcp probe 通过，四个工具
都按 <服务器名>__memory-<名字> 注册），配置已恢复原状。

### 新增(portable)：与 octop-memory 的 .hmpkg 互通（导出 + 读入 + 自动识别）

规划见 docs/INTEGRATION_PLAN_20261021.md 轨道 A。这笔做掉本仓侧的三件：
1. **格式核过了**：两侧同扩展名、同版本号，但**布局不同**——
   octop 是 manifest.json + data.jsonl.gz + {v,table,data} 信封；
   本仓是 10 个分类 jsonl + 铺平记录。
   而两侧的**记录体字段几乎逐字段一致**（同一设计的两个实现），本仓是超集
   ⇒ 互通要补的只有**信封 / 表名 / 键名**三样。
2. **导出（--format octop）**：写 octop 的布局 ⇒ **对方零改动**即可读。
   octop 那边已经是那个格式的作者，它的读端现成。
3. **读入 + 自动识别**：Import 见到 octop 布局就自动走另一条读法
   （宿主拿到一个包不该先问「这是谁导的」——而「导入失败」尤其
   「导入成功但内容全空」极难归因）。
4. **键名要显式翻译**：本仓的领域类型**没有 json tag** ⇒ 序列化出来是
   Go 字段名（EntityID），而 octop 是 snake_case（entity_id）。
   键名对不上时 Go 的 json 会**静默丢掉**那个键 ⇒「导入成功但内容全空」。
   故按机械规则翻译（不逐字段手抄），映射表写进包注释（它是契约）。
5. 表名对照与多出字段的处置写进注释：本仓多的 episodes/digests 在
   octop 那边没有 ⇒ 导出跳过、读入为空；反向 octop 多的
   thread_active_entities 本仓没有 ⇒ 跳过。
   本仓多出的 scope/kind/valid_from/valid_to/version 过去后被丢弃，
   回来时取零值 ⇒ 「老数据语义不变」那条纪律在跨仓时同样成立。
用例钉住三样：布局（manifest.json + data.jsonl.gz + {v,table,data}）、
键名（snake_case，不是 Go 字段名）、自环（导出→读回→逐字段相等）。
跨仓那一半（黄金样本对账）需 octop 侧配合，形态见规划 A-3。

### 新增(cli)：memory mcp 入口 + 三条集成线的规划

一、`memory mcp`：以 stdio 跑 MCP 服务器（拉起后一直跑着，与其它命令形态不同）。
   它的价值是让「宿主直接认 MCP」这件事变成**一条命令 + 一份配置** ✓，
   不必写 Node 插件（那是别人的运行时形状决定的，不是协议要求 ✓）。
   为此把 CLI 的入口拆出 RunWithIO（显式给 stdin）—— MCP 要读 stdin，
   而直接用 os.Stdin 就没法单测（起真进程做协议往返既慢又脆）。
   policy 同步加 adapters-mcp 方向（按需依赖：只有那个子命令才装配）。
二、docs/INTEGRATION_PLAN_20261021.md：三条集成线的规划。
   规划前把事实核了一遍，有三条与直觉相反：
   1. **两侧的 .hmpkg 布局并不相同**（同扩展名 ≠ 同布局）：octop 是
      manifest.json + data.jsonl.gz + {v,table,data} 记录壳；本仓是
      10 个分类 jsonl + 铺平记录。⇒ 互通的第一件事是**格式对齐决策**，
      不是写测试（现在直接写，只会得到「两边都过、但对方读不了」的假绿）。
      好消息：两侧的**记录体字段几乎逐字段一致**（同一设计的两个实现 ✓），
      本仓是超集 ⇒ 只差信封与布局。
   2. **OpenClaw 认 MCP**（第三方记忆产品普遍用 MCP + hooks 接它），
      而 octop 写 Node 插件是它的 Python 运行时形状决定的 ✓ → 我们零 Node 代码 ✓。
   3. **扁鹊的画像表是它自己的**（bq_memory_subject_page，PK 无团队列），
      ⇒ 团队维度**不必**先升本仓（给它自己的表加一列 team 即可 ✓）；
      升版换来的是「用内核的页面机制」✓，那是可选收益，不是前提 ✓。
      （订正我此前「必须升 v0.8.0 才能接团队维度」的说法。）

### 修复(store)：对外发布的契约面冻结——宽松检索改走可选接口（issue #12）

v0.8.0 给 store.AtomStore 加 SearchAtomsLoose，把**仓外**的实现者打红了：
宿主仓 bianque 的 `var _ store.AtomStore = (*Store)(nil)` 当场编译失败
⇒ 上游发 tag 的当天下游整个仓编译不过。
本轮拆成三件事：
- **契约面冻结**：宽松检索移出 store.AtomStore 与 recall.Reader，
  落成可选能力 store.AtomLooseSearcher + 探测助手 store.LooseSearcher。
  已实现的下游照旧能用，没实现的也能编译——两种宿主都不再被打红。
  ⚠️「把大接口拆小」**不解决**它：拆开只是把一个大面换成几个小面，
  任何一个被仓外实现的面，加方法仍然打红下游。
- **降级路径**：召回链在 New 里探测能力；没有就整条路不存在，
  回落到严格路（召回照跑），并在 Result.LooseSearchUnavailable 与指标
  recall.loose.unavailable 上说清——降级不许静默。
  判据是「增强 vs 前提」：增强降级（少召回 + 报出来），
  前提响亮失败（捕获面缺了就装配期报错）。
- **防复发**：application/contract_test.go 把已发布接口的方法集钉死，
  改它 = 一次有意的破坏性变更（走大版本 + 发布说明点名）；
  新能力一律走可选接口 ⇒ 清单不用动。
  清单放 application 是因为它是唯一同时看见 store/ports/recall 三张面的地方。
顺带修掉两处「注释替机制吹牛」：
- 宽松路的旧用例是**假绿**：那条「只命中部分词」的事实是从兜底路
  （ListAtoms）进的，不是从宽松路进的——把宽松路整个废掉它照样绿
  （变异验证实测）。新用例让兜底路只给严格命中的那些，
  宽松路坏了用例立刻红。
- cloneResult 上写着「加字段时编译器会指出来」——Go 的结构体字面量
  不要求写全字段，编译器不会指。改由 TestCloneResultCopiesEveryField
  （反射逐字段比对）兜底。
文件体量门禁顺带把 recall.go 顶到 826 行（上限 800），
于是把「结果面」拆成 result.go、把宽松接线拆成 loose.go。

- 杂务(deps)：oekit pkg v0.24.0 / hertz v0.8.3

oekit 依赖抬到最新发布 tag（pkg v0.24.0 / hertz v0.8.3），零 replace。
- pkg v0.24.0：补 cache.Stats 观测计数面 + GetOrLoadWithSource 按调用注入回源（pkg#76 / pkg#77 已修并发布）
- hertz v0.8.3：就绪探活被正常关停打断时不再误报 ERROR「ready probe failed」（oekit/hertz#48 已修并发布）
两项均为增量 API，无破坏性变更。

- 杂务(deps)：oekit 家族升级到本轮最新发布版本

oekit 依赖抬到最新发布 tag（bootstrap v0.6.5 / bootstrap-nacos v0.3.7 / bootstrap-obs v0.2.10 / concurrency v0.20.4 / core v0.12.0 / hertz v0.8.2 / metrics v0.1.9 / middleware v0.13.2 / nns v0.4.4 / pkg v0.23.1 / redisx v0.10.4 / registry v0.5.0 / tracing v0.3.0），零 replace。
pipeline/recall 手搓 TTL 缓存 -> pkg/cache.LocalCache；update_llm 手搓围栏剥离+JSON 切片 -> pkg/jsonrepair.Unmarshal


- 文档(plan)：订正轨道 B 的现状——升级面早已完成，剩下的是画像的团队维度

复核发现先前写的「扁鹊钉着 agent-memory v0.6.0」**已过期**：
- 它**已经升到 v0.8.0**（提交 79cfb88e「批次三百八十八 用最新上游简化本仓」），
- **编译干净**（CGO_ENABLED=0 go build ./... 退出码 0），
- **新能力真在用**（SearchAtomsLoose 14 处、AtomFilter.EventSince 31 处、
  recall.Config.EventWindow 15 处、types.FactKind 14 处、lifecycle.Expire /
  WithEventRetention、GetEntityPage 68 处、MarkEntityPageDirty 61 处）——
  不是「升了但没接线」。
⇒ 轨道 B 的升级面**早已完成**，剩下的**只有画像的团队维度**（B-5）。
⇒ B-5 已在扁鹊仓落地（批次三百八十九）：画像按 (锚点, 队) 分行，堵住
  「各队事实在画像里互相可见」那个泄漏。

- 文档(plan)：三条集成线的进度（A-0…A-2 与 C-0 已落地）

## [v0.8.0] — 2026-10-21

### 新增(recall)：事件窗口缺省与接线——过期这件事要有入口

上一笔把「按类别分治」的能力做出来了，但有两处**没接线**，
而「实现了却没有入口」等于没有（它的症状不是报错，
是「调用它时安静地什么都不做」）。
1. 召回侧事件窗口缺省 = 30 天（与 lifecycle 的事件保留期一致）。
   ⚠️ 不影响既有事实：它们的 kind 是空串（= 长期经验），
   而窗口只作用于事件类——所以开启它不改变任何老数据的行为。
   ⚠️ 两处口径必须一致：一条事件的「过期」在生命周期与召回侧是同一件事，
   口径不同就会出现「生命周期说它过期了、召回却还端上来」的错位
   （症状是「模型把三周前的一次重启当成当前状态」）。
2. 新增 `memory expire [--days N]`：按龄退役**事件类**事实。
   退役是软退役（不删）——被推翻的事实曾经是真的，
   而「当初发生过什么」仍要能查到。

### 新增(recall)：判据面 + 事实时效性分治 + 宽松词面路（基线数字驱动的一轮）

这一轮的起点是一个问题：「我们召回准不准」只能靠感觉。
于是先把它变成**可算、可复现、可对比**的数字，再让数字指出该修什么。
一、判据面（evaluation/）
- token 效率：每次查询的 token（成本）、每千 token 命中数（效率）、
  每命中一条的 token（单位收益成本）。三个一起给——
  只看第一个会被「什么都不召回」骗过去，只看后两个会被「召回巨量内容」骗过去。
- 问答/行为口径：QACase（问题 + 标准答案 + 支撑事实）+ 宿主给的
  `Answerer` / `Judge`（**语义强度归宿主、管线归内核**）；
  没配就是「未测」而不是 0（「没测」与「全答错」必须能分开）。
- 基线与护栏（integration/baseline_test.go + docs/EVAL_BASELINE.md）：
  合成集 40 条走真召回链，数字可复现，且设了下限护栏（明显退化才红）。
二、基线指出来的真问题：词面路是**隐式 AND**，多话题查询打不中
- 现象：Recall@3 之后**不再上升**（部分匹配的事实进不来），
  上下文充分率只有 0.025（一次查询给不出答一道多事实问题的上下文）。
- 修法：**加一条宽松路**（`store.FTS5QueryAny` + `SearchAtomsLoose`），
  与严格路**并列**喂给 RRF 三路融合：完全匹配的那条在两路都出现（融合分最高），
  部分匹配的那些从宽松路进来。
  ⚠️ 不把 AND 改成 OR：噪声会一次性灌进池子，而池内排序靠打分器（词面重叠），
  于是「命中任一词」与「词面重叠高」高度相关，噪声反而排前面。
  ⚠️ 放宽的是**词的匹配方式**，不是**可见性**：归属/时序过滤一条没少。
- 效果（基线数字）：Recall@3 0.533→**0.646**、nDCG@5 0.631→**0.699**、
  上下文充分率 0.025→**0.300**（×12）、每命中一条 372.6→**335.1** token（更便宜）。
三、事实时效性分治（types.FactKind + 存储 + 生命周期 + 召回）
- 三类：短期事件（会过期）/ 有效期状态（被取代）/ 长期经验（不因时间失效）。
  一套策略套所有事实必然两头不讨好：事件被永久留着（污染召回），
  经验被按时间淘汰（丢掉知识）。
- 零值落在**不淘汰**那一类（长期经验）：既有事实卡没有这个维度，
  零值若落在「会被淘汰」的那一类，等于升级一次就丢一批老事实。
- `lifecycle.Expire`：事件类事实按龄**软退役**（不是删除——
  被推翻的事实曾经是真的）；保留期零值回落缺省（30 天），
  「忘配」不能被执行成「全过期」。
- 召回侧 `Config.EventWindow`：事件类事实的默认召回窗口。
  ⚠️ 事件窗口与时间窗是**两个维度**：`OccurredFrom/To` 是「问哪个时间段」
  （用户明确问历史），`EventWindow` 是「默认别把太老的事件端上来」；
  **用户显式问了历史时，窗口让位**。
- 存储侧：atoms 加 `kind` 列（迁移 v7 幂等）+ `AtomFilter.EventSince`
  （只作用于事件类，`COALESCE` 口径与 `types.AtomCard.ValidRange` 一致）。

### 新增(evaluation)：token 效率口径、问答/行为口径与第一组基线数字

P0 的三件：把「我们准不准、贵多少、够不够答」变成**可算、可复现、可对比**的数字。
1. token 效率（evaluation/qa.go）
   - AvgTokensPerQuery：每次查询带出去的 token（**成本**）。
   - HitsPerKToken：每千 token 命中多少条相关事实（**效率**）。
   - TokensPerHit：每命中一条相关事实花多少 token（**单位收益成本**）。
   三个一起给：只看第一个会被「少召回」骗过去（什么都不召回成本最低），
   只看后两个会被「召回巨量内容」骗过去。
   ⚠️ 「命中」用**检索口径**的命中数，不用「上下文充分」——
   充分要求全部支撑事实都在（多事实问题上极罕见），
   拿它当分子会让「每命中一条的 token」大出一个量级（实测 20491 vs 372.6）。
   召回器报不出 token 时标 `Measured=false`，**不冒充 0**。
2. 问答/行为口径
   - QACase：问题 + 标准答案 + 支撑事实（公开基准那种形态）。
   - 充分性：确定性判据（支撑事实齐不齐），不依赖模型。
   - 答对率：需要宿主给 `evaluation.Answerer`（读上下文回答）与
     `evaluation.Judge`（判对错）——**语义强度归宿主、管线归内核**；
     没配就是「未测」而不是 0（「没测」与「全答错」必须能分开）。
   - 合成集新增**问答视图**（同一份语料、同一批用例），
     于是检索口径与问答口径的数字能并排看。
3. 第一组基线数字（integration/baseline_test.go + docs/EVAL_BASELINE.md）
   - 合成集 40 条用例走**真召回链**：Recall@5=0.533、nDCG@5=0.631、
     MRR=1.0、HitRate=1.0、上下文充分率=0.025、每次查询 512.3 token。
   - 数字**可复现**（有专门的用例钉住），并设了**下限护栏**
     （明显退化才红，正常波动不红）。
   - ⚠️ 这组数字**暴露了一个真问题**（见 docs/EVAL_BASELINE.md）：
     词面路是隐式 AND ⇒ 多话题查询几乎打不中、部分匹配的事实进不来
     （Recall@3 之后不再上升）⇒ 上下文充分率极低。
     待办是加一条「宽松匹配」路喂给 RRF 融合，而不是把 AND 改成 OR
     （OR 会把噪声一次性灌进池子）。

### 新增(types)：事实的时效类别（FactKind）与事件召回窗口的过滤面

P2 的第一笔（契约先行）：事实分三类——短期事件 / 有效期状态 / 长期经验，
它们的生命周期不同，而**一套策略套所有事实**必然两头不讨好：
事件被永久留着（污染召回），经验被按时间淘汰（丢掉知识）。
- FactKind 零值 = 默认（按长期经验处理）：既有事实卡没有这个维度，
  零值必须落在**最保守**的一类——零值落到「会被淘汰」的那一类，
  等于升级一次就丢一批老事实。
- AtomFilter.EventSince：事件类事实的发生时刻不得早于它（零值=不限）。
  只作用于事件类：状态由取代链表达过期，经验不按时间筛。
  ⚠️ 它与 OccurredFrom/OccurredTo 是两个维度：后者是「问哪个时间段」
  （用户明确问历史），本字段是「默认别把太老的事件端上来」。
  用户显式给了时间窗时，召回应当不受本字段限制——那由召回层决定。

- 杂务(release)：v0.8.0 版本面（判据面 + 时效性分治 + 宽松词面路）

## [v0.7.1] — 2026-10-21

### 修复(release)：发布闸的 recipe 必须用 bash（v0.7.0 的 tag 闸红在这）

**现象**：v0.7.0 的 tag 上 CI 的发布闸红了，而 main 绿、本地 `make release-check` 也绿。
**根因**：`make` 的默认 shell 是 **/bin/sh**——CI 的 Ubuntu 跑客上是 dash，
而 `release-check` 的 recipe 用了 `set -euo pipefail`（**bash 专有**），
于是它在「set」那一步就报 `Illegal option -o pipefail`。
**为什么本地复现不出来**：macOS 的 /bin/sh 是 bash 的 sh 模式，认 pipefail。
所以「本地跑得通」这件事在 macOS 上**不成立为证据**——
本地复现路径与远端行为不是一回事。
**修法**（两处都改，别只改一处）：
- Makefile 顶部加 `SHELL := /bin/bash`——把「recipe 一律用 bash」显式钉住，
  而不是让每个 recipe 自己记得。
- CI 的 release 段加 `shell: bash`——不依赖 Makefile 的默认值。
⚠️ tag 不移动（它是校验和的锚点），故按纪律**往上一个版本号跳**（v0.7.1）。

## [v0.7.0] — 2026-10-21

### 新增(storage)：持久化向量索引（向量轨跟着记忆库一起活）

此前本仓只有**单进程**的内存索引（ports.MemIndex）——进程一退，索引就没了，
语义召回随之失效，等于这条轨白装。故给两个后端加**持久化**向量索引。
1. 新表 `atom_vectors(namespace, id, scope, dim, vec, updated_at)`
   + 索引 `(namespace, scope)`；迁移 v6（幂等，DDL 与建表脚本共用同一份）。
   向量存 **float32 小端 BLOB**：不用 JSON/文本——
   文本往返两次舍入会丢精度，破坏「向量逐位可复现」。
2. `ports.VectorIndex` 由两个后端直接实现（方法挂 `vectorStore` 窄结构，
   与 edgeStore 同一形状；`var _ ports.VectorIndex = (*Store)(nil)` 编译期钉住）。
   装配面按类型断言取用：宿主显式给了就用宿主的，
   没给而存储自己实现了就用存储的。
3. **归属过滤进 SQL**（`namespace = ? AND (scope = '' OR scope = ?)`）：
   后置过滤会让别家占掉 topK 槽位——「该给我的那条」根本没进候选池，
   而症状只是「语义召回像是没生效」，极难归因。
4. 维度不一致**报错**（`ports.ErrDimensionMismatch`）而不是返回空集：
   空集的失效形态是「语义召回永远空」，而调用方看不出是维度错了。
   零向量跳过（无从比较）；同分按 ID 升序（可复现性的一部分——
   融合层按名次算分，名次不稳则结果不可复现）。
5. 这是**暴力扫描**（小库够用、零外部依赖），注释里写明适用面：
   大规模部署宿主应注入 Chroma / Qdrant / PG 向量扩展。
   本仓不替宿主选向量技术。
6. parity 加一组向量断言（独立入口 `parity.RunVector`：向量不是 store.Store 的一翼，
   共享包又不能 import ports（方向限制），故复述最小面并用泛型适配器桥接——
   端口改形状则各后端测试文件编译不过，形状不会漂）。
反向验证过：去掉归属过滤 → 隔离断言红；去掉维度校验 → 维度断言红。
真库（PG）72 条 PASS、零 skip；-race 绿；make check 全绿。

### 新增(cli)：向量轨接进命令行 + 补算命令；订正「向量不接」的过期定案

1. CLI 装上零依赖的哈希嵌入（不用模型）+ 存储自带的向量索引，
   于是命令行的召回也走语义路。
2. 新增 `memory reindex`：给**存量**事实补算向量。
   向量是写路径产生的（晋升时顺手写），
   于是存量事实没有向量 → 语义召回一条都捞不到，
   症状是「语义轨像是没生效」而查不出原因（它确实在跑，只是索引是空的）。
   补算是幂等的（按事实卡 ID 覆盖写）。
3. 订正两处**过期定案**：
   - AGENTS.md「已定案、勿重复评估」里那条「向量索引不接」已被本轮推翻
     （推翻的理由写清：它把「语义强度」（归宿主）与「管线」（归内核）混成了一件事）；
   - AI_GUIDE 的落点地图与已知限制里「reserved：当前无消费点」的说法已作废。

### 新增(vector)：向量索引可由存储自己提供（装配面自动发现）

宿主传的是「用哪个索引」，而**存储自带的那个是最省心的选择**——
向量跟着记忆库一起活，进程一退索引不消失（内存索引一退就没了，
语义召回随之失效，等于这条轨白装）。
故装配面按类型断言发现：宿主显式给了就用宿主的，
没给而存储自己实现了 ports.VectorIndex 就用存储的；
并把它包一层（事实退役时同步删向量）。
用例钉住「只给向量化器、不给索引」时向量轨照样通。

### 新增(vector)：向量轨真正接通——内核接管线，宿主换语义强度

此前只有一条窄缝（SemanticReader：宿主给「最像的 topK」），
宿主得自己写「算向量 + 搜索引 + 归属过取」那一层胶水；
胶水每家写一遍就会各写一遍（漏了归属过取的那家，语义路的配额会被别家吃掉）。
这一笔让 `ports.Embedder` / `ports.VectorIndex` 这两个**原本「零消费者」的预留口**
真正有消费点：写路径（晋升成事实后写向量）、读路径（召回时算查询向量 →
搜索引 → 与词面路 RRF 融合）、删路径（事实退役时删向量）。
**分工不变**：语义**强度**归宿主（内核不引模型 SDK——核心一旦引了 SDK，
「没配模型也能手工写入 + 词法召回必须能跑」那条降级路径就塌在生产环境里），
但**管线**归内核。
1. ports/vector.go：可跑且**诚实**的实现
   - HashEmbedder：词面哈希嵌入（确定性，标准库 FNV，同义/近义由宿主模型负责）。
     ⚠️ 它**不是语义嵌入**——它捕捉词面软匹配，不是意思相近；
     名字与注释都写死这一点，免得后人把它当成「语义检索已通」。
   - MemIndex：内存向量索引（单进程；多副本部署要注入宿主自己的持久索引）。
   - VectorSemanticReader：把上面两件组合成语义召回面。
   - 切词走上游 textutil.Tokens（与检索面同一套）——两处切法不一致，
     融合出来的结果就不可复现。
2. **归属必须进索引**（这一条是复查时才发现的纪律问题）
   语义路的归属过滤若放在调用方**后置**，被别家占掉的 topK 槽位已经花掉了——
   而「该给我的那条」根本没进候选池、症状却是「语义召回像是没生效」。
   过取只是缓解不是修法。故 `VectorIndex.Search` 带 scope，
   **过滤在取数层生效**（进 SQL，与 TopK 同一次求值）。
   过取仍留着，但只为补**失效事实**占掉的槽位（索引里没有时间维度）。
3. 向量是**派生物**：写路径在晋升后（application 的装配面），
   删路径在退役时（`application.vectorStore` 装饰器：软退役/作废同步删向量）。
   事实不成立而向量还在 → 语义召回先捞到一条**死事实**，
   症状是「语义路命中率莫名偏低」，很难归因。
   写向量失败**不阻断晋升**（事实卡是真源），但留痕（`types.JournalVectorSync`）。
4. **降级**：不给 Embedder/VectorIndex = 不启用，召回**逐条**回到纯词面
   （`TestVectorTrackIsOptional` 比的是**结果逐条相同**，不是「没报错」）。
5. 端口的「reserved」注释已作废：它现在有真的消费点。
反向验证过：断掉写路径 → 集成用例红；断掉读路径（不装配语义召回面）→ 用例红
（第一版这条用例是**假绿**——召回的「高价值兜底」那路把目标事实也捞进来了，
于是它没在守语义路；重写夹具后才真正区分）。

### 新增(graph)：图谱投影与多跳检索

关系型事实投影成边，再按跳数取回相关实体。
边是**派生物**：投影丢了可以按事实卡重投影，事实卡丢了投影救不回来——
两者的地位不对称。故本包只提供「落边 / 清投影 / 多跳取」这套机制，
**不判定任何关系是否成立**（那是事实卡的事，宿主给声明，本包落成边）。
三条不可协商的纪律（都写进了包注释与用例）：
1. **边必须有溯源。** 没有溯源的边是「没人敢删的垃圾」：
   事实卡被推翻后清不掉它，图就开始说谎——而这种失效没有任何报错。
   故没有出处的边**不许落**（ErrMissingSource）。
2. **多跳必须按实体去重。** 关系图里必然有回路，不去重就会无限展开
   （而且它不报错，只是越跑越慢直到把预算耗光）。
   故实体只到达一次：深度即最短到达距离，与路径顺序无关。
3. **归属判定要在存储层之外再判一次。** 漏过滤的失效形态是**串户**
   （把别家的关系接进当前链路），而那在结果里完全看不出来——
   路径看着仍然自洽。漏一条的代价比少一条严重得多。
其余取舍：强度用**乘积**（最弱的一环决定这条链有多可信；求和会让
「多经过几段无关关系」反而变强）；跳数硬顶 4（再深既不可解释也不可预算）；
同强度按到达顺序稳定排序（排序不稳，调用方拿不到可复现的结果）。
反向验证过：去掉环去重 → 环用例与强度用例都红；去掉归属复查 →
「后端漏过滤」用例红。

### 新增(recall)：查询理解 + 混合检索 + 事实更新 + 评测面 + MCP 入口

把「智能召回」那一层接上：问句先被翻译成检索条件，两路召回融合，
事实更新分「取代」与「矛盾」，并给这一层配上了判据与第二个入口。
1. 查询理解（pipeline/query）
   相对时间窗（上周/昨天/最近三天/绝对日期/时段收窄）、代词共指、实体提示。
   **零值=不约束**：抽不出时间窗就不按时间过滤，与既有过滤面一致。
   规则表驱动（可复现），不调模型——模型那一层在「判定」侧。
2. 混合检索（pipeline/recall/hybrid.go + ports.RRF）
   词面路 + 语义路 → RRF 融合 → **决定候选池里有什么**；
   排序仍归打分器（融合分作为一路信号交给它）。
   没装语义面时融合那一行都不生效 ⇒ 行为与装之前逐条一致。
3. 指标面（ports/metrics + 召回埋点）
   耗时/缓存命中/片段数/token/丢弃原因，口径由内核定。
   指标名集中在召回包，宿主只负责搬运——口径分散在两处时数字不可比。
4. 事实更新（pipeline/promotion/update.go + update_llm.go）
   「同一个东西换了值」（1.32→1.36）**不是矛盾**，否定词检查看不见它 ⇒
   补一条独立的取代判定：同一主体 + 差异落在**值**上（形态判据，不用词表）
   ⇒ 新事实取代旧事实，旧事实到期、版本号 +1。
   规则判不清时**问模型**，但模型只回答关系（ADD/UPDATE/DELETE/NOOP/CONFLICT），
   处置由固定表定：**作废与裁决矛盾永远落人工**。模型失败上抛。
5. 缓存键补维度（这一条对自己也成立）
   新加的时间维度与**查询解析结果**都进键：解析依赖活跃实体栈，
   而栈会变——不进键的话，同一句「那个呢」会端上一次的解析结果，
   症状是「同一句话两次问，答案不一样」，看着像随机。
6. 评测面（evaluation/ + integration 适配）
   合成数据集（私持随机源 ⇒ 可复现；带近义干扰 ⇒ 指标分得开）
   + Recall@k / Precision@k / nDCG@k / MRR / HitRate，口径写死在一处。
   integration 里用**真召回链**跑一遍，并钉住「同 seed 两次结果相同」。
7. MCP 入口（adapters/mcp）
   stdio 上的 JSON-RPC：initialize / ping / tools/list / tools/call。
   工具**恰好四个**（capture/search/recall/stats），无逃逸口；
   通知不回复、一条坏消息不废整轮。
8. CLI 有一条自查口（memory query）
   把一句问句翻成「可执行的检索条件」并打印——查询理解是静默的，
   宿主不该靠猜「召回为什么变了」。
9. 装配面：application.Deps 加 Promotion（让宿主能注入晋升用的模型，
   否则「智能事实更新」在真实宿主里永远是死的）。
10. 迁移包的已知取舍写明：关系边**不在包里**（边是派生物，
    包里带的是真源），迁到新宿主后由宿主按事实重投影。

### 新增(storage)：时序事实与关系边 + 混合检索/评测的契约层

给「智能召回」补齐契约与存储：这一笔只落**契约与持久化**，
流水线与入口在下一笔（便于逐层复核）。
1. 时序事实（types/atom.go）
   事实卡加 ValidFrom / ValidTo / Version：旧事实不是被删掉，
   而是「到这一刻为止有效」——于是「现在的状态」与「历史上曾经的状态」
   能同时被查到。ValidFrom 零值取 OccurredAt ⇒ 老数据语义不变。
2. 关系边（types/graph.go + store.EdgeStore）
   边是**派生物**（投影丢了可重投影，事实卡丢了投影救不回来），
   故只给「落边/读边」面，不给任何改写事实的入口。
   溯源（SourceAtomID）是**必需**的：没有溯源的边是没人敢删的垃圾，
   事实被推翻后清不掉它，图就开始说谎。
3. 混合检索与观测的端口（ports/{semantic,rerank,metrics}.go）
   - SemanticReader：语义召回的**窄**缝（宿主给「最像的 topK」，
     内核不碰向量化与索引——那是宿主的事，且向量轨仍标为未验证）。
   - RRF：跨路融合只看**名次**（跨路可比、跨库稳定）；
     加权分数要先归一，而归一依赖数据分布。
   - Metrics：采集面（口径由内核定，宿主只搬运）+
     内存实现（P50/P95 走最近秩，不插值出「不存在的时长」）。
4. 排序侧接住融合分（ports/rank.go）
   RankCandidate 加 FusionScore、RankWeights 加 Fusion（缺省 0）。
   缺省权重为 0 ⇒ 没装语义面时行为与装之前**逐条一致**（零漂移，
   由 ports/rank_test.go 钉住）。
5. 存储层落地（两个后端 + 幂等迁移 v4/v5）
   - v4：atoms 加三列（老库打开即升级，迁移幂等）；
   - v5：建 entity_edges 表与索引（DDL 与建表脚本共用同一份）。
   - AtomFilter 新增 At（生效区间，左闭右开）与 IDs（多路融合后一次取回）。
   - parity 加 7 条断言（时序边界、边的归属隔离/时间过滤/来源清理/两端过滤），
     46 条断言两个后端都真跑。
6. policy 先行：新登记 pipeline-query / pipeline-graph / evaluation /
   adapters-mcp 四个模块，并补上 adapters-cli→pipeline-query、
   adapters-mcp→pipeline-recall+storage-sqlite 两条方向。

### 修复(recall)：排序方向、缓存键隔离与画像按归属分视角

三处静默失效（外部评审点的 P0，源码核实为真）+ 一处附带发现的死功能。
1. 排序方向与端口契约相反（pipeline/recall/rank.go）
   契约明写「分数越大越靠前」，实现是升序。真正致命的不是次序：
   排序后紧接着是配额分配，而分配按次序逐条接收——
   于是方向一反，预算一紧就先丢最该保留的事实，
   症状只是「召回少了」，看着像内容不够。
   附带挖出：那条一直绿的用例是两处方向相反的错互相抵消的产物
   （夹具给第一条最高分 + 升序把它压到最后），绿不等于对。
   修法：方向改回 + 新写两条不依赖那个抵消的用例（次序、预算取舍）。
2. 缓存键漏了 Scope / Entities / Limit（pipeline/recall/recall.go）
   漏 Scope 不是「缓存不准」，是跨租户内容泄漏：
   缓存默认关着只是配置在替代码兜底，一开缓存就泄漏。
   顺带修两处同源小洞：Entities 进键前排序（否则命中率被遍历顺序吃掉）、
   缓存命中也要推活跃实体栈（栈是会话状态，不是召回结果的一部分）。
3. 画像按归属分视角（types/page.go + store.PageStore 全接口）
   画像是事实的摘要，而事实已按归属隔离：摘要若不分视角，
   等于把别家的事实揉进一张所有人都能看的画像。
   逻辑主键扩成 (namespace, entity_id, scope)；scope 是键的一部分而非过滤面
   （与 AtomFilter.Scope 的空值语义刻意不同：全局页若也收别人的事实就是泄漏）；
   全局页只汇总全局事实，读者先取本视角、没有就退到全局页（退过去是少内容，
   绝不是别人的内容）；标脏按归属传播（全局事实一变，所有视角都过时）。
   落地：两个后端加列 + 改主键 + 幂等迁移（SQLite 重建表，整段在一个事务里）、
   页面事实集合在进提示词之前过滤、召回按视角取键 + 全局页回退 + 可见性复查、
   迁移包导出导入带上归属（拍平成全局就是泄漏）、parity 加两条跨后端断言。
4. 附带发现：画像功能是死的
   pipeline/page 的包注释写着「写入侧只标脏」，但全仓没有任何地方标脏——
   于是页面永远停在晋升之前那一版，而宿主侧 page.regen 只重生成脏页面，
   于是它安静地什么都不做。修在装配面（application.Service.Promote → markPageDirty），
   不扩大晋升面的窄接口；标脏失败不阻断晋升（事实是真源、画像是派生物）但留痕。
验证：反向验证过（把三个 bug 改回去，对应新用例逐一变红），
make check 全绿，PG 真库 39 条 parity 断言零 skip，-race 绿。

- 杂务(release)：v0.7.0 发布准备（版本面 + 发布闸的清单同源 + lint 收口）

1. 版本面
   - `adapters/mcp` 的 `ServerVersion` 0.6.0 → 0.7.0（MCP 的 initialize
     会把它报给客户端，版本号对不上会被当成「服务端没升」）。
   - README 的「现在有什么」与破坏性变更清单按 v0.7.0 逐条重写
     （下游要改的清单**逐条列全**，不留「等下补」）。
   - HOST_WIRING 的版本对照表更新，并**如实写明落差**：扁鹊仍钉 v0.6.0，
     落后一个版本；以及「若要接画像的团队维度则必须升 v0.7.0，
     否则得在宿主仓另做一套拆分」。
2. 发布闸：清单**同源**
   CI 的发布段原本与 Makefile 各存一份包清单——两份漂了就是这道闸漏检的
   直接原因（v0.4.0 实跑过：build 了 storage/postgres 却没 get 它，
   报 missing go.sum entry）。故 CI 改为直接调 `make release-check`，
   清单只留一份；并把新增的公开包（pipeline/query、pipeline/graph、
   evaluation、adapters/mcp）补进那份清单。
3. lint 收口（发版不该带新问题）
   - 去掉本次新增的 2 条 ineffassign（一处是 ports/vector.go 的无效赋值，
     一处是 PG 侧末尾那次没人用的 start++）——后者已用真库跑过（占位符
     编号没被改坏，46→39 条 parity 全绿）。
   - 去掉 page_test.go 里已无使用者的 now()。
   现状：22 条（errcheck 10 / staticcheck 5 / unused 7），
   比 v0.6.0 的基线 24 条还少 2 条。


- 文档(wiring)：宿主已升到 agent-memory v0.6.0，归属这条线在宿主侧收口

- 测试(cli)：钉住 memory reindex（补算存量事实的向量，且幂等）

夹具**不走提炼链**：提炼与晋升要模型，模型没配时那条链跑不起来——
用它当夹具会让本用例整个跳过，而跳过的用例不证明任何东西。
故直接写一条「存量事实」（早就入库、没有向量），
这正是补算命令要处理的对象。

- 测试(graph)：补图谱用例（路径与深度自洽、边界与归因）

worker 收尾时补的一批用例，与已提交的实现一并复核后入库：
路径长度与跳数必须自洽（深度 d 的路径该有 d+1 个实体），
否则调用方没法把多跳结果画成链——它会以为结果是坏的，
而真实原因在本包内部。


## [v0.6.0] — 2026-10-20

### 新增(recall)：补记录级归属维度与排序注入缝（issue #10 #11）

两条都是下游实测报上来的缺口，形状不同：读起来像有那个能力，实际没有。
一起改是因为它们落在同一面（召回），且改动互相牵引（都要动 ports/store 契约）。
## 归属维度（issue #10）
`Namespace` 是**整库级**隔离；多组共用一套库时还需要**记录级**归属
（`types.Scope`）：空 = 全局可见（人人都该看到），非空 = 仅同归属可见。
用 Namespace 表达第二层意味着「一条全局事实」得向每个组复制一份——
写放大，且全局事实一新增就得回头补所有组的副本。那是不可行的建模，不是配置问题。
- 归属**在捕获时定下**：事件带着它走 → 候选继承 → 事实卡继承，
  中途不让模型参与（模型看不见归属，让它写只会写出一个编的值）。
  故三张表都加了列：raw_events / candidates / atoms
  （少候选那一列，归属就在「提炼→晋升」之间断掉，这一组的事实会变成全局的）。
- 过滤**在取数层**（进 SQL，与 TopK 配额同一次求值）：后置过滤时，
  被别家占掉的配额已经花掉了，「该给我的那条」根本没进候选池——
  症状只是「召回变少了」，看着像排序问题，根因却在取数。
  召回层另有一道复查兜底（纵深防御：漏过滤的失效形态是**泄漏**不是少取）。
- 原始事件面同样按归属隔离：它是召回的兜底源，一次就取 MaxRaw 条。
- 签名**含归属**（`types.AtomSignature`）：不含的话，跨归属的重复检测会把
  一组的卡并进另一组的卡——那是内容泄漏，不是去重。
  为此全局卡的签名保持与加归属之前**逐字相同**（签名是存起来的列）。
- 过滤条件里归属为空 = **不隔离**（与 `EntityIDs` 空=不限一致）：
  同一份过滤条件里不该有两种空值语义。
**顺带逼出来的一件事**：本仓的建表脚本**没有 ALTER**，「加一列」一直没有
落地路径。加归属列逼出了**版本化迁移**（`storage/{sqlite,postgres}/migrate.go`）：
版本号记在 meta 表，迁移按版本号线性追加，**每段必须幂等**
（版本号可能被改过/覆盖过/库是拷来的 ⇒ 同一段迁移会跑第二遍，
而跑第二遍失败的话**库就打不开了**）；索引归迁移管（建表脚本每次打开都跑，
旧库的表还没有新列，那句 CREATE INDEX 会当场失败）。
## 排序注入缝（issue #11）
- `ports.Ranker` 是端口，缺省实现 `ports.LexicalRanker`
  （词面+重要度+新鲜度，权重 0.6/0.25/0.15）。**缺省下与注入前逐条等价**——
  钉法是「把旧公式在测试里重写一遍当期望值」，而不是「实现自己算自己」。
  契约只有一条要紧的：分数与候选**一一对应且同序**，长度不符即报错
  （错位不会崩，只会静默换序，而换序在结果里完全看不出来）。
  打分失败**上抛**，不许静默回退到缺省（静默回退 = 排序悄悄变回老样子）。
- `types.AtomCard.Vector` 是类型层的落脚点（可选、默认不进持久化）。
  本仓两个后端**不落向量**——带着向量入库**响亮报错**：
  静默丢掉的话症状是「相似度检索永远空」，而它与「存错了地方」毫无相似之处。
- `ports.Embedder` / `ports.VectorIndex` 标注为 **reserved（当前无消费点）**：
  一个定义了却零消费者的导出接口，读起来像能力，实际不是。
## 破坏性变更（下游要跟着改）
- `store.AtomStore.SearchAtoms(ctx, ns, query, f AtomFilter)`（原来收 limit）
- `store.RawStore.ListRaw(ctx, ns, scope, before, p)` / `SearchRaw(ctx, ns, scope, query, limit)`
- `application.Service.Search(ctx, ns, query, f store.AtomFilter)`
- 新增 `recall.Request.Scope` / `capture.Input.Scope`；
  bridge 的 `memory.search` / `memory.recall` / `memory.capture` 与
  CLI 的 `--scope` 都接受归属。
## 验证
- `make check` 全绿（fmt + vet + test + 架构门禁 + tidy）。
- 跨后端断言从 31 条增到 **36 条**（新增归属隔离 4 条 + 候选归属往返 1 条
  + 带向量入库被拒 1 条），两个后端都真跑：PG 侧对着**真实 PostgreSQL**。
- 迁移另有专项用例（SQLite + PG 各两条）：造「旧形状的库」的方式是
  **按新代码建库再把新列去掉**，不是复制一份旧 DDL（复制的那份会与真 DDL 漂移）。

- 杂务(deps)：oekit/pkg 升到 v0.18.0（textx/guardx 已是最新）

v0.18.0 只新增 jsonrepair.ExtractArray 原语（数组面兜底与对象面对称），
无破坏性变更，本仓未触及该包。
依赖契约测试（TestUpstreamMaskerShortHitIsFullyMasked）仍绿：v0.17.0 修好的
「短值整段全量打码」在 v0.18.0 上未回退。


- 文档：README 与 AGENTS.md 同步到 v0.6.0
- 文档：补宿主接线现状，并同步改造指南与纪律

新增 docs/HOST_WIRING_20261020.md——本仓与 agent-harness 的接线现状（事实全部
来自本地核对，每条给出可复查路径）。此前只有 PLAN（决策依据）与 AI_GUIDE
（改造地图），缺「今天实际长什么样」这一层。
顺带把三处已过期的记录同步到当前事实：
- AGENTS.md：脱敏/召回面的实弹教训补到 2026-10-20；补「与 agent-harness 的
  接缝在宿主」与「通用件的验收标准是本仓自己在用」两条定案。
- README.md：端口表补词元计量；存储面按 8 个子接口列出；补 make check 两段门禁
  与 release-check 的说明。
- AI_GUIDE.md：门禁规则分段来源（rules/thresholds/工具侧）说清；加检索口径
  钉子与命名空间隔离的落点。


## [v0.5.1] — 2026-10-20

- 文档：补一份 AI 改造指南——落点地图、验证路径、症状索引

面向接手本仓的 AI 代理。与已有三份文档岔开定位：
README 答「这是什么」，方案文档（50KB）答「为什么这么设计」，
AGENTS.md 答「不许做什么」，本文件答「怎么做」。
docs/AI_GUIDE.md（新增）八节：
1. 30 秒心智模型：主链图 + 依赖方向图 + 六条改不动的红线
2. 改动落点地图：14 行表（要改什么 → 落在哪 → 连带要改 → 门禁注意）
3. 常见改造剧本：加后端 / 加端口 / 改召回 / 加 CLI 命令
4. 验证路径：三级验证 + 反向验证（回退生产代码看用例红不红）+ 门禁自证
5. 症状 → 病因索引：15 条对号入座
6. 60 秒上手路径：4 条可复制命令
7. 已知限制：7 条「别当 bug 修」
8. 改造检查清单：11 项
刻意不复述纪律——AGENTS.md 已写，指南再抄一遍就是双源漂移，
改一边忘了另一边。指南只做导航，真冲突以 AGENTS.md 为准。
开头按任务分三条读法（加东西 / 改行为 / 修 bug），不通读。
README 标题的版本号从 v0.4.0 更新到当前版本（此前过期两版）。
验证：make check 全绿；指南 §6 的上手命令真跑过（capture → stats）；
引用的 37 个文件路径与 TestBucketsAreIndependent 逐个核对存在。


## [v0.5.0] — 2026-10-20

### 新增(ports)：分桶配额与丢弃计数解耦成契约件（issue #9）

「桶 + 配额 + 丢弃计数」原本与召回实现绑死：桶是 pipeline/recall 私有的枚举
（bucket int），丢弃是三条硬编码的 int 字段。于是**自造召回的宿主用不上**——
它有自己的召回链（取数与排序策略归它自定），要的只是这两件。
而它要的正是最贵的那两件：没有它们，召回质量出问题时能观测到的只有
「结果少了」，看不到「为什么少」——排查只能靠猜是桶配额满了、总量满了，
还是内容重复。
一、新的契约件：ports/budget.go
- BucketID：桶标识，刻意是**字符串**而不是枚举——桶的含义由宿主定，
  内核替宿主枚举桶等于把宿主的语义写进本仓。
- DropReason + 六个原因（总量不足 / 桶不足 / 低分 / 过期 / 重复 / 多样性）。
- Budget：Admit(bucket, tokens) 逐条判定配额、Drop(reason, n) 上报
  召回器自己的丢弃、Dropped() / Kept() / Used() 读计数。
- 占比是**相对权重**（按总和归一），与本仓既有的 recall.Config.normalize
  同口径——两处口径若不同，同一个宿主在两处会拿到不同的配额。
二、丢失去向从「一个总数」变成**按原因可归因**
原先 allocate 把「桶装不下」与「总量装不下」合成一个数。两者处置完全不同
（该调分桶占比 vs 该调总预算），合成一个就看不出是哪一种。
现在分开记 DropQuota / DropBudget。
三、本仓的召回**自己先用起来**（否则那是一件「已实现但没接线」的抽象）
- allocate 改走 ports.Budget；
- Result 新增 Drops（按原因）与 Buckets（各桶计数）两张通用表；
  旧的 DroppedByBudget / DroppedByDiversity / DroppedByDuplicate 保留，
  是这两张表在本仓召回里的**投影**（宿主读旧字段不破）。
四、顺带修一个真实的漏抄
Result 加了新字段，而 cloneResult 忘了抄——于是「第一次有、第二次没了」。
而缓存命中只在「第二次问同一个问题」时才显现，正是最难复现的那种形态。
用例 `TestCacheHitCarriesDropTables` 钉住。
（cloneResult 刻意逐字段手抄：加字段时编译器会指出来，
而「遍历反射复制」不会——它把漏抄变成静默。）
五、门禁抓到我自己写的禁词
ports/budget.go 的首版文档里用了本仓禁词表里的词，被
memory-no-domain-vocabulary 判红 3 处。改写（不逐字复述词表）。
用例与变异验证（三轮）：
- ① 把桶不足与总量不足合并成一个原因 → 端口层与召回层用例同时变红；
- ② 召回侧不走上游的件（本地合并计数）→ 召回层用例变红；
- ③ cloneResult 漏抄那两张表 → 缓存用例变红。
还原后复绿。
门禁：make check 全绿 / -race 全绿。

- 文档(plan)：补上深度评审的元教训，并更新两处已过期的记录

issue #7（深度评审结论）里明确要求把「夹具替实现背书」这条元教训写进
方案文档——代码里已写（storage/parity/parity_atoms.go 的夹具注释），
文档里一直没有。补上，并记下它后来的两个变体。
一、新增 §4.5「夹具是为实现量身写的：实现错了夹具也会绿」
夹具一度是照着实现写的（汉字之间带空格，unicode61 正好切成两个词元），
于是「中文子串检索失效」那条缺陷在跨后端断言套件下全绿。门禁侧有
「坏代码必须红」的自证用例，parity 夹具侧没有对应的「坏实现必须红」。
纪律：每加一条跨后端断言，问一次——**这条断言在实现是错的时候会不会红？**
另记两个同族变体：
- 断言在两种情形下结果相同时，断言等于没断言（「查不到东西」 vs
  「出错被吞成空结果」不可区分）；
- 断言写成「实现现在的样子」时，实现错了断言也绿。
二、§7.2「待拍板」表更新：两项其实都已定案
- portable `.hmpkg`：评审时建议 B（降级为 JSON 导入导出），后来实测
  迁移面确有两个消费方，**已做 A** 并发布；
- 召回走 vector：**A 维持**（与 §5 一致）。
留着「待拍板」而不更新，等于让人以为那还是个开口子——与「树实现了却
明写暂不做」是同一类毛病。
三、§8.6 补记：上表八条上游 issue 全部已修并发布，本仓又提的三条
（oekit/pkg #31/#32/#33）同样已修。
这三条的共同形状值得记：**「本该跑的检查没跑，却报通过」**——
凡是被跳过的检查，都必须让调用方看得见。


## [v0.4.2] — 2026-10-20

- 文档：记发版流程与发布物自检的两条纪律

- 「本机构建」后补「发版」段：`make release-check TAG=...`（与 CI 的 release
  段同一套动作，本地能跑）。
- 记下 v0.4.0 那次红的两条教训：
  ① 闸里两份必须同步的清单（go get 的 / build 的）要由同一份驱动，
     不能靠人记得改两处；
  ② 只能远端触发的闸必须有一条本地复现路径——否则「没人看它红」不是疏忽，
     是本地根本看不见它红。
- 再记一次：tag 推过就不再移动，闸坏了往上一个版本号跳。


## [v0.4.1] — 2026-10-20

### 修复(ci)：发布物自检的探针取法错了——「go get 的包清单」与「build 的包清单」漂了

v0.4.0 的发布物自检红了。**不是发布物坏了**——是闸本身的写法错了。
一、根因
`go get <模块>@版本`（模块路径）只登记**模块图**的校验和，不写传递依赖的
**包级**校验和（Go 的既定行为）。下游 import 那些包时就会撞上
"missing go.sum entry"。
v0.4.0 之前 store 与 storage/postgres 没有第三方依赖（或没被这段探针编译），
所以这道闸一直没露馅。v0.4.0 起两处同时撞上：
- store import 了 oagentkit/textx（检索词元的契约钉子），
  而第一段用模块路径 get 完就去 build store → missing go.sum entry；
- 第二段 build 了 storage/postgres（import lib/pq），
  却只 get 了 recall 与 ports → 同样 missing go.sum entry。
两段的毛病是同一个：**「go get 的包清单」与「build 的包清单」漂了**。
加一个包要记得改两处——于是漏了一处。
二、修法
- 第一段只验**模块级**可解析性，只 build 零依赖的 types；
  带依赖的包一律归第二段（第一段验不到带依赖的包，这点原来就没人说清）。
- 第二段用**同一份清单**驱动 get 与 build 两个动作：
  加一个包不会再漏一处。
三、把这段探针做成**本地可跑**的目标（`make release-check TAG=...`）
这道闸从 v0.1.0 起就一直是红的（版本号形态错）却「没人看它红」——
一次错两处。闸红着没人察觉的根因是：**红只发生在 CI 上**，
本地没有任何一处会复现它，于是发版变成「推上去看红不红」。
本地目标让「红」在推 tag 之前就能看到。
这条比闸本身更值钱：闸的形状会变，能本地复现它的动作不会变。
验证：`make release-check TAG=v0.4.0` → 全绿
（**证明 v0.4.0 的发布物本身没问题**，坏的只是闸）。
门禁：make check 全绿。

## [v0.4.0] — 2026-10-20

### 新增(portable)：迁移包读面改走 oekit/pkg/pack —— 体检补三条核对 + 导入改流式

上游 pack v0.17.0 修掉「条数核对被静默跳过」之后，它的计数匹配支持
「文件名去扩展名」的基名宽松匹配——正好覆盖本仓按**类别名**作键的口径
（键 "raw" ↔ 文件 "raw.jsonl"，上游的注释里就举着这个例子）。
于是本仓的包**不用改格式**就能享受上游的体检。
一、体检补三条核对（原先只有摘要与「必需数据文件齐全」）
- 条数不符：清单说 99 条、文件里 1 条——清单自身记账坏了。
  这一条原先查不出来：文件内容没变（摘要照过），坏的是清单的账。
  而「导出多少、导回多少」这个迁移最该被核对的核对，正建立在条数上。
- 多余文件：清单之外塞进来的条目。
- 非法文件名：zip 路径穿越形态。包是**可被替换的产物**——
  导出侧的文件名是本仓给的（安全），但读面必须自己查。
二、导入改流式（`pack.Archive.ReadJSONL`）
原先是「整文件读进内存 → string(b) 再切行」：峰值内存是包体积的两倍
（字节切片 + 字符串副本各一份）。记忆库会长大，迁移包里的事实表
动辄几十上百 MB。改走流式后峰值内存与**单行**同量级，与包体积无关。
三、写面**不换**（理由与上条同源）
① 本仓清单多了 source_namespace / source_host 两个溯源字段，
   上游 pack.Manifest 没有扩展位，pack.Writer 又自己写清单，塞不进去；
② 换过去 counts 键口径会变（本仓按类别名、pack.Writer 按文件名），
   等于改**包格式**——已发布的 .hmpkg 导不回来。
而上游最大的卖点（逐行流式**写出**）在本仓写面落不了地：
portable.Source 的读口返回的是整片切片，导出前数据已全在内存里，
容器层再流式也省不掉那份内存。真要治内存，该改的是 Source 的读口
（分页读），不是容器。
清单与 pack.Manifest 的字段一一对应由 TestManifestSchemaMatchesPack 钉住：
漂了的话 pack 解析出的清单全空，体检会报「缺少文件」——
那是**响亮的失败**而不是静默放过，但仍没人知道是字段漂移。
用例与变异验证：
- TestDoctorDetectsCountMismatch / DetectsExtraFile / DetectsIllegalFileName
- 变异验证：摘掉 arc.Verify() → 三条同时变红；还原后复绿。
门禁：make check 全绿。

### 新增(llm)：开箱的 OpenAI 兼容模型适配器 + CLI 打通端到端链路 + 检索口径单源收口

三块，指向同一个缺口：**能力实现了却没有入口，或入口的失败形态是静默的**。
1) 新增 adapters/llm：面向 OpenAI 兼容端点的模型适配器，让 ports.LLM 开箱可用。
   为什么要有这一层：ports 段只声明协议不含实现（刻意如此——核心不绑死 SDK），
   但 ports.LLM 只有一个 Complete 方法，形状极其简单，于是每个宿主都要重写
   同样的东西：拼请求体、解析响应、判错误码、算超时、处理 JSON 模式。
   这正是「各仓都要重写一遍且边界易错」的那类轮子——错误码判错会静默把失败当成功。
   纯标准库，不引 SDK：本仓零第三方依赖纪律覆盖 adapters 段。这条纪律不是洁癖——
   核心一旦引了 SDK，「没有模型时手工写入 + 词法召回必须能跑」这条降级路径就没了，
   而它塌了之后不会立刻暴露，它塌在「用户配了模型、装了数据库」的生产环境里。
   OpenAI 兼容协议足够简单，net/http + encoding/json 就能覆盖，且比引 SDK 更可控
   （SDK 会带来自己的重试/超时/日志语义，本包的行为逐行可查）。
   也不引 oagentkit/llm：那是 eino 重仓，会把「非 eino 生态的宿主」挡在门外。
   关键不变量（用例钉住）：**空正文必须报错**。返回空串会被提炼面当成
   「模型说了没什么」，从而静默丢弃整批候选——基础设施故障被伪装成正常但空洞的提炼。
2) CLI 打通 extract / promote：从「只能捕获与召回」到「捕获→提炼→晋升→召回」全链可跑。
   原先 CLI 一律不注入模型，理由是「CLI 是看与搬的口子」。这理由在搬的那半边成立，
   在看的那半边不成立：没有模型时库里永远不会长出事实卡，recall 永远召回不到东西，
   而用户看到的是「（无召回）」，没有任何迹象表明是缺模型而不是没东西可记。
   改为按需注入：未配置模型时其余命令零影响，要模型的动作显式要求配置。
   顺带修两处静默失效：
   - MEMORY_LLM_TIMEOUT 解析失败曾被**静默回落**成默认 60s。用户写 "300"（想 300 秒，
     漏了单位）得到的是 60 秒，不报错、不提示、零诊断——配错的值返回一个看起来完全
     正常的结果。现改为记录原值与原因并在 extract/promote 入口显式报出。
   - extract 的汇总数曾报「提炼出 N 条」而非实际存成功数，存库失败时汇总说谎。
     现改为分别报提炼数与存成功数。
3) 检索口径单源收口（store.FTS5Query 钉子）。
   索引侧走 LexicalTokens 钉子，查询侧却直接调上游 textutil.FTS5Query，成了
   **第二个上游入口**，不在钉子下。今天两者行为一致（不是当下的错），但上游一旦改
   FTS5Query 的切法，就会出现索引侧走 Tokens、查询侧走新口径 → 两侧分叉 →
   **静默漏命中**（用户看到「明明记过却查不到」，排查方向会完全跑偏）。
   AGENTS.md 写的「上游改切法时在这一处吸收并复核」若查询侧绕过钉子就漏掉了这个入口。
   新增用例断言「查询侧词元集合 == 索引侧词元集合」——此前测试层面完全看不见这个破口。
门禁：make check 全绿（22 模块 / 5 规则 / tidy-check 通过）。

### 修复(ports)：撤掉已失效的脱敏长度地板兜底，改由依赖契约测试钉住上游保证

上游 oekit/pkg v0.17.0 已原生做「命中长度 < Keep*4 → 整段全量打码」，
本仓为修复前的缺陷自带的两层兜底（声明式 minLen + 运行期预扫）**全部撤除**。
为什么必须撤：留着它们不只是双源漂移——更危险的是让人误以为
「这条是本仓保证的」，于是上游真回退时没人去查。而本仓自己那份
「声明式地板」曾经就是**拿声明比声明**：minLen 从未传进下游
（上游 stringx.Rule 只有 Name/Re/Keep 三个字段），运行期零作用；
真正保护秘密的物理量是**实际命中长度**，它只有运行期按实际匹配值才知道。
撤除后这条保证改由**依赖契约测试**钉住：
- TestUpstreamMaskerShortHitIsFullyMasked：断言的是「可复原性」
  （剥掉形态标签后拼不回原值），不是「查不到东西」——
  后者在有地板与没地板两端都成立，钉不住任何东西。
- 变异验证：把 oekit/pkg 降到 v0.16.0 → 用例立刻变红，输出正是
  `句柄 aa«long-hex:****»aa 记一下`（4 位原值完整可见）；还原后复绿。
用例方向调了一处：原先断言「短命中不带形态标签」——那是本仓兜底的
实现细节（它把短值整段替换、不留标签）。上游 v0.17.0 的口径是**保留标签**
（`«long-hex:****»`）：形态标签不泄露原值，而且它正是审计读
「打的是哪一类」的依据。断言跟着真源走。
同时把「声明的地板」与「正则的真实下限」这两件事分开：
前者是双源漂移（撤），后者是规则本身的语义（留，由
TestRedactorLongHexRegexFloor 钉住：31 位不算凭据，32 位才算）。
门禁：make check 全绿。

### 修复(redact)：脱敏长度地板改为运行期按实际命中长度判定——原先是「拿声明比声明」，运行期零作用

安全缺陷：短凭据可被完整复原。
机制：上游 stringx.Masker 的 Keep 档在「匹配长度 ≤ 2*Keep」时，头尾两段
保留位恰好拼回**完整原文**（掩码主体 `****` 只是插在中间的装饰）。
而上游 stringx.Rule（v0.16.0 实测）**没有 MinLen 字段**，
所以长度地板无法交给上游执行。
原先的规避手段是「声明 minLen + 构造期校验 minLen ≥ 4*keep」——
那是**拿声明比声明**：
  - minLen 从未传进下游（Rule 只有 Name/Re/Keep 三个字段），
    全仓 7 处出现全在声明与注释里，**运行期零作用**；
  - 构造期校验的只是「声明值够不够」，不是「正则的真实下限」。
真正保护秘密的物理量是**实际命中长度**——它只有运行期按实际匹配值才知道。
失效形态（一个人「为了多抓一点」的常见小改）：
把 long-hex 的量词从 {32,} 改成 {8,}，声明的 minLen 仍是 32（构造期不炸），
但 8 位密钥被掩成 `0123«long-hex:****»4567`——**完整可复原**。
修法：地板改为**运行期按实际命中长度判定**——长度不足 keep*4 的命中
整段全量打码（不留任何线索，形态标签也不带：标签会暴露是哪条规则命中）。
口径与同族 guardx/logredact 的 HintMinLenRatio=4 一致（那是上游同族的正确做法：
按实际命中长度在运行期判定，不是拿声明互比）。
实现要点：短值探测必须在常规掩码**之前**做——反向做的话，
第 3 步已把匹配换成 `«long-hex:****»` 形态，第 1 步就再也认不出哪段是原值。
声明式地板（minLen 字段）**保留不删**：它是构造期的自检信号——
量词写错时立刻炸，而不是静默等到线上。运行期兜底防的是「量词没错但值本身短」。
两者互补：声明管「规则写对没有」，运行期管「这次命中够长没有」。
用例与变异验证：
- TestRedactorShortHitIsFullyMasked：断言「可复原性」——剥掉形态标签后
  拼不回原值才算过。这条断言在「有地板」时红、「没地板」时绿，**能区分**。
  （不拿「查不到东西」当断言：那种断言分不清两种情形。）
- 变异验证：注入「运行期地板失效」→ 用例立即变红，输出正是
  `0123«long-hex:****»4567`；还原后复绿。**用例确实钉住了东西。**
- TestRedactorLongHitKeepsLocator：命中够长时仍保留定位线索
  （地板不能把有用信息也抹掉）。
门禁：make check 全绿。

- 杂务(deps)：升到 pkg v0.17.0 / textx v0.6.1 / guardx v0.8.2

三处升级，后两处是补丁级，pkg 那一处带三个上游修复。
- oekit/pkg v0.16.0 → v0.17.0：带三个本仓提的 issue 的修复
  （#31 Masker 短值整段全量打码 / #32 pack 的条数核对不再静默跳过 /
  #33 pack 的 Writer 时钟注入）。
- oagentkit/textx v0.6.0 → v0.6.1：文档修正（guardx/audit 已迁至 obs/audit）
  + 地板降到 go 1.24。
- oagentkit/guardx v0.8.0 → v0.8.2：egress 的 scheme 校验文档修正
  + 地板降到 go 1.24。
升级本身零破坏：guardx v0.7.0 把 audit/bashguard 移出本模块，
本仓只 import logredact，不受影响。
go.sum 同步清理：旧版本（v0.15.1 / v0.5.0 / v0.6.0 / v0.8.0）的哈希残留，
`go mod tidy -diff` 现在无差异。


- CI：发布物自检加验「带依赖的包」——原闸只编译无依赖的那两个

「下游能按版本号引入」那道闸只 build types 与 store，
而它们**没有第三方依赖**——于是它从不验「发布物的 go.mod/go.sum
对带依赖的包是否自洽」。
而这类失效的症状极隐蔽：下游 import 到那个包时才发现
「missing go.sum entry for module providing package ...」，
报错完全指不到本仓（它指向下游自己的 go.sum）。
补一段按**下游真实路径**走的自检：
  go get <模块>/<包>@版本   ← 只 go get <模块>@版本 不会把
                              传递依赖的校验和写进下游的 go.sum
                              （Go 的既定行为，不是本仓的缺陷，
                                但发布闸必须按真实路径走）
然后 build pipeline/recall、ports、storage/postgres——三个带依赖的包。
本地已对已发布的 v0.3.0 模拟该段并通过。


- 文档：README 与 AGENTS.md 同步到 v0.4.0

README：
- 「现在有什么」从 v0.2.0 更到 v0.4.0，补上模型适配器（adapters/llm）
  与脱敏两行，模块数 21 → 22。
- 原来那句「CLI 是管理口，不注入模型」已被 v0.4.0 的改动推翻：
  extract / promote 现在按需注入模型（端点从环境变量读，密钥不进命令行）。
  照抄旧文会让人以为 CLI 用不了模型。
- Go 版本要求 1.25+ 是旧的，go.mod 说 1.27。
- 设计取舍补两条：上游修好了、自己那份兜底就得撤；口径要单源且钉在一处。
AGENTS.md：
- 写入面纪律：长度地板那条改写——兜底已全部撤除（连同「拿声明比声明」
  的失效形态与「上游修好后必须撤」的教训），改由依赖契约测试钉住。
- 已定案：迁移包决策从「不整体换 pack」修订为「读面已换、写面不换」，
  并记下写面不换的两条理由（溯源字段无扩展位、counts 键口径即包格式）。


## [v0.3.0] — 2026-10-19

### 新增(recall)：事实卡可按宿主实体限定 + 无捕获面形态 + 库管理面挂 bridge

补 issue #8 挖出的两条契约空洞。
一、事实卡按宿主实体限定（Config.RestrictAtomsToEntities，缺省 false）
store.AtomFilter.EntityIDs 与 recall.Request.Entities 都存在，却在召回路径上
互不相干——宿主传了实体，内核照收不误，召回回来的是全库事实。
但不能无条件接过滤：Request.Entities 同时承载两种**语义相反**的用法——
「本轮提到的实体」（补画像页，不该收窄事实范围）与
「本会话的锚点实体」（给事实卡划范围）。内核从字段本身区分不了。
故给显式开关由宿主声明「这一轮的 Entities 就是事实边界」，
缺省保持不过滤（既有调用方一行不改）。
接错了的后果是「本轮随口提到的一个对象，把整个会话的事实集清空」。
空值语义是要点：开关为真而 Entities 为空 → **返回空集**，不是全库。
「显式声明了范围但范围为空」与「没声明范围」是两种不同的意图；
退化成全库会把「限定」变成「装饰」——而宿主正是靠它挡全库事实的。
检索路不受存储层过滤保护（SearchAtoms 没有实体过滤维度），故在召回侧兜一道。
二、无捕获面形态（Config.NoCaptureFace，缺省 false；Reader 拆出 RawReader）
只有采集原始事件的宿主才有捕获面；只蒸馏终稿的宿主没有。
让那种宿主实现 ListRaw 只能给一个「永远返回空集」的桩——那是**契约洗白**
（声称支持捕获面，实际不支持），而内核会据此在 raw 源上做出「有原始事件」的假设。
故 ListRaw 从 recall.Reader 拆出来成为**可选**的 recall.RawReader：
- 存储实现了它 → raw 源照常进路由；
- 没实现而配置又要 raw 源 → recall.New **在构造期报错**（不是静默空集）；
- NoCaptureFace = true → raw 源不出现在路由里，且该开关**优先于** Sources。
三、库管理面挂 bridge（memory.page.regen / memory.episode.extract /
memory.episode.digest）
页面重生成、情节抽取、周期摘要都需要模型，而服务面刻意不装它们。
它们此前在本仓内**没有跨进程入口**——跨语言宿主（不必是 Go）触达不到。
以独立的 bridge.Management 结构装配，**不动服务面**；
未装配时那三个方法返回 CodeUnavailable（能力未装配），而不是永远空集的桩。
顺带：storage/postgres 的包注释补「宿主如何改走本仓 PG 后端」的三步
（装配 / 接进内核链 / 验一致性）。
裁定与理由记进方案文档 §7.2.1。

## [v0.2.0] — 2026-10-19

### 修复(ci)：发布物自检的版本号形态错——这道闸从 v0.1.0 起一直是红的

`go get module@${ver}` 里 ver 是剥掉 v 的版本号（0.2.0），
而 Go 的模块版本号**必须**以 v 开头：
    go get github.com/yi-nology/agent-memory@0.2.0
    → invalid version: unknown revision 0.2.0
后果：每次发版的「下游能按版本号引入」自检都失败——
而它恰好是唯一能提前发现「发布物下游引不进」的那道闸。
v0.1.0 的那次运行同样是红的（unknown revision 0.1.0），
也就是说这道闸**从来没绿过**，也没人看它红。
改成用带 v 的 tag 名。本地已模拟该步骤通过：
go get ...@v0.2.0 → go list -m → build types/store 均通过。

- 文档(plan)：记接缝自查四条——归属迁移是契约、退役随卡写回是内核空洞

M7 落地后对宿主适配层做了一轮逐行自查，查出四条契约面问题（§6.3）：
- 归属迁移是 entity.Store 的明文契约（合并时要改归属），宿主适配器原本
  只改内容不改归属 → 归一合并在宿主侧静默失效
- 新增第三条内核契约空洞：SaveAtom 的退役状态随卡写回，入卡不带退役信息
  就会把退役清空 → 跨宿主适配器映射时丢了退役信息 = 复活已退役的事实
- 宿主四态 ↔ 内核两态的映射表（新增第四条刻意偏离）
- 来源性质映射要双向可逆：createdByOf 是 originOf 的逆映射

- 文档(plan)：M7-b2「召回切流」裁定不做 + M7-b3 数据区标记落地

M7-b1 落地后，M7 按 §六 的定义已达成（subject_kind → Entity 投影 +
repo_memory → store 适配层，留门面保调用点）。原计划的「M7-b2 = 召回改走内核
八段链」经实查**不做**，三条理由：
1. **两边的召回模型相反**。内核（及它对标的上游 octop）都是查询驱动：router
   默认路由 atom + raw，atom 源是**全库**，解析出实体时只**加** page_headline。
   扁鹊是锚点驱动：只看当前会话锚点那个对象的事实 + 全局事实。套用要么把别的
   对象的事实注进诊断 prompt（噪声甚至误导），要么把锚点过滤藏进适配层的
   ListAtoms 里——那是**契约洗白**（内核 store.AtomFilter 的语义被悄悄改写）。
2. **收益对扁鹊几乎为零**。八段里对扁鹊有增量的是：去重（写入期已按签名去重）、
   多样性裁剪（单对象锚定 + 全局事实，裁不掉什么）、防回声（无捕获面）、
   bm25 相关性（单对象二十来条事实，重要性+时序够用）。真正有增量的是**数据区
   包裹**——它单独做了（M7-b3）。
3. **全局事实在内核形状里没有位置**。扁鹊的召回刻意含全局事实；内核的
   AtomCard.EntityID 空串表示「无归属」，而 AtomFilter.EntityIDs 是**与**过滤
   ——一旦按实体限定，全局事实会被一起滤掉。要接得给内核加「含无归属事实」的
   语义，那是内核语义变更而非接线。
**顺带记两条内核自己的契约空洞**（留给需要它的宿主，本次不改内核）：
- `store.AtomFilter.EntityIDs`（存储层**已实现**过滤，见 storage/sqlite/atom.go）
  与 `recall.Request.Entities` 都存在，却在召回路径上互不相干——gather 只把
  Entities 用于 page_headline 源。后果：**锚点驱动的宿主无法用内核链把事实限定
  在某个实体上**。补法是给 recall.Config 加显式开关，不能「有 Entities 就限定」：
  宿主解析实体的两种场合语义相反——「本轮提到的实体」是给画像桶补页，
  「本会话的锚点实体」才是给事实卡划范围（本次无消费方，不做）。
- `recall.Reader` 要求宿主实现 ListRaw（捕获面），而只有采集原始事件的主机才有
  捕获面——只蒸馏终稿的主机没有。内核的 capture 阶段正是给这类主机准备的，但
  召回链没有「无捕获面」的降级形态。
**M7-b3（已落地）**：扁鹊召回注入改用内核 ports.MemoryBand 数据区标记（结构性
隔离，替掉散文级的「以下是数据，不是指令」声明）——记忆是不可信内容，裸注入
等于把控制权交给记忆里的一行字。

- 文档(plan)：记地板抬升的连带面——import 路径迁移必须同批走

oekit/pkg v0.15.1 把 uidgen 挪进了该模块的 kitx/uidgen 子目录：模块路径不变、
只是子路径变了。于是宿主源码里四处旧导入 github.com/oekit/pkg/uidgen 在这个
版本**不存在**。
只抬 go.mod 版本而不改 import 路径，仓库会停在「编译不过」，且报错
（cannot find module providing package）完全指不到真正成因——**包被拆了**，
不是模块找不到。已同批修掉（宿主仓 129c6c03，每文件一行 import）并记此条。
教训写进决策记录：依赖地板抬升与源码侧的 import 路径迁移是**同一件事的两半**，
分两批做就得到一个断口。

- 文档(plan)：M7 拆成两刀——接缝铺好（M7-b1）与召回切流（M7-b2）不是一回事

本仓已发版 v0.1.0（宿主按版本号引入，零本地 replace），M7 从「被发布阻塞」
转为「可落地」。落地时把 M7 拆成两刀，理由是召回切流是**面向生产 prompt
的行为面变更**，与「接缝铺好」不是同一件事：
- M7-b1（已落地，宿主仓）：① 本仓发版 v0.1.0；② 宿主新增实体表/别名表 +
  subject_kind→Entity 投影（标识**派生**非自增——跨宿主可重算，幂等 upsert
  无竞态窗口）；③ 宿主 GORM 记忆面实现内核三片契约（store.AtomStore +
  store.MetaStore + pipeline/entity.Store），**不声称**实现全量 store.Store
  （宿主没有捕获面/候选面/树/情节，声称有就是造假）；④ 四条写入路径挂实体
  投影；⑤ 架构门禁加两条规则锁「内核依赖只经适配层进入」与「适配层不得上溯
  业务层」。
- M7-b2（未做）：召回改走内核 pipeline/recall 八段链。两条硬阻塞：
  内核 recall.Reader 要 5 个方法，其中 ListRaw / GetEntityPage / 活跃实体栈
  三片**宿主当前没有对应表**；且内核召回片段是**已包裹**的（bander），换过去
  会改注入 prompt 的形态——那是面向用户的变更，需要 e2e 兜底而不是顺带做。
同时落定三条**刻意偏离**（写在宿主适配层包注释里，勿在别处"修"回去）：
签名不换（存量唯一索引含 kind，内核签名不含——换了会多出重复卡）、
QuoteEventID 不满足内核的「可解析到捕获事件」不变量（宿主无捕获面）、
实体计数不增量维护（增量的漂移不可自愈，重算永远正确）。
宿主侧依赖地板两处**非自主**抬升（本地 replace ../agent-harness 随上游发版
被抬，按房规精确同步、零 tidy）：oagentkit/guardx v0.4.0→v0.4.1→v0.5.0、
oagentkit/agent v0.8.0→v0.10.0 等八模块。


- release：v0.2.0 —— 补齐 PG 后端、接上库管理面、修复六处契约缺陷

v0.1.0 之后的第一次发布。范围分三块。
一、缺陷修复（六处，每处都有「实现错时必须红」的用例）
- 晋升通过后不落库：五道检查全过、返回 promote 判决，却不产生事实卡、
  不落定候选、不增计数、不写审计——记忆主链断在最后一环。
  修法：落库四步放进同一个事务，用回调收到的那份存储；
  事实卡 ID 由候选 ID 派生（重复晋升幂等），来源凭证取自证据事件。
- 召回遇空正文空指针崩溃：snippet() 返回 (nil, nil) 而调用点直接解引用，
  而「空正文能落库」当时是常态——一条空事件足以让之后每次召回都崩。
  修法：空是「没有」不是错误，snippet 改为返回「有没有这一条」；
  类型层另加空白内容校验，捕获层拒收空白正文。
- 实体归一吞掉存储故障：注释写「往上抛」而代码 return "", nil，
  于是归一「成功」但实体为空，产出无归属的事实卡。
  修法：resolveEntity 拿到 error 返回位，「查不动」与「查不到」在类型上分开。
- FTS5 对中文子串检索失效：unicode61 把连续汉字算作一个词元，
  查「磁盘」搜不到「磁盘长期偏小」，且静默返回空集。
  修法：切词规则提到 store.LexicalTokens（契约层，两个后端共用同一套）；
  parity 夹具改成真实中文形态——原先汉字之间带空格的写法替实现蒙混过关。
- 重要度排序是字典序：TEXT 列下 'med' > 'low' > 'high'，最重要的排最后，
  而召回候选池就是前 N 条，最重要的那批最先被裁掉。
  修法：排序表达式从 types.Importance 程序化生成（真源唯一，不漂移），
  跨后端 parity 断言守住。
- 召回取数不消费查询词：候选池与「用户问了什么」无关，再相关的事实
  只要排在池外就永远进不了 prompt，而重排再准也无济于事。
  修法：取数改两路——查询命中 ∪ 高价值兜底，合并去重后统一重排。
二、补齐与接线
- PostgreSQL 后端：从 88 行空壳到完整实现。全文检索用 tsvector + GIN，
  不建虚表——PG 的虚表不会自动同步正文与索引，漂了的索引表现为
  「搜得到但打开是空的」，不报任何错。31 条 parity 断言对着真实 PostgreSQL 跑通，
  CI 加 postgres service 让它每次推送都真跑（skipped 的用例不证明兼容）。
  顺带修掉两条跨后端缺陷：签名分隔符用 NUL（PG 的 TEXT 禁止 0x00，
  同一份签名在一个后端落库、另一个报编码错）、占位符方言（? vs $1）。
- 树投影整体删除：方案 §5 与 AGENTS.md 都明写「暂不做」，实现却全套落地。
  留着它等于让人以为那是个待启用的能力。
- 补 ListAtomsByEntity 到存储契约：pipeline/entity 与 pipeline/page 要的
  这个方法在存储层从未实现，于是两个包无法用真后端实例化——
  不是「没接线」，是「根本跑不起来」。
- CLI 库管理面：gc / consolidate / retention / vacuum / entity / episode。
  之前这些能力实现了却没有入口，既不会被用也不会被验证。
- policy 登记 integration 模块：给跨包端到端测试一个专属位置。
  门禁的依赖方向规则对 _test.go 同样生效，没有这个位置，
  能抓住主链断裂的那类用例就写不进任何包。
- lifecycle / portable 接入可注入时钟；portable 导出与导入的计数键统一
  （原先一边按文件名一边按类别名，「导出多少导回多少」这个核对做不成）。
三、并行会话的改动（同一工作树，一并纳入本次发布）
- 门禁加受控开口 allow_third_party：纯 stdlib 工具级模块可进核心，
  逐条登记 reason + until + removal，缺一条加载即失败、到期即红。
- 脱敏接 guardx/logredact 的规则表：两道处置，顺序是本仓在前、上游在后；
  掩码类型名避开敏感键名表（否则审计失真）。
- 情节的日历边界算术改走 oekit/pkg/timex
  （Truncate 在非整小时偏移的时区会算错墙钟边界）。
对宿主（下游）的影响见 release notes。


## [v0.1.0] — 2026-10-19

### 新增(pipeline)：M4–M6 归一/页面/情节 + 迁移包 + bridge + CLI + 生命周期

## M4 三个流水线
- **pipeline/entity**：按名查实体、登记别名、合并实体。归一**不新建**——
  内核手里只有一条断言，新建错了会把同一对象裂成两个；合并是**显式**的
  （方向由宿主定，内核只做机械迁移），被并方退役而非删除。
- **pipeline/page**：脏标记 + 重生成。失败**保持脏**并留原因——清掉脏标记
  就永远不再进队列，而用户看到的「页面存在、内容过时」与「页面正常」长得
  一模一样。模型输出不合法即记失败，不产出空页面。
- **pipeline/episode**：情节摘要 + 日/周/月 digest。周期边界一律 **UTC**
  （记忆库可能被多宿主共写，「周三」在不同时区指不同的日子，而那种对不上
  人是查不出来的）。无模型时落**确定性回退清单**而不是整条失败——清单不
  假装是总结，诚实的清单不会误导读者。
## M5 迁移包 + bridge + CLI
- **operations/portable**：.hmpkg = zip 容器 + JSONL + SHA-256 清单。
  三条闭环 pack/adopt/doctor。doctor 存在的原因是迁移**最容易出静默错误**：
  包缺一半，导入「成功」了，而宿主从此拥有一段残缺的记忆且不知道缺了什么。
  坏包**一个字节都不导**。
- **adapters/bridge**：JSON-RPC 2.0 over stdio，方法白名单（不提供
  「执行任意 SQL」式的逃逸口——逃逸口一旦存在就迟早被用）。错误码可分辨：
  方法不存在 / 参数不合法 / 目标不存在 / 能力未装配，四者分开。
- **adapters/cli**：`Run(args, stdout, stderr) int` 而非 main()，命令逻辑
  可被单测直接调用。CLI **不注入模型**：它是看与搬的口子，不是生成口。
## M6 生命周期
回收（原始事件按龄 + 证据全没了的事实顺带退役）、合并（同签名事实归一，
其余**软退役**）、保留期（只退役「证据全部来自运行期错误」的事实——混着
正常证据的不动）、压缩（后端专属能力，SQLite 的 VACUUM；不支持的后端必须
明说而不是静默跳过）。
## 本轮抓到三个自身缺陷
1. **SaveAtom 是纯 INSERT** ——事实卡会被更新（并入补证据、退役改状态、合并
   迁归属），第二次保存就撞唯一键。这个 bug 潜伏了整个 M1：晋升层的「并入
   并补证据」路径同样会踩，直到生命周期的合并用例第一次真正触发它。改 upsert，
   并把「重复保存是更新而不是冲突」写进 parity（跨后端契约）。
2. **ListAtoms 的 ORDER BY 没有并列时的稳定序** ——同重要度同时间的两张卡
   顺序随机，于是「同签名合并时保留哪一张」是随机的：同一批数据合并两次
   得到不同结果。这与 M1 在原始事件面上修过的是同一课的第二处。
3. **门禁自身的 bug**：遍历模块目录时没跳过「别的模块的根」，导致
   operations/portable 的文件被算成 operations 模块的，依赖方向检查失真。
另：bridge 的 toRPCError 不认 *rpcError，把「方法不存在」与「参数不合法」
都降级成 internal——宿主于是分不清「调错了」与「服务端坏了」。
验证=make check 全绿+完全离线（GOPROXY=off、GOFLAGS 未设）。Go 12.8k 行。

### 新增(pipeline)：M3 捕获/提炼/晋升三段，落库前打码 + 五道检查

capture / extractor / promotion 三段落地，方案 §7.1 点名的宿主隐私缺口
在**本仓侧**补上了接线点与兵底处置。
## 方案 §三 第三条裁定也被实弹推翻：pii 不借道
与 budget 同理：harness 的 pii 是「策略注册表 + 执行面」的完整平面，
而记忆要的是「落库前必过一过」的窄接口——两者形态不同，且借道撞破同样的
两条门禁红线。改为 ports.Redactor（**必需依赖**，capture.New 传 nil 直接失败）
+ ports.PatternRedactor（只认形态的兵底）。
兵底收录标准三条同时成立：① 不认识领域词；② 误报代价只是少几个字符；
③ **漏报代价不可逆**（凭据一旦落库要靠轮换补救，而轮换往往来不及）。
## 核心用例：存储层拿不到明文
TestPlaintextNeverReachesStorage 断言的是**落库那一刻**的内容——正文、载荷、
嵌套载荷三层全查。这是整个 M3 存在的理由：记忆是可导出、可复制出去的留存
资产，处置点必须早于持久化。反过来（先落库后打码）等于把明文先写出去再擦：
WAL、备份、那一刻的任何读操作都已经看见了它。
## 兵底的形态覆盖是被用例逼出来的
初版只收 key=value 敏感赋值与 AWS 前缀，于是「部署用的访问凭据是 ghp_XXXX」
这种**散文形态**整条漏掉——而它恰是最常见的一种。补上已知凭据前缀族
（ghp_/glpat-/xox*-/sk-/AKIA/AIza…）：形态确定、零误伤、且这一类漏报不可逆。
**兵底漏掉不可逆的那一类，兵底就没有意义。**
## 写入面四条纪律（写进 AGENTS.md）
- 脱敏早于持久化，且是必需依赖——缺失的默认值不是宽容，是事故
- 抽取与判定必须分层：抽取调模型（会幻觉），判定要可复现。混在一起时一次
  模型抖动就把幻觉写进长期记忆，而数据看着完全正常
- **唯一防幻觉闸**：逐字引用必须真在素材里出现过；出处不成立时整条候选作废，
  **不修补、不猜它指哪条**——猜出来的证据等于编造
- 送人工是默认姿态：漏记的代价是「用户再说一遍」，误记的代价是「记忆库开始
  编造且用户无从分辨哪条是真的」。矛盾尤其不能自动裁决——规则和模型都分不清
  「已修复」与「未修复」哪个是新的
## 五道检查，失败方向各不相同
价值→丢弃 / 证据→丢弃（不降级成「无出处但先记着」）/ 实体→送人工（归一错了
会把两条不相干的事实并成一张卡）/ 重复→并入并补证据 / 矛盾→送人工。
两处细节值得记：存储故障**不是**「判不了」，上抛而不记成「需人工」，否则
待办队列被噪音淹没；矛盾判据要「同实体 + 词面相似 + 否定方向相反」三条同时
成立，只看相似度会把「两件都是磁盘相关」判成矛盾。
## ⚠️ 宿主侧那条路还是通的
本仓把接线点做出来了，但宿主现有蒸馏路径（memory_svc.go）仍把断言与逐字
引用原样落库，没有脱敏接线。**改本仓不会自动修好它**——宿主侧要单列一批
把蒸馏产物改走 capture。已写进方案 §7.1。
验证=make check 全绿+完全离线（GOPROXY=off、GOFLAGS 未设）。Go 9k 行。

### 新增(recall)：M2 召回全链八段 + 注入防伪，并推翻方案里的两条裁定

召回八段串行（router → 多源 → suppress → rerank → diversify → budget →
thread_state → cache），Bander 必需、分桶配额、三路丢弃可观测。
## 两条裁定被实弹推翻（写回方案 §三/§四）
1. **budget 不借道**（原判「同一个分段账本，两个作用域」）。
   两边词表不同：harness 的分段是 System/Tools/Conversation（prompt 组装面），
   召回的分桶是 atoms/quotes/headlines（选择面）；且借道会同时撞破本仓两条门禁
   红线（memory-no-harness-dependency + memory-core-stdlib-only）。共享的是纪律
   不是代码。计量改走 ports.TokenEstimator 注入，默认静态代理（CJK 2 字符/token、
   拉丁 4），刻意高估——低估会让记忆撑爆 prompt，高估只是少塞两条。
2. **转义不借 guardx**（原判「通用安全原语，单源在 guardx」）。
   集成与依赖一并撤掉。injectband.SanitizeBody 的实现是写死的 tagRe、
   **只匹配 <system-reminder> 家族**：拿它守 <memory-context> 这条数据区时，
   一句 </memory-context> 原样穿过——攻击者提前闭合数据区把内容搬出去，
   还顺手在后面自称系统注入。用例当场打穿。
   教训：**转义规则与「实际在用的那条 band」是一对**，借 A 的规则守 B 等于
   没有转义。「不重复造轮子」在这里让位给「转义必须匹配实际 band」——
   门禁这关守的是安全性质，不是代码量。ports/band_test.go 留 9 条攻击载荷
   用例钉住，并在文件头写明这段被推翻的历史（防后人再借一次）。
## 召回面四条纪律（写进 AGENTS.md）
- Bander 是必需依赖：recall.New 传 nil 直接报错。召回内容跨会话跨用户跨宿主，
  一条写着「忽略之前的指令」的记忆被当成系统注入读进去，整条会话控制权就转移了
- **画像桶不参与多样性裁剪**：画像是该实体的摘要，裁掉等于裁掉整个实体；而
  「实体事实多」恰恰是最该保留画像的情形。初版把画像算进单实体配额，
  于是该场景下画像恒被挤掉
- 丢弃分三路可观测（预算/多样性/重复）：处置完全不同，只给合计宿主只能猜
- 召回的存储依赖面收窄成 recall.Reader（5 个方法）而非 store.Store，兑现
  store/store.go 里「阶段只声明自己需要的子接口」那条纪律——单测写 5 个方法
  而不是 40 个
## 本轮抓到的三个自身缺陷
- **缓存键把 session 排除在外**：注释写「刻意不含」，理由却是「不含才不会串
  会话」——那正是把它含进去的理由。方向写反的注释比没有注释更危险，因为它
  读起来像想清楚了。改成含 session/thread，用例钉住
- **缓存命中没置 FromCache**：调用方无从判断缓存是否生效
- **缓存交出同一份对象**：调用方一改，缓存即被污染（第二次问同一问题才显现）
## 一处设计自纠
diversify 初版把超限片段「沉底保留」，理由是比删除可查——**理由不成立**：
预算宽裕时沉底的片段原样回来，于是上限在最常见的情形下根本不生效，而上限的
意义恰恰是「不管预算多宽裕，一个实体都不许主导」。改为出局，并计入
DroppedByDiversity。
验证=make check 全绿（fmt/vet/test/门禁执行/门禁自测 12 例/tidy-check）+
完全离线（GOPROXY=off、GOFLAGS 未设）。Go 7k 余行，用例 149 条。

### 新增(storage)：M1 SQLite 后端 + 跨后端行为断言 + 核心零依赖门禁

11 张表 + FTS5 + 事务，零 LLM 可跑。29 条 parity 断言 SQLite 全绿，
门禁加 memory-core-stdlib-only 并把自证用例扩到 12 条。
存储层实现（storage/sqlite/，按表拆成 9 个内嵌窄结构）：
- 拒绝上游的「每 namespace 一套表」方案：本仓 store.Namespace 是显式参数，
  一个后端服务多个记忆库，表前缀要求每库跑一遍 DDL，且「这个实体在全库有几种
  叫法」无从问起。改单套表 + namespace 列，所有主键与热点索引以 ns 起头。
- 时刻存 Unix 纳秒：ListRaw 的分页游标按 ts 排，同一秒写入的多条在秒精度下
  顺序不确定——那会让「倒序取最后 N 条」每次跑出不同结果，表现为偶发失败。
- FTS5 索引按「孤儿」清理而非按同一时间窗反查：后者条件一旦与 DELETE 有任何
  细微差异，就留下指向已删事件的残行，症状是「搜到了但打开是空的」，不报错。
- 退役不删索引：被推翻的事实仍可搜到并显示「已被取代」——「当初为什么这么判」
  是记忆系统必须能回答的问题。
跨后端行为断言（storage/parity/，对标上游 parity 门禁）：
29 条断言定义「记忆库该有什么行为」，两个后端跑同一份。各自的单测都绿 ≠ 行为
一致：软退役过滤漏一列、分页游标用 <= 还是 <、时间窗闭开区间——这类不一致在
各自单测里全看不出来，而「换部署形态召回结果不变」是产品承诺。
⚠️ PG 用例是**跳过**的，且跳过可见。**skipped 用例不证明兼容**——上游对同一件事
的措辞是「skipped PG cases do not prove compatibility」。落地顺序不能反：先实现
后端、再接 parity、最后配 DSN；先放一个能编译的空实现糊过门禁，会让「后端没实现」
与「后端行为不对」两种病因混在一起。
存储层四条实弹教训（M1 全部踩到，写进 AGENTS.md）：
1. 事务里用回调收到的那份 store 而非外层的——否则连接池为 1 时挂起，
   池宽裕时更糟：静默写到事务外，原子性没了且不报错
2. 游标必须关掉才能发下一条查询；修法是把约束抽成一个方法，只留一处
3. rows.Err() 在 0 行时是 nil，notFound(nil) 把「不存在」翻成「成功 + nil 对象」
4. god object 门禁对本仓也生效：Store 73 方法被逮住，按表拆后最大 12 个
   （与扁鹊「搬实现 ≠ 降数」同款判决）
门禁新增 memory-core-stdlib-only：第三方包只许出现在 storage/，其余只准 stdlib
与本仓。理由不是洁癖，是降级路径——核心引了 SDK，「没模型也能手工写入 + 词法
召回」这条路就塌在生产环境而不是 CI 里。
另修一处门禁自身的假通过：scope 原本按 cwd 解析，`go -C` 切走 cwd 后它扫的是
门禁自己的源码。现在一律相对仓根解析。
验证=make check 全绿（fmt/vet/test/门禁执行/门禁自测 12 例/tidy-check）+
完全离线（GOPROXY=off、GOFLAGS 未设）。Go 5521 行。

### 新增(memory)：M0 建仓——领域模型+端口契约+架构门禁

对标 TencentCloud/octop-memory v1.0.2（ed11d54 克隆核对）抽出独立记忆内核，
定位「记忆是代码，LLM 只做单次提炼」：本仓管捕获/提炼/晋升/召回/存储/迁移，
宿主管 agent 执行与模型调度。与 agent-harness 平级内核，两侧互不 import。
领域模型（types/，对标上游 types.py）：
- RawEvent → Candidate → AtomCard → EntityPage，leaf 只持 AtomID 不复制正文
  （一旦复制，「事实被推翻」就只改 atoms 表、树上旧正文还在，召回会捞到已退役事实）
- Provenance 做成**类型**而非 payload 字符串键（刻意偏离上游）——消费方读字段，
  写错键名编译期即暴露；判据只有「是不是合成/运行期错误」，不做意图关键词黑名单
- 事实卡只软退役不硬删：被推翻的事实曾经是真的，硬删会让「当初为什么这么判」无从查证
- Candidate 的 Decided（已表态，含转人工）与 Settled（已落定，召回面只认这个）分开
端口（ports/）：模型/向量化/向量索引全是接口 + Noop 占位。未注入必须返回
ErrNoLLM 而非空串——空串会被下游读成「模型说没什么」，静默丢弃整批候选。
存储契约（store/）：按消费面拆成 9 个小接口而非上游 60 方法巨型 Protocol——
「阶段能碰哪些表」在类型上可见，漏写越权调用即编译不过。namespace 显式传参。
架构门禁：policy 先行，16 个模块方向 + 4 条规则已登记（未落地模块也先钉方向）。
门禁工具单独成模块，只为一个 YAML 库——这样主模块 require 段为空，
「零核心依赖」是 go list -m all 能验的事实，make tidy-check 逐行核对。
首轮实弹的门禁三条血泪（写进 policy 与 AGENTS.md）：
- 假红是门禁头号死法：禁词表原收了中文「命名空间」，与记忆隔离维度
  store.Namespace 同形不同义，首跑打红 2 处。已撤出并与 host/node 并列记入
  memory-vocabulary-false-positive-guard 条目
- 已知限制：纯文本匹配连「解释禁词的那行注释」都拦，禁用词不能出现在任何文本里
- 门禁最大失败模式是假通过（规则写错工具退出 0），故 tools/archcheck/gate_test.go
  测的是**坏代码必须红**：域词在代码/注释/测试数据三处、import 编排、模块方向写反、
  pipeline 层出现 SQL，另加「本仓自身干净」与「承重词永不误伤」
验证=make check 全绿（fmt/vet/test/门禁执行/门禁自测，9 用例）+ 完全离线
（GOPROXY=off，GOFLAGS 未设）+ 主模块零依赖机检通过。Go 2232 行。

### 修复(archcheck)：修冗余条件导致门禁自测编译失败

上一提交把门禁自测漏跑了（只跑了执行器），门禁自身编译不过。
教训与本轮另两条一致：**「门禁跑过了」不等于「门禁是好的」**——
执行器通过而自测编译失败，说明我验证的粒度不够。
make check 复跑全绿。

- CI：补 GitHub Actions 门禁与发布物自检

组织内 yi-nology 仓的默认口径：每个仓都带 .github/workflows/ci.yml。
本仓此前只有本地 make check，本地跑得动不等于合规——门禁的检出能力要靠 CI 保证。
两段 job：
- check：make check（fmt-check + vet + test + architecture-check + tidy-check）。
  门禁本体全在 Makefile 里，ci.yml 只执行不重复实现规则——否则就成了
  「推送后门禁」，本地开发者要到推送之后才知道自己违规。
  钉死 ubuntu-24.04 而非 ubuntu-latest：后者自 2026-10-19 起迁到 Ubuntu 26，
  门禁在与开发者不同的镜像上跑，绿灯可信度要打折。
- release：仅 tag 触发，验证「发布物可被下游按版本号引入」——
  这是与主模块门禁**不同的**一件事（主模块验零依赖，tag 验发布可解析性）。
  版本化引入失败的症状是 unknown revision / 模块找到了但不含该包，
  报错完全指不到本仓代码，故在发布侧先自证。
踩过的坑：YAML 块标量里不能嵌顶格 heredoc——顶格会截断块标量本身，
YAML 解析期就红。改成直接 go build 依赖包，不写 probe 文件。


- 文档(plan)：更新 M7 状态——宿主侧隐私缺口已清偿，M7-b 待本仓发布

宿主仓提交 ad3ada51：记忆写入路径（CreateManual/CreateFromEvent/promote）
在「构造事实卡」动作上收口脱敏，用例钉住「从库里读出来也是脱敏后的」。
M7-b（记忆平面接 Store、subject_kind → Entity 投影）**待本仓先打 tag**：
宿主要版本化引入，不能用本地 replace（发布物不能指向工作树）。
