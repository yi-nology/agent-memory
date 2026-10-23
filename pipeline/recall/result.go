package recall

import (
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：**召回结果面**（来源枚举 / 片段 / 结果 / 渲染）。
//
// 为什么单独成文件：结果面是宿主的**唯一**消费面（它把 Result 渲染进 prompt），
// 而召回链的其余部分（配置、取数、融合、排序、预算、缓存）是**内核内部**的事。
// 两者混在一个文件里，改结果字段时会顺手碰到召回流程——
// 而「改结果」与「改流程」的风险来源完全不同。

// Source 召回来源。
type Source string

const (
	// SourceAtom 事实卡。召回的**主力**——它是判断过的稳定事实。
	SourceAtom Source = "atom"
	// SourceRaw 原始捕获事件。只作兜底：持久命中足够时就不该塞原始对话，
	// 原始事件体量大、噪声高，进 prompt 的性价比远低于事实卡。
	SourceRaw Source = "raw"
	// SourcePageHeadline 实体页面的一句话画像。
	//
	// 只在宿主已解析出实体时才加：它是一个固定成本（每个实体一句），
	// 而它换来的上下文收益只在话题确实锁定时才兑现。
	SourcePageHeadline Source = "page_headline"
)

// Snippet 一段可注入的内容。
type Snippet struct {
	// Source 来源。
	Source Source
	// RefID 溯源标识（事实卡 ID / 事件 ID / 实体 ID）。
	RefID string
	// EntityID 归属实体（raw 可能为空）。
	EntityID string
	// Role 渲染角色（"记忆" / "历史" / "画像"）。
	Role string
	// Body 正文，**已经过 Bander 包裹**。
	Body string
	// Importance 重要度（raw 无此概念，取零值）。
	Importance types.Importance
	// OccurredAt 发生时刻（排序用）。
	OccurredAt time.Time
	// Tokens 该段占用的估算 token（进预算用）。
	Tokens int
}

// Result 召回结果。
type Result struct {
	// Snippets 按优先级排序，可直接渲染。
	Snippets []Snippet
	// Dropped 被丢弃的总条数（预算 + 多样性两路之和）。
	//
	// 刻意暴露：装不下就是装不下，而「用户以为带上了记忆、实际被静默丢掉」
	// 是最难被发现的失效。分两路给出，是因为处置不同——预算不足说明该调大
	// TotalBudget 或改进提炼，多样性超限说明该调大 PerEntityMax。
	Dropped int
	// DroppedByBudget 预算装不下而丢弃的条数。
	DroppedByBudget int
	// DroppedByDiversity 单一实体超额而丢弃的条数。
	DroppedByDiversity int
	// DroppedByDuplicate 内容重复而丢弃的条数。
	//
	// 三路分开给是因为处置不同：重复说明提炼层在制造同一件事（改 extractor），
	// 多样性说明实体偏斜（改 PerEntityMax 或改分桶），预算说明装不下（改总量）。
	DroppedByDuplicate int
	// Drops 按原因分路的丢弃计数（**通用面**）。
	//
	// 上面的三条具名字段是这张表在本仓召回里的**投影**（宿主读旧字段不破），
	// 这张表才是可复用的那一面：宿主自造召回时用 ports.Budget 拿到同样形状的
	// 计数，于是「为什么少了」在两边都能归因——
	// 而分桶不足（DropQuota）与总量不足（DropBudget）必须分开：
	// 前者该调分桶占比，后者该调总量，合成一个数就看不出是哪一种。
	Drops map[ports.DropReason]int
	// Buckets 各配额桶收到的条数（**通用面**）。
	//
	// 宿主能据此看到「这一轮事实桶收了几条、画像桶收了几条」，
	// 而不必自己按 Role/Source 猜——画像桶恒为 0 是个值得发现的症状
	//（画像被挤空会让页面重生成这个功能形同虚设）。
	Buckets map[ports.BucketID]int
	// FromCache 是否命中缓存。
	FromCache bool
	// Entities 本次推入活跃实体栈的实体（按栈顶在前）。
	Entities []string
	// LooseSearchUnavailable 宽松词面检索这一路**不可用**（宿主没实现那个能力）。
	//
	// 存在的理由：降级若不可见，症状就只剩「多话题查询打不中」——
	// 看着像内容不够，而病因是「宿主没实现 store.AtomLooseSearcher」。
	// 两者处置完全不同：前者该补内容，后者该实现那个接口。
	//
	// 它是**能力缺失**，不是**丢弃**：与 Drops 不是一回事，
	// 故单独一个字段（不塞进 DropReason——那会让「少召回」与「能力没有」混在一起）。
	LooseSearchUnavailable bool
}

// Empty 结果是否为空。
func (r *Result) Empty() bool { return r == nil || len(r.Snippets) == 0 }

// Render 渲染成可注入的文本块。
//
// 空结果返回空串而不是一句「没有记忆」——后者会占预算且诱导模型去想记忆这件事。
func (r *Result) Render() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder
	for _, s := range r.Snippets {
		b.WriteString("- [")
		b.WriteString(s.Role)
		b.WriteString("] ")
		b.WriteString(s.Body)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
