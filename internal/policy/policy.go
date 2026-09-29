// Package policy 审批状态机：mutating 动作落库 pending → 人工卡片决策 →
// 执行 → 结果。异步跨重启（显式落库状态机，不用运行中 interrupt，DESIGN.md §5）。
package policy

import (
	"context"
	"fmt"
	"time"

	"github.com/cnyup/alert-agent/internal/store"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

// Executor 动作执行器：按工具名调用，返回执行结果文本。
type Executor func(ctx context.Context, tool, argsJSON string) (string, error)

// Manager 审批与反馈管理。
type Manager struct {
	store    *store.Store
	executor Executor
}

// New 构造审批管理器。executor 为 nil 时批准后直接标记失败（无执行器）。
func New(st *store.Store, ex Executor) *Manager {
	return &Manager{store: st, executor: ex}
}

// CreateApprovals 为报告中的 mutating 动作创建 pending 审批。
// suggest-only 场景由 main 决定是否调用。幂等：已存在的记录（含终态）
// 原样保留——重跑报告/崩溃恢复不产生二次审批（PLAN Wave 0.2）。
func (m *Manager) CreateApprovals(ctx context.Context, rep *coremodel.DiagnosisReport) error {
	for _, a := range rep.Actions {
		if a.Risk != coremodel.RiskMutating {
			continue
		}
		if err := m.store.CreateApprovalIfAbsent(ctx, store.Approval{
			EventID:     rep.AlertID,
			ActionID:    a.ID,
			Title:       a.Title,
			Risk:        string(a.Risk),
			Tool:        a.Tool,
			ArgsJSON:    string(a.Args),
			Status:      "pending",
			RequestedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	return nil
}

// Decide 处理卡片按钮决策。返回跟进文案（回调层据此发跟进卡片/Toast）。
// 状态推进原子化（条件 UPDATE）：双击/重投并发下恰好一方推进成功，
// 失败方收到「已处理过」错误；executor 由推进成功者独占执行。
func (m *Manager) Decide(ctx context.Context, eventID, actionID string, approve bool, operator string) (string, error) {
	a, err := m.store.GetApproval(ctx, eventID, actionID)
	if err != nil {
		return "", fmt.Errorf("审批记录不存在: %w", err)
	}
	if a.Status != "pending" {
		return "", fmt.Errorf("审批已处理过（当前状态 %s），请勿重复操作", a.Status)
	}
	now := time.Now().UTC()
	if !approve {
		ok, err := m.store.TransitionApproval(ctx, eventID, actionID, "rejected", now, operator)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("审批已处理过，请勿重复操作")
		}
		return fmt.Sprintf("已拒绝：%s（由 %s）", a.Title, operatorMark(operator)), nil
	}
	ok, err := m.store.TransitionApproval(ctx, eventID, actionID, "approved", now, operator)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("审批已处理过，请勿重复操作")
	}
	// 执行（仅状态推进成功者到达这里）
	if m.executor == nil {
		_ = m.store.FinalizeApproval(ctx, eventID, actionID, "failed", "无执行器（未配置工具）")
		return "", fmt.Errorf("批准成功但执行失败：无执行器（未配置工具）")
	}
	out, err := m.executor(ctx, a.Tool, a.ArgsJSON)
	if err != nil {
		_ = m.store.FinalizeApproval(ctx, eventID, actionID, "failed", err.Error())
		return "", fmt.Errorf("执行失败：%v", err)
	}
	if err := m.store.FinalizeApproval(ctx, eventID, actionID, "executed", out); err != nil {
		return "", err
	}
	return fmt.Sprintf("已批准并执行：%s\n结果：%s", a.Title, out), nil
}

// Feedback 记录人工反馈（认领/误报/根因确认）。
func (m *Manager) Feedback(ctx context.Context, eventID, kind, operator string) (string, error) {
	switch kind {
	case "claim", "false-positive", "root-confirmed":
	default:
		return "", fmt.Errorf("未知反馈类型 %q", kind)
	}
	if err := m.store.SaveFeedback(ctx, store.Feedback{
		EventID: eventID, Kind: kind, Operator: operator, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	names := map[string]string{
		"claim": "已认领", "false-positive": "已标记误报", "root-confirmed": "根因已确认",
	}
	return fmt.Sprintf("%s（事件 %s，由 %s）", names[kind], eventID, operatorMark(operator)), nil
}

func operatorMark(operator string) string {
	if operator == "" {
		return "飞书用户"
	}
	return operator
}
