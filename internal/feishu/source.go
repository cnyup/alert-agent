// Package feishu 飞书自建应用双向通道（P1）：长连接（WebSocket）事件订阅。
// 入向：单聊/群内 @机器人 消息作为告警源；对报告卡片的【回复】作为闭环指令
// （Go SDK 长连接暂不支持卡片按钮回调，故以 root_id 关联卡片 + 指令文本实现同等闭环）。
package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// value: {"type":"approve|reject|claim|false-positive|root-confirmed|recheck",
//         "event_id":..., "action_id":...}；返回跟进文案。
// recheck（重查）走追问续查通道：带原事件上下文重新排查。
type DecisionHandler func(ctx context.Context, value map[string]any, operator string) (string, error)

// CardResolver 卡片 message_id → event_id（main 注入 store 查询）。
type CardResolver func(messageID string) (eventID string, ok bool)

// FollowupHandler 追问续查处理（main 注入排查内核闭包）：
// 对已有报告的事件带着追问继续排查，返回跟进卡片文案。
type FollowupHandler func(ctx context.Context, eventID, question, chatID string) (string, error)

type source struct {
	appID, appSecret   string
	verificationToken  string
	encryptKey         string
	whitelist          map[string]bool
	client             *lark.Client

	mu       sync.Mutex
	decide   DecisionHandler
	followup FollowupHandler
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

// SetFollowupHandler 装配期注入追问续查处理。
func (s *source) SetFollowupHandler(h FollowupHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.followup = h
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
	decide, followup, resolve := s.decide, s.followup, s.resolve
	s.mu.Unlock()

	if msg.MessageType == nil || *msg.MessageType != "text" {
		return nil // P1 只处理文本
	}
	chatID := deref(msg.ChatId)
	chatType := deref(msg.ChatType)
	rootID := deref(msg.RootId)
	text := extractText(deref(msg.Content))

	parentID := deref(msg.ParentId)

	// ① 回复报告卡片 → 闭环指令（root/parent 命中卡片映射）
	if cmdID := firstNonEmpty(rootID, parentID); cmdID != "" && isCommand(text) && resolve != nil {
		if eventID, ok := resolve(cmdID); ok {
			return s.handleCommand(ctx, eventID, text, chatID, decide)
		}
	}
	// ①b 回复报告卡片 + 自由文本 → 追问续查（带原事件上下文的二次排查）
	if repID := firstNonEmpty(rootID, parentID); repID != "" && !isCommand(text) &&
		strings.TrimSpace(text) != "" && followup != nil && resolve != nil {
		if eventID, ok := resolve(repID); ok {
			return s.handleFollowup(ctx, eventID, text, chatID, followup)
		}
	}

	// 群聊要求 @机器人、白名单过滤（单聊直接响应）
	if chatType == "group" {
		if len(s.whitelist) > 0 && !s.whitelist[chatID] {
			slog.Debug("飞书群消息忽略（不在白名单）", "chat", chatID)
			return nil
		}
		if !strings.Contains(deref(msg.Content), "@_user_") {
			slog.Info("飞书群消息忽略（未 @机器人）", "chat", chatID, "text", truncateRunes(text, 40))
			return nil
		}
	}

	// ② 引用消息排查（设计初衷场景）：被引用的消息内容作为告警；
	// 特例——引用闭环提示卡/报告 + 指令文本（如"重查"）：被引内容含事件 ID 时
	// 分发为该事件的闭环指令（用户可能用"引用"而非"回复"，两个入口都接）。
	if quotedID := firstNonEmpty(parentID, rootID); quotedID != "" {
		if resolve == nil || func() bool { _, ok := resolve(quotedID); return !ok }() {
			if isCommand(text) && decide != nil {
				if eventID := s.eventIDFromQuoted(ctx, quotedID); eventID != "" {
					return s.handleCommand(ctx, eventID, text, chatID, decide)
				}
			}
			return s.investigateQuoted(ctx, quotedID, chatID, msgID, emit)
		}
	}

	// ③ 普通文本消息 → 新告警
	if strings.TrimSpace(text) == "" {
		return nil
	}
	evt, err := model.NewEvent("feishu/message", model.SeverityWarning, truncateRunes(text, 80),
		model.Labels{"via": "feishu", "chat_id": chatID, "alert_key": shortHash(text)},
		time.Now().UTC(), []byte(deref(msg.Content)),
		map[string]string{"feishu_chat_id": chatID, "feishu_message_id": msgID})
	if err != nil {
		return err
	}
	evt.Description = text
	return emit(ctx, evt)
}

// investigateQuoted 引用消息排查：拉取被引用消息，其内容作为告警。
func (s *source) investigateQuoted(ctx context.Context, quotedID, chatID, msgID string, emit plugin.EmitFunc) error {
	mtype, content, err := s.fetchMessage(ctx, quotedID)
	if err != nil {
		slog.Error("引用消息拉取失败（应用需开通 im:message 读取权限）",
			"quoted", quotedID, "err", err)
		return nil // 不中断，也不误把回复文本当告警
	}
	title, desc := extractQuotedAlert(mtype, content)
	if strings.TrimSpace(title) == "" && strings.TrimSpace(desc) == "" {
		slog.Warn("引用消息无可解析内容", "quoted", quotedID, "type", mtype)
		return nil
	}
	slog.Info("引用消息排查", "quoted", quotedID, "type", mtype, "title", truncateRunes(title, 40))
	evt, err := model.NewEvent("feishu/quote", model.SeverityWarning, truncateRunes(title, 80),
		model.Labels{"via": "feishu", "chat_id": chatID, "alert_key": "fq_" + quotedID},
		time.Now().UTC(), []byte(content),
		map[string]string{"feishu_chat_id": chatID, "feishu_message_id": msgID, "quoted_message_id": quotedID})
	if err != nil {
		return err
	}
	evt.Description = desc
	return emit(ctx, evt)
}

var reEventID = regexp.MustCompile(`evt_[A-Z0-9]{20,32}`)

// eventIDFromQuoted 从被引消息内容提取事件 ID（提示卡/报告卡文案携带）。
func (s *source) eventIDFromQuoted(ctx context.Context, quotedID string) string {
	_, content, err := s.fetchMessage(ctx, quotedID)
	if err != nil {
		return ""
	}
	return reEventID.FindString(content)
}

// handleFollowup 追问续查：回答经跟进卡片回给来源会话。
func (s *source) handleFollowup(ctx context.Context, eventID, question, chatID string, h FollowupHandler) error {
	slog.Info("追问续查", "event", eventID, "text", truncateRunes(question, 60))
	answer, err := h(ctx, eventID, question, chatID)
	if err != nil {
		slog.Error("追问续查失败", "event", eventID, "err", err)
		answer = "追问处理失败：" + err.Error()
	}
	if chatID != "" {
		if err := s.sendFollowUp(ctx, chatID, answer); err != nil {
			slog.Error("跟进卡片发送失败", "err", err)
		}
	}
	return nil
}

// fetchMessage 调用 im API 取一条消息（type, content）。
func (s *source) fetchMessage(ctx context.Context, messageID string) (mtype, content string, err error) {
	req := larkim.NewGetMessageReqBuilder().MessageId(messageID).Build()
	resp, err := s.client.Im.Message.Get(ctx, req)
	if err != nil {
		return "", "", err
	}
	if !resp.Success() {
		return "", "", fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	items := resp.Data.Items
	if len(items) == 0 || items[0].Body == nil {
		return "", "", fmt.Errorf("消息体为空")
	}
	return deref(items[0].MsgType), deref(items[0].Body.Content), nil
}

// extractQuotedAlert 从被引用消息提取告警标题与描述：
// text → 原文；post → 拼接富文本段；interactive（告警卡片）→ 卡片标题 + 原始 JSON
//（卡片 JSON 直接进 description，排查 LLM 可读）。
func extractQuotedAlert(mtype, content string) (title, desc string) {
	switch mtype {
	case "text":
		var c struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(content), &c) == nil {
			// 清 @占位符；清后仅剩空白的"空引用"回落原文（含占位），由调用方判无可解析内容
			if t := extractText(content); strings.TrimSpace(t) != "" {
				return t, t
			}
			return c.Text, c.Text
		}
		return content, content
	case "post":
		var sb strings.Builder
		var c map[string]any
		if json.Unmarshal([]byte(content), &c) == nil {
			collectPostText(c, &sb)
		}
		t := sb.String()
		return t, t
	case "interactive":
		var c struct {
			// title 兼容两种形态：标准卡片 schema 是 {content: ...} 对象，
			// 部分监控机器人直接发纯字符串（宽松格式兼容，不绑定特定平台）
			Title json.RawMessage      `json:"title"`
			Elements []json.RawMessage `json:"elements"`
		}
		if json.Unmarshal([]byte(content), &c) != nil {
			return "飞书引用卡片告警", content
		}
		title := parseCardTitle(c.Title)
		// 部分机器人的 elements 是 [[{tag,text},...],...] 嵌套数组（分段文本），
		// 递归展平成可读行文给 keyword 匹配与排查用；原始 JSON 仍留底 raw
		desc := title + "\n" + flattenCardElements(c.Elements)
		if strings.TrimSpace(desc) == "" {
			desc = content
		}
		return title, desc
	default:
		return "引用消息告警（" + mtype + "）", content
	}
}

// parseCardTitle 兼容字符串与 {content} 对象两种 title 形态。
func parseCardTitle(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "飞书引用卡片告警"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) != "" {
			return s
		}
		return "飞书引用卡片告警"
	}
	var obj struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Content != "" {
		return obj.Content
	}
	return "飞书引用卡片告警"
}

// flattenCardElements 宽松展平卡片元素为可读文本：兼容任意嵌套数组、
// {tag,text} 文本元素、{text:{content}} 结构与 elements/children 递归——
// 不假设发送方遵循标准卡片 schema。
func flattenCardElements(elements []json.RawMessage) string {
	var sb strings.Builder
	for _, el := range elements {
		collectCardText(el, &sb)
	}
	return sb.String()
}

func collectCardText(node any, sb *strings.Builder) {
	switch v := node.(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			sb.WriteString(s)
			sb.WriteString("\n")
		}
	case []any:
		for _, x := range v {
			collectCardText(x, sb)
		}
	case []json.RawMessage:
		for _, x := range v {
			collectCardText(x, sb)
		}
	case json.RawMessage:
		var arr []any
		if json.Unmarshal(v, &arr) == nil {
			collectCardText(arr, sb)
			return
		}
		var obj map[string]any
		if json.Unmarshal(v, &obj) == nil {
			collectCardText(obj, sb)
		}
	case map[string]any:
		// text 元素：{"tag":"text","text":"..."} 或 {"text":{"content":"..."}}
		if s, ok := v["text"].(string); ok && v["tag"] != "img" {
			if s = strings.TrimSpace(s); s != "" {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
		if t, ok := v["text"].(map[string]any); ok {
			if c, ok := t["content"].(string); ok && strings.TrimSpace(c) != "" {
				sb.WriteString(c)
				sb.WriteString("\n")
			}
		}
		for _, k := range []string{"elements", "children"} {
			if sub, ok := v[k].([]any); ok {
				collectCardText(sub, sb)
			}
		}
	}
}

func collectPostText(node any, sb *strings.Builder) {
	switch v := node.(type) {
	case string:
		sb.WriteString(v)
		sb.WriteString(" ")
	case []any:
		for _, x := range v {
			collectPostText(x, sb)
		}
	case map[string]any:
		if t, ok := v["text"].(string); ok {
			sb.WriteString(t)
			sb.WriteString(" ")
		}
		for _, x := range v {
			collectPostText(x, sb)
		}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, x := range ss {
		if x != "" {
			return x
		}
	}
	return ""
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// ---- 闭环指令 ----

var (
	reApprove = regexp.MustCompile(`(?i)^(批准|approve|同意)\s*(A\d+)?$`)
	reReject  = regexp.MustCompile(`(?i)^(拒绝|reject|不同意)\s*(A\d+)?$`)
	reClaim     = regexp.MustCompile(`(?i)^(认领|claim)$`)
	reFalsePos  = regexp.MustCompile(`(?i)^(误报|false.?positive)$`)
	reConfirm   = regexp.MustCompile(`(?i)^(根因确认|确认根因|confirm)$`)
	reRecheck   = regexp.MustCompile(`(?i)^(重查|重新排查|recheck|re-?run)$`)
)

func isCommand(text string) bool {
	t := strings.TrimSpace(text)
	return reApprove.MatchString(t) || reReject.MatchString(t) ||
		reClaim.MatchString(t) || reFalsePos.MatchString(t) || reConfirm.MatchString(t) ||
		reRecheck.MatchString(t)
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
	case reRecheck.MatchString(t):
		value = map[string]any{"type": "recheck", "event_id": eventID}
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
