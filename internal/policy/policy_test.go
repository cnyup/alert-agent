package policy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/internal/store"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

func newTestManager(t *testing.T, ex Executor) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, ex), st
}

func rep() *coremodel.DiagnosisReport {
	return &coremodel.DiagnosisReport{
		AlertID: "evt_t1", SkillID: "s", SkillMatched: true,
		Summary:  "发布引入",
		Severity: coremodel.SeverityCritical,
		RootCauses: []coremodel.RootCause{
			{Hypothesis: "h", Confidence: 0.9, Evidence: []string{"T1"}},
		},
		Actions: []coremodel.ActionProposal{
			{ID: "A1", Title: "回滚", Risk: coremodel.RiskMutating, Tool: "k8s_get_pod", Args: []byte(`{"namespace":"prod"}`)},
			{ID: "A2", Title: "只读检查", Risk: coremodel.RiskReadOnly, Tool: "prometheus_query"},
		},
	}
}

func TestApprovalStateMachine(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, func(_ context.Context, tool, args string) (string, error) {
		if tool != "k8s_get_pod" {
			return "", errors.New("unexpected tool " + tool)
		}
		return "rollout ok", nil
	})

	if err := m.CreateApprovals(ctx, rep()); err != nil {
		t.Fatal(err)
	}
	// 只有 mutating 的 A1 进入审批
	if _, err := m.store.GetApproval(ctx, "evt_t1", "A2"); err == nil {
		t.Fatal("只读动作不应进入审批")
	}

	// 批准 → 执行 → executed
	out, err := m.Decide(ctx, "evt_t1", "A1", true, "ou_test")
	if err != nil || out == "" {
		t.Fatalf("批准执行失败: %v %q", err, out)
	}
	a, _ := m.store.GetApproval(ctx, "evt_t1", "A1")
	if a.Status != "executed" || a.Result != "rollout ok" || a.DecidedBy != "ou_test" {
		t.Fatalf("审批终态不符: %+v", a)
	}

	// 重复操作拒绝
	if _, err := m.Decide(ctx, "evt_t1", "A1", true, "ou_test"); err == nil {
		t.Fatal("重复决策应报错")
	}
}

func TestApprovalRejectAndExecFailure(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, func(context.Context, string, string) (string, error) {
		return "", errors.New("boom")
	})
	_ = m.CreateApprovals(ctx, rep())

	// 拒绝路径
	if _, err := m.Decide(ctx, "evt_t1", "A1", false, "ou_x"); err != nil {
		t.Fatal(err)
	}
	a, _ := m.store.GetApproval(ctx, "evt_t1", "A1")
	if a.Status != "rejected" {
		t.Fatalf("应 rejected: %+v", a)
	}

	// 执行失败路径（新事件）
	r2 := rep()
	r2.AlertID = "evt_t2"
	_ = m.CreateApprovals(ctx, r2)
	if _, err := m.Decide(ctx, "evt_t2", "A1", true, ""); err == nil {
		t.Fatal("执行失败应报错")
	}
	a2, _ := m.store.GetApproval(ctx, "evt_t2", "A1")
	if a2.Status != "failed" || a2.Result != "boom" {
		t.Fatalf("失败终态不符: %+v", a2)
	}
}

func TestFeedback(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, nil)
	if _, err := m.Feedback(ctx, "evt_f1", "claim", "ou_u"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Feedback(ctx, "evt_f1", "bad-kind", ""); err == nil {
		t.Fatal("未知类型应报错")
	}
	_ = time.Now()
}

// --- Wave 0.2：审批幂等与双击竞态（PLAN-CASE-SUPERVISOR §5 Wave 0.2）---

// T1 幂等：已终态（executed）的审批被 CreateApprovals 重建时不得回退 pending
// （F3 探针场景固化：INSERT OR REPLACE 会把 executed 重置回 pending）。
func TestApprovalIdempotentRecreate(t *testing.T) {
	ctx := context.Background()
	m, _ := newTestManager(t, func(context.Context, string, string) (string, error) {
		return "rollout ok", nil
	})
	_ = m.CreateApprovals(ctx, rep())
	// 走完整批准执行路径到 executed
	if _, err := m.Decide(ctx, "evt_t1", "A1", true, "ou_first"); err != nil {
		t.Fatal(err)
	}
	// 崩溃重跑场景：同一报告再次 CreateApprovals
	_ = m.CreateApprovals(ctx, rep())
	a, err := m.store.GetApproval(ctx, "evt_t1", "A1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "executed" {
		t.Fatalf("重建后状态回退了：want executed, got %s", a.Status)
	}
	if a.DecidedBy != "ou_first" || a.Result != "rollout ok" {
		t.Fatalf("决策信息被清空：%+v", a)
	}
}

// T2 并发：双击/重投并发 Decide(approve=true)，executor 恰好执行 1 次。
func TestApprovalConcurrentDecideExecutesOnce(t *testing.T) {
	ctx := context.Background()
	var execCount int32
	m, _ := newTestManager(t, func(context.Context, string, string) (string, error) {
		atomic.AddInt32(&execCount, 1)
		return "rollout ok", nil
	})
	_ = m.CreateApprovals(ctx, rep())

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Decide(ctx, "evt_t1", "A1", true, fmt.Sprintf("ou_%d", i))
		}(i)
	}
	wg.Wait()
	if n := atomic.LoadInt32(&execCount); n != 1 {
		t.Fatalf("executor 应恰好执行 1 次，实际 %d 次", n)
	}
	// 一方成功，另一方必须收到「已处理过」错误
	okCount := 0
	for _, err := range errs {
		if err == nil {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("应恰好 1 个 goroutine 成功，实际 %d（errs=%v）", okCount, errs)
	}
}

// T3 乱序：先 reject 后并发 approve，终态必须 rejected 且 executor 0 次。
func TestApprovalRejectThenApproveStaysRejected(t *testing.T) {
	ctx := context.Background()
	var execCount int32
	m, _ := newTestManager(t, func(context.Context, string, string) (string, error) {
		atomic.AddInt32(&execCount, 1)
		return "rollout ok", nil
	})
	_ = m.CreateApprovals(ctx, rep())
	if _, err := m.Decide(ctx, "evt_t1", "A1", false, "ou_rej"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Decide(ctx, "evt_t1", "A1", true, "ou_approver"); err == nil {
		t.Fatal("rejected 后 approve 应被拒绝")
	}
	a, _ := m.store.GetApproval(ctx, "evt_t1", "A1")
	if a.Status != "rejected" || a.DecidedBy != "ou_rej" {
		t.Fatalf("终态应保持 rejected：%+v", a)
	}
	if n := atomic.LoadInt32(&execCount); n != 0 {
		t.Fatalf("executor 应 0 次执行，实际 %d", n)
	}
}
