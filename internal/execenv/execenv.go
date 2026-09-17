// Package execenv 排查内核的执行环境：把"按剧本跑 CLI"落成可调用能力。
// 两个后端对模型暴露同一 exec 工具：
//   - local：宿主机直接 exec（白名单 + 超时 + 输出截断），开发与轻量部署用；
//   - docker：eino-ext 沙箱，一个事件一个容器（Acquire/Close 之间独占）。
package execenv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cnyup/alert-agent/internal/config"
)

// Result 一次命令执行的产物（进证据链 T<n> 的内容）。
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// Env 一次排查独占的执行环境。并发排查各持各的 Env，互不加锁。
type Env interface {
	// Run 执行 argv[0] 白名单内的命令；stdin 非空时喂给进程标准输入
	//（tt-devops-cli 等协议要求复杂输入走 `--input -`）。
	Run(ctx context.Context, argv []string, stdin string) (Result, error)
	Close(ctx context.Context)
}

// Factory 按事件获取 Env：local 返回共享实例（无状态），docker 每次起一个容器。
type Factory interface {
	Acquire(ctx context.Context) (Env, error)
}

// ---- ctx 携带 ----

type ctxKey struct{}

// WithEnv 把本次排查的 Env 放进 ctx（handleEvent 装配，exec 工具取用）。
func WithEnv(ctx context.Context, env Env) context.Context {
	return context.WithValue(ctx, ctxKey{}, env)
}

// FromContext 取出当前排查的 Env。
func FromContext(ctx context.Context) (Env, bool) {
	env, ok := ctx.Value(ctxKey{}).(Env)
	return env, ok
}

// ---- local 后端 ----

type localEnv struct {
	resolve map[string]string // argv[0]（名字或绝对路径）→ 可执行文件
	timeout time.Duration
	maxOut  int
}

type localFactory struct{ env Env }

func (f *localFactory) Acquire(context.Context) (Env, error) { return f.env, nil }

type dockerFactory struct {
	cfg      dockerCfg
	matchSet map[string]bool
}

type dockerCfg struct {
	Image         string
	Network       bool
	MemoryLimit   int64
	CPULimit      float64
	Timeout       time.Duration
	Env           []string
	MaxOutput     int
}

// NewFactory 按配置构造执行环境工厂。enabled=false 返回 nil。
func NewFactory(cfg config.ToolsConfig) (Factory, error) {
	if !cfg.Exec.Enabled {
		return nil, nil
	}
	timeout, err := time.ParseDuration(cfg.Exec.Timeout)
	if err != nil {
		return nil, fmt.Errorf("execenv: timeout 非法: %w", err)
	}
	switch cfg.Exec.Backend {
	case "local":
		resolve := map[string]string{}
		for _, name := range cfg.Exec.Allow {
			path, err := resolveBinary(name)
			if err != nil {
				return nil, fmt.Errorf("execenv: 白名单二进制不可用: %w", err)
			}
			resolve[name] = path
			resolve[filepath.Base(path)] = path
		}
		return &localFactory{env: &localEnv{resolve: resolve, timeout: timeout, maxOut: cfg.Exec.MaxOutputBytes}}, nil
	case "docker":
		dTimeout, err := time.ParseDuration(cfg.Exec.Docker.Timeout)
		if err != nil {
			return nil, fmt.Errorf("execenv: docker timeout 非法: %w", err)
		}
		network := true // CLI 要访问 API，默认开（与 eino 沙箱默认 none 相反）
		if cfg.Exec.Docker.Network != nil {
			network = *cfg.Exec.Docker.Network
		}
		mem := cfg.Exec.Docker.MemoryMB
		if mem <= 0 {
			mem = 512
		}
		cpu := cfg.Exec.Docker.CPU
		if cpu <= 0 {
			cpu = 1.0
		}
		match := map[string]bool{}
		for _, name := range cfg.Exec.Allow {
			match[name] = true
			match[filepath.Base(strings.TrimPrefix(name, "/"))] = true
		}
		return &dockerFactory{
			cfg: dockerCfg{
				Image: cfg.Exec.Docker.Image, Network: network,
				MemoryLimit: mem * 1024 * 1024, CPULimit: cpu,
				Timeout: dTimeout, Env: cfg.Exec.Docker.Env, MaxOutput: cfg.Exec.MaxOutputBytes,
			},
			matchSet: match,
		}, nil
	default:
		return nil, fmt.Errorf("execenv: 未知 backend %q", cfg.Exec.Backend)
	}
}

// resolveBinary 名字走 PATH 查找；绝对路径直接验存在且可执行。
func resolveBinary(name string) (string, error) {
	if filepath.IsAbs(name) {
		if fi, err := os.Stat(name); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return name, nil
		}
		return "", fmt.Errorf("%s 不存在或不可执行", name)
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	return p, nil
}

// softErr 预期内的拒绝/失败以 Result 返回（不打挂引擎），模型可读 stderr 自行调整。
func (e *localEnv) softErr(msg string) (Result, error) {
	return Result{Stderr: "[execenv] " + msg, ExitCode: -2}, nil
}

func (e *localEnv) usableList() string {
	names := make([]string, 0, len(e.resolve))
	for name := range e.resolve {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (e *localEnv) Run(ctx context.Context, argv []string, stdin string) (Result, error) {
	if len(argv) == 0 {
		return e.softErr("argv 不能为空")
	}
	path, ok := e.resolve[argv[0]]
	if !ok {
		return e.softErr(fmt.Sprintf("二进制 %q 不在白名单内，可用：%s（不要尝试 python3/sh 等其他解释器）", argv[0], e.usableList()))
	}
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, path, argv[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf limitedBuffer
	out.limit, errBuf.limit = e.maxOut, e.maxOut
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errBuf.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if cctx.Err() == context.DeadlineExceeded {
		res.Stderr += "\n[execenv] 命令超时被终止"
	}
	if err != nil && res.ExitCode == 0 {
		// 启动失败等无退出码场景，保留错误信息
		res.Stderr += "\n[execenv] " + err.Error()
	}
	return res, nil // 退出码非零不算工具错误，让模型读到输出自行判断
}

func (*localEnv) Close(context.Context) {}

// limitedBuffer 截到上限即丢（防止巨型输出打爆上下文）。
type limitedBuffer struct {
	buf     bytes.Buffer
	limit   int
	trunced bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.limit = 64 * 1024
	}
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			b.buf.Write(p)
			return len(p), nil
		}
		b.buf.Write(p[:room])
		b.trunced = true
		return len(p), nil // 谎报全收，丢弃超出部分
	}
	b.trunced = true
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	s := b.buf.String()
	if b.trunced {
		s += "\n[execenv] 输出超限已截断"
	}
	return s
}
