// 引擎封装：把 spike 验证的机制固化为排查内核唯一入口（SPIKE-EINO.md 结论）。
// 上层（internal/agent）只依赖本文件暴露的 Run/EngineInput/EngineOutput，
// 不直接接触 adk/compose——Eino 相关 import 全部收敛在本包。
package eino

import (
	"context"
	"fmt"
	"sync"

	"github.com/cloudwego/eino/adk"
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

// next 原子分配下一个证据编号并登记条目，返回条目下标。
func (st *evidenceState) next(name, args string) (string, int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	id := fmt.Sprintf("T%d", len(st.entries)+1)
	st.entries = append(st.entries, Evidence{ID: id, Tool: name, Args: args})
	return id, len(st.entries) - 1
}

func (st *evidenceState) settle(idx int, result string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if idx >= 0 && idx < len(st.entries) {
		st.entries[idx].Result = result
	}
}

// labeledTool 给每个工具调用打上证据编号：编号写进返回内容开头，
// 模型在对话里能"看见"自己的证据编号，报告才能正确引用（此前编号只在
// 落库侧生成，模型只能瞎猜 T1/T2）。
type labeledTool struct {
	tool.InvokableTool
	col *evidenceState
}

func (t *labeledTool) InvokableRun(ctx context.Context, argsJSON string, opts ...tool.Option) (string, error) {
	var name string
	if info, err := t.Info(ctx); err == nil && info != nil {
		name = info.Name
	}
	id, idx := t.col.next(name, argsJSON)
	res, err := t.InvokableTool.InvokableRun(ctx, argsJSON, opts...)
	if err != nil {
		t.col.settle(idx, "error: "+err.Error())
		return "", err
	}
	t.col.settle(idx, res)
	return "[" + id + "] " + res, nil
}

func wrapTools(tools []tool.BaseTool, col *evidenceState) []tool.BaseTool {
	out := make([]tool.BaseTool, len(tools))
	for i, t := range tools {
		if inv, ok := t.(tool.InvokableTool); ok {
			out[i] = &labeledTool{InvokableTool: inv, col: col}
		} else {
			out[i] = t
		}
	}
	return out
}

// Run 执行一次排查。并发安全：无共享可变状态（每次运行独立 agent 实例）。
func Run(ctx context.Context, in EngineInput) EngineOutput {
	if in.MaxIter <= 0 {
		in.MaxIter = 12
	}
	st := &evidenceState{}
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "diagnosis",
		Instruction:   in.System,
		Model:         in.Model,
		ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: wrapTools(in.Tools, st)}},
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
	}, adk.WithAfterToolCallsHook(func(context.Context) error { steps++; return nil }))

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
