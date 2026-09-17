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
	"strings"
	"syscall"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/cnyup/alert-agent/internal/agent"
	"github.com/cnyup/alert-agent/internal/config"
	"github.com/cnyup/alert-agent/internal/execenv"
	"github.com/cnyup/alert-agent/internal/fanout"
	"github.com/cnyup/alert-agent/internal/feishu"
	"github.com/cnyup/alert-agent/internal/llm"
	mcpagent "github.com/cnyup/alert-agent/internal/mcp"
	_ "github.com/cnyup/alert-agent/internal/notifier" // 注册内置通知器（log + feishu-card）
	"github.com/cnyup/alert-agent/internal/pipeline"
	"github.com/cnyup/alert-agent/internal/policy"
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
		distill    = flag.Bool("distill", false, "把人工反馈蒸馏为剧本修订建议后退出")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("alert-agent", version)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	loadDotEnv(filepath.Join(filepath.Dir(*configPath), ".env"))

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

	// distill 模式：反馈 → LLM → 剧本修订建议（不自动生效，用户确认后应用）
	if *distill {
		if err := runDistill(st, cfg); err != nil {
			slog.Error("蒸馏失败", "err", err)
			os.Exit(1)
		}
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
	var execFactory execenv.Factory
	reasoner, err := llm.New(context.Background(), cfg.LLM.Reasoner)
	switch {
	case errors.Is(err, llm.ErrNotConfigured):
		slog.Warn("LLM reasoner 未配置，排查内核禁用（仅管道+落库模式）")
	case err != nil:
		slog.Error("LLM 构造失败", "err", err)
		os.Exit(1)
	default:
		var agentTools []tool.BaseTool
		// MCP 配置合并：目录式（mcp/<name>.yaml）打底，config.yaml 内联覆盖
		mcpServers, err := mcpagent.LoadDir(cfg.MCP.Dir)
		if err != nil {
			slog.Error("MCP 目录配置装载失败", "dir", cfg.MCP.Dir, "err", err)
			os.Exit(1)
		}
		for n, sc := range cfg.MCP.Servers {
			mcpServers[n] = sc
		}
		if len(mcpServers) > 0 {
			agentTools, err = mcpagent.BuildTools(ctx0(), mcpServers)
			if err != nil {
				// 单个 MCP 配错不应打挂服务：降级为无工具排查（管道/通知照常）
				slog.Error("MCP 工具装配失败，内核降级为无工具排查", "err", err)
				agentTools = nil
			} else {
				slog.Info("MCP 工具就绪", "tools", mcpagent.ToolNames(agentTools))
			}
		}
		// 内置 exec/read 工具：技能文档即调用规范，CLI 经白名单执行（与后端无关）
		execFactory, err = execenv.NewFactory(cfg.Tools)
		if err != nil {
			slog.Error("exec 工具装配失败", "err", err)
			os.Exit(1)
		}
		if execFactory != nil {
			t, err := execenv.NewExecTool()
			if err != nil {
				slog.Error("exec 工具构造失败", "err", err)
				os.Exit(1)
			}
			agentTools = append(agentTools, t)
			slog.Info("exec 工具就绪", "backend", cfg.Tools.Exec.Backend, "allow", cfg.Tools.Exec.Allow)
		}
		if cfg.Tools.Read.Enabled {
			t, err := execenv.NewReadTool(cfg.Skills.Dir)
			if err != nil {
				slog.Error("read 工具构造失败", "err", err)
				os.Exit(1)
			}
			agentTools = append(agentTools, t)
			slog.Info("read 工具就绪", "dir", cfg.Skills.Dir)
		}
		diagnoser = agent.New(reasoner, agentTools, cfg.Agent.MaxIterations)
		slog.Info("排查内核就绪", "max_iterations", cfg.Agent.MaxIterations)
	}

	// 通知器装配
	var notifiers []plugin.Notifier
	for _, nc := range cfg.Notifiers {
		n, err := plugin.NewNotifier(nc.Type, nc.Options)
		if err != nil {
			// 通知器是非关键路径：凭证缺失等装配失败只告警跳过，不阻断主流程
			slog.Warn("通知器装配失败，已跳过", "type", nc.Type, "err", err)
			continue
		}
		// 卡片发送回调：记录 message_id → event_id 映射（回复卡片驱动闭环指令）
		if setter, ok := n.(interface{ SetOnSent(func(string, string)) }); ok {
			setter.SetOnSent(func(eventID, messageID string) {
				if err := st.SaveCardRef(context.Background(), eventID, messageID); err != nil {
					slog.Error("卡片映射落库失败", "err", err)
				}
			})
		}
		notifiers = append(notifiers, n)
	}

	// P1 审批状态机：执行器在已装配的 MCP 工具集中按名查找
	var execTools []tool.BaseTool
	if diagnoser != nil {
		execTools = diagnoser.Tools()
	}
	policyMgr := policy.New(st, func(ctx context.Context, toolName, argsJSON string) (string, error) {
		for _, t := range execTools {
			info, err := t.Info(ctx)
			if err != nil || info == nil || info.Name != toolName {
				continue
			}
			inv, ok := t.(tool.InvokableTool)
			if !ok {
				return "", fmt.Errorf("工具 %s 不可调用", toolName)
			}
			return inv.InvokableRun(ctx, argsJSON)
		}
		return "", fmt.Errorf("工具 %s 未装配（检查 mcp.servers 或工具名）", toolName)
	})
	decideHandler := func(ctx context.Context, value map[string]any, operator string) (string, error) {
		kind, _ := value["type"].(string)
		eventID, _ := value["event_id"].(string)
		actionID, _ := value["action_id"].(string)
		switch kind {
		case "approve":
			return policyMgr.Decide(ctx, eventID, actionID, true, operator)
		case "reject":
			return policyMgr.Decide(ctx, eventID, actionID, false, operator)
		case "claim", "false-positive", "root-confirmed":
			return policyMgr.Feedback(ctx, eventID, kind, operator)
		default:
			return "", fmt.Errorf("未知按钮类型 %q", kind)
		}
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
		// 每事件独占执行环境（docker 后端 = 一事件一容器；local 后端无状态共享）
		if execFactory != nil {
			env, err := execFactory.Acquire(ctx)
			if err != nil {
				slog.Error("执行环境获取失败，跳过排查", "id", evt.ID, "err", err)
				return nil
			}
			defer env.Close(ctx)
			ctx = execenv.WithEnv(ctx, env)
		}
		skill := selectSkill(evt, res.Route)
		if skill == nil {
			slog.Warn("无可用剧本（含通用兜底），跳过排查", "id", evt.ID)
			return nil
		}
		extra := ""
		if res.Aggregate != nil {
			extra = fmt.Sprintf("incident %s：本窗口第 %d 条同指纹告警", res.Aggregate.IncidentID, res.Aggregate.Occurrences)
			if res.Aggregate.Escalated {
				extra += "（已达到风暴升级阈值，等级提为 critical）"
			}
			slog.Info("开始排查", "id", evt.ID, "skill", skill.Name,
				"incident", res.Aggregate.IncidentID, "occurrences", res.Aggregate.Occurrences,
				"escalated", res.Aggregate.Escalated)
		} else {
			slog.Info("开始排查", "id", evt.ID, "skill", skill.Name, "matched", skill.Name != "generic")
		}

		report, evidence, err := diagnoser.Diagnose(ctx, evt, skill, extra)
		// 证据链落 trace（T<n> 编号，报告据此引用）
		for i, e := range evidence {
			_ = st.AppendTrace(ctx, evt.ID, store.TraceEntry{
				Seq: 100 + i, ID: e.ID, Kind: "tool", Name: e.Tool,
				Input: e.Args, Output: e.Result, At: time.Now().UTC(),
			})
		}
		if err != nil {
			slog.Error("排查失败", "id", evt.ID, "skill", skill.Name, "err", err)
			// 失败也不能静默：降级为失败报告继续走通知，让用户知道排查中断及原因
			if report == nil {
				report = &model.DiagnosisReport{}
			}
			report.AlertID = evt.ID
			report.Refs = evt.Refs
			report.SkillID = skill.Name
			report.SkillMatched = skill.Name != "generic"
			if report.Severity == "" {
				report.Severity = evt.Severity
			}
			report.Summary = "排查执行失败：" + truncateStr(err.Error(), 300)
			report.NeedsHuman = true
			report.Unresolved = append(report.Unresolved, "排查中断，需人工介入或重试")
			report.Actions = nil
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
		// P1：mutating 动作进入审批状态机（卡片按钮批准后才执行）
		if err := policyMgr.CreateApprovals(ctx, report); err != nil {
			slog.Error("审批创建失败", "id", evt.ID, "err", err)
		}

		// 通知扇出：路由指定的目标优先，未指定则全部已装配通知器。
		// xdag 任务图：并行执行 + 指数退避重试 + 取消收敛（P2 技术验证）
		targets := notifiers
		if res.Route != nil && len(res.Route.Notifiers) > 0 {
			targets = filterNotifiers(notifiers, res.Route.Notifiers)
		}
		notifyCtx, notifyCancel := context.WithTimeout(ctx, 60*time.Second)
		failed, err := fanout.Notify(notifyCtx, targets, report)
		notifyCancel()
		if err != nil {
			slog.Error("通知扇出执行失败", "err", err)
		} else if len(failed) > 0 {
			slog.Error("通知重试后仍失败", "channels", failed)
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
		if f, ok := src.(interface {
			SetDecisionHandler(feishu.DecisionHandler)
			SetCardResolver(feishu.CardResolver)
		}); ok {
			f.SetDecisionHandler(decideHandler)
			f.SetCardResolver(st.EventIDByCard)
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

// ctx0 启动期用的基础 ctx。
func ctx0() context.Context { return context.Background() }

// loadDotEnv 极简 .env 装载（已存在的环境变量优先，不覆盖）。
func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if os.Getenv(k) == "" {
				_ = os.Setenv(k, v)
			}
		}
	}
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

// runDistill 剧本蒸馏：汇总人工反馈 + 对应排查报告 → LLM 生成修订建议。
func runDistill(st *store.Store, cfg *config.Config) error {
	ctx := context.Background()
	fbs, err := st.ListFeedback(ctx)
	if err != nil {
		return err
	}
	if len(fbs) == 0 {
		fmt.Println("暂无人工反馈（先在飞书回复卡片：认领 / 误报 / 根因确认），无蒸馏输入。")
		return nil
	}
	type record struct {
		Feedback  store.Feedback `json:"feedback"`
		Title     string         `json:"title"`
		Labels    map[string]string `json:"labels"`
		SkillID   string         `json:"skill_id,omitempty"`
		Summary   string         `json:"report_summary,omitempty"`
		RootCause string         `json:"root_cause,omitempty"`
	}
	var records []record
	for _, fb := range fbs {
		rec := record{Feedback: fb}
		if evt, err := st.GetEvent(ctx, fb.EventID); err == nil {
			rec.Title, rec.Labels = evt.Title, evt.Labels
			if trace, err := st.Trace(ctx, fb.EventID); err == nil {
				for _, e := range trace {
					if e.ID != "R1" {
						continue
					}
					var rep struct {
						SkillID    string `json:"skill_id"`
						Summary    string `json:"summary"`
						RootCauses []struct {
							Hypothesis string `json:"hypothesis"`
						} `json:"root_causes"`
					}
					if json.Unmarshal([]byte(e.Output), &rep) == nil {
						rec.SkillID, rec.Summary = rep.SkillID, rep.Summary
						if len(rep.RootCauses) > 0 {
							rec.RootCause = rep.RootCauses[0].Hypothesis
						}
					}
				}
			}
		}
		records = append(records, rec)
	}
	data, _ := json.MarshalIndent(records, "", "  ")

	prompt := "你是告警排查系统的剧本（SKILL.md）维护助手。以下是运维人员对排查报告的人工反馈与对应告警/报告数据。\n\n" +
		"请分析这些反馈，输出 Markdown 格式的剧本修订建议，规则：\n" +
		"1. 误报反馈（false-positive）→ 分析共性（labels/标题模式），建议对应剧本的 triggers 如何收紧或排除；\n" +
		"2. 根因确认（root-confirmed）→ 建议把验证过的判断标准沉淀进剧本的『判断标准』小节；\n" +
		"3. 认领（claim）→ 仅统计，不必给建议；\n" +
		"4. 每条建议必须注明依据的反馈条目；只基于数据，不臆造；无足够依据的模式宁可不建议。\n\n" +
		"反馈数据：\n```json\n" + string(data) + "\n```"

	reasoner, err := llm.New(ctx, cfg.LLM.Reasoner)
	if err != nil {
		return fmt.Errorf("蒸馏需要 reasoner（%w）", err)
	}
	resp, err := reasoner.Generate(ctx, []*schema.Message{
		{Role: schema.User, Content: prompt},
	})
	if err != nil {
		return fmt.Errorf("LLM 生成失败: %w", err)
	}

	out := "# 剧本修订建议（蒸馏于 " + time.Now().Format("2006-01-02 15:04") + "）\n\n" +
		"> 由人工反馈自动生成，**未经确认不会生效**：请审阅后手工合并进对应 SKILL.md。\n\n" +
		resp.Content + "\n"
	os.MkdirAll("data", 0o755)
	path := "data/distill-" + time.Now().Format("20060102-150405") + ".md"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return err
	}
	fmt.Println(out)
	fmt.Printf("建议已写入 %s（共 %d 条反馈输入）\n", path, len(fbs))
	return nil
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

// truncateStr 截断长文本。
func truncateStr(s string, n int) string {
	b := []rune(s)
	if len(b) <= n {
		return s
	}
	return string(b[:n]) + "…"
}
