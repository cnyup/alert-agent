// recovery 崩溃恢复（PLAN Wave 4.3）：启动时扫描超时 running attempt →
// 回滚（置 failed）→ 同 Case 以新 attempt 重新入队。依赖 Wave 0.2 审批幂等
// （重跑不重复建审批）。轮级重跑，不恢复 Eino 内部栈。
package caseflow

import (
	"context"
	"log/slog"
	"time"
)

// RecoverCases 启动恢复：返回需要重跑的 Case ID 列表（调用方重新入队）。
// staleBefore 之前的 running attempt 视为崩溃残留。
func (s *Supervisor) RecoverCases(ctx context.Context, staleBefore time.Time) []string {
	attempts, err := s.st.RunningRoundAttempts(ctx, staleBefore)
	if err != nil {
		slog.Warn("崩溃恢复扫描失败", "err", err)
		return nil
	}
	var rerun []string
	seen := map[string]bool{}
	for _, a := range attempts {
		if err := s.st.RollbackRoundAttempt(ctx, a.CaseID, a.Round, a.AttemptID); err != nil {
			slog.Warn("attempt 回滚失败", "case", a.CaseID, "attempt", a.AttemptID, "err", err)
			continue
		}
		slog.Info("崩溃恢复：回滚 running attempt", "case", a.CaseID,
			"round", a.Round, "attempt", a.AttemptID)
		if !seen[a.CaseID] {
			seen[a.CaseID] = true
			rerun = append(rerun, a.CaseID)
		}
	}
	return rerun
}
