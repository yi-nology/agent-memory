package evaluation

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/yi-nology/agent-memory/types"
)

// synthVocab 合成数据集用的中性词表（固定，不随 seed 变）。
//
// 刻意全是中性的具体名词（矿物、颜色、植物、地貌），不含任何领域词：
// 门禁拦的是**词**，而**夹具里的词表也是词表**——
// 评测数据一旦带上领域词，它就成了「本仓认识那些对象」的证据，
// 而本仓认识的是事实、实体与会话，不是任何宿主世界里的东西。
//
// 固定（而不是随 seed 换一套）是为了让两份不同 seed 的数据集**只在组合上不同**，
// 不在词性上不同：否则跨 seed 比分数时，分数差异里混着「词难不难召回」这一层噪声。
var synthVocab = []string{
	"amber", "basalt", "cedar", "dune", "ember", "fjord", "glacier", "hollow",
	"ivory", "juniper", "kelp", "lantern", "marble", "nimbus", "ochre", "pebble",
	"quartz", "river", "silver", "thistle", "umber", "violet", "walnut", "xylem",
	"yarrow", "zephyr", "acorn", "bramble", "cinder", "driftwood", "feather", "gossamer",
	"hazel", "indigo", "jasmine", "kernel", "lichen", "mosaic", "nectar", "opal",
	"prairie", "quiver", "ripple", "saffron", "thicket", "umbra", "valley", "willow",
	"xenon", "yard", "zinnia", "almond", "balsa", "clover", "dahlia", "elm",
	"flint", "gorse", "heron", "iris", "lotus", "moss", "onyx", "pansy",
}

// synthScopes 合成数据集里出现的归属。
//
// 三个取值覆盖三种可见性局面：全局读者只看得到全局事实，
// 带归属的读者看得到「全局 + 同归属」。空值与两个互不相干的归属各占一档，
// 归属隔离才有被评测的机会。
var synthScopes = []types.Scope{"", "sc-a", "sc-b"}

// SyntheticSet 一份合成数据集与它配套的事实卡。
//
// 数据集与事实卡必须**成对**给出：只有标注没有语料，标注无从验证；
// 只有语料没有标注，语料里就没有「相关」这回事。
type SyntheticSet struct {
	// Dataset 用例集。
	Dataset Dataset
	// QA 同一份语料的**问答视图**。
	//
	// 刻意是同一份语料、同一批用例：这样检索口径与问答口径的数字
	// 可以**并排看**（同一批查询，检索指标多少、上下文够不够答），
	// 而不是两套不相干的数据集各说各话。
	QA    QADataset
	facts []*types.AtomCard
}

// Facts 返回这份数据集配套的事实卡（供召回器灌库）。
//
// 返回的是**副本**：调用方灌库时可能会改卡上的字段，
// 改的是副本就不会污染这份数据集本身（否则同一份 seed 生成两次
// 会得到两份不同的语料，可复现承诺当场破掉）。
func (s *SyntheticSet) Facts() []*types.AtomCard {
	out := make([]*types.AtomCard, len(s.facts))
	for i, f := range s.facts {
		c := *f
		out[i] = &c
	}
	return out
}

// Synthetic 生成一份合成数据集（确定性：同 seed 必得同结果）。
//
// 事实卡用 NewSynthetic 拿（两者同源，故语料与标注必然对得上）。
func Synthetic(seed int64, n int) Dataset {
	return NewSynthetic(seed, n).Dataset
}

// NewSynthetic 生成一份合成数据集与配套事实卡。
//
// # 一条用例的语料长什么样
//
// 每条用例的语料由四类事实卡组成（按**词面重叠度**区分）：
//
//   - 相关事实：与查询共享 3 / 2 / 1 个词（条数 2 或 3）。
//   - 近义干扰：与查询共享 2 个词，但**有一个词不同**。
//   - 无关事实：与查询一个词都不共享。
//   - 不可见事实：与查询共享全部 3 个词，但归属属于**别的读者**。
//
// # 为什么一定要混近义干扰
//
// 不混的话，词面召回器要么全中要么全不中，所有指标都是 1.0 或 0.0，
// **测不出任何东西**：排序变了、查询理解变了，指标一动不动，
// 于是「评测通过」什么也没证明。
// 混了之后 Recall 与 Precision 才会拉开差距——
// 近义干扰排在相关事实前面时，召回率掉、精确率也掉，两者的差额
// 正好指认「排序把次要的东西当成了答案」。
//
// # 为什么要有不可见事实
//
// 归属越权在数据上完全看不出来（内容一条不少，只是多了不该看的），
// 只有当它**排在结果里**时才会被精确率扣分。
// 故语料里必须有一条「词面完美但归属不对」的卡等着越权发生。
func NewSynthetic(seed int64, n int) *SyntheticSet {
	// 自己造随机源，不碰全局 rand。
	//
	// 全局 rand 的状态会被同进程里别的包推进（谁先调用谁改结果），
	// 于是「同 seed 同结果」这条保证在别的包动过全局 rand 之后就不成立——
	// 而失效形态是**静默的**：数据集变了，指标跟着变，没人说得清为什么。
	// 自己持源则把这件事钉死在本包内。
	rng := rand.New(rand.NewSource(seed))

	set := &SyntheticSet{}
	ds := Dataset{Name: fmt.Sprintf("synthetic(seed=%d,cases=%d)", seed, n)}
	qa := QADataset{Name: fmt.Sprintf("synthetic-qa(seed=%d,cases=%d)", seed, n)}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// 词元发放器：词表里的词 + 一个递增后缀，保证**全库唯一**。
	//
	// 唯一是硬要求：两个用例共用一个词时，词面召回器会把它们的卡混在一起，
	// 标注随即失效（一条卡成了两条用例的「相关」）。
	serial := 0
	nextWord := func() string {
		w := synthVocab[serial%len(synthVocab)] + strconv.Itoa(serial/len(synthVocab))
		serial++
		return w
	}

	for i := 0; i < n; i++ {
		caseID := fmt.Sprintf("case-%03d", i)
		entityID := fmt.Sprintf("ent-%03d", i)
		scope := synthScopes[rng.Intn(len(synthScopes))]

		q := []string{nextWord(), nextWord(), nextWord()}
		relevantN := 2 + rng.Intn(2) // 2 或 3 条相关事实

		var (
			relevantIDs []string
			nonRelIDs   []string
			at          = base.Add(time.Duration(i) * time.Hour)
		)

		// 相关事实：共享词数逐条递减（3 / 2 / 1）。
		// 递减是有意的：共享 1 个词的那条排在最后，
		// 于是「前 k 条装不装得下全部相关」成为一道真问题，
		// Recall@k 随 k 变化，而不是一条直线。
		for r := 0; r < relevantN; r++ {
			var tokens []string
			switch r {
			case 0:
				tokens = []string{q[0], q[1], q[2]}
			case 1:
				tokens = []string{q[0], q[1], nextWord()}
			default:
				tokens = []string{q[0], nextWord()}
			}
			id := fmt.Sprintf("%s-r%d", caseID, r)
			set.facts = append(set.facts, synthCard(id, entityID, scope, tokens, at, len(set.facts)))
			relevantIDs = append(relevantIDs, id)
		}

		// 近义干扰：共享 2 个词，另一个词换掉。
		for d := 0; d < 2; d++ {
			var tokens []string
			if d == 0 {
				tokens = []string{q[0], q[1], nextWord()}
			} else {
				tokens = []string{q[1], q[2], nextWord()}
			}
			id := fmt.Sprintf("%s-d%d", caseID, d)
			set.facts = append(set.facts, synthCard(id, entityID, scope, tokens, at, len(set.facts)))
			nonRelIDs = append(nonRelIDs, id)
		}

		// 无关事实：与查询一个词都不共享。
		for u := 0; u < 3; u++ {
			tokens := []string{nextWord(), nextWord()}
			id := fmt.Sprintf("%s-u%d", caseID, u)
			set.facts = append(set.facts, synthCard(id, entityID, scope, tokens, at, len(set.facts)))
			nonRelIDs = append(nonRelIDs, id)
		}

		// 不可见事实：词面完美但归属属于别的读者。
		//
		// 归属必须**非空且不同于本用例的归属**：
		// 全局卡对任何读者都可见（types.Scope.VisibleTo），
		// 用全局归属做这张卡，它根本不构成越权陷阱。
		foreign := synthScopes[1]
		if scope == foreign {
			foreign = synthScopes[2]
		}
		trapID := fmt.Sprintf("%s-x0", caseID)
		set.facts = append(set.facts,
			synthCard(trapID, entityID, foreign, []string{q[0], q[1], q[2]}, at, len(set.facts)))
		nonRelIDs = append(nonRelIDs, trapID)

		ds.Cases = append(ds.Cases, Case{
			ID:          caseID,
			Query:       strings.Join(q, " "),
			Scope:       scope,
			Relevant:    relevantIDs,
			NonRelevant: nonRelIDs,
			Entities:    []string{entityID},
			SessionID:   fmt.Sprintf("sess-%d", i%2),
			ThreadID:    fmt.Sprintf("thr-%d", i%2),
		})

		// 问答视图：同一批用例，多三样东西。
		//
		//   - Question：人话形态的问句（与检索串不同，但共享词元）。
		//   - Answer：标准答案（合成集里取**第一条相关事实的正文**——
		//     它不是自然语言答案，但可复现且可判分；
		//     真实基准的自然语言答案由宿主的 Judge 判）。
		//   - SupportIDs：支撑该答案的事实（= 相关集合）。
		qa.Cases = append(qa.Cases, QACase{
			Case:       ds.Cases[len(ds.Cases)-1],
			Question:   "关于 " + strings.Join(q, " ") + " 的情况是什么？",
			Answer:     synthAnswerOf(set.facts, relevantIDs),
			SupportIDs: relevantIDs,
		})
	}

	set.Dataset = ds
	set.QA = qa
	return set
}

// synthAnswerOf 取第一条相关事实的正文当标准答案。
func synthAnswerOf(facts []*types.AtomCard, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	for _, f := range facts {
		if f.ID == ids[0] {
			return f.Assertion
		}
	}
	return ""
}

// synthImps / synthConf 合成事实卡用的档位轮转表。
//
// 轮转而不是随机：档位不该给数据集带来额外噪声——
// 评测要比的是召回质量，不是重要度分布。
var (
	synthImps = []types.Importance{types.ImportanceHigh, types.ImportanceMed, types.ImportanceLow}
	synthConf = []types.Confidence{types.ConfidenceHigh, types.ConfidenceMed, types.ConfidenceLow}
)

// synthCard 造一张合成事实卡。
//
// 断言正文就是几个中性词的拼装：它不是散文，
// 因为评测只看**词面可达性**——写成散文只会引入无法控制的额外词。
func synthCard(id, entityID string, scope types.Scope, tokens []string, at time.Time, idx int) *types.AtomCard {
	return &types.AtomCard{
		ID:           id,
		EntityID:     entityID,
		Scope:        scope,
		CandidateID:  "cand-" + id,
		RawEventIDs:  []string{"ev-" + id},
		Assertion:    strings.Join(tokens, " "),
		SearchTerms:  append([]string(nil), tokens...),
		OccurredAt:   at,
		Confidence:   synthConf[idx%len(synthConf)],
		Importance:   synthImps[idx%len(synthImps)],
		CreatedAt:    at,
		UpdatedAt:    at,
		QuoteEventID: "ev-" + id,
		ValidFrom:    at,
		Version:      1,
	}
}
