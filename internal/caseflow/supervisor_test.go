package caseflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/internal/skills"
	"github.com/cnyup/alert-agent/internal/store"
	"github.com/cnyup/alert-agent/pkg/model"
)

// fakeInv 可编程 Investigator（按轮返回预设 Delta）。
type fakeInv struct {
	deltas []InvestigationDelta
	errs   []error
	calls  int
}

func (f *fakeInv) Investigate(_ context.Context, in InvestigationInput) (InvestigationDelta, error) {
	i := f.calls
	f.calls++
	var d InvestigationDelta
	if i < len(f.deltas) {
		d = f.deltas[i]
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return d, f.errs[i]
	}
	d.Cost = model.ReportCost{Steps: 1, TokensIn: 100, TokensOut: 50}
	return d, nil
}

// fakeRouter 固定剧本（SWITCH 场景换名）。
type fakeRouter struct{ alt *skills.Skill }

func (f *fakeRouter) SelectSkill(_ context.Context, _ *model.AlertEvent, _ []Fact) *skills.Skill {
	return f.alt
}

func rep(needsHuman bool, conf float64, unresolved ...string) *model.DiagnosisReport {
	r := &model.DiagnosisReport{
		Summary:  "s",
		Severity: model.SeverityWarning,
		RootCauses: []model.RootCause{
			{Hypothesis: "h", Confidence: conf, Evidence: []string{"T1"}},
		},
		NeedsHuman: needsHuman,
		Unresolved: unresolved,
	}
	return r
}

func mkCase(t *testing.T) (*DomainCase, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	skill := &skills.Skill{Name: "s1", Body: "b"}
	evt, _ := model.NewEvent("t", model.SeverityWarning, "x", nil, time.Now().UTC(), nil, nil)
	return &DomainCase{ID: "case-1", Fingerprint: "fp", Skill: skill, Event: evt}, st
}

// 表驱动：状态 × 输入 → 转移。
func TestSupervisorStateTransitions(t *testing.T) {
	tests := []struct {
		name       string
		deltas     []InvestigationDelta
		errs       []error
		wantState  State
		wantRounds int
	}{
		{
			name:      "needs_human 首轮即升级",
			deltas:    []InvestigationDelta{{Report: rep(true, 0.9)}},
			wantState: StateEscalated, wantRounds: 1,
		},
		{
			name:      "干净收敛单轮结束",
			deltas:    []InvestigationDelta{{Report: rep(false, 0.9)}},
			wantState: StateFinished, wantRounds: 1,
		},
		{
			name: "有未解之问跑两轮后收敛",
			deltas: []InvestigationDelta{
				{Report: rep(false, 0.9, "q1")},
				{Report: rep(false, 0.9)},
			},
			wantState: StateFinished, wantRounds: 2,
		},
		{
			name: "轮数硬顶强制升级",
			deltas: []InvestigationDelta{
				{Report: rep(false, 0.9, "q1")},
				{Report: rep(false, 0.9, "q2")},
				{Report: rep(false, 0.9, "q3")},
			},
			wantState: StateEscalated, wantRounds: 3,
		},
		{
			name:      "调查失败即升级不静默",
			errs:      []error{errors.New("engine boom")},
			wantState: StateEscalated, wantRounds: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st := mkCase(t)
			inv := &fakeInv{deltas: tt.deltas, errs: tt.errs}
			sup := NewSupervisor(Config{MaxRounds: 3}, inv,
				&fakeRouter{alt: c.Skill}, nil, st)
			out, err := sup.RunCase(context.Background(), c)
			if err != nil {
				t.Fatalf("RunCase: %v", err)
			}
			if out.State != tt.wantState {
				t.Fatalf("want %s got %s（reason %s）", tt.wantState, out.State, out.Reason)
			}
			if c.Round != tt.wantRounds {
				t.Fatalf("want %d rounds got %d", tt.wantRounds, c.Round)
			}
		})
	}
}

// fakeRouter 交替换剧本（每次返回与当前不同的名字——模拟"证据持续指向他域"）。
type altRouter struct{ cur *skills.Skill }

func (f *altRouter) SelectSkill(_ context.Context, _ *model.AlertEvent, _ []Fact) *skills.Skill {
	next := &skills.Skill{Name: f.cur.Name + "+", Body: "b"}
	f.cur = next
	return next
}

// SWITCH 连续 ≥2 次强制 ESCALATE（防无限换剧本）。
func TestSupervisorSwitchStreakEscalates(t *testing.T) {
	c, st := mkCase(t)
	inv := &fakeInv{deltas: []InvestigationDelta{
		{Report: rep(false, 0.9, "q1")}, // open item + router 换剧本 → SWITCH#1
		{Report: rep(false, 0.9, "q2")}, // 又换 → SWITCH#2 → 强制升级
	}}
	sup := NewSupervisor(Config{MaxRounds: 5}, inv, &altRouter{cur: c.Skill}, nil, st)
	out, _ := sup.RunCase(context.Background(), c)
	if out.State != StateEscalated {
		t.Fatalf("SWITCH 连续两次应升级，got %s", out.State)
	}
	if inv.calls > 2 {
		t.Fatalf("应在第 2 次 SWITCH 后停，实际调查 %d 次", inv.calls)
	}
}

// Case 预算熔断：tokens 累计超限 → ESCALATE。
func TestSupervisorBudgetFuse(t *testing.T) {
	c, st := mkCase(t)
	inv := &fakeInv{deltas: []InvestigationDelta{
		{Report: rep(false, 0.9, "q1")}, // 150 tokens
		{Report: rep(false, 0.9, "q2")}, // 150 → 300 ≥ 300 熔断
	}}
	sup := NewSupervisor(Config{MaxRounds: 5, MaxTokensPerCase: 300}, inv,
		&fakeRouter{alt: c.Skill}, nil, st)
	out, _ := sup.RunCase(context.Background(), c)
	if out.State != StateEscalated || out.Reason == "" {
		t.Fatalf("预算熔断应升级带原因，got %s %q", out.State, out.Reason)
	}
}

// 双轮调查 evidence 无跨轮泄漏（每轮 Delta 独立）。
func TestSupervisorNoCrossRoundLeak(t *testing.T) {
	c, st := mkCase(t)
	inv := &fakeInv{deltas: []InvestigationDelta{
		{Report: rep(false, 0.9, "q1"), Evidence: []EvidenceRef{{ID: "T1", Tool: "exec", Round: 1}}},
		{Report: rep(false, 0.9), Evidence: []EvidenceRef{{ID: "T1", Tool: "exec", Round: 2}}},
	}}
	sup := NewSupervisor(Config{MaxRounds: 3}, inv, &fakeRouter{alt: c.Skill}, nil, st)
	if _, err := sup.RunCase(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, e := range inv.deltas[1].Evidence {
		if e.Round != 2 {
			t.Fatalf("第二轮 evidence 应带 Round=2: %+v", e)
		}
	}
}
