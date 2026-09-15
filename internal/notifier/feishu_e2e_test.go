package notifier

import (
	"context"
	"os"
	"testing"

	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

// TestFeishuCardE2E 真实发送验证（默认跳过）：
// 需 FEISHU_E2E=1 且 FEISHU_APP_ID/FEISHU_APP_SECRET/FEISHU_CHAT_ID 就绪。
// 发送一份模拟排查报告卡片到目标群，验证凭证换取 token、卡片格式、投递全链路。
func TestFeishuCardE2E(t *testing.T) {
	if os.Getenv("FEISHU_E2E") != "1" {
		t.Skip("需 FEISHU_E2E=1 且真实凭证（远程联调用）")
	}
	appID := os.Getenv("FEISHU_APP_ID")
	secret := os.Getenv("FEISHU_APP_SECRET")
	chatID := os.Getenv("FEISHU_CHAT_ID")
	if appID == "" || secret == "" || chatID == "" {
		t.Fatal("FEISHU_APP_ID / FEISHU_APP_SECRET / FEISHU_CHAT_ID 未设置")
	}
	n, err := newFeishuCardNotifier(map[string]any{
		"app_id": appID, "app_secret": secret, "chat_id": chatID,
	})
	if err != nil {
		t.Fatalf("通知器装配失败: %v", err)
	}

	rep := &coremodel.DiagnosisReport{
		AlertID:      "evt_SMOKE_TEST_001",
		SkillID:      "http-5xx-spike",
		SkillMatched: true,
		Severity:     coremodel.SeverityCritical,
		Summary:      "【联通性测试·模拟数据】order-api v1.2.3 发布引入连接超时，5xx 错误率 0.2%→6.8%",
		RootCauses: []coremodel.RootCause{
			{
				Hypothesis: "v1.2.3 发布引入超时（错误率跳变与发布时间吻合）",
				Confidence: 0.8,
				Evidence:   []string{"T1", "T2"},
			},
		},
		Actions: []coremodel.ActionProposal{
			{ID: "A1", Title: "回滚 order-api 到 v1.2.2", Risk: coremodel.RiskMutating, Tool: "k8s.rollout_undo", RollbackHint: "再次发布 v1.2.3"},
		},
		Unresolved: []string{"新版本 pod 尚未完全替换，回滚窗口建议 10 分钟内"},
		NeedsHuman: false,
		Cost:       coremodel.ReportCost{Steps: 4, DurationMs: 8200},
	}
	if err := rep.Validate(); err != nil {
		t.Fatalf("模拟报告未过契约: %v", err)
	}
	if err := n.Notify(context.Background(), rep); err != nil {
		t.Fatalf("真实发送失败: %v", err)
	}
}
