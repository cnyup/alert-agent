// llmcall LLM 调用的无 eino 门面（PLAN Wave 3.3）：cmd 与其他 internal 包
// 需要一次性 LLM 调用（蒸馏/评测）时经此转出，schema.Message 不越过 agent 边界。
package agent

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// Tool eino 工具接口的包内别名（cmd 等外部包经此引用，不直连 eino）。
type Tool = tool.BaseTool

// GenerateText 一次性文本调用（系统提示可选）。eino 类型在此封包。
func GenerateText(ctx context.Context, m model.BaseModel[*schema.Message], system, user string) (string, error) {
	msgs := make([]*schema.Message, 0, 2)
	if system != "" {
		msgs = append(msgs, &schema.Message{Role: schema.System, Content: system})
	}
	msgs = append(msgs, &schema.Message{Role: schema.User, Content: user})
	resp, err := m.Generate(ctx, msgs)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// ToolReasoner 透出推理模型类型（eval 等既有路径用）。
type ToolReasoner = model.BaseModel[*schema.Message]
