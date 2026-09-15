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

	"github.com/cnyup/alert-agent/internal/agent"
	"github.com/cnyup/alert-agent/internal/config"
	"github.com/cnyup/alert-agent/internal/llm"
	_ "github.com/cnyup/alert-agent/internal/notifier" // 注册 log 通知器
	"github.com/cnyup/alert-agent/internal/pipeline"
	"github.com/cnyup/alert-agent/internal/skills"
	"github.com/cnyup/alert-agent/internal/store"
	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"

	// 内置源插件集：init() 注册工厂
	_ "github.com/cnyup/alert-agent/internal/webhook"
)

var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "config.yaml", "配置文件路径")
		showVer    = flag.Bool("version", false, "打印版本")
		replayID   = flag.String("replay", "", "回放指定事件的 trace 后退出")
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

	// replay 模式：读库打印事件与全链路 trace，退出
	if *replayID != "" {
		replay(st, *replayID)
		return
	}

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

	// 剧本索引与通用兜底（两阶段加载的第一阶段：仅元数据）
	skillIdx, err := skills.Load(cfg.Skills.Dir)
	if err != nil {
		slog.Error("剧本加载失败", "err", err)
		os.Exit(1)
	}
	generic, err := skills.LoadGeneric(cfg.Skills.Dir)
	if err != nil {
		slog.Warn("通用兜底剧本缺失（_generic/SKILL.md），未命中剧本的告警将跳过排查", "err", err)
	}
	slog.Info("剧本索引就绪", "skills", skillIdx.Names())

	// 排查内核：reasoner 未配置则优雅降级（管道与落库照常，仅跳过排查）
	var diagnoser *agent.Runner
	reasoner, err := llm.New(context.Background(), cfg.LLM.Reasoner)
	switch {
	case errors.Is(err, llm.ErrNotConfigured):
		slog.Warn("LLM reasoner 未配置，排查内核禁用（仅管道+落库模式）")
	case err != nil:
		slog.Error("LLM 构造失败", "err", err)
		os.Exit(1)
	default:
		diagnoser = agent.New(reasoner, nil, 12) // P0: MCP 工具接入前为空集
		slog.Info("排查内核就绪")
	}

	// 通知器装配
	var notifiers []plugin.Notifier
	for _, nc := range cfg.Notifiers {
		n, err := plugin.NewNotifier(nc.Type, nc.Options)
		if err != nil {
			slog.Error("通知器装配失败", "type", nc.Type, "err", err)
			os.Exit(1)
		}
		notifiers = append(notifiers, n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// selectSkill 剧本选择：路由指定优先 → 规则匹配 → 通用兜底
	selectSkill := func(evt *model.AlertEvent, route *pipeline.RouteResult) *skills.Skill {
		if route != nil {
			for _, name := range route.Skills {
				if s, ok := skillIdx.Get(name); ok {
					return s
				}
			}
		}
		if hits := skillIdx.Match(evt); len(hits) > 0 {
			return hits[0]
		}
		return generic
	}

	handleEvent := func(ctx context.Context, evt *model.AlertEvent) error {
		if err := st.SaveEvent(ctx, evt); err != nil {
			slog.Error("事件落库失败", "id", evt.ID, "err", err)
		}
		res := runner.Run(ctx, evt)
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
		slog.Info("事件通过管道", "id", evt.ID, "severity", evt.Severity, "title", evt.Title)

		// 排查内核
		if diagnoser == nil {
			slog.Warn("排查内核未启用，事件仅落库", "id", evt.ID)
			return nil
		}
		skill := selectSkill(evt, res.Route)
		if skill == nil {
			slog.Warn("无可用剧本（含通用兜底），跳过排查", "id", evt.ID)
			return nil
		}
		slog.Info("开始排查", "id", evt.ID, "skill", skill.Name, "matched", skill.Name != "generic")

		report, evidence, err := diagnoser.Diagnose(ctx, evt, skill)
		// 证据链落 trace（T<n> 编号，报告据此引用）
		for i, e := range evidence {
			_ = st.AppendTrace(ctx, evt.ID, store.TraceEntry{
				Seq: 100 + i, ID: e.ID, Kind: "tool", Name: e.Tool,
				Input: e.Args, Output: e.Result, At: time.Now().UTC(),
			})
		}
		if err != nil {
			slog.Error("排查失败", "id", evt.ID, "skill", skill.Name, "err", err)
			return nil
		}
		// 报告本身入 trace
		if b, err := json.Marshal(report); err == nil {
			_ = st.AppendTrace(ctx, evt.ID, store.TraceEntry{
				Seq: 500, ID: "R1", Kind: "model", Name: "diagnosis_report",
				Output: string(b), At: time.Now().UTC(),
			})
		}
		slog.Info("排查完成", "id", evt.ID, "skill", skill.Name,
			"summary", report.Summary, "needs_human", report.NeedsHuman,
			"steps", report.Cost.Steps, "root_causes", len(report.RootCauses))

		// 通知：路由指定的目标优先，未指定则全部已装配通知器
		targets := notifiers
		if res.Route != nil && len(res.Route.Notifiers) > 0 {
			targets = filterNotifiers(notifiers, res.Route.Notifiers)
		}
		for _, n := range targets {
			if err := n.Notify(ctx, report); err != nil {
				slog.Error("通知失败", "notifier", n.Name(), "err", err)
			}
		}
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

func filterNotifiers(all []plugin.Notifier, want []string) []plugin.Notifier {
	set := map[string]bool{}
	for _, w := range want {
		set[w] = true
	}
	var out []plugin.Notifier
	for _, n := range all {
		if set[n.Name()] {
			out = append(out, n)
		}
	}
	return out
}

// replay 回放一个事件的全链路 trace。
func replay(st *store.Store, eventID string) {
	evt, err := st.GetEvent(context.Background(), eventID)
	if err != nil {
		slog.Error("事件不存在", "id", eventID, "err", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(evt, "", "  ")
	fmt.Printf("=== 事件 ===\n%s\n", b)
	trace, err := st.Trace(context.Background(), eventID)
	if err != nil {
		slog.Error("trace 读取失败", "err", err)
		os.Exit(1)
	}
	fmt.Printf("=== Trace（%d 步）===\n", len(trace))
	for _, e := range trace {
		line := fmt.Sprintf("%s [%s/%s]", e.ID, e.Kind, e.Name)
		if e.Input != "" {
			line += " in=" + truncateStr(e.Input, 120)
		}
		if e.Output != "" {
			line += " out=" + truncateStr(e.Output, 120)
		}
		if e.Err != "" {
			line += " err=" + e.Err
		}
		fmt.Println(line)
	}
}

func truncateStr(s string, n int) string {
	b := []rune(s)
	if len(b) <= n {
		return s
	}
	return string(b[:n]) + "…"
}
