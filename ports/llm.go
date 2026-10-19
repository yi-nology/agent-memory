// Package ports 记忆内核对外部能力的**接口面**：模型调用、向量化、向量索引。
//
// 本包只声明协议，不含任何实现、不 import 任何第三方 SDK。
// 「装了包不等于配好了模型」是本仓的显式立场——模型能力必须由宿主注入，
// 否则核心就绑死在一个 SDK 上，且无模型时无法降级运行。
package ports

import (
	"context"
	"errors"
)

// ErrNoLLM 未注入模型能力时的哨兵。
//
// 提炼、晋升升级、页面重生成都允许在没有模型的情况下**显式失败**：
// 调用方据此降级（手工 store + 词法召回照常工作），而不是静默产出一段
// 幻觉正文。**未注入必须报错，不许返回空串假装成功**——空串会被下游
// 当成「模型说了没什么」，从而静默丢弃整批候选。
var ErrNoLLM = errors.New("ports: no LLM client injected")

// LLMRequest 一次模型调用请求。
type LLMRequest struct {
	// Prompt 提示正文。
	Prompt string
	// Purpose 用途标识（抽取 / 晋升升级 / 页面重生成 / 情节摘要 / 周期摘要）。
	//
	// 宿主据此选模型与预算。不给的话宿主只能猜——而不同用途的模型档位
	// 差着数量级（摘要是批量作业，晋升升级是交互路径）。
	Purpose string
	// MaxTokens 输出上限（0=用宿主默认）。
	MaxTokens int
	// JSON 期望结构化输出（宿主据此切解析模式）。
	JSON bool
}

// LLM 模型能力端口。
//
// 单一方法，不做多级抽象：记忆内核只做「把提示送出去、把文本拿回来」，
// 重试、路由、降级、限流都是宿主侧的事（那属于编排归 harness）。
type LLM interface {
	Complete(ctx context.Context, req LLMRequest) (string, error)
}

// NoopLLM 未注入时的占位实现：任何调用都返回 ErrNoLLM。
//
// 存在的意义是**让降级路径可以被正常装配与测试**——宿主在无模型环境下
// 也能跑通全部用例，只是提炼类动作按契约失败。返回一个具体类型而不是 nil，
// 免得每个调用点都判 nil。
type NoopLLM struct{}

// Complete 恒返回 ErrNoLLM。
func (NoopLLM) Complete(context.Context, LLMRequest) (string, error) {
	return "", ErrNoLLM
}
