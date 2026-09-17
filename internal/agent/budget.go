// tokenModel 排查内核的模型包装：从每次 Generate 的 ResponseMeta.Usage 累计
// token 用量（报告 Cost 与预算控制的依据），超限后拒绝继续调用——预算熔断
// 以错误形式终止排查，经 Diagnose 错误路径产出"部分结论"报告（needs_human）。
package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

var errTokenBudget = errors.New("agent: token 预算已耗尽（agent.max_tokens），中止排查产出部分结论")

type tokenModel struct {
	inner     model.BaseModel[*schema.Message]
	maxTokens int // <=0 不限

	mu  sync.Mutex
	in  int // 累计 PromptTokens
	out int // 累计 CompletionTokens
}

func newTokenModel(inner model.BaseModel[*schema.Message], maxTokens int) *tokenModel {
	return &tokenModel{inner: inner, maxTokens: maxTokens}
}

func (m *tokenModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if m.maxTokens > 0 {
		m.mu.Lock()
		over := m.in+m.out >= m.maxTokens
		m.mu.Unlock()
		if over {
			return nil, errTokenBudget
		}
	}
	resp, err := m.inner.Generate(ctx, input, opts...)
	if err == nil && resp != nil && resp.ResponseMeta != nil && resp.ResponseMeta.Usage != nil {
		u := resp.ResponseMeta.Usage
		m.mu.Lock()
		m.in += u.PromptTokens
		m.out += u.CompletionTokens
		m.mu.Unlock()
	}
	return resp, err
}

// Stream 透传（排查内核非流式；流式会话不计量、不受预算拦截）。
func (m *tokenModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, input, opts...)
}

func (m *tokenModel) usage() (in, out int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.in, m.out
}
