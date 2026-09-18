// RegexParser 正则解析：面向文本类告警（日志平台/脚本推送等非 JSON 报文），
// 命名捕获组映射到告警字段——与 JMESPathParser 共同覆盖"任意报文"零代码接入。
package parsers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cnyup/alert-agent/internal/webhook"
	"github.com/cnyup/alert-agent/pkg/model"
)

// RegexParser 命名捕获组映射解析器。
type RegexParser struct {
	cfg RegexConfig
}

type RegexConfig struct {
	Pattern  string   `json:"pattern"`  // 必填：含命名捕获组的正则
	Severity string   `json:"severity"` // 固定等级（可选，默认 warning）
	Status   string   `json:"status"`   // 固定状态（可选）
	Labels   []string `json:"labels"`   // 捕获组名 → 进 labels（白名单声明）
}

func (*RegexParser) Name() string { return "regex" }

// RegexPrototype ConfiguredParser 原型。
type RegexPrototype struct{ RegexParser }

func (*RegexPrototype) Name() string { return "regex" }

func (*RegexPrototype) Configure(opts map[string]any) (webhook.Parser, error) {
	b, err := json.Marshal(opts)
	if err != nil {
		return nil, err
	}
	var cfg RegexConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return configureRegex(cfg)
}

// NewRegexParser 带配置构造。
func NewRegexParser(cfg RegexConfig) (*RegexParser, error) {
	return configureRegex(cfg)
}

func configureRegex(cfg RegexConfig) (*RegexParser, error) {
	if strings.TrimSpace(cfg.Pattern) == "" {
		return nil, fmt.Errorf("regex: pattern 必填")
	}
	re, err := regexp.Compile(cfg.Pattern)
	if err != nil {
		return nil, fmt.Errorf("regex: 编译失败: %w", err)
	}
	names := map[string]bool{}
	for _, n := range re.SubexpNames() {
		if n != "" {
			names[n] = true
		}
	}
	if !names["title"] {
		return nil, fmt.Errorf("regex: pattern 必须含命名捕获组 title")
	}
	return &RegexParser{cfg: cfg}, nil
}

func (p *RegexParser) Parse(_ context.Context, body []byte) ([]*model.AlertEvent, error) {
	re := regexp.MustCompile(p.cfg.Pattern)
	m := re.FindStringSubmatch(string(body))
	if m == nil {
		return nil, fmt.Errorf("regex: 报文不匹配 pattern")
	}
	groups := map[string]string{}
	for i, name := range re.SubexpNames() {
		if name != "" && i < len(m) {
			groups[name] = m[i]
		}
	}
	labels := model.Labels{}
	for _, k := range p.cfg.Labels {
		if v := groups[k]; v != "" {
			labels[k] = v
		}
	}
	sev := firstStr(p.cfg.Severity, groups["severity"], string(model.SeverityWarning))
	evt, err := base("webhook/regex", sev, truncate(groups["title"], 200), labels, time.Now(), body, nil)
	if err != nil {
		return nil, err
	}
	evt.Description = groups["description"]
	if strings.EqualFold(firstStr(p.cfg.Status, groups["status"]), "resolved") {
		evt.Status = model.StatusResolved
	}
	return []*model.AlertEvent{evt}, nil
}
