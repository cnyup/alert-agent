// Package mcp MCP 工具装配：按配置拉起 stdio MCP server 子进程，
// 经 eino-ext 适配器把其工具桥接为 Eino Tool（DESIGN.md §0 MCP 接入）。
package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

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

// LoadDir 目录式 MCP 配置发现：mcp/<name>.yaml 一个文件一个 server，
// 文件名即 server 名（与 skills/<name>/SKILL.md 同构的用户体验）。
// env 引用（${VAR}）由进程环境展开，凭证不落盘。
func LoadDir(dir string) (map[string]config.MCPServerConfig, error) {
	out := map[string]config.MCPServerConfig{}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil // 目录不存在 = 无目录式配置，合法
		}
		return nil, fmt.Errorf("mcp: 读取目录 %s 失败: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ext)
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("mcp: 读取 %s 失败: %w", e.Name(), err)
		}
		expanded := envRef.ReplaceAllStringFunc(string(raw), func(m string) string {
			return os.Getenv(envRef.FindStringSubmatch(m)[1])
		})
		var sc config.MCPServerConfig
		if err := yaml.Unmarshal([]byte(expanded), &sc); err != nil {
			return nil, fmt.Errorf("mcp: %s 解析失败: %w", e.Name(), err)
		}
		if sc.Command == "" {
			return nil, fmt.Errorf("mcp: %s 缺少 command", e.Name())
		}
		out[name] = sc
	}
	return out, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
