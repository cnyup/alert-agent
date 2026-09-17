package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
)

func init() {
	RegisterParser(&AlertmanagerParser{})
}

// AlertmanagerParser 解析 Alertmanager webhook 报文（version 4）。
type AlertmanagerParser struct{}

func (*AlertmanagerParser) Name() string { return "alertmanager" }

type amPayload struct {
	Version string    `json:"version"`
	Status  string    `json:"status"`
	Alerts  []amAlert `json:"alerts"`
}

type amAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
	GeneratorURL string           `json:"generatorURL"`
}

func (*AlertmanagerParser) Parse(_ context.Context, body []byte) ([]*model.AlertEvent, error) {
	var p amPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("alertmanager: 报文非法 JSON: %w", err)
	}
	var out []*model.AlertEvent
	for _, a := range p.Alerts {
		sev := mapSeverity(a.Labels["severity"])
		title := firstNonEmpty(a.Annotations["summary"], a.Labels["alertname"], "Alertmanager 告警")
		desc := firstNonEmpty(a.Annotations["description"], a.Annotations["runbook_url"])

		refs := map[string]string{}
		if a.GeneratorURL != "" {
			refs["generator_url"] = a.GeneratorURL
		}
		evt, err := model.NewEvent("webhook/alertmanager", sev, title,
			model.Labels(a.Labels), a.StartsAt, body, refs)
		if err != nil {
			return nil, fmt.Errorf("alertmanager: 构造事件失败: %w", err)
		}
		evt.Description = desc
		// resolved 不丢弃：进 Status 字段（不进 labels，指纹与 firing 保持一致），
		// 由入口层取消同指纹在途排查（DESIGN.md §4 取消纪律）
		if strings.EqualFold(a.Status, "resolved") {
			evt.Status = model.StatusResolved
		}
		out = append(out, evt)
	}
	return out, nil
}

// mapSeverity 归一化各家的等级写法到三档契约。
func mapSeverity(s string) model.Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical", "crit", "page", "sev1", "p0", "p1":
		return model.SeverityCritical
	case "warning", "warn", "sev2", "p2":
		return model.SeverityWarning
	case "info", "none", "sev3", "p3", "":
		return model.SeverityInfo
	default:
		return model.SeverityWarning
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
