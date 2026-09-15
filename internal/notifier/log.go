// Package notifier 内置通知插件集。log 通知器是 P0 的可验证默认通道；
// feishu-card 单向卡片在用户提供应用凭证后接入（P1 双向）。
package notifier

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func init() {
	plugin.RegisterNotifierFactory("log", newLogNotifier)
}

type logNotifier struct{}

func newLogNotifier(map[string]any) (plugin.Notifier, error) { return &logNotifier{}, nil }

func (*logNotifier) Name() string { return "log" }

func (*logNotifier) Notify(_ context.Context, report *model.DiagnosisReport) error {
	b, _ := json.Marshal(report)
	slog.Info("诊断报告（log 通道）", "alert_id", report.AlertID, "report", string(b))
	return nil
}
