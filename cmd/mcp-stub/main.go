// mcp-stub 验证用 MCP server（stdio 传输）：固化数据的 prometheus_query / k8s_get_pod。
package main

import (
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/server"

	"github.com/cnyup/alert-agent/internal/mcpstub"
)

func main() {
	if err := server.ServeStdio(mcpstub.NewServer()); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-stub:", err)
		os.Exit(1)
	}
}
