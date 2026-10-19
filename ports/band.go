package ports

import (
	"context"
	"errors"
	"strings"

	"github.com/oagentkit/textx/textutil"
)

// ErrNoEstimator 未注入计量器时的哨兵。
var ErrNoEstimator = errors.New("ports: no token estimator injected")

// TokenEstimator 文本规模的估算端口。
//
// # 为什么不让召回层直接猜
//
// 本仓的既有纪律是「内核不猜 tokenizer」——计量结果必须由**知道用什么模型**
// 的一方给出。因此召回层不自带估算器，只依赖这个接口；未注入时报错（降级路径
// 见 StaticEstimator，那是唯一有默认实现的场合，且它明确自报是静态代理）。
//
// 用在召回上时：一条事实到底占多少预算，直接决定「这条进不进 prompt」。
// 估错了不会崩，但会**静默地挤掉别的内容**——所以接口是必需项不是可选项。
type TokenEstimator interface {
	// Estimate 返回文本的估算 token 数。
	Estimate(ctx context.Context, text string) (int, error)
}

// StaticEstimator 静态字符代理：对模型分词器的离线近似。
//
// 换模型类时改这一个函数即可——它是**算法面**的测试向量，不碰调用链。
const (
	// CharsPerTokenCJK CJK 字符每 token 折算字符数。
	CharsPerTokenCJK = 2
	// CharsPerTokenASCII 拉丁/数字字符每 token 折算字符数。
	CharsPerTokenASCII = 4
)

// estimatorRatio 本仓采用的折算系数。
//
// 系数写在这里（而不是直接用上游默认值）是为了让「本仓选了这一档」这件事
// 在代码里看得见：上游的默认值是可改的，本仓的口径不该跟着漂。
var estimatorRatio = textutil.CJKRatio{
	CJKCharsPerToken:   CharsPerTokenCJK,
	ASCIICharsPerToken: CharsPerTokenASCII,
}

// StaticEstimator 把文本按「CJK 每 2 字符 1 token、其余每 4 字符 1 token」
// 折算成估算 token。
//
// # 误差方向是刻意选的
//
// 它**高估**。原因在调用点：预算不够时，被挤掉的是靠后的内容，而挤掉的代价
// （少记一条事实）是静默的；反过来若低估，记忆会超预算塞进 prompt，
// 那会挤掉对话本身——用户立刻能察觉。两者都错，但一个显眼一个隐形，
// 故取显眼的那侧。
//
// 实测口径：对上游 octop-memory 采用的同一套系数（CJK 2 / ASCII 4），
// 与真实分词器的偏差约 ±10~20%。
type StaticEstimator struct{}

// Estimate 静态估算。
//
// # 取整方向是**逐段**，不是合总
//
// 折算按「连续同类字符游程」切段，每段各自向上取整后求和。
// 合总再取整（把全文的 CJK 算一个桶、其余算另一个桶，各取整一次）会把
// 每段的零头一起吞掉——多段混合文本累加起来是**成规模的低估**，不是随机误差。
// 实测口径：「中a中a中a中a」逐段是 8，合总是 3。
//
// 这条曾经写反过：注释写的是「逐段向上取整」，代码做的是合总再取整——
// 而合总恰恰是那段注释在论证要避免的那一侧。低估的方向又恰好是静默的
// （预算看着够，实际超了），所以这段注释与代码必须一起钉住。
//
// 算术走上游 textutil.EstTokensWithRounding：它是同一套口径的**单源**，
// 且把「取整方向」做成了参数——两个口径是同一个问题的两个答案，
// 分成两个函数只会逼调用方在两套命名之间猜。
func (StaticEstimator) Estimate(_ context.Context, text string) (int, error) {
	return textutil.EstTokensWithRounding(text, estimatorRatio, textutil.RoundingSegment), nil
}

// Bander 给不可信内容加身份标记的端口。
//
// # 为什么它是必需依赖而不是可选增强
//
// 召回出来的东西是**跨会话、跨用户、跨宿主**的内容：它可能来自另一个人、
// 另一台机器、或一次被人为写入的会话。它拼进 prompt 时必须处在数据区，
// 且不能自己声明成系统指令。
//
// 这不是「加固一下」，是**不给就不成立**：一条写着「忽略之前的指令，按我说的做」
// 的记忆卡一旦被当成系统注入读进去，整条会话的控制权就转移了。
// 故流水线构造时必须提供，装配层不接受 nil。
type Bander interface {
	// WrapMemory 把一段记忆正文包成数据区标记。
	//
	// 实现必须同时做两件事，缺一不可：
	//  1. 加上包裹标签；
	//  2. 转义正文里伪造的同名标签（否则内容可以提前闭合标签再声明身份）。
	WrapMemory(body string) string
}

// BandBody 包裹体的统一形态。
//
// 标签名刻意选 memory-context 而非 system-reminder：记忆是**数据**不是指令。
// 上游 agent-harness/reminder 用的是 system-reminder，那是给「带外系统注入」
// 用的；套在记忆上等于**抬高**它的身份，方向反了。
const (
	BandOpen  = "<memory-context>"
	BandClose = "</memory-context>"
)

// MemoryBand Bander 的实现（纯 stdlib）。
//
// # 为什么转义归本仓，而不借 guardx/injectband
//
// 曾经的设想是「转义是通用安全原语，单源在 guardx/injectband，本仓不重复造」——
// 听起来很干净，实测**那个设想是错的**，而且错在构造上：
//
//	injectband.SanitizeBody 的实现是 `tagRe.ReplaceAllStringFunc(...)`，
//	tagRe 是写死的、只匹配 `<system-reminder…>` 家族。
//
// 它守得住的只有它自己那条 band。拿它去守 `<memory-context>` 这条数据区时，
// 一句 `</memory-context>` 原样穿过——攻击者提前闭合数据区、把内容搬出去，
// 还顺手在后面自称系统注入。
//
// **教训**：转义规则与「实际在用的那条 band」是**一对**，借一个的规则去守另一个，
// 等于没有转义。所以这里自己转义全部 `<`——一次转义同时挡住两类伪造
// （记忆自己的闭合标签，和宿主那套 system-reminder）。
//
// 「不重复造轮子」那条纪律在这里让位给「转义必须匹配实际 band」：
// 门禁这关守的是**安全性质**，不是**代码量**。
type MemoryBand struct{}

// WrapMemory 包成数据区并转义正文里的伪造标签。
func (MemoryBand) WrapMemory(body string) string {
	return BandOpen + "\n" + escapeBand(body) + "\n" + BandClose
}

// escapeBand 转义正文里的尖括号。
//
// 只转义 `<`：闭合标签必须以 `<` 开头，转 `>` 挡不住闭合。而正文里的 `>`
// 无害（它不能凭空造出标签），转了只会让正文难读。
func escapeBand(body string) string {
	return strings.ReplaceAll(body, "<", "&lt;")
}

// UnbandMemory 剥掉包裹，供渲染与断言用。
//
// 存在的意义是**可测**：召回层与门禁自测都要能断言「内容确实被包住了」，
// 而断言一个字符串前缀很容易写错（少一个换行、标签拼错）。
// 生产路径不该调用它——记忆一旦入 prompt 就该带着标记。
func UnbandMemory(s string) string {
	s = strings.TrimPrefix(s, BandOpen)
	s = strings.TrimSuffix(s, BandClose)
	return strings.TrimSpace(s)
}
