package caseflow

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/internal/skills"
	"github.com/cnyup/alert-agent/internal/store"
	"github.com/cnyup/alert-agent/pkg/model"
)

// 崩溃恢复（PLAN Wave 4.6）：running attempt 超时 → 回滚 → 重跑恰一次。
func TestRecoverCases(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	c, _ := mkCase(t)
	inv := &fakeInv{deltas: []InvestigationDelta{
		{Report: rep(false, 0.9, "q1")}, // 重跑后的第 1 轮（产生 open item）
		{Report: rep(false, 0.9)},        // 第 2 轮干净收敛
	}}
	sup := NewSupervisor(Config{MaxRounds: 3}, inv, &fakeRouter{alt: c.Skill}, nil, st)

	// 前置：模拟崩溃——r1 的 attempt 卡在 running
	if err := st.InsertRoundAttempt(ctx, c.ID, 1, "att-crash", "s1", `{"round":1}`); err != nil {
		t.Fatal(err)
	}
	// Case 状态留档 investigating
	if err := st.UpsertCase(ctx, store.CaseRow{
		ID: c.ID, Fingerprint: c.Fingerprint, State: string(StateInvestigating),
		CurrentSkill: "s1", Round: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// 恢复：att-crash 已超时
	rerun := sup.RecoverCases(ctx, time.Now().UTC().Add(time.Minute))
	if len(rerun) != 1 || rerun[0] != c.ID {
		t.Fatalf("应回收 1 个 Case，got %v", rerun)
	}
	// 回滚断言：att-crash 状态 failed
	status, err := st.RoundAttemptStatus(ctx, c.ID, 1, "att-crash")
	if err != nil || status != "failed" {
		t.Fatalf("att-crash 应回滚为 failed，got %q err=%v", status, err)
	}

	// 重跑：Round 从 0 重新起算（新 Case 实例，快照即恢复点）
	c2, _ := mkCase(t)
	c2.ID = c.ID // 同 Case
	out, err := sup.RunCase(ctx, c2)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != StateFinished {
		t.Fatalf("重跑应收敛 finished，got %s（%s）", out.State, out.Reason)
	}
	// 恰一次：重跑只新建 attempt（r1/r2），att-crash 不复活
	if inv.calls != 2 {
		t.Fatalf("重跑应恰调查 2 轮，实际 %d", inv.calls)
	}
	if s, _ := st.RoundAttemptStatus(ctx, c.ID, 1, "att-crash"); s != "failed" {
		t.Fatalf("旧 attempt 状态被改动: %s", s)
	}
}

// 审批不重复：重跑同一报告的 CreateApprovals 幂等（Wave 0.2 保证，此处集成断言）。
func TestRecoverApprovalsIdempotent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "r2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	r := rep(false, 0.9)
	r.Actions = []model.ActionProposal{
		{ID: "A1", Title: "回滚", Risk: model.RiskMutating, Tool: "exec"},
	}
	// “重跑”同一报告两次建审批
	for i := 0; i < 2; i++ {
		if err := st.CreateApprovalIfAbsent(ctx, store.Approval{
			EventID: "case-ap", ActionID: r.Actions[0].ID, Title: "回滚",
			Risk: "mutating", Tool: "exec", Status: "pending",
			RequestedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 只有一行 pending（幂等）
	var n int
	if err := func() error {
		row := st.QueryRowApprovalCount(ctx, "case-ap")
		return row.Scan(&n)
	}(); err != nil || n != 1 {
		t.Fatalf("审批应恰 1 行，got %d err=%v", n, err)
	}
	_ = model.SeverityWarning
	_ = skills.Skill{}
}
