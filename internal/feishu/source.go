// Package feishu 飞书自建应用双向通道（P1）：长连接（WebSocket）事件订阅。
// 入向：单聊/群内 @机器人 消息作为告警源；对报告卡片的【回复】作为闭环指令
// （Go SDK 长连接暂不支持卡片按钮回调，故以 root_id 关联卡片 + 指令文本实现同等闭环）。
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func init() {
	plugin.RegisterSourceFactory("feishu-app", newSource)
}

// DecisionHandler 闭环指令处理（main 注入 policy.Manager 闭包）。
// value: {"type":"approve|reject|claim|false-positive|root-confirmed",
//         "event_id":..., "action_id":...}；返回跟进文案。
type DecisionHandler func(ctx context.Context, value map[string]any, operator string) (string, error)

// CardResolver 卡片 message_id → event_id（main 注入 store 查询）。
type CardResolver func(messageID string) (eventID string, ok bool)

type source struct {
	appID, appSecret   string
	verificationToken  string
	encryptKey         string
	whitelist          map[string]bool
	client             *lark.Client

	mu       sync.Mutex
	decide   DecisionHandler
	resolve  CardResolver
	seenMsg  map[string]time.Time // message_id 去重（飞书至少一次投递）
}

type sourceOptions struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
	// 可选：开放平台"事件与回调"的 Verification Token / Encrypt Key（长连接可不配）
	VerificationToken string   `json:"verification_token"`
	EncryptKey        string   `json:"encrypt_key"`
	Chats             []string `json:"chats"` // 可选：仅处理这些群
}

func newSource(opts map[string]any) (plugin.Source, error) {
	o := sourceOptions{}
	if err := decode(opts, &o); err != nil {
		return nil, err
	}
	if o.AppID == "" || o.AppSecret == "" {
		return nil, fmt.Errorf("feishu-app: app_id/app_secret 不能为空")
	}
	wl := map[string]bool{}
	for _, c := range o.Chats {
		wl[c] = true
	}
	return &source{
		appID: o.AppID, appSecret: o.AppSecret,
		verificationToken: o.VerificationToken, encryptKey: o.EncryptKey,
		whitelist: wl,
		client:    lark.NewClient(o.AppID, o.AppSecret),
		seenMsg:   map[string]time.Time{},
	}, nil
}

func (*source) Name() string { return "feishu-app" }

// SetDecisionHandler 装配期注入闭环指令处理。
func (s *source) SetDecisionHandler(h DecisionHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decide = h
}

// SetCardResolver 装配期注入卡片映射查询。
func (s *source) SetCardResolver(r CardResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolve = r
}

// Start 建立长连接：消息事件 → 新告警 或 闭环指令。
func (s *source) Start(ctx context.Context, emit plugin.EmitFunc) error {
	d := dispatcher.NewEventDispatcher(s.verificationToken, s.encryptKey).
		OnP2MessageReceiveV1(func(ctx context.Context, ev *larkim.P2MessageReceiveV1) error {
			return s.onMessage(ctx, ev, emit)
		})
	cli := ws.NewClient(s.appID, s.appSecret,
		ws.WithEventHandler(d),
		ws.WithAutoReconnect(true),
		ws.WithOnReady(func() { slog.Info("飞书长连接就绪", "app_id", s.appID) }),
		ws.WithOnError(func(err error) { slog.Warn("飞书长连接错误", "err", err) }),
	)
	slog.Info("飞书源启动（长连接）", "app_id", s.appID)
	return cli.Start(ctx)
}

func (s *source) onMessage(ctx context.Context, ev *larkim.P2MessageReceiveV1, emit plugin.EmitFunc) error {
	if ev == nil || ev.Event == nil || ev.Event.Message == nil {
		return nil
	}
	msg := ev.Event.Message
	msgID := deref(msg.MessageId)
	// message_id 去重
	s.mu.Lock()
	if _, dup := s.seenMsg[msgID]; dup && msgID != "" {
		s.mu.Unlock()
		return nil
	}
	if msgID != "" {
		s.seenMsg[msgID] = time.Now()
		if len(s.seenMsg) > 2000 {
			for k, ts := range s.seenMsg {
				if time.Since(ts) > 15*time.Minute {
					delete(s.seenMsg, k)
				}
			}
		}
	}
	decide, resolve := s.decide, s.resolve
	s.mu.Unlock()

	if msg.MessageType == nil || *msg.MessageType != "text" {
		return nil // P1 只处理文本
	}
	chatID := deref(msg.ChatId)
	chatType := deref(msg.ChatType)
	rootID := deref(msg.RootId)
	text := extractText(deref(msg.Content))

	// ① 回复报告卡片 → 闭环指令（root_id 命中卡片映射）
	if rootID != "" && resolve != nil && isCommand(text) {
		if eventID, ok := resolve(rootID); ok {
			return s.handleCommand(ctx, eventID, text, chatID, decide)
		}
	}

	// ② 普通消息 → 新告警：单聊直接响应；群聊要求 @机器人
	if chatType == "group" {
		if len(s.whitelist) > 0 && !s.whitelist[chatID] {
			slog.Debug("飞书群消息忽略（不在白名单）", "chat", chatID)
			return nil
		}
		if !strings.Contains(deref(msg.Content), "@_user_") {
			slog.Info("飞书群消息忽略（未 @机器人）", "chat", chatID, "text", truncateRunes(text, 40))
			return nil
		}
	} else {
		slog.Info("飞书单聊消息", "chat", chatID, "text", truncateRunes(text, 40))
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	evt, err := model.NewEvent("feishu/message", model.SeverityWarning, truncateRunes(text, 80),
		model.Labels{"via": "feishu", "chat_id": chatID},
		time.Now().UTC(), []byte(deref(msg.Content)),
		map[string]string{"feishu_chat_id": chatID, "feishu_message_id": msgID})
	if err != nil {
		return err
	}
	evt.Description = text
	return emit(ctx, evt)
}

// ---- 闭环指令 ----

var (
	reApprove = regexp.MustCompile(`(?i)^(批准|approve|同意)\s*(A\d+)?$`)
	reReject  = regexp.MustCompile(`(?i)^(拒绝|reject|不同意)\s*(A\d+)?$`)
	reClaim     = regexp.MustCompile(`(?i)^(认领|claim)$`)
	reFalsePos  = regexp.MustCompile(`(?i)^(误报|false.?positive)$`)
	reConfirm   = regexp.MustCompile(`(?i)^(根因确认|确认根因|confirm)$`)
)

func isCommand(text string) bool {
	t := strings.TrimSpace(text)
	return reApprove.MatchString(t) || reReject.MatchString(t) ||
		reClaim.MatchString(t) || reFalsePos.MatchString(t) || reConfirm.MatchString(t)
}

func (s *source) handleCommand(ctx context.Context, eventID, text, chatID string, decide DecisionHandler) error {
	t := strings.TrimSpace(text)
	var value map[string]any
	switch {
	case reApprove.MatchString(t):
		value = map[string]any{"type": "approve", "event_id": eventID, "action_id": actionID(t, "A1")}
	case reReject.MatchString(t):
		value = map[string]any{"type": "reject", "event_id": eventID, "action_id": actionID(t, "A1")}
	case reClaim.MatchString(t):
		value = map[string]any{"type": "claim", "event_id": eventID}
	case reFalsePos.MatchString(t):
		value = map[string]any{"type": "false-positive", "event_id": eventID}
	case reConfirm.MatchString(t):
		value = map[string]any{"type": "root-confirmed", "event_id": eventID}
	default:
		return nil
	}
	slog.Info("闭环指令", "type", value["type"], "event", eventID, "chat", chatID)
	if decide == nil {
		return nil
	}
	followup, err := decide(ctx, value, "")
	if err != nil {
		slog.Error("闭环指令处理失败", "type", value["type"], "err", err)
		followup = "处理失败：" + err.Error()
	}
	if chatID != "" {
		if err := s.sendFollowUp(ctx, chatID, followup); err != nil {
			slog.Error("跟进卡片发送失败", "err", err)
		}
	}
	return nil
}

func actionID(text, def string) string {
	m := regexp.MustCompile(`(?i)A\d+`).FindString(text)
	if m == "" {
		return def
	}
	return strings.ToUpper(m)
}

// SendFollowUp 对外暴露跟进卡片发送（审批执行结果等）。
func (s *source) SendFollowUp(ctx context.Context, chatID, text string) error {
	return s.sendFollowUp(ctx, chatID, text)
}

func (s *source) sendFollowUp(ctx context.Context, chatID, text string) error {
	cardJSON := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": "green",
			"title":    map[string]any{"tag": "plain_text", "content": "🔔 排查闭环跟进"},
		},
		"elements": []any{map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": text},
		}},
	}
	b, _ := json.Marshal(cardJSON)
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(chatID).MsgType("interactive").Content(string(b)).Build()).
		Build()
	resp, err := s.client.Im.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

// ---- 工具函数 ----

var mentionPlaceholder = regexp.MustCompile(`@_user_\d+`)

func extractText(content string) string {
	var c struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		return mentionPlaceholder.ReplaceAllString(strings.TrimSpace(content), "")
	}
	return strings.TrimSpace(mentionPlaceholder.ReplaceAllString(c.Text, ""))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func decode(opts map[string]any, target any) error {
	if len(opts) == 0 {
		return nil
	}
	b, err := json.Marshal(opts)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}
