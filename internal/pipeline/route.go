package pipeline

import (
	"context"
	"fmt"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func init() {
	plugin.RegisterStageFactory("route", newRoute)
}

// route 路由：规则顺序匹配（labels 全对上即命中），首条命中生效。
// 产出定级/候选剧本/通知目标，写入本次运行的 runState（经 ctx）。
type route struct {
	rules []routeRule
}

type routeRule struct {
	Name      string            `json:"name"`
	Match     map[string]string `json:"match"`
	Severity  string            `json:"severity"`
	Skills    []string          `json:"skills"`
	Notifiers []string          `json:"notifiers"`
}

type routeOptions struct {
	Rules []routeRule `json:"rules"`
}

func newRoute(opts map[string]any) (plugin.Stage, error) {
	o := routeOptions{}
	if err := DecodeOptions(opts, &o); err != nil {
		return nil, err
	}
	for i, r := range o.Rules {
		if r.Name == "" {
			return nil, fmt.Errorf("route: rules[%d].name 不能为空", i)
		}
		if len(r.Match) == 0 {
			return nil, fmt.Errorf("route: rules[%d].match 不能为空", i)
		}
		if r.Severity != "" && !model.Severity(r.Severity).Valid() {
			return nil, fmt.Errorf("route: rules[%d] 非法 severity %q", i, r.Severity)
		}
	}
	return &route{rules: o.Rules}, nil
}

func (*route) Name() string { return "route" }

func (r *route) Process(ctx context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	for _, rule := range r.rules {
		if !labelsMatch(evt.Labels, rule.Match) {
			continue
		}
		// 命中：定级覆盖（合法时）
		if model.Severity(rule.Severity).Valid() {
			evt.Severity = model.Severity(rule.Severity)
		}
		if st := StateFrom(ctx); st != nil {
			st.SetRoute(&RouteResult{
				Severity:  model.Severity(rule.Severity),
				Skills:    rule.Skills,
				Notifiers: rule.Notifiers,
				Matched:   rule.Name,
			})
		}
		return evt, plugin.ActionContinue, nil
	}
	// 无命中：路由结果为空，排查内核走通用剧本兜底
	return evt, plugin.ActionContinue, nil
}

func labelsMatch(labels model.Labels, want map[string]string) bool {
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}
