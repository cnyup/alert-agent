package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLabelsCanonicalStable(t *testing.T) {
	// 同一组 labels 无论 map 迭代顺序如何，规范化串必须一致（去重键的前提）
	a := Labels{"env": "prod", "service": "order-api", "component": "postgres"}
	b := Labels{"component": "postgres", "env": "prod", "service": "order-api"}
	if a.Canonical() != b.Canonical() {
		t.Fatalf("labels 规范化不稳定:\n a=%s\n b=%s", a.Canonical(), b.Canonical())
	}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("同 labels 指纹不同")
	}
	if (Labels{}).Canonical() != "" {
		t.Fatal("空 labels 应规范化为空串")
	}
}

func TestNewEventAndValidate(t *testing.T) {
	now := time.Now().UTC()
	evt, err := NewEvent("webhook", SeverityCritical, "订单服务 5xx 飙升",
		Labels{"service": "order-api", "env": "prod"}, now,
		json.RawMessage(`{"foo":1}`), map[string]string{"feishu_message_id": "om_1"})
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if !strings.HasPrefix(evt.ID, "evt_") {
		t.Fatalf("ID 前缀非法: %s", evt.ID)
	}
	if evt.OccurredAt.IsZero() || evt.ReceivedAt.IsZero() {
		t.Fatal("时间戳未补齐")
	}
	if err := evt.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// occurredAt 为零值时回退为接收时间
	evt2, err := NewEvent("webhook", SeverityWarning, "x", nil, time.Time{}, nil, nil)
	if err != nil {
		t.Fatalf("NewEvent(零值时间): %v", err)
	}
	if evt2.OccurredAt != evt2.ReceivedAt {
		t.Fatal("零值 occurred_at 应回退为 received_at")
	}

	if _, err := NewEvent("", SeverityCritical, "t", nil, now, nil, nil); err == nil {
		t.Fatal("空 source 应报错")
	}
	if _, err := NewEvent("s", "sev1", "t", nil, now, nil, nil); err == nil {
		t.Fatal("非法 severity 应报错")
	}
}

func TestEventValidateRejectsBadFingerprint(t *testing.T) {
	evt := &AlertEvent{ID: "evt_x", Fingerprint: "zzz", Source: "s",
		Severity: SeverityInfo, Title: "t", ReceivedAt: time.Now(), OccurredAt: time.Now()}
	if err := evt.Validate(); err == nil {
		t.Fatal("非法 fingerprint 应报错")
	}
}

func TestReportValidateEvidenceEnforced(t *testing.T) {
	rep := &DiagnosisReport{
		AlertID:  "evt_x",
		Summary:  "连接池泄漏导致 5xx",
		Severity: SeverityCritical,
		RootCauses: []RootCause{
			{Hypothesis: "v1.2.3 发布引入泄漏", Confidence: 0.8, Evidence: []string{"T3", "T5"}},
		},
		Actions: []ActionProposal{
			{ID: "A1", Title: "回滚 order-api 到 v1.2.2", Risk: RiskMutating, Tool: "k8s.rollout_undo"},
		},
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("合法报告被拒: %v", err)
	}

	// 证据链强制：无 Evidence 的根因不允许出厂
	bad := rep
	bad.RootCauses = []RootCause{{Hypothesis: "猜测", Confidence: 0.5}}
	if err := bad.Validate(); err == nil {
		t.Fatal("缺证据链的根因应被拒绝")
	}

	// confidence 越界
	bad2 := &DiagnosisReport{AlertID: "evt_x", Summary: "s",
		RootCauses: []RootCause{{Hypothesis: "h", Confidence: 1.5, Evidence: []string{"T1"}}}}
	if err := bad2.Validate(); err == nil {
		t.Fatal("confidence>1 应被拒绝")
	}

	// 非法 risk
	bad3 := &DiagnosisReport{AlertID: "evt_x", Summary: "s",
		Actions: []ActionProposal{{ID: "A1", Title: "t", Tool: "x", Risk: "dangerous"}}}
	if err := bad3.Validate(); err == nil {
		t.Fatal("非法 risk 应被拒绝")
	}
}
