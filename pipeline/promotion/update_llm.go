package promotion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yi-nology/agent-memory/ports"
	"github.com/yi-nology/agent-memory/types"
)

// 本文件：事实更新关系里的**模型辅助判定**。
//
// # 为什么需要模型
//
// 规则看得见「数字变了」（differsInValue），但看不见「换了个说法、值其实也变了」：
//
//	旧：机器甲使用版本 1.32
//	新：机器甲已升级到版本 1.36
//
// 这两条的词面相似度并不高（「使用」与「已升级到」差得远），
// 故规则判不出「这是同一个东西换了值」——而它确实���。
//
// # 模型只做**路由**，动作仍由规则定
//
// 纪律同抽取面那条：**抽取调模型（会幻觉），判定要可复现**。
// 故模型只回答「这两条是什么关系」，处置由固定的表决定：
//
//	ADD     新增一条独立事实（默认：两条说的不是同一件事）
//	UPDATE  同一个东西换了值 → 取代
//	DELETE  旧事实该作废 → **送人工**（绝不由模型一句话作废一条已存的事实）
//	NOOP    重复（与并入同义）→ 并入
//	CONFLICT互相矛盾 → **送人工**（不自动裁决）
//
// 处置表里凡是「重」的动作（作废、裁决矛盾）都落到人工，
// 模型能触发的只有「取代」与「并入」——
// 而取代还有一道闸：相似度低于地板时**仍然送人工**。
//
// # 模型失败一律上抛
//
// 不静默退化成「不取代」（那会把「模型挂了」变成「事实更新悄悄停了」，
// 而症状是「记忆里留着两个版本的事实」，看不出来）；
// 也不退化成「全部送人工」（那是一次模型抖动把人审队列灌满）。
// 上抛是诚实的：调用方看得见失败。

// relation 两条陈述之间的关系。
type relation string

const (
	relationAdd      relation = "add"
	relationUpdate   relation = "update"
	relationDelete   relation = "delete"
	relationNoop     relation = "noop"
	relationConflict relation = "conflict"
)

// UpdatePrompt 事实更新判定的提示词。
//
// 只给「要什么形状的输出」，不教模型怎么判断——那是模型的事。
// 提示里**不出现任何领域指令**（本包不认识任何领域）。
//
// 为什么不给判定标准：给了标准，模型的输出就依赖提示词的措辞，
// 而提示词一改，全部历史判定的口径就变了。
// 口径要稳定，所以只描述任务。
func UpdatePrompt(oldAssertion, newAssertion string) string {
	var b strings.Builder
	b.WriteString("判断下面两条陈述之间的关系，只输出 JSON：\n")
	b.WriteString(`{"relation":"add|update|delete|noop|conflict","reason":"一句话理由"}` + "\n")
	b.WriteString("关系含义：\n")
	b.WriteString("  add      两条说的是不同的两件事\n")
	b.WriteString("  update   说的是同一件事，但其中的值变了（新的取代旧的）\n")
	b.WriteString("  delete   新的这条说明旧的这条不该存在\n")
	b.WriteString("  noop     两条说的是同一件事，且没有变化\n")
	b.WriteString("  conflict 两条互相矛盾\n")
	b.WriteString("不得添加两条之外的信息；不确定就给 conflict。\n")
	b.WriteString("旧：")
	b.WriteString(oldAssertion)
	b.WriteString("\n新：")
	b.WriteString(newAssertion)
	b.WriteString("\n")
	return b.String()
}

// classifyUpdate 问模型：候选与既有事实是什么关系。
//
// # 归一
//
// 模型可能给大写、带空格、带 markdown 围栏。归一在这里做一次，
// 让下游只面对五个确定的值。
//
// # 判不出来就是 conflict
//
// 模型给不出合法 JSON、或给了不认识的关系 → 当作 conflict（送人工）。
// 理由：判不出意味着**不该自动裁决**，而「送人工」正是本仓的默认姿态。
// 但「模型调用失败」（网络/超时）是另一回事 → 上抛。
func classifyUpdate(out string) (relation, string, error) {
	s := strings.TrimSpace(out)
	s = stripFence(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			s = s[i : j+1]
		}
	}
	var obj struct {
		Relation string `json:"relation"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return relationConflict, "", fmt.Errorf("判定结果解析失败: %w", err)
	}
	switch relation(strings.ToLower(strings.TrimSpace(obj.Relation))) {
	case relationAdd:
		return relationAdd, obj.Reason, nil
	case relationUpdate:
		return relationUpdate, obj.Reason, nil
	case relationDelete:
		return relationDelete, obj.Reason, nil
	case relationNoop:
		return relationNoop, obj.Reason, nil
	case relationConflict:
		return relationConflict, obj.Reason, nil
	default:
		return relationConflict, obj.Reason, nil
	}
}

// stripFence 剥掉模型输出可能带的 ``` 围栏。
//
// 被要求输出 JSON 时，模型经常连着围栏一起给；
// 不剥的后果是**每次判定都失败**，而失败按上面的约定会变成「全送人工」——
// 于是人审队列被格式噪声灌满，而没人看得出真问题在哪。
func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// askModel 调模型问关系。
func (p *Promoter) askModel(ctx context.Context, c *types.Candidate, old *types.AtomCard) (relation, string, error) {
	if p.cfg.LLM == nil {
		// 没装模型 = 没有这一支判定：调用点不该走到这里。
		return relationConflict, "", errors.New("promotion: 未装配模型，却走到了模型判定")
	}
	out, err := p.cfg.LLM.Complete(ctx, ports.LLMRequest{
		Prompt:    UpdatePrompt(old.Assertion, c.Assertion),
		Purpose:   "memory.fact.update",
		JSON:      true,
		MaxTokens: 300,
	})
	if err != nil {
		return relationConflict, "", fmt.Errorf("事实更新判定调用模型失败: %w", err)
	}
	rel, reason, err := classifyUpdate(out)
	if err != nil {
		return relationConflict, "", err
	}
	return rel, reason, nil
}
