package execenv

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cnyup/alert-agent/internal/config"
)

// TestDockerBackendStdin 真实 Docker 沙箱验证（默认跳过）：
//
//	TT_DOCKER_TEST=1 go test ./internal/execenv/ -run TestDockerBackend
//
// 需要：本机 dockerd、镜像 tt-devops-cli-sandbox、TT_YW_AUTHORIZATION 环境变量。
func TestDockerBackend(t *testing.T) {
	if os.Getenv("TT_DOCKER_TEST") != "1" {
		t.Skip("设 TT_DOCKER_TEST=1 启用（需 Docker 与沙箱镜像）")
	}
	var c config.ToolsConfig
	c.Exec.Enabled = true
	c.Exec.Backend = "docker"
	c.Exec.Allow = []string{"echo", "cat", "tt-devops-cli"}
	c.Exec.Timeout = "60s"
	c.Exec.MaxOutputBytes = 65536
	c.Exec.Docker.Image = "tt-devops-cli-sandbox:1.0.15"
	c.Exec.Docker.Timeout = "60s"
	c.Exec.Docker.Env = []string{"TT_YW_AUTHORIZATION=" + os.Getenv("TT_YW_AUTHORIZATION")}

	f, err := NewFactory(c)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env, err := f.Acquire(ctx)
	if err != nil {
		t.Fatalf("容器启动失败（Docker 未运行或镜像缺失？）: %v", err)
	}
	defer env.Close(ctx)

	// 基础：argv 直执行
	res, err := env.Run(ctx, []string{"echo", "hi"}, "")
	if err != nil || res.Stdout != "hi\n" {
		t.Fatalf("echo: err=%v res=%+v", err, res)
	}
	// 白名单软失败
	res, err = env.Run(ctx, []string{"python3", "-c", "x"}, "")
	if err != nil || res.ExitCode != -2 || !strings.Contains(res.Stderr, "不在白名单内") {
		t.Fatalf("白名单: err=%v res=%+v", err, res)
	}
	// stdin 链路：cat 回显（不依赖 CLI/token）
	res, err = env.Run(ctx, []string{"cat"}, `{"probe":"stdin"}`)
	if err != nil || res.Stdout != `{"probe":"stdin"}` {
		t.Fatalf("stdin: err=%v res=%+v", err, res)
	}
	// CLI 真实调用（需 token）
	if os.Getenv("TT_YW_AUTHORIZATION") != "" {
		res, err = env.Run(ctx,
			[]string{"tt-devops-cli", "databases", "+doctor"}, "")
		if err != nil || strings.Contains(res.Stdout, `"success": false`) {
			t.Fatalf("doctor: err=%v res=%.200s", err, res.Stdout)
		}
		res, err = env.Run(ctx,
			[]string{"tt-devops-cli", "databases", "resources", "+search", "--input", "-"},
			`{"resource_type":"instance","query":"allvoice","capability":"readonly_query","page":{"limit":3}}`)
		if err != nil || strings.Contains(res.Stdout, `"success": false`) {
			t.Fatalf("resources+stdin: err=%v stdout=%.300s stderr=%.200s", err, res.Stdout, res.Stderr)
		}
	} else {
		t.Log("TT_YW_AUTHORIZATION 未设置，跳过 CLI 真实调用断言")
	}
}
