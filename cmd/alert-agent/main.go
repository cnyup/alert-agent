// alert-agent 入口：配置装载 → 插件注册（各插件包 init）→ 生命周期管理。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cnyup/alert-agent/internal/config"
)

var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "config.yaml", "配置文件路径")
		showVer    = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("alert-agent", version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("配置装载失败", "err", err)
		os.Exit(1)
	}
	slog.Info("配置装载完成",
		"addr", cfg.Server.Addr,
		"policy", cfg.Policy.Execution,
		"skills_dir", cfg.Skills.Dir,
		"store", cfg.Store.Path,
	)

	// 里程碑3 起：此处按 cfg 装配 sources / pipeline / notifiers 并启动。
	// P0 骨架：仅暴露健康检查端点，验证生命周期与构建链路。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: cfg.Server.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP 服务启动", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		slog.Error("服务异常退出", "err", err)
		os.Exit(1)
	case <-ctx.Done():
	}
	slog.Info("收到退出信号，优雅关闭中")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("优雅关闭失败", "err", err)
		os.Exit(1)
	}
	slog.Info("已退出")
}
