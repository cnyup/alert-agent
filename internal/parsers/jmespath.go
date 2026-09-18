// JMESPathParser 声明式 JSON 解析：用户配置 JMESPath 表达式把任意 JSON 报文
// 投影到告警字段（零代码对接自定义告警系统的主力）。
package parsers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmespath/go-jmespath"

	"github.com/cnyup/alert-agent/internal/webhook"
	"github.com/cnyup/alert-agent/pkg/model"
)

// JMESPathParser JMESPath 表达式映射解析器。
type JMESPathParser struct {
	cfg JMESPathConfig
}

type JMESPathConfig struct {
	Title     string `json:"title"`      // 必填：标题表达式
	Desc      string `json:"description"`
	Severity  string `json:"severity"`
	Status    string `json:"status"`     // firing|resolved（可配表达式）
	OccurAt   string `json:"occurred_at"` // RFC3339/Unix 秒
	Labels    map[string]string `json:"labels"` // label 名 → 表达式
	Fingerprint map[string]string `json:"fingerprint"` // 未用；labels 决定指纹
}

func (*JMESPathParser) Name() string { return "jmespath" }

// JMESPathPrototype ConfiguredParser 原型（webhook 源按 parse_options 构造实例）。
type JMESPathPrototype struct{ JMESPathParser }

func (*JMESPathPrototype) Name() string { return "jmespath" }

func (*JMESPathPrototype) Configure(opts map[string]any) (webhook.Parser, error) {
	b, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	var cfg JMESPathConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Title) == "" {
		return nil, fmt.Errorf("jmespath: title 表达式必填")
	}
	// 全部表达式构造期编译：配置错误在启动期暴露，而非首条报文
	for name, expr := range map[string]string{
		"title": cfg.Title, "description": cfg.Desc, "severity": cfg.Severity,
		"status": cfg.Status, "occurred_at": cfg.OccurAt,
	} {
		if strings.TrimSpace(expr) == "" {
			continue
		}
		if _, err := jmespath.Compile(expr); err != nil {
			return nil, fmt.Errorf("jmespath: %s 表达式非法 %q: %w", name, expr, err)
		}
	}
	for k, expr := range cfg.Labels {
		if _, err := jmespath.Compile(expr); err != nil {
			return nil, fmt.Errorf("jmespath: labels.%s 表达式非法 %q: %w", k, expr, err)
		}
	}
	return &JMESPathParser{cfg: cfg}, nil
}

func (p *JMESPathParser) Parse(_ context.Context, body []byte) ([]*model.AlertEvent, error) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("jmespath: 报文非法 JSON: %w", err)
	}
	get := func(expr string) (string, error) {
		if strings.TrimSpace(expr) == "" {
			return "", nil
		}
		r, err := jmespath.Search(expr, doc)
		if err != nil {
			return "", fmt.Errorf("表达式 %q: %w", expr, err)
		}
		return jmesToString(r), nil
	}
	title, err := get(p.cfg.Title)
	if err != nil {
		return nil, fmt.Errorf("jmespath: %w", err)
	}
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("jmespath: title 投影为空（表达式 %q）", p.cfg.Title)
	}
	desc, _ := get(p.cfg.Desc)
	sev, _ := get(p.cfg.Severity)
	if sev == "" {
		sev = string(model.SeverityWarning)
	}
	status, _ := get(p.cfg.Status)
	labels := model.Labels{}
	for k, expr := range p.cfg.Labels {
		if v, gerr := get(expr); gerr == nil && v != "" {
			labels[k] = v
		}
	}
	occurred := time.Now()
	if p.cfg.OccurAt != "" {
		if v, gerr := get(p.cfg.OccurAt); gerr == nil && v != "" {
			if ts, perr := parseTime(v); perr == nil {
				occurred = ts
			}
		}
	}
	evt, err := base("webhook/jmespath", sev, truncate(title, 200), labels, occurred, body, nil)
	if err != nil {
		return nil, err
	}
	evt.Description = desc
	if strings.EqualFold(status, "resolved") {
		evt.Status = model.StatusResolved
	}
	return []*model.AlertEvent{evt}, nil
}

func jmesToString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	case bool:
		return fmt.Sprintf("%t", x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if n, err := fmt.Sscanf(s, "%d", new(int64)); err == nil && n == 1 && len(s) <= 13 {
		ms := int64(0)
		_, _ = fmt.Sscanf(s, "%d", &ms)
		if len(s) == 13 { // 毫秒
			return time.UnixMilli(ms), nil
		}
		return time.Unix(ms, 0), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("时间格式无法解析: %q", s)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
