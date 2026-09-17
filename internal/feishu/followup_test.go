package feishu

import (
	"context"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func ptr(s string) *string { return &s }

func mkReply(msgID, parent, text, chatType, chatID string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				MessageId:   ptr(msgID),
				ParentId:    ptr(parent),
				ChatId:      ptr(chatID),
				ChatType:    ptr(chatType),
				MessageType: ptr("text"),
				Content:     ptr(`{"text":"` + text + `"}`),
			},
		},
	}
}

func noopEmit(context.Context, *model.AlertEvent) error { return nil }

// 回复报告卡片的自由文本 → 追问续查（不再落成新告警事件）。
func TestFollowupDispatch(t *testing.T) {
	s := &source{seenMsg: map[string]time.Time{}}
	s.SetCardResolver(func(id string) (string, bool) {
		if id == "card1" {
			return "evt1", true
		}
		return "", false
	})
	var gotEvent, gotQ string
	s.SetFollowupHandler(func(_ context.Context, eventID, question, _ string) (string, error) {
		gotEvent, gotQ = eventID, question
		return "answer", nil
	})

	// chatID 留空：handleFollowup 只分发不真实发卡
	if err := s.onMessage(context.Background(), mkReply("m1", "card1", "为什么这个工作流失败", "p2p", ""), noopEmit); err != nil {
		t.Fatal(err)
	}
	if gotEvent != "evt1" || gotQ != "为什么这个工作流失败" {
		t.Fatalf("追问未正确分发: event=%s q=%s", gotEvent, gotQ)
	}

	// 指令文本仍优先走闭环指令（不进追问分支）
	var followupHit bool
	s.SetFollowupHandler(func(context.Context, string, string, string) (string, error) {
		followupHit = true
		return "", nil
	})
	if err := s.onMessage(context.Background(), mkReply("m2", "card1", "批准 A1", "p2p", ""), noopEmit); err != nil {
		t.Fatal(err)
	}
	if followupHit {
		t.Fatal("指令文本不应进追问分支")
	}

	// 追问处理器返回错误 → 转为失败文案仍走跟进路径（chatID 空则不发）
	s.SetFollowupHandler(func(context.Context, string, string, string) (string, error) {
		return "", context.DeadlineExceeded
	})
	if err := s.onMessage(context.Background(), mkReply("m3", "card1", "再查一下", "p2p", ""), noopEmit); err != nil {
		t.Fatal(err)
	}
}

var _ plugin.EmitFunc = noopEmit
