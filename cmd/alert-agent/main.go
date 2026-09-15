// alert-agent 入口：配置装载 → 插件注册（各插件包 init）→ 装配与生命周期管理。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cnyup/alert-agent/internal/config"
	"github.com/cnyup/alert-agent/internal/pipeline"
	"github.com/cnyup/alert-agent/internal/store"
	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"

	// 内置插件集：init() 注册工厂（编译期注册，DESIGN.md 三层扩展模型第二层）
	_ "github.com/cnyup/alert-agent/internal/webhook"
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

	// 存储
	if dir := filepath.Dir(cfg.Store.Path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Error("创建数据目录失败", "dir", dir, "err", err)
			os.Exit(1)
		}
	}
	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		slog.Error("存储打开失败", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// 管道装配
	specs := make([]pipeline.StageSpec, 0, len(cfg.Pipeline))
	for _, sc := range cfg.Pipeline {
		specs = append(specs, pipeline.StageSpec{Name: sc.Stage, Options: sc.Options})
	}
	runner, err := pipeline.New(specs)
	if err != nil {
		slog.Error("管道装配失败", "err", err)
		os.Exit(1)
	}
	slog.Info("管道装配完成", "stages", runner.Names())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 事件出口：落库 → 管道 → trace。P0 的管道终点是路由完成；
	// 里程碑4 在此接入排查内核（按 res.Route.Skills 选剧本）。
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	handleEvent := func(ctx context.Context, evt *model.AlertEvent) error {
		if err := st.SaveEvent(ctx, evt); err != nil {
			slog.Error("事件落库失败", "id", evt.ID, "err", err)
		}
		res := runner.Run(ctx, evt)
		// 管道各阶段写入 trace（S<n> 编号）
		for i, sl := range res.StageLog {
			b, _ := json.Marshal(sl)
			_ = st.AppendTrace(ctx, evt.ID, store.TraceEntry{
				Seq: i, ID: fmt.Sprintf("S%d", i+1), Kind: "stage",
				Name: sl.Stage, Output: string(b), At: sl.At,
			})
		}
		if res.Dropped {
			slog.Info("事件被管道终止", "id", evt.ID, "stage", res.DropBy, "title", evt.Title)
			return nil
		}
		slog.Info("事件通过管道", "id", evt.ID, "severity", evt.Severity,
			"title", evt.Title, "route", jsonOrNull(res.Route))
		return nil
	}

	// 源装配与启动
	for _, sc := range cfg.Sources {
		src, err := plugin.NewSource(sc.Type, sc.Options)
		if err != nil {
			slog.Error("源装配失败", "type", sc.Type, "err", err)
			os.Exit(1)
		}
		if m, ok := src.(interface{ SetMux(*http.ServeMux) }); ok {
			m.SetMux(mux)
		}
		go func(name string) {
			if err := src.Start(ctx, func(ctx context.Context, evt *model.AlertEvent) error {
				return handleEvent(ctx, evt)
			}); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("源退出", "type", name, "err", err)
			}
		}(sc.Type)
	}

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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

func jsonOrNull(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
