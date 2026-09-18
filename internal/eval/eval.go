// Package eval 剧本评测集（D1）：SKILL.md 的自动化回归——
// 合成告警 → 路由/排查 → 断言。case 是静态数据（非插件），两档执行：
//   - route-only：只测剧本路由，不调 LLM，零成本可常跑；
//   - full：完整排查（真实 LLM+工具），改剧本时跑，断言链路与契约。
// case 可手写，也可经 -eval-add 从真实事件沉淀（events.raw + 当时 trace）。
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/cnyup/alert-agent/internal/agent/eino"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

// AlertSpec 合成告警载荷（对齐 AlertEvent 的核心字段；labels 进指纹）。
type AlertSpec struct {
	Title       string            `yaml:"title"`
	Description string            `yaml:"description,omitempty"`
	Labels      coremodel.Labels  `yaml:"labels,omitempty"`
	Severity    string            `yaml:"severity,omitempty"`
}

// AssertSpec 断言集：全部满足才通过。只断结构不断内容（根因文案依赖线上数据）。
type AssertSpec struct {
	Route            string   `yaml:"route,omitempty"`             // 期望命中的剧本名（route-only 也测）
	RouteIn          []string `yaml:"route_in,omitempty"`          // 期望命中集合之一（如 [generic, ""]）
	RouteNot         []string `yaml:"route_not,omitempty"`          // 不得命中
	StepsLe          int      `yaml:"steps_le,omitempty"`           // full：模型轮次 ≤ 基线（预算回归）
	TraceContains    []string `yaml:"trace_contains,omitempty"`     // full：工具调用输入/输出须包含
	TraceNotContains []string `yaml:"trace_not_contains,omitempty"` // full：不得包含（如 resources +search——直查模板生效的反证）
	FirstExecPrefix  []string `yaml:"first_exec_prefix,omitempty"`  // full：首个 exec 的 argv 前缀（如 [tt-devops-cli, databases, query]）
	EvidenceExecOnly bool     `yaml:"evidence_exec_only,omitempty"` // full：报告证据编号只绑 exec 步（反臆造）
	NeedsHuman       *bool    `yaml:"needs_human,omitempty"`        // full：needs_human 期望值
}

// Case 一个评测样例。
type Case struct {
	Name   string     `yaml:"name"`
	Alert  AlertSpec  `yaml:"alert"`
	Assert AssertSpec `yaml:"assert"`
}

// Engine 评测引擎：main 注入（复用生产装配，route-only 与 full 两实现）。
type Engine interface {
	Route(ctx context.Context, evt *coremodel.AlertEvent) (skill string)
	Diagnose(ctx context.Context, evt *coremodel.AlertEvent) (*coremodel.DiagnosisReport, []eino.Evidence, error)
}

// Result 单 case 结果。
type Result struct {
	Name   string
	Passed bool
	Errors []string
	Skill  string // 实际路由结果
	Full   bool   // 是否跑了完整排查
}

// LoadCases 读目录下所有 cases.yaml（一个剧本一份，放 skills/<name>/evals/）。
func LoadCases(dir string) ([]Case, error) {
	var out []Case
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("eval: 读取目录失败: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		p := filepath.Join(dir, e.Name(), "evals", "cases.yaml")
		b, err := os.ReadFile(p)
		if err != nil {
			continue // 无评测集的剧本跳过
		}
		var cs []Case
		if err := yaml.Unmarshal(b, &cs); err != nil {
			return nil, fmt.Errorf("eval: %s 非法: %w", p, err)
		}
		for i := range cs {
			if cs[i].Name == "" {
				cs[i].Name = fmt.Sprintf("%s[%d]", e.Name(), i)
			}
		}
		out = append(out, cs...)
	}
	return out, nil
}

// MkEvent 把载荷合成事件（与生产同构：指纹/时间戳齐全）。
func (c *Case) MkEvent() (*coremodel.AlertEvent, error) {
	sev := coremodel.Severity(c.Alert.Severity)
	if sev == "" {
		sev = coremodel.SeverityWarning
	}
	evt, err := coremodel.NewEvent("eval", sev, c.Alert.Title, c.Alert.Labels, time.Now(), nil, nil)
	if err != nil {
		return nil, err
	}
	evt.Description = c.Alert.Description
	return evt, nil
}

// RunRoute route-only 档：只测路由（不调 LLM、零成本）。
func RunRoute(ctx context.Context, cs []Case, route func(context.Context, *coremodel.AlertEvent) string) []Result {
	var out []Result
	for _, c := range cs {
		r := Result{Name: c.Name, Skill: "（未测）"}
		evt, err := c.MkEvent()
		if err != nil {
			r.Errors = append(r.Errors, err.Error())
			out = append(out, r)
			continue
		}
		r.Skill = route(ctx, evt)
		r.Errors = assertRoute(&c.Assert, r.Skill)
		r.Passed = len(r.Errors) == 0
		out = append(out, r)
	}
	return out
}

// RunFull full 档：完整排查 + 全量断言。每个 case 一次真实 LLM 排查。
func RunFull(ctx context.Context, cs []Case, eng Engine) []Result {
	var out []Result
	for _, c := range cs {
		r := Result{Name: c.Name, Full: true}
		evt, err := c.MkEvent()
		if err != nil {
			r.Errors = append(r.Errors, err.Error())
			out = append(out, r)
			continue
		}
		r.Skill = eng.Route(ctx, evt)
		r.Errors = assertRoute(&c.Assert, r.Skill)

		rep, ev, err := eng.Diagnose(ctx, evt)
		if err != nil {
			r.Errors = append(r.Errors, "排查失败: "+err.Error())
			r.Passed = false
			out = append(out, r)
			continue
		}
		r.Errors = append(r.Errors, assertTrace(&c.Assert, ev)...)
		r.Errors = append(r.Errors, assertReport(&c.Assert, rep, ev)...)
		r.Passed = len(r.Errors) == 0
		out = append(out, r)
	}
	return out
}

func assertRoute(a *AssertSpec, skill string) []string {
	var errs []string
	if a.Route != "" && skill != a.Route {
		errs = append(errs, fmt.Sprintf("路由期望 %s，实际 %s", a.Route, skill))
	}
	if len(a.RouteIn) > 0 && !contains(a.RouteIn, skill) {
		errs = append(errs, fmt.Sprintf("路由期望 ∈ %v，实际 %s", a.RouteIn, skill))
	}
	if contains(a.RouteNot, skill) {
		errs = append(errs, fmt.Sprintf("路由不得命中 %s", skill))
	}
	return errs
}

func assertTrace(a *AssertSpec, ev []eino.Evidence) []string {
	var errs []string
	for _, e := range ev {
		if e.Tool == "exec" {
			// 排除断言只对 exec 生效：read 步返回的是技能文档内容，不受排查行为控制
			for _, s := range a.TraceNotContains {
				if hay := e.Args + " " + e.Result; strings.Contains(hay, s) {
					errs = append(errs, fmt.Sprintf("trace 不应包含 %q（%s）", s, e.ID))
				}
			}
		}
		for _, s := range a.TraceContains {
			if hay := e.Args + " " + e.Result; !strings.Contains(hay, s) {
				errs = append(errs, fmt.Sprintf("trace 缺少 %q（%s）", s, e.ID))
			}
		}
	}
	if len(a.FirstExecPrefix) > 0 {
		if err := assertFirstExec(a, ev); err != "" {
			errs = append(errs, err)
		}
	}
	return errs
}

// assertFirstExec 首个 exec 的 argv 前缀断言（直查模板生效的判据）。
func assertFirstExec(a *AssertSpec, ev []eino.Evidence) string {
	for _, e := range ev {
		if e.Tool != "exec" {
			continue
		}
		var in struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(e.Args), &in); err != nil || len(in.Argv) == 0 {
			return "首个 exec 参数不可解析"
		}
		if len(in.Argv) < len(a.FirstExecPrefix) {
			return fmt.Sprintf("首个 exec argv 过短: %v", in.Argv)
		}
		for i, want := range a.FirstExecPrefix {
			if in.Argv[i] != want {
				return fmt.Sprintf("首个 exec 前缀期望 %v，实际 %v", a.FirstExecPrefix, in.Argv)
			}
		}
		return ""
	}
	return "无 exec 调用"
}

func assertReport(a *AssertSpec, rep *coremodel.DiagnosisReport, ev []eino.Evidence) []string {
	var errs []string
	if a.StepsLe > 0 && rep.Cost.Steps > a.StepsLe {
		errs = append(errs, fmt.Sprintf("步数 %d 超基线 %d（预算回归）", rep.Cost.Steps, a.StepsLe))
	}
	if a.NeedsHuman != nil && rep.NeedsHuman != *a.NeedsHuman {
		errs = append(errs, fmt.Sprintf("needs_human 期望 %v，实际 %v", *a.NeedsHuman, rep.NeedsHuman))
	}
	if a.EvidenceExecOnly {
		execIDs := map[string]bool{}
		for _, e := range ev {
			if e.Tool == "exec" {
				execIDs[e.ID] = true
			}
		}
		for _, rc := range rep.RootCauses {
			for _, id := range rc.Evidence {
				if !execIDs[id] {
					errs = append(errs, fmt.Sprintf("证据 %s 未绑定 exec 步（反臆造约束）", id))
				}
			}
		}
	}
	return errs
}

// ReportSummary 汇总输出。
func ReportSummary(rs []Result) string {
	var b strings.Builder
	pass := 0
	for _, r := range rs {
		tag := "PASS"
		if r.Passed {
			pass++
		} else {
			tag = "FAIL"
		}
		fmt.Fprintf(&b, "[%s] %-28s skill=%s", tag, r.Name, r.Skill)
		if r.Full {
			fmt.Fprintf(&b, " (full)")
		}
		b.WriteString("\n")
		for _, e := range r.Errors {
			fmt.Fprintf(&b, "       ✗ %s\n", e)
		}
	}
	fmt.Fprintf(&b, "\n%d/%d 通过", pass, len(rs))
	return b.String()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
