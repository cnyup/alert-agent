// Package llm LLM 接线：eino-ext OpenAI 兼容模型（base_url 全可配，
// 覆盖 99% 供应商与内网网关，DESIGN.md §0）。
package llm

import (
	"context"
	"errors"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/cnyup/alert-agent/internal/config"
)

// ErrNotConfigured 表示该端点未配置（调用方可据此优雅降级）。
var ErrNotConfigured = errors.New("llm: 端点未配置（base_url/model 缺失）")

// New 按配置构造 OpenAI 兼容 ChatModel。
func New(ctx context.Context, ref config.ModelRef) (model.BaseModel[*schema.Message], error) {
	if ref.BaseURL == "" || ref.Model == "" {
		return nil, ErrNotConfigured
	}
	return einoopenai.NewChatModel(ctx, &einoopenai.ChatModelConfig{
		BaseURL: ref.BaseURL,
		APIKey:  ref.APIKey,
		Model:   ref.Model,
	})
}
