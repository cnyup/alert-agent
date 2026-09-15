// Package eino 是排查内核的 Eino 集成隔离层（DESIGN.md §6）：
// 所有 cloudwego/eino 相关 import 收敛在此包内，上层只见 DiagnosisRunner。
//
// 本文件是 P0 spike：用脚本化假模型驱动真实的 ChatModelAgent ReAct loop，
// 确定性验证设计文档 §4 的三个硬诉求（不烧 token）：
//  1. callbacks 能否记录证据链（工具调用编号 + 输入输出）
//  2. ToolsConfig 按次构造能否实现按剧本收权（模型侧只见允许的工具）
//  3. MaxIterations 能否硬性卡住预算
package eino

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// ---- 假模型：按脚本逐轮返回，同时记录每轮被授予的工具清单 ----

type scriptedTurn struct {
	toolCalls []schema.ToolCall
	content   string
}

type fakeModel struct {
	mu   sync.Mutex
	turns []scriptedTurn
	next  int

	// 每轮 Generate 实际拿到的工具（收权验证的事实来源）
	offeredTools  [][]*schema.ToolInfo
	generateCalls int
}

func (m *fakeModel) Generate(_ context.Context, _ []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := model.GetCommonOptions(&model.Options{}, opts...)
	m.offeredTools = append(m.offeredTools, o.Tools)
	m.generateCalls++
	if m.next >= len(m.turns) {
		return nil, errors.New("fakeModel: 脚本轮次耗尽")
	}
	t := m.turns[m.next]
	m.next++
	if len(t.toolCalls) > 0 {
		return &schema.Message{Role: schema.Assistant, ToolCalls: t.toolCalls}, nil
	}
	return &schema.Message{Role: schema.Assistant, Content: t.content}, nil
}

func (*fakeModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("spike: 流式未使用")
}

// ---- 假工具 ----

type fakeTool struct {
	name string
	mu   sync.Mutex
	args []string
}

func (f *fakeTool) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.args...)
}

func (f *fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "spike: " + f.name}, nil
}

func (f *fakeTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	f.mu.Lock()
	f.args = append(f.args, args)
	f.mu.Unlock()
	return fmt.Sprintf(`{"result":"%s ok"}`, f.name), nil
}

// ---- 证据链记录器：callbacks.Handler，给每次工具调用编号 T<n> ----

type evidenceEntry struct {
	ID      string
	RunName string // RunInfo.Name——spike 实证它是否为工具名
	Args    string
	Result  string
}

type evidenceRecorder struct {
	mu      sync.Mutex
	entries []evidenceEntry
}

func (r *evidenceRecorder) snapshot() []evidenceEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]evidenceEntry(nil), r.entries...)
}

func (r *evidenceRecorder) handler() callbacks.Handler {
	return callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			if info == nil || info.Component != components.ComponentOfTool {
				return ctx
			}
			in := tool.ConvCallbackInput(input)
			if in == nil {
				return ctx
			}
			r.mu.Lock()
			r.entries = append(r.entries, evidenceEntry{
				ID:      fmt.Sprintf("T%d", len(r.entries)+1),
				RunName: info.Name,
				Args:    in.ArgumentsInJSON,
			})
			r.mu.Unlock()
			return ctx
		}).
		OnEndFn(func(ctx context.Context, info *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
			if info == nil || info.Component != components.ComponentOfTool {
				return ctx
			}
			out := tool.ConvCallbackOutput(output)
			r.mu.Lock()
			if out != nil && len(r.entries) > 0 && r.entries[len(r.entries)-1].Result == "" {
				r.entries[len(r.entries)-1].Result = out.Response
			}
			r.mu.Unlock()
			return ctx
		}).
		Build()
}

// ---- 运行助手 ----

func runAgent(t *testing.T, fm *fakeModel, tools []tool.BaseTool, maxIter int,
	rec callbacks.Handler, hook func(context.Context) error) (string, error) {
	t.Helper()
	ctx := context.Background()
	agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "spike-agent",
		Instruction:   "你是告警排查引擎（spike）",
		Model:         fm,
		ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}},
		MaxIterations: maxIter,
	})
	if err != nil {
		t.Fatalf("NewChatModelAgent: %v", err)
	}
	// 关键：WithCallbacks 的 handler 只在 flowAgent 层生效（flow.go: initAgentCallbacks），
	// 裸 ChatModelAgent 不挂 handler，必须经 AgentWithOptions 包装
	var runner adk.Agent = adk.AgentWithOptions(ctx, agent)
	var opts []adk.AgentRunOption
	if rec != nil {
		opts = append(opts, adk.WithCallbacks(rec))
	}
	if hook != nil {
		opts = append(opts, adk.WithAfterToolCallsHook(hook))
	}
	iter := runner.Run(ctx, &adk.AgentInput{
		Messages: []*schema.Message{{Role: schema.User, Content: "排查：订单服务 5xx 飙升"}},
	}, opts...)

	var finalErr error
	var lastContent string
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			finalErr = ev.Err
			continue
		}
		if msg, _, err := adk.GetMessage(ev); err == nil && msg != nil &&
			msg.Role == schema.Assistant && msg.Content != "" && len(msg.ToolCalls) == 0 {
			lastContent = msg.Content
		}
	}
	return lastContent, finalErr
}

// ---- Spike 1：证据链 —— callbacks 记录每次工具调用的编号/入参/结果 ----

func TestSpikeEvidenceChain(t *testing.T) {
	fm := &fakeModel{turns: []scriptedTurn{
		{toolCalls: []schema.ToolCall{
			{ID: "call_1", Function: schema.FunctionCall{Name: "prometheus_query",
				Arguments: `{"query":"rate(http_requests_total{code=~\"5..\"}[5m])"}`}},
			{ID: "call_2", Function: schema.FunctionCall{Name: "k8s_get_pod", Arguments: `{"ns":"prod"}`}},
		}},
		{content: `{"summary":"发布引入超时", "evidence":["T1","T2"]}`},
	}}
	tq := &fakeTool{name: "prometheus_query"}
	tk := &fakeTool{name: "k8s_get_pod"}
	rec := &evidenceRecorder{}

	content, err := runAgent(t, fm, []tool.BaseTool{tq, tk}, 10, rec.handler(), nil)
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if content == "" {
		t.Fatal("未捕获最终回答")
	}

	entries := rec.snapshot()
	if len(entries) != 2 {
		t.Fatalf("证据链应记 2 条工具调用，实得 %d: %+v", len(entries), entries)
	}
	for i, e := range entries {
		if e.ID != fmt.Sprintf("T%d", i+1) {
			t.Errorf("编号不连续: %+v", e)
		}
		if e.RunName == "" {
			t.Errorf("RunInfo.Name 为空，无法定位工具: %+v", e)
		}
		if e.Args == "" || e.Result == "" {
			t.Errorf("入参或结果缺失: %+v", e)
		}
	}
	totalCalls := len(tq.calls()) + len(tk.calls())
	if totalCalls != 2 {
		t.Fatalf("工具应各执行一次，实得 %d 次", totalCalls)
	}
	t.Logf("证据链验证通过: %+v", entries)
}

// ---- Spike 2：收权 —— ToolsConfig 按次构造，模型只见剧本允许的工具 ----

func TestSpikeToolScoping(t *testing.T) {
	metricTool := &fakeTool{name: "prometheus_query"}
	k8sTool := &fakeTool{name: "k8s_get_pod"}
	psqlTool := &fakeTool{name: "psql_read"}

	// 模拟剧本 pg-conn-exhaust 的 tools: [prometheus_query, psql_read]：
	// 每次排查按剧本声明构造该次运行的 agent
	fm := &fakeModel{turns: []scriptedTurn{{content: "done"}}}
	if _, err := runAgent(t, fm, []tool.BaseTool{metricTool, psqlTool}, 10, nil, nil); err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	if len(fm.offeredTools) == 0 {
		t.Fatal("模型侧未记录到任何工具授予记录")
	}
	for round, tools := range fm.offeredTools {
		names := map[string]bool{}
		for _, ti := range tools {
			names[ti.Name] = true
		}
		if names["k8s_get_pod"] {
			t.Fatalf("第 %d 轮模型侧仍能看到未授权工具 k8s_get_pod: %v", round+1, names)
		}
		if !names["prometheus_query"] || !names["psql_read"] {
			t.Fatalf("第 %d 轮工具集不完整: %v", round+1, names)
		}
	}
	if got := len(k8sTool.calls()); got != 0 {
		t.Fatalf("未授权工具被执行了 %d 次", got)
	}
	t.Log("收权验证通过：模型侧与执行侧均只见允许的工具")
}

// ---- Spike 3：预算 —— MaxIterations 硬性截断 + 工具后钩子可用 ----

func TestSpikeBudgetControl(t *testing.T) {
	const maxIter = 3
	// 脚本永远要求调工具，逼迫 loop 触顶
	turns := make([]scriptedTurn, 20)
	for i := range turns {
		turns[i] = scriptedTurn{toolCalls: []schema.ToolCall{
			{ID: fmt.Sprintf("c%d", i), Function: schema.FunctionCall{Name: "prometheus_query", Arguments: `{}`}},
		}}
	}
	fm := &fakeModel{turns: turns}
	hookCalls := 0

	_, err := runAgent(t, fm, []tool.BaseTool{&fakeTool{name: "prometheus_query"}}, maxIter, nil,
		func(context.Context) error { hookCalls++; return nil })

	if !errors.Is(err, adk.ErrExceedMaxIterations) {
		t.Fatalf("预期 ErrExceedMaxIterations，实得: %v", err)
	}
	fm.mu.Lock()
	gen := fm.generateCalls
	fm.mu.Unlock()
	if gen > maxIter {
		t.Fatalf("模型生成 %d 轮，超出 MaxIterations=%d", gen, maxIter)
	}
	if hookCalls == 0 {
		t.Fatal("WithAfterToolCallsHook 未被调用")
	}
	t.Logf("预算验证通过: 生成 %d 轮截断(上限 %d), 工具后钩子触发 %d 次", gen, maxIter, hookCalls)
}

// ---- 诊断：不过滤任何组件，记录全部回调触发 ----

func TestSpikeDebugAllCallbacks(t *testing.T) {
	var mu sync.Mutex
	var fired []string
	h := callbacks.NewHandlerBuilder().
		OnStartFn(func(ctx context.Context, info *callbacks.RunInfo, input callbacks.CallbackInput) context.Context {
			mu.Lock()
			fired = append(fired, fmt.Sprintf("OnStart name=%q comp=%q type=%T", infoName(info), infoComp(info), input))
			mu.Unlock()
			return ctx
		}).
		Build()

	fm := &fakeModel{turns: []scriptedTurn{
		{toolCalls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "prometheus_query", Arguments: `{}`}},
		}},
		{content: "done"},
	}}
	_, err := runAgent(t, fm, []tool.BaseTool{&fakeTool{name: "prometheus_query"}}, 5, h, nil)
	if err != nil {
		t.Fatalf("运行失败: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fired) == 0 {
		t.Fatal("没有任何 OnStart 触发")
	}
	for _, f := range fired {
		t.Log(f)
	}
}

func infoName(info *callbacks.RunInfo) string {
	if info == nil {
		return ""
	}
	return info.Name
}

func infoComp(info *callbacks.RunInfo) string {
	if info == nil {
		return ""
	}
	return string(info.Component)
}
