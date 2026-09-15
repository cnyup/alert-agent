// Package mcp MCP 工具装配：按配置拉起 stdio MCP server 子进程，
// 经 eino-ext 适配器把其工具桥接为 Eino Tool（DESIGN.md §0 MCP 接入）。
package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpext "github.com/cloudwego/eino-ext/components/tool/mcp"
	"github.com/cloudwego/eino/components/tool"
	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/cnyup/alert-agent/internal/config"
)

// BuildTools 逐个拉起配置的 MCP server（stdio），返回桥接后的全部工具。
// 单个 server 失败直接报错：工具缺失会让排查结论失真，宁可启动期失败。
func BuildTools(ctx context.Context, servers map[string]config.MCPServerConfig) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	for name, sc := range servers {
		if sc.Command == "" {
			return nil, fmt.Errorf("mcp: server %q 缺少 command", name)
		}
		env := make([]string, 0, len(sc.Env))
		for k, v := range sc.Env {
			env = append(env, k+"="+v)
		}
		cli, err := mcpclient.NewStdioMCPClient(sc.Command, env, sc.Args...)
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q 客户端构造失败: %w", name, err)
		}
		if err := cli.Start(ctx); err != nil {
			return nil, fmt.Errorf("mcp: server %q 启动失败: %w", name, err)
		}
		initReq := mcp.InitializeRequest{}
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "alert-agent", Version: "0.1.0"}
		if _, err := cli.Initialize(ctx, initReq); err != nil {
			return nil, fmt.Errorf("mcp: server %q 握手失败: %w", name, err)
		}
		ts, err := mcpext.GetTools(ctx, &mcpext.Config{Cli: cli})
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q 获取工具失败: %w", name, err)
		}
		tools = append(tools, ts...)
	}
	return tools, nil
}

// ToolNames 工具名单（诊断日志用）。
func ToolNames(tools []tool.BaseTool) string {
	var sb strings.Builder
	for i, t := range tools {
		if i > 0 {
			sb.WriteString(",")
		}
		if info, err := t.Info(context.Background()); err == nil && info != nil {
			sb.WriteString(info.Name)
		}
	}
	return sb.String()
}
