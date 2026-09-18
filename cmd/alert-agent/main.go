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
	"gopkg.in/yaml.v3"

	"github.com/cnyup/alert-agent/internal/agent"
	"github.com/cnyup/alert-agent/internal/config"
	einoengine "github.com/cnyup/alert-agent/internal/agent/eino"
	"github.com/cnyup/alert-agent/internal/eval"
	"github.com/cnyup/alert-agent/internal/execenv"
	"github.com/cnyup/alert-agent/internal/fanout"
	"github.com/cnyup/alert-agent/internal/inflight"
	"github.com/cnyup/alert-agent/internal/feishu"
	"github.com/cnyup/alert-agent/internal/llm"
	"github.com/cnyup/alert-agent/internal/metrics"
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
	// 声明式解析器集（grafana 固定格式 + jmespath/regex 表达式映射）
	_ "github.com/cnyup/alert-agent/internal/parsers"
)

var version = "dev"

func main() {
	var (
		configPath = flag.String("config", "config.yaml", "配置文件路径")
		showVer    = flag.Bool("version", false, "打印版本")
		replayID   = flag.String("replay", "", "回放指定事件的 trace 后退出")
		distill    = flag.Bool("distill", false, "把人工反馈蒸馏为剧本修订建议后退出")
		evalSpec   = flag.String("eval", "", "剧本评测：路由档（默认）/ full=完整排查；如 -eval 或 -eval full")
		evalAddID  = flag.String("eval-add", "", "从指定真实事件沉淀评测 case（写 skills/<命中剧本>/evals/cases.yaml）")
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

	// eval-add 模式：真实事件 → 评测 case（金样例沉淀，脱敏只保留结构与断言）
	if *evalAddID != "" {
		if err := runEvalAdd(st, cfg, *evalAddID); err != nil {
			slog.Error("评测 case 沉淀失败", "err", err)
			os.Exit(1)
		}
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

	// 剧本语义路由（三级路由第二级）：router 小模型未配置则降级为 规则→generic
	var semRouter *agent.SemanticRouter
	routerLLM, err := llm.New(context.Background(), cfg.LLM.Router)
	switch {
	case errors.Is(err, llm.ErrNotConfigured):
		slog.Warn("LLM router 未配置，剧本路由为 规则→generic（无语义匹配）")
	case err != nil:
		// 路由是增强能力：构造失败降级，不打挂服务
		slog.Warn("LLM router 构造失败，语义路由禁用", "err", err)
	default:
		semRouter = agent.NewSemanticRouter(routerLLM, skillIdx)
		slog.Info("剧本语义路由就绪")
	}

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
		diagnoser = agent.New(reasoner, agentTools, cfg.Agent.MaxIterations, cfg.Agent.MaxTokens)
		slog.Info("排查内核就绪", "max_iterations", cfg.Agent.MaxIterations, "max_tokens", cfg.Agent.MaxTokens)
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

	// P1 审批状态机：批准后的执行直接走 execenv 全量环境（审批路径的 ctx
	// 不带排查 Env，也不再从排查工具集按名查找——报告动作的 tool 固定 exec，
	// argv 存于动作 args）。suggest-only 策略下禁止一切变更执行。
	policyMgr := policy.New(st, func(ctx context.Context, toolName, argsJSON string) (string, error) {
		if cfg.Policy.Execution == "suggest-only" {
			return "", fmt.Errorf("当前执行策略为 suggest-only：变更动作仅建议不执行（如需执行请将 policy.execution 配置为 approval-required）")
		}
		if toolName != "exec" || execFactory == nil {
			return "", fmt.Errorf("动作工具 %q 不可执行（当前仅支持 tool=exec 且 args 携带 argv 的动作）", toolName)
		}
		var in struct {
			Argv  []string `json:"argv"`
			Stdin string   `json:"stdin"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &in); err != nil || len(in.Argv) == 0 {
			return "", fmt.Errorf("动作 args 缺少可执行 argv（应为 {\"argv\":[...],\"stdin\":\"可选\"}）")
		}
		env, err := execFactory.AcquireFull(ctx)
		if err != nil {
			return "", fmt.Errorf("执行环境获取失败: %w", err)
		}
		defer env.Close(context.Background())
		// 子命令存在性探测：拦截模型臆造的命令（如不存在的 workflows retry），
		// 避免批准后执行才发现 unknown command
		if len(in.Argv) >= 2 {
			probe, err := env.Run(ctx, append(append([]string{}, in.Argv[0], in.Argv[1]), "--help"), "")
			if err == nil && probe.ExitCode != 0 &&
				(strings.Contains(probe.Stdout, "unknown command") || strings.Contains(probe.Stderr, "unknown command")) {
				return "", fmt.Errorf("命令不存在：%s %s（技能文档未记录该命令，请人工经业务平台执行）", in.Argv[0], in.Argv[1])
			}
		}
		res, err := env.Run(ctx, in.Argv, in.Stdin)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("exit=%d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr), nil
	})
	var followup feishu.FollowupHandler // 定义在源装配前（A5 追问续查），decideHandler 的重查指令引用

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
		case "recheck":
			return followup(ctx, eventID,
				"请对该告警重新执行完整排查（时间窗以当前时刻回溯），刷新证据后给出最新结论。", "")
		default:
			return "", fmt.Errorf("未知按钮类型 %q", kind)
		}
	}

	// 自身可观测性（C3）：/metrics，Prometheus 文本格式
	reg := metrics.New()
	reg.MustCounter("alert_agent_events_total", "接收的告警事件数", "source", "status")
	reg.MustCounter("alert_agent_events_dropped_total", "被管道终止的事件数", "stage")
	reg.MustCounter("alert_agent_route_total", "剧本路由分级计数", "tier")
	reg.MustCounter("alert_agent_diagnosis_total", "排查完成计数", "skill", "result")
	reg.MustCounter("alert_agent_diagnosis_seconds_total", "排查耗时累计（秒）", "skill")
	reg.MustCounter("alert_agent_diagnosis_steps_total", "排查步数累计", "skill")
	reg.MustCounter("alert_agent_diagnosis_tokens_total", "排查 token 消耗累计", "direction")
	reg.MustGauge("alert_agent_inflight_diagnoses", "在途排查数")
	reg.MustCounter("alert_agent_notifications_total", "通知发送计数", "result")

	// pickSkill 三级路由取剧本（评测复用；返回完整对象含 Body），埋路由分级指标。
	pickSkill := func(ctx context.Context, evt *model.AlertEvent) *skills.Skill {
		if hits := skillIdx.Match(evt); len(hits) > 0 {
			reg.Inc("alert_agent_route_total", "rule")
			return hits[0]
		}
		if semRouter != nil {
			if s := semRouter.Select(ctx, evt); s != nil {
				reg.Inc("alert_agent_route_total", "semantic")
				return s
			}
			reg.Inc("alert_agent_route_total", "generic")
			return generic
		}
		reg.Inc("alert_agent_route_total", "generic")
		return generic
	}

	// eval 模式：剧本评测（D1）。route 档零 LLM 成本；full 档每 case 一次真实排查。
	if *evalSpec != "" {
		runEval(*evalSpec == "full", cfg, pickSkill, diagnoser, execFactory)
		return
	}

	// 在途排查注册表：resolved 事件按指纹取消（DESIGN.md §4 取消纪律）
	inFlight := inflight.New()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("GET /metrics", reg.Handler())

	// selectSkill 剧本选择三级路由：路由指定优先 → 规则匹配 → 语义路由 → 通用兜底
	selectSkill := func(ctx context.Context, evt *model.AlertEvent, route *pipeline.RouteResult) *skills.Skill {
		if route != nil {
			for _, name := range route.Skills {
				if s, ok := skillIdx.Get(name); ok {
					reg.Inc("alert_agent_route_total", "route_rule")
					return s
				}
			}
		}
		return pickSkill(ctx, evt)
	}

	// feishuSend 由源装配期注入（dedup 命中引用类事件的提示卡发送）
	var feishuSend func(context.Context, string, string) error

	// 追问续查：回复报告卡片的自由文本 → 带原事件上下文的二次排查（A5）
	followup = func(ctx context.Context, eventID, question, chatID string) (string, error) {
		evt, err := st.GetEvent(ctx, eventID)
		if err != nil {
			return "", fmt.Errorf("原事件不存在或已清理: %w", err)
		}
		if diagnoser == nil {
			return "", errors.New("排查内核未启用")
		}
		skill := selectSkill(ctx, evt, nil)
		if skill == nil {
			return "", errors.New("无可用剧本")
		}
		if execFactory != nil {
			env, err := execFactory.Acquire(ctx)
			if err != nil {
				return "", fmt.Errorf("执行环境获取失败: %w", err)
			}
			defer env.Close(ctx)
			ctx = execenv.WithEnv(ctx, env)
		}
		extra := "## 人工追问（针对本事件已有报告的续查）\n" + question +
			"\n请围绕该问题继续排查；已有证据足以回答时可直接作答，新证据须引用工具调用编号。"
		report, evidence, err := diagnoser.Diagnose(ctx, evt, skill, extra)
		for i, e := range evidence {
			_ = st.AppendTrace(ctx, eventID, store.TraceEntry{
				Seq: 1000 + i, ID: e.ID, Kind: "tool", Name: e.Tool,
				Input: e.Args, Output: e.Result, At: time.Now().UTC(),
			})
		}
		if err != nil {
			return "", fmt.Errorf("续查执行失败: %w", err)
		}
		if b, err := json.Marshal(report); err == nil {
			_ = st.AppendTrace(ctx, eventID, store.TraceEntry{
				Seq: 1500, ID: "R2", Kind: "model", Name: "followup_report",
				Output: string(b), At: time.Now().UTC(),
			})
		}
		slog.Info("追问续查完成", "event", eventID, "skill", skill.Name,
			"summary", report.Summary, "needs_human", report.NeedsHuman)
		var sb strings.Builder
		sb.WriteString("**追问**：" + question + "\n\n**结论**：" + report.Summary)
		for i, rc := range report.RootCauses {
			sb.WriteString(fmt.Sprintf("\n\n根因%d（置信 %.0f%%，证据 %s）：%s",
				i+1, rc.Confidence*100, strings.Join(rc.Evidence, "/"), rc.Hypothesis))
		}
		for _, a := range report.Actions {
			sb.WriteString(fmt.Sprintf("\n建议 %s[%s]：%s", a.ID, a.Risk, a.Title))
		}
		if report.NeedsHuman {
			sb.WriteString("\n\n⚠️ 需人工介入")
		}
		return sb.String(), nil
	}

	handleEvent := func(ctx context.Context, evt *model.AlertEvent) error {
		reg.Inc("alert_agent_events_total", evt.Source, string(evt.Status))
		// resolved 相位：不进管道（同指纹会被 dedup 丢弃），取消在途排查后落库留痕
		if evt.Status == model.StatusResolved {
			n := inFlight.Cancel(evt.Fingerprint)
			reg.SetGauge("alert_agent_inflight_diagnoses", int64(inFlight.Len()))
			slog.Info("告警已恢复，取消在途排查", "id", evt.ID, "fingerprint", evt.Fingerprint, "cancelled", n)
			if err := st.SaveEvent(ctx, evt); err != nil {
				slog.Error("resolved 事件落库失败", "id", evt.ID, "err", err)
			}
			return nil
		}
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
			reg.Inc("alert_agent_events_dropped_total", res.DropBy)
			slog.Info("事件被管道终止", "id", evt.ID, "stage", res.DropBy, "title", evt.Title)
			// 引用是人的主动动作：同卡被 dedup 合并时不能沉默，回提示卡并引导重查
			if evt.Source == "feishu/quote" && res.DropBy == "dedup" && feishuSend != nil {
				notifyDedupHit(ctx, evt, feishuSend, st)
			}
			return nil
		}
		slog.Info("事件通过管道", "id", evt.ID, "severity", evt.Severity, "title", evt.Title)

		// 排查内核
		if diagnoser == nil {
			slog.Warn("排查内核未启用，事件仅落库", "id", evt.ID)
			return nil
		}
		// 在途登记：resolved 到达时按指纹取消（取消纪律）；排查结束注销
		diagCtx, diagCancel := context.WithCancel(ctx)
		unregister := inFlight.Add(evt.Fingerprint, diagCancel)
		reg.SetGauge("alert_agent_inflight_diagnoses", int64(inFlight.Len()))
		diagStart := time.Now()
		defer func() {
			unregister()
			diagCancel()
			reg.SetGauge("alert_agent_inflight_diagnoses", int64(inFlight.Len()))
		}()
		ctx = diagCtx
		// 每事件独占执行环境（docker 后端 = 一事件一容器；local 后端无状态共享）
		if execFactory != nil {
			env, err := execFactory.Acquire(ctx)
			if err != nil {
				slog.Error("执行环境获取失败，跳过排查", "id", evt.ID, "err", err)
				return nil
			}
			// 清理用独立 ctx：排查 ctx 被 resolved 取消时 Docker API 不能随之失败（容器会残留）
			defer env.Close(context.Background())
			ctx = execenv.WithEnv(ctx, env)
		}
		skill := selectSkill(ctx, evt, res.Route)
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
			// 被 resolved 取消属正常收敛：静默退出，不发失败卡打扰
			if errors.Is(err, context.Canceled) {
				reg.Inc("alert_agent_diagnosis_total", "cancelled", skill.Name)
				slog.Info("在途排查已被取消（告警恢复）", "id", evt.ID, "skill", skill.Name)
				return nil
			}
			reg.Inc("alert_agent_diagnosis_total", "failed", skill.Name)
			reg.Add("alert_agent_diagnosis_seconds_total", time.Since(diagStart).Milliseconds()/1000, skill.Name)
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
		result := "ok"
		if report.NeedsHuman {
			result = "needs_human"
		}
		reg.Inc("alert_agent_diagnosis_total", result, skill.Name)
		reg.Add("alert_agent_diagnosis_seconds_total", time.Since(diagStart).Milliseconds()/1000, skill.Name)
		reg.Add("alert_agent_diagnosis_steps_total", int64(report.Cost.Steps), skill.Name)
		reg.Add("alert_agent_diagnosis_tokens_total", int64(report.Cost.TokensIn), "in")
		reg.Add("alert_agent_diagnosis_tokens_total", int64(report.Cost.TokensOut), "out")
		slog.Info("排查完成", "id", evt.ID, "skill", skill.Name,
			"summary", report.Summary, "needs_human", report.NeedsHuman,
			"steps", report.Cost.Steps, "root_causes", len(report.RootCauses))
		// P1：mutating 动作进入审批状态机（卡片按钮批准后才执行）。
		// 风险归一化：动作携带 argv 时以配置分类（RiskOf）为准——模型标成
		// read-only 但命令按前缀属变更的，强制提级为 mutating 落审批。
		if execFactory != nil {
			for i := range report.Actions {
				a := &report.Actions[i]
				if a.Tool != "exec" {
					continue
				}
				var in struct {
					Argv []string `json:"argv"`
				}
				if json.Unmarshal(a.Args, &in) != nil || len(in.Argv) == 0 {
					continue
				}
				if execFactory.RiskOf(in.Argv) == model.RiskMutating {
					a.Risk = model.RiskMutating
				}
			}
		}
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
		reg.Add("alert_agent_notifications_total", int64(len(targets)-len(failed)), "ok")
		reg.Add("alert_agent_notifications_total", int64(len(failed)), "failed")
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
		if f, ok := src.(interface{ SetFollowupHandler(feishu.FollowupHandler) }); ok {
			f.SetFollowupHandler(followup)
		}
		if f, ok := src.(interface {
			SendFollowUp(context.Context, string, string) error
		}); ok {
			feishuSend = f.SendFollowUp
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

// runEval 剧本评测执行：engine 复用生产装配（路由/排查与线上一致）。
func runEval(full bool, cfg *config.Config,
	pick func(context.Context, *model.AlertEvent) *skills.Skill,
	diagnoser *agent.Runner, execFactory execenv.Factory) {
	cases, err := eval.LoadCases(cfg.Skills.Dir)
	if err != nil {
		slog.Error("评测集装载失败", "err", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Println("（无评测 case：在 skills/<剧本>/evals/cases.yaml 添加，或用 -eval-add 从真实事件沉淀）")
		return
	}
	route := func(ctx context.Context, evt *model.AlertEvent) string {
		return pick(ctx, evt).Name
	}
	var rs []eval.Result
	if !full {
		rs = eval.RunRoute(context.Background(), cases, route)
	} else {
		if diagnoser == nil {
			slog.Error("full 档需要排查内核（LLM reasoner 未配置）")
			os.Exit(1)
		}
		eng := &prodEvalEngine{pick: pick, diagnoser: diagnoser, execFactory: execFactory}
		rs = eval.RunFull(context.Background(), cases, eng)
	}
	fmt.Println(eval.ReportSummary(rs))
	for _, r := range rs {
		if !r.Passed {
			os.Exit(1)
		}
	}
}

// prodEvalEngine 生产装配的评测引擎（full 档）。
type prodEvalEngine struct {
	pick        func(context.Context, *model.AlertEvent) *skills.Skill
	diagnoser   *agent.Runner
	execFactory execenv.Factory
}

func (e *prodEvalEngine) Route(ctx context.Context, evt *model.AlertEvent) string { return e.pick(ctx, evt).Name }

func (e *prodEvalEngine) Diagnose(ctx context.Context, evt *model.AlertEvent) (*model.DiagnosisReport, []einoengine.Evidence, error) {
	if e.execFactory != nil {
		env, err := e.execFactory.Acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
		defer env.Close(context.Background())
		ctx = execenv.WithEnv(ctx, env)
	}
	// 与生产同构：注入命中剧本全文（Body 含纪律与输出契约）
	return e.diagnoser.Diagnose(ctx, evt, e.pick(ctx, evt), "")
}

// runEvalAdd 从真实事件沉淀评测 case：取事件载荷与当时命中的剧本，
// 生成最小断言（路由命中 + 首个 exec 形态），人工再补强。
func runEvalAdd(st *store.Store, cfg *config.Config, eventID string) error {
	ctx := context.Background()
	evt, err := st.GetEvent(ctx, eventID)
	if err != nil {
		return fmt.Errorf("事件不存在: %w", err)
	}
	trace, err := st.Trace(ctx, eventID)
	if err == nil {
		_ = trace // 证据细节人工补充；这里只沉淀路由与形态基线
	}
	skillName := "generic"
	if trace, terr := st.Trace(ctx, eventID); terr == nil {
		for _, e := range trace {
			if e.ID != "R1" {
				continue
			}
			var rep struct {
				SkillID string `json:"skill_id"`
			}
			if json.Unmarshal([]byte(e.Output), &rep) == nil && rep.SkillID != "" {
				skillName = rep.SkillID
			}
		}
	}
	name := fmt.Sprintf("%s-%s", evt.Title, time.Now().Format("0102-1504"))
	labels := model.Labels{}
	for k, v := range evt.Labels { // 滤掉消息级 labels（单条消息指纹，进 case 无意义）
		if k == "via" || k == "chat_id" || k == "alert_key" {
			continue
		}
		labels[k] = v
	}
	c := eval.Case{
		Name: truncateStr(name, 60),
		Alert: eval.AlertSpec{
			Title:       evt.Title,
			Description: truncateStr(evt.Description, 400),
			Labels:      labels,
			Severity:    string(evt.Severity),
		},
		Assert: eval.AssertSpec{Route: skillName},
	}
	dir := filepath.Join(cfg.Skills.Dir, skillName, "evals")
	if err := os.MkdirAll(dir, 0o755); skillName != "generic" && err != nil {
		return err
	}
	// generic 命中写到 _generic/evals（LoadCases 跳过 _ 前缀目录——统一放 examples 根）
	if skillName == "generic" {
		dir = filepath.Join(cfg.Skills.Dir, "_generic", "evals")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	p := filepath.Join(dir, "cases.yaml")
	var existing []eval.Case
	if b, err := os.ReadFile(p); err == nil {
		_ = yaml.Unmarshal(b, &existing)
	}
	existing = append(existing, c)
	b, err := yaml.Marshal(existing)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("已沉淀 case：%s → %s（断言基线：route=%s；请按当时的 trace 补强 StepsLe/FirstExecPrefix 等）\n", c.Name, p, skillName)
	return nil
}

// notifyDedupHit 引用类事件被去重合并：取同指纹最近排查结论，回提示卡并引导"重查"。
// 文案携带事件 ID 明文——引用该卡 + 指令文本（如"重查"）也能定位事件（引用与回复是两个入口）。
func notifyDedupHit(ctx context.Context, evt *model.AlertEvent,
	send func(context.Context, string, string) error, st *store.Store) {
	chatID := evt.Refs["feishu_chat_id"]
	if chatID == "" {
		return
	}
	text := fmt.Sprintf("该告警卡片在去重窗口内已排查过，本次引用已合并，未重复排查。\n（事件 %s）\n如需以当前时刻重新排查，请回复本卡：**重查**", evt.ID)
	if prev, err := st.LatestEventByFingerprint(ctx, evt.Fingerprint, evt.ID); err == nil && prev.ID != "" {
		if trace, terr := st.Trace(ctx, prev.ID); terr == nil {
			for _, e := range trace {
				if e.ID != "R1" {
					continue
				}
				var rep struct {
					Summary string `json:"summary"`
				}
				if json.Unmarshal([]byte(e.Output), &rep) == nil && rep.Summary != "" {
					text = fmt.Sprintf("该卡片 %s 已排查过（本次引用在去重窗口内被合并）：\n\n**上次结论**：%s\n\n（事件 %s）\n如需以当前时刻重新排查，请回复本卡：**重查**",
						prev.ReceivedAt.Local().Format("15:04"), truncateStr(rep.Summary, 200), prev.ID)
					break
				}
			}
		}
	}
	sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := send(sendCtx, chatID, text); err != nil {
		slog.Error("去重提示卡发送失败", "id", evt.ID, "err", err)
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
