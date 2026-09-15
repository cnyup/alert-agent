// Package notifier 内置通知插件集。log 是零依赖默认通道；
// feishu-card 单向报告卡片（P1 双向闭环在用户提供完整凭证后）。
package notifier

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func init() {
	plugin.RegisterNotifierFactory("log", newLogNotifier)
	plugin.RegisterNotifierFactory("feishu-card", newFeishuCardNotifier)
}

type logNotifier struct{}

func newLogNotifier(map[string]any) (plugin.Notifier, error) { return &logNotifier{}, nil }

func (*logNotifier) Name() string { return "log" }

func (*logNotifier) Notify(_ context.Context, report *model.DiagnosisReport) error {
	b, _ := json.Marshal(report)
	slog.Info("诊断报告（log 通道）", "alert_id", report.AlertID, "report", string(b))
	return nil
}

// ---- feishu-card ----

type feishuCardNotifier struct {
	appID, appSecret, chatID string
	client                   *lark.Client
	onSent                   func(eventID, messageID string) // 卡片 message_id → event_id 映射落库
}

// SetOnSent 装配期注入发送回调（闭环指令靠回复卡片消息关联事件）。
func (f *feishuCardNotifier) SetOnSent(fn func(eventID, messageID string)) { f.onSent = fn }

type feishuOptions struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	ChatID    string `json:"chat_id"`
}

func newFeishuCardNotifier(opts map[string]any) (plugin.Notifier, error) {
	o := feishuOptions{}
	if err := decodeOpts(opts, &o); err != nil {
		return nil, err
	}
	if o.AppID == "" || o.ChatID == "" {
		return nil, fmt.Errorf("feishu-card: app_id/chat_id 不能为空")
	}
	if o.AppSecret == "" {
		return nil, fmt.Errorf("feishu-card: app_secret 缺失（发送卡片需用它换取 tenant_access_token）")
	}
	return &feishuCardNotifier{
		appID: o.AppID, appSecret: o.AppSecret, chatID: o.ChatID,
		client: lark.NewClient(o.AppID, o.AppSecret),
	}, nil
}

func (*feishuCardNotifier) Name() string { return "feishu-card" }

func (f *feishuCardNotifier) Notify(ctx context.Context, report *model.DiagnosisReport) error {
	card, err := buildReportCard(report)
	if err != nil {
		return fmt.Errorf("feishu-card: 卡片构造失败: %w", err)
	}
	content, _ := json.Marshal(card)
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(f.chatID).
			MsgType("interactive").
			Content(string(content)).
			Build()).
		Build()
	resp, err := f.client.Im.Message.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("feishu-card: 发送失败: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("feishu-card: 发送失败 code=%d msg=%s", resp.Code, resp.Msg)
	}
	if f.onSent != nil && resp.Data != nil && resp.Data.MessageId != nil {
		f.onSent(report.AlertID, *resp.Data.MessageId)
	}
	slog.Info("飞书卡片已发送", "alert_id", report.AlertID, "chat_id", f.chatID)
	return nil
}

// buildReportCard 报告卡片（schema 1.0 交互卡片）。
func buildReportCard(r *model.DiagnosisReport) (map[string]any, error) {
	headerColor := "orange"
	emoji := "🟠"
	switch r.Severity {
	case model.SeverityCritical:
		headerColor, emoji = "red", "🔴"
	case model.SeverityInfo:
		headerColor, emoji = "green", "🟢"
	}
	if r.NeedsHuman {
		emoji = "🙋"
	}

	var md strings.Builder
	fmt.Fprintf(&md, "**告警**：%s\n**结论**：%s\n**剧本**：%s（命中=%v）\n**步骤**：%d 步 / %dms",
		r.AlertID, escape(r.Summary), r.SkillID, r.SkillMatched, r.Cost.Steps, r.Cost.DurationMs)
	for i, rc := range r.RootCauses {
		fmt.Fprintf(&md, "\n---\n**根因 %d**（置信 %.0f%%，证据 %s）：%s",
			i+1, rc.Confidence*100, strings.Join(rc.Evidence, ","), escape(rc.Hypothesis))
	}
	for _, a := range r.Actions {
		risk := "只读"
		if a.Risk == model.RiskMutating {
			risk = "⚠️需审批"
		}
		fmt.Fprintf(&md, "\n**建议[%s]**：%s（%s · %s）", risk, escape(a.Title), a.ID, a.Tool)
	}
	if r.NeedsHuman {
		md.WriteString("\n---\n**需要人工介入**：无法自动收敛")
	}
	if len(r.Unresolved) > 0 {
		fmt.Fprintf(&md, "\n**未解之问**：%s", escape(strings.Join(r.Unresolved, "；")))
	}

	elements := []any{map[string]any{
		"tag":  "div",
		"text": map[string]any{"tag": "lark_md", "content": md.String()},
	}}
	// P1 双向闭环（回复消息驱动）：mutating 动作给出批准/拒绝指令格式
	for _, a := range r.Actions {
		if a.Risk == model.RiskMutating {
			md.WriteString(fmt.Sprintf("\n**闭环指令**：回复本消息 \"批准 %s\" 或 \"拒绝 %s\"", a.ID, a.ID))
		}
	}
	md.WriteString("\n**反馈**：回复本消息 \"认领\" / \"误报\" / \"根因确认\"")
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": headerColor,
			"title":    map[string]any{"tag": "plain_text", "content": fmt.Sprintf("%s 告警排查报告 · %s", emoji, r.Severity)},
		},
		"elements": elements,
	}, nil
}

func escape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

func decodeOpts(opts map[string]any, target any) error {
	if len(opts) == 0 {
		return nil
	}
	b, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}
