// Package mcpstub 验证用 MCP server：提供排查剧本会用的两个工具，
// 返回固化数据。cmd/mcp-stub 经 stdio 对外提供；测试经 re-exec 复用同一实现。
package mcpstub

import (
	"context"
	"encoding/json"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// NewServer 构造 stub MCP server（prometheus_query + k8s_get_pod）。
func NewServer() *server.MCPServer {
	s := server.NewMCPServer("alert-agent-stub", "0.1.0")

	q := mcp.NewTool("prometheus_query",
		mcp.WithDescription("执行 PromQL 查询，返回时间序列样本（stub：返回固化数据）"),
		mcp.WithString("query", mcp.Required(), mcp.Description("PromQL 表达式")))
	s.AddTool(q, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Query string `json:"query"`
		}
		b, _ := json.Marshal(req.GetArguments())
		_ = json.Unmarshal(b, &args)
		// 固化数据：5xx 错误率在 06:10（一次发布后）从 0.2% 跳到 6.8%
		payload := map[string]any{
			"query": args.Query,
			"series": []map[string]any{
				{"ts": "2026-09-15T06:00Z", "value": 0.002},
				{"ts": "2026-09-15T06:05Z", "value": 0.002},
				{"ts": "2026-09-15T06:10Z", "value": 0.068, "note": "order-api v1.2.3 发布完成后 1 分钟"},
				{"ts": "2026-09-15T06:15Z", "value": 0.071},
			},
			"deploy_event": "order-api v1.2.3 于 06:09 发布（滚动更新完成）",
		}
		out, _ := json.Marshal(payload)
		return mcp.NewToolResultText(string(out)), nil
	})

	p := mcp.NewTool("k8s_get_pod",
		mcp.WithDescription("查询 k8s pod 状态（stub：返回固化数据）"),
		mcp.WithString("namespace", mcp.Description("命名空间")))
	s.AddTool(p, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload := map[string]any{
			"pods": []map[string]any{
				{"name": "order-api-7d9f6b-x2lq", "age": "6m", "restarts": 0, "ready": true, "note": "新版本 pod（v1.2.3）"},
				{"name": "order-api-5c8d4a-mn8p", "age": "34d", "restarts": 0, "ready": true, "note": "旧版本 pod 残留"},
			},
		}
		out, _ := json.Marshal(payload)
		return mcp.NewToolResultText(string(out)), nil
	})
	return s
}
