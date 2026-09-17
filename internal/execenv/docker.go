package execenv

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino-ext/components/tool/commandline"
	"github.com/cloudwego/eino-ext/components/tool/commandline/sandbox"

	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

// dockerEnv 一个事件独占一个容器：Acquire 时 Create，Close 时 Cleanup。
// 并发排查各自持有独立 DockerEnv，规避了沙箱组件本身无锁的问题。
type dockerEnv struct {
	sb       *sandbox.DockerSandbox
	match    map[string]bool
	readonly map[string][]string
	readOnly bool // 排查阶段 = true，拒绝 mutating 命令
	maxOut   int
	closed   bool
}

const stdinFile = "/tmp/execenv-stdin"

// runArgv 直接执行（无 stdin）。
func (e *dockerEnv) runArgv(ctx context.Context, argv []string) (Result, error) {
	out, err := e.sb.RunCommand(ctx, argv)
	if err != nil {
		return Result{}, fmt.Errorf("沙箱执行失败: %w", err)
	}
	return e.truncOut(out), nil
}

// runWithStdin 沙箱组件 RunCommand 不支持写 stdin，用 WriteFile + sh 重定向实现：
// 输入先落到容器内文件，再以 `exec "$0" "$@" < file` 形态执行——命令行由框架
// 构造，模型的 argv 只作为独立参数传入，不经 shell 展开，无注入面。
func (e *dockerEnv) runWithStdin(ctx context.Context, argv []string, stdin string) (Result, error) {
	if err := e.sb.WriteFile(ctx, stdinFile, stdin); err != nil {
		return Result{Stderr: "[execenv] stdin 写入沙箱失败: " + err.Error(), ExitCode: -2}, nil
	}
	defer func() { _, _ = e.sb.RunCommand(ctx, []string{"rm", "-f", stdinFile}) }()
	cmd := append([]string{"sh", "-c", `exec "$0" "$@" < ` + stdinFile}, argv...)
	out, err := e.sb.RunCommand(ctx, cmd)
	if err != nil {
		return Result{}, fmt.Errorf("沙箱执行失败: %w", err)
	}
	return e.truncOut(out), nil
}

func (e *dockerEnv) truncOut(out *commandline.CommandOutput) Result {
	res := Result{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode}
	if e.maxOut > 0 {
		res.Stdout = truncateBytes(res.Stdout, e.maxOut)
		res.Stderr = truncateBytes(res.Stderr, e.maxOut)
	}
	return res
}

func (e *dockerEnv) Run(ctx context.Context, argv []string, stdin string) (Result, error) {
	if len(argv) == 0 {
		return Result{Stderr: "[execenv] argv 不能为空", ExitCode: -2}, nil
	}
	if !e.match[argv[0]] {
		return Result{Stderr: "[execenv] 二进制 " + argv[0] + " 不在白名单内", ExitCode: -2}, nil
	}
	if e.readOnly && classify(e.readonly, argv) == coremodel.RiskMutating {
		return Result{Stderr: "[execenv] 命令 " + strings.Join(argv, " ") +
			" 属于变更类（或未声明只读），排查阶段禁止执行；请作为 mutating 动作写入报告 actions，经审批后执行", ExitCode: -2}, nil
	}
	if stdin == "" {
		return e.runArgv(ctx, argv)
	}
	return e.runWithStdin(ctx, argv, stdin)
}

func (e *dockerEnv) Close(ctx context.Context) {
	if !e.closed {
		e.closed = true
		e.sb.Cleanup(ctx)
	}
}

// Acquire 排查阶段：起一个新容器并挂只读门禁（拒绝变更类命令）。
func (f *dockerFactory) Acquire(ctx context.Context) (Env, error) {
	return f.acquire(ctx, true)
}

// AcquireFull 审批后执行：同样一执行一容器，但不设只读门禁。
func (f *dockerFactory) AcquireFull(ctx context.Context) (Env, error) {
	return f.acquire(ctx, false)
}

func (f *dockerFactory) RiskOf(argv []string) coremodel.ToolRisk { return classify(f.readonly, argv) }

func (f *dockerFactory) acquire(ctx context.Context, readOnly bool) (Env, error) {
	sb, err := sandbox.NewDockerSandbox(ctx, &sandbox.Config{
		Image:         f.cfg.Image,
		NetworkEnabled: f.cfg.Network,
		MemoryLimit:   f.cfg.MemoryLimit,
		CPULimit:      f.cfg.CPULimit,
		Timeout:       f.cfg.Timeout,
		Env:           f.cfg.Env,
	})
	if err != nil {
		return nil, fmt.Errorf("execenv: 沙箱构造失败: %w", err)
	}
	if err := sb.Create(ctx); err != nil {
		return nil, fmt.Errorf("execenv: 沙箱容器启动失败: %w", err)
	}
	return &dockerEnv{sb: sb, match: f.matchSet, readonly: f.readonly, readOnly: readOnly, maxOut: f.cfg.MaxOutput}, nil
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[execenv] 输出超限已截断"
}
