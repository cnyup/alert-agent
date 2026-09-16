package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/mark3labs/mcp-go/server"

	"github.com/cnyup/alert-agent/internal/config"
	"github.com/cnyup/alert-agent/internal/mcpstub"
)

// re-exec 模式：测试进程以 AA_MCP_STUB=1 再执行自己时，变身 stdio MCP server。
func TestMain(m *testing.M) {
	if os.Getenv("AA_MCP_STUB") == "1" {
		if err := server.ServeStdio(mcpstub.NewServer()); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// 确定性 MCP 链路验证：拉起 stub server 子进程 → eino-ext 桥接 →
// 工具列举 + 真实调用返回固化数据。不依赖 LLM。
func TestBuildToolsAndCall(t *testing.T) {
	if _, err := os.Stat(os.Args[0]); err != nil {
		t.Skip("无测试二进制可用")
	}
	ctx := context.Background()
	tools, err := BuildTools(ctx, map[string]config.MCPServerConfig{
		"stub": {Command: os.Args[0], Env: map[string]string{"AA_MCP_STUB": "1"}},
	})
	if err != nil {
		t.Fatalf("MCP 装配失败: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("应桥接 2 个工具，实得 %d（%s）", len(tools), ToolNames(tools))
	}
	for _, tl := range tools {
		info, err := tl.Info(ctx)
		if err != nil {
			t.Fatalf("工具信息获取失败: %v", err)
		}
		if info.Name != "prometheus_query" {
			continue
		}
		inv, ok := tl.(tool.InvokableTool)
		if !ok {
			t.Fatalf("prometheus_query 未实现 InvokableRun（实际类型 %T）", tl)
		}
		out, err := inv.InvokableRun(ctx, `{"query":"rate(http_requests_total{code=~\"5..\"}[5m])"}`)
		if err != nil {
			t.Fatalf("MCP 工具调用失败: %v", err)
		}
		if !strings.Contains(out, "0.068") || !strings.Contains(out, "v1.2.3") {
			t.Fatalf("stub 数据不符: %s", out)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("返回应为合法 JSON: %v", err)
		}
	}
	t.Log("MCP 链路验证通过:", ToolNames(tools))
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "workflow-logs.yaml"),
		[]byte("command: /bin/echo\nargs: [\"--mcp\"]\nenv:\n  K: v\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("非 yaml 忽略"), 0o644)
	t.Setenv("MCP_TEST_TOKEN", "tok123")
	os.WriteFile(filepath.Join(dir, "secure.yaml"),
		[]byte("command: /bin/srv\nenv:\n  TOKEN: ${MCP_TEST_TOKEN}\n"), 0o644)

	got, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应发现 2 个 server，实得 %d: %v", len(got), got)
	}
	if got["workflow-logs"].Command != "/bin/echo" || len(got["workflow-logs"].Args) != 1 {
		t.Fatalf("workflow-logs 解析不符: %+v", got["workflow-logs"])
	}
	if got["secure"].Env["TOKEN"] != "tok123" {
		t.Fatalf("env 展开失败: %+v", got["secure"])
	}

	// 目录不存在 = 合法空集
	empty, err := LoadDir(filepath.Join(dir, "nope"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("不存在目录应返回空集: %v %v", err, empty)
	}
}
