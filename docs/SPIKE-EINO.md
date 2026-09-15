# Eino Spike 结论（P0 里程碑 2）

> 日期：2026-09-15 · eino v0.9.19 · 测试：`internal/agent/eino/spike_test.go`
> 方法：脚本化假 ChatModel 驱动真实 ChatModelAgent ReAct loop，确定性验证，零 token 消耗。

## 结论：Eino 可用，无需回退自研 loop

设计文档 §4 的三个硬诉求全部验证通过：

| # | 硬诉求 | 结果 | 机制 |
|---|---|---|---|
| 1 | 证据链（工具调用编号 + IO 记录） | ✅ | `WithCallbacks(handler)` 按次挂载；工具运行的 RunInfo 为 `comp="Tool"`、`name=<工具名>`；`tool.ConvCallbackInput/Output` 取入参与结果；T\<n\> 编号自实现 |
| 2 | 按剧本收权（ToolsConfig 按次构造） | ✅ | 每次排查按 `skill.tools` 构造 `adk.ToolsConfig{ToolsNodeConfig{Tools}}` 新建 agent；模型侧（`model.GetCommonOptions` 证据）只见授权工具；未授权工具从未被执行 |
| 3 | 预算硬顶（MaxIterations） | ✅ | `MaxIterations: N` 恰好在第 N 轮截断（N=3 → 3 次生成），`ErrExceedMaxIterations` 经 `AgentEvent.Err` 浮出；`WithAfterToolCallsHook` 每批工具调用后触发，可作第二道预算闸 |

回调可观测拓扑（诊断测试 `TestSpikeDebugAllCallbacks` 实测）：

```
Agent(spike-agent) → Chain → Graph(ReAct)
  → ChatModel → ToolNode → Tool(prometheus_query)   ← 证据链在此采集
  → AfterToolCalls → ChatModel → ... (loop)
```

即 ChatModel 每轮调用、每次工具执行、每步 hook 都有回调切点——trace 需要的一切都在。

## 踩坑记录（实现 DiagnosisRunner 时必须遵守）

1. **必须用 `adk.AgentWithOptions(ctx, agent)` 包装**才能使 `WithCallbacks` 的
   handler 生效——handler 挂载发生在 flowAgent 层（`flow.go: initAgentCallbacks`），
   裸 `ChatModelAgent.Run` 不挂任何 handler（spike 第一版因此 0 回调）。
2. `callbacks.NewHandlerBuilder().OnStartFn/OnEndFn` 的函数签名返回
   `context.Context`（用于 OnStart→OnEnd 传状态），不是 CallbackOutput。
3. `sonic` 在 go1.27 环境回落 `encoding/json`，仅一条 WARNING，无功能影响
   （sonic 尚未适配 1.27，升级 eino 后可消除）。
4. `AgentEvent.Err` 携带运行期错误（含预算触顶），消费 iterator 时必须检查。

## 后续动作

- 里程碑 4 的 DiagnosisRunner 直接按本结论实现：flowAgent 包装 + 证据链
  handler + 按剧本构造 ToolsConfig + MaxIterations 预算 + hook 二道闸。
- token 级预算（非步数级）在 hook 中按 `ResponseMeta.Usage` 累计实现。
