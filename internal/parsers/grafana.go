// GrafanaParser 解析 Grafana Alerting webhook 报文（unified alerting，v9+ 格式）。
package parsers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
)

// GrafanaParser Grafana 告警 webhook。
type GrafanaParser struct{}

func (*GrafanaParser) Name() string { return "grafana" }

type gfPayload struct {
	Title        string          `json:"title"`
	RuleID       int64           `json:"ruleId"`
	RuleName     string          `json:"ruleName"`
	Status       string          `json:"status"`
	State        string          `json:"state"`
	Severity     string          `json:"severity"` // 部分通知策略注入；标准报文可能没有
	Message      string          `json:"message"`
	StartsAt     time.Time       `json:"startsAt"`
	EndsAt       time.Time       `json:"endsAt"`
	GeneratorURL string          `json:"generatorURL"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	// alerts 数组形态（一条通知含多告警）
	Alerts []gfAlert `json:"alerts"`
}

type gfAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	ValueString  string            `json:"valueString"`
}

func (*GrafanaParser) Parse(_ context.Context, body []byte) ([]*model.AlertEvent, error) {
	var p gfPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("grafana: 报文非法 JSON: %w", err)
	}
	// alerts 数组形态优先
	if len(p.Alerts) > 0 {
		var out []*model.AlertEvent
		for _, a := range p.Alerts {
			evt, err := gfToEvent("grafana/alert", firstNonZero(p.RuleID), p.RuleName, p.Title,
				a.Status, a.Labels, a.Annotations, a.StartsAt, a.GeneratorURL, a.ValueString, body)
			if err != nil {
				return nil, err
			}
			out = append(out, evt)
		}
		return out, nil
	}
	// 顶层单告警形态
	evt, err := gfToEvent("grafana/alert", firstNonZero(p.RuleID), p.RuleName, p.Title,
		firstStr(p.Status, p.State), p.Labels, p.Annotations, p.StartsAt, p.GeneratorURL, "", body)
	if err != nil {
		return nil, err
	}
	return []*model.AlertEvent{evt}, nil
}

func gfToEvent(source string, ruleID int64, ruleName, title, status string,
	labels model.Labels, ann map[string]string, startsAt time.Time,
	genURL, valueString string, raw json.RawMessage) (*model.AlertEvent, error) {
	if labels == nil {
		labels = model.Labels{}
	}
	// Grafana 常用等级在 labels.severity 或 annotations；兜底按状态
	sev := firstStr(labels["severity"], labels["Severity"], ann["severity"])
	if sev == "" {
		if strings.EqualFold(status, "alerting") || strings.EqualFold(status, "firing") {
			sev = "critical"
		} else {
			sev = "warning"
		}
	}
	t := firstStr(title, ruleName, ann["summary"], "Grafana 告警")
	if ruleName != "" {
		labels["rule_name"] = ruleName
	}
	if ruleID != 0 {
		labels["rule_id"] = fmt.Sprint(ruleID)
	}
	desc := firstStr(ann["description"], ann["summary"])
	if valueString != "" {
		desc = firstStr(desc, "当前值: "+valueString)
	}
	refs := map[string]string{}
	if genURL != "" {
		refs["generator_url"] = genURL
	}
	evt, err := base(source, sev, t, labels, startsAt, raw, refs)
	if err != nil {
		return nil, fmt.Errorf("grafana: %w", err)
	}
	evt.Description = desc
	if strings.EqualFold(status, "resolved") || strings.EqualFold(status, "ok") ||
		strings.EqualFold(status, "normal") {
		evt.Status = model.StatusResolved
	}
	return evt, nil
}

func firstNonZero(v int64) int64 { return v }
