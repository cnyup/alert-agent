package execenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnyup/alert-agent/internal/config"
)

func testExecCfg(allow []string, timeout string) config.ToolsConfig {
	var c config.ToolsConfig
	c.Exec.Enabled = true
	c.Exec.Backend = "local"
	c.Exec.Allow = allow
	c.Exec.Timeout = timeout
	c.Exec.MaxOutputBytes = 4096
	return c
}

func TestLocalWhitelist(t *testing.T) {
	f, err := NewFactory(testExecCfg([]string{"echo"}, "5s"))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := f.AcquireFull(context.Background())
	res, err := env.Run(context.Background(), []string{"echo", "hello"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hello\n" || res.ExitCode != 0 {
		t.Fatalf("echo 结果异常: %+v", res)
	}
	// 白名单外二进制：软失败（不打挂引擎），模型可读 stderr 自行调整
	res, err = env.Run(context.Background(), []string{"python3", "-c", "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != -2 || !strings.Contains(res.Stderr, "不在白名单内") {
		t.Fatalf("白名单外应软失败: %+v", res)
	}
	res, err = env.Run(context.Background(), nil, "")
	if err != nil || res.ExitCode != -2 {
		t.Fatalf("空 argv 应软失败: err=%v res=%+v", err, res)
	}
}

func TestLocalStdin(t *testing.T) {
	f, err := NewFactory(testExecCfg([]string{"cat"}, "5s"))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := f.AcquireFull(context.Background())
	res, err := env.Run(context.Background(), []string{"cat"}, `{"q":"omni"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != `{"q":"omni"}` {
		t.Fatalf("stdin 未透传: %+v", res)
	}
}

func TestLocalTimeout(t *testing.T) {
	f, err := NewFactory(testExecCfg([]string{"sleep"}, "100ms"))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := f.AcquireFull(context.Background())
	res, err := env.Run(context.Background(), []string{"sleep", "5"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "超时") {
		t.Fatalf("超时未生效: %+v", res)
	}
}

func TestLocalOutputTruncation(t *testing.T) {
	cfg := testExecCfg([]string{"echo"}, "5s")
	cfg.Exec.MaxOutputBytes = 10
	f, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := f.AcquireFull(context.Background())
	res, err := env.Run(context.Background(), []string{"echo", "aaaaaaaaaaaaaaaaaaaaaaaa"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "截断") {
		t.Fatalf("截断未生效: %q", res.Stdout)
	}
}

func TestLocalUnknownBinaryFailsFast(t *testing.T) {
	if _, err := NewFactory(testExecCfg([]string{"no-such-bin-xyz"}, "5s")); err == nil {
		t.Fatal("白名单含不可用二进制应在装配期报错")
	}
}

func TestReadToolScope(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := NewReadTool(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if info, err := rt.Info(ctx); err != nil || info.Name != "read" {
		t.Fatalf("read 工具构造异常: %v %v", info, err)
	}
	out, err := rt.InvokableRun(ctx, `{"path":"a.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("读到的内容异常: %s", out)
	}
	for _, evil := range []string{`{"path":"../secret"}`, `{"path":"/etc/passwd"}`, `{"path":"a/../../escape"}`} {
		out, err := rt.InvokableRun(ctx, evil)
		if err != nil {
			t.Fatalf("越界路径应软失败（不出 Go error）: %s err=%v", evil, err)
		}
		if !strings.Contains(out, "错误") {
			t.Fatalf("越界路径应返回错误内容: %s → %s", evil, out)
		}
	}
}

func TestExecToolRequiresEnvInCtx(t *testing.T) {
	et, err := NewExecTool()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := et.InvokableRun(context.Background(), `{"argv":["echo","x"]}`); err == nil {
		t.Fatal("ctx 无 Env 时应报错")
	}
}

func TestCtxPlumbing(t *testing.T) {
	f, err := NewFactory(testExecCfg([]string{"echo"}, "5s"))
	if err != nil {
		t.Fatal(err)
	}
	env, _ := f.Acquire(context.Background())
	if got, ok := FromContext(WithEnv(context.Background(), env)); !ok || got == nil {
		t.Fatal("ctx 未携带 Env")
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("空 ctx 不应取到 Env")
	}
}
