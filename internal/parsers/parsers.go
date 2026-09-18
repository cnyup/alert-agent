// Package parsers 声明式解析器（N3）：grafana 固定格式 + jmespath/regex
// 通用表达式映射——让用户零代码对接任意告警报文（DESIGN §2.2 零代码层承诺）。
package parsers

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/cnyup/alert-agent/internal/webhook"
	"github.com/cnyup/alert-agent/pkg/model"
)

// 固定格式解析器编译期注册；jmespath/regex 是带配置解析器（ConfiguredParser），
// 经 webhook 源 parse_options 构造实例。
func init() {
	webhook.RegisterParser(&GrafanaParser{})
	webhook.RegisterConfiguredParser("jmespath", &JMESPathPrototype{})
	webhook.RegisterConfiguredParser("regex", &RegexPrototype{})
}

// ---- 通用辅助 ----

// base 构造事件公共路径（NewEvent 内含 severity 校验与指纹生成）。
func base(source, severity, title string, labels model.Labels, occurredAt time.Time,
	raw json.RawMessage, refs map[string]string) (*model.AlertEvent, error) {
	if model.Severity(severity) == "" {
		severity = string(model.SeverityWarning)
	}
	return model.NewEvent(source, model.Severity(severity), title, labels, occurredAt, raw, refs)
}

// firstStr 依序取第一个非空。
func firstStr(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
