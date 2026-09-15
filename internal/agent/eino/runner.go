// 引擎封装：把 spike 验证的机制固化为排查内核唯一入口（SPIKE-EINO.md 结论）。
// 上层（internal/agent）只依赖本文件暴露的 Run/EngineInput/EngineOutput，
// 不直接接触 adk/compose——Eino 相关 import 全部收敛在本包。
package eino

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// EngineInput 一次排查执行的入参。System 为剧本全文+输出格式约束；
// Tools 为按剧本收权后的工具集；MaxIterations 硬顶预算。
type EngineInput struct {
	Model   model.BaseModel[*schema.Message]
	System  string
	User    string
	Tools   []tool.BaseTool
	MaxIter int
}

// Evidence 证据链条目：T<n> 编号 + 工具名 + 输入输出，报告据此引用。
type Evidence struct {
	ID     string `json:"id"`
	Tool   string `json:"tool"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
}

// EngineOutput 排查执行产出。
type EngineOutput struct {
	Content  string // 最终回答（预期为报告 JSON）
	Evidence []Evidence
	Steps    int // 实际模型生成轮次
	Err      error
}

type evidenceState struct {
	mu      sync.Mutex
	entries []Evidence
}

func evidenceHandler(st *evidenceState) callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			if info == nil || info.Component != components.ComponentOfTool {
				return ctx
			}
			in := tool.ConvCallbackInput(input)
			if in == nil {
				return ctx
			}
			st.mu.Lock()
			st.entries = append(st.entries, Evidence{
				ID:   fmt.Sprintf("T%d", len(st.entries)+1),
				Tool: info.Name,
				Args: in.ArgumentsInJSON,
			})
			st.mu.Unlock()
			return ctx
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			if info == nil || info.Component != components.ComponentOfTool {
				return ctx
			}
			out := tool.ConvCallbackOutput(output)
			st.mu.Lock()
			if out != nil && len(st.entries) > 0 && st.entries[len(st.entries)-1].Result == "" {
				st.entries[len(st.entries)-1].Result = out.Response
			}
			st.mu.Unlock()
			return ctx
		}).
		Build()
}

// Run 执行一次排查。并发安全：无共享可变状态（每次运行独立 agent 实例）。
func Run(ctx context.Context, in EngineInput) EngineOutput {
	if in.MaxIter <= 0 {
		in.MaxIter = 12
	}
	st := &evidenceState{}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:        "diagnosis",
		Instruction: in.System,
		Model:       in.Model,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: in.Tools}},
		MaxIterations: in.MaxIter,
	})
	if err != nil {
		return EngineOutput{Err: fmt.Errorf("eino: 构建 agent 失败: %w", err)}
	}
	// SPIKE 结论：必须经 AgentWithOptions 包装，WithCallbacks 的 handler 才生效
	runner := adk.AgentWithOptions(ctx, agent)

	var steps int
	iter := runner.Run(ctx, &adk.AgentInput{
		Messages: []*schema.Message{{Role: schema.User, Content: in.User}},
	}, adk.WithCallbacks(evidenceHandler(st)),
		adk.WithAfterToolCallsHook(func(context.Context) error { steps++; return nil }))

	out := EngineOutput{}
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			out.Err = ev.Err
			continue
		}
		if msg, _, err := adk.GetMessage(ev); err == nil && msg != nil &&
			msg.Role == schema.Assistant && msg.Content != "" && len(msg.ToolCalls) == 0 {
			out.Content = msg.Content
		}
	}
	out.Steps = steps
	out.Evidence = st.entries
	return out
}
