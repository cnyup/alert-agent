// supervisor Case 级调查编排（PLAN Wave 4.2）：FINISH|CONTINUE|SWITCH|ESCALATE
// 四态循环 + Judge 条件触发。纯 Go 状态机，零 eino import。
// 状态转移规则（v1 从简）：
//
//	needs_human=true            → ESCALATE
//	有新 open item 且轮数<上限   → CONTINUE/SWITCH（SWITCH 连续≥2 强制 ESCALATE）
//	否则                        → FINISH
//
// Judge 条件触发（成本纪律，非每轮）：
//
//	max(confidence)<0.6 || severity==critical || 无根因 → 评审产缺口清单。
package caseflow

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cnyup/alert-agent/internal/store"
	"github.com/cnyup/alert-agent/pkg/model"
)

// Outcome Case 终态产出。
type Outcome struct {
	CaseID      string
	State       State
	Reason      string
	FinalReport *model.DiagnosisReport
}

// State Case 状态。
type State string

const (
	StateInvestigating State = "investigating"
	StateFinished      State = "finished"
	StateEscalated     State = "escalated"
	StateCancelled     State = "cancelled"
)

// Verdict 单轮结束后的下一动作判定。
type Verdict string

const (
	Finish   Verdict = "FINISH"
	Continue Verdict = "CONTINUE"
	Switch   Verdict = "SWITCH"
	Escalate Verdict = "ESCALATE"
)

// Config Supervisor 配置。
type Config struct {
	MaxRounds        int     // 轮数硬顶（默认 3）
	JudgeMinConf     float64 // Judge 触发阈值（默认 0.6）
	MaxTokensPerCase int64   // Case 级 token 预算（<=0 不限）
}

// Supervisor 编排器。
type Supervisor struct {
	cfg    Config
	inv    Investigator
	router Router
	val    Validator
	st     *store.Store
	clock  Clock
}

// NewSupervisor 构造。
func NewSupervisor(cfg Config, inv Investigator, router Router, val Validator, st *store.Store) *Supervisor {
	if cfg.MaxRounds <= 0 {
		cfg.MaxRounds = 3
	}
	if cfg.JudgeMinConf <= 0 {
		cfg.JudgeMinConf = 0.6
	}
	return &Supervisor{cfg: cfg, inv: inv, router: router, val: val, st: st,
		clock: realClock{}}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// RunCase 完整 Case 执行（model.AlertEvent 由调用方传入 domainCase）。
func (s *Supervisor) RunCase(ctx context.Context, c *DomainCase) (*Outcome, error) {
	out := &Outcome{CaseID: c.ID}
	switchStreak := 0
	for c.Round <= s.cfg.MaxRounds {
		// 1) 选剧本：首轮由调用方预选（入口三级路由产物）；后续轮按 facts 重选
		skill := c.Skill
		if c.Round > 0 {
			skill = s.router.SelectSkill(ctx, c.Event, c.Facts)
			if skill == nil {
				skill = c.Skill // fail-closed：回落原剧本
			}
		}
		if skill == nil {
			out.State = StateEscalated
			out.Reason = "无可用剧本"
			return out, nil
		}
		c.Skill, c.Round = skill, c.Round+1
		c.OpenItems = nil // 每轮重置：OpenItems 语义=「本轮新产生」，reduce 按本轮报告填入
		attempt := fmt.Sprintf("%s-r%d-%d", c.ID, c.Round, s.clock.Now().UnixNano())
		// 2) 轮开始：attempt running + 输入快照（崩溃恢复点）
		if err := s.st.InsertRoundAttempt(ctx, c.ID, c.Round, attempt, skill.Name, snapshotJSON(c)); err != nil {
			return out, err
		}
		// 3) 调查
		delta, err := s.inv.Investigate(ctx, InvestigationInput{
			CaseID: c.ID, Round: c.Round, Skill: skill,
			Event: c.Event, Facts: c.Facts, Extra: c.Extra,
		})
		if err != nil {
			_ = s.st.FailRoundAttempt(ctx, c.ID, c.Round, attempt, "failed")
			// 调查失败：预算/引擎异常 → ESCALATE（不静默丢）
			out.State = StateEscalated
			out.Reason = fmt.Sprintf("第 %d 轮调查失败: %v", c.Round, err)
			s.persist(ctx, c, out.State)
			return out, nil
		}
		// 4) 校验 Delta（schema + evidence 不变量）
		if s.val != nil {
			if verr := s.val.Validate(delta); verr != nil {
				_ = s.st.FailRoundAttempt(ctx, c.ID, c.Round, attempt, "failed")
				out.State = StateEscalated
				out.Reason = fmt.Sprintf("第 %d 轮 Delta 校验失败: %v", c.Round, verr)
				s.persist(ctx, c, out.State)
				return out, nil
			}
		}
		// 5) 轮完成落库 + Reducer 合并
		if err := s.st.CompleteRoundAttempt(ctx, c.ID, c.Round, attempt, deltaJSON(delta)); err != nil {
			return out, err
		}
		c.reduce(delta)
		// 6) Case 预算熔断
		if s.cfg.MaxTokensPerCase > 0 && c.TokensUsed >= s.cfg.MaxTokensPerCase {
			out.State = StateEscalated
			out.Reason = fmt.Sprintf("Case 预算熔断（tokens=%d）", c.TokensUsed)
			s.persist(ctx, c, StateEscalated)
			return out, nil
		}
		// 7) Judge 条件触发（缺口清单进 open items）
		if delta.Report != nil && s.judgeNeeded(delta.Report) {
			c.OpenItems = append(c.OpenItems, judgeGaps(delta.Report)...)
		}
		// 8) 判定下一状态
		v := s.verdict(c, delta, switchStreak)
		switch v {
		case Finish:
			c.FinalReport = delta.Report
			out.State = StateFinished
			out.FinalReport = delta.Report
			s.persist(ctx, c, StateFinished)
			return out, nil
		case Escalate:
			c.FinalReport = delta.Report
			out.State = StateEscalated
			out.FinalReport = delta.Report
			s.persist(ctx, c, StateEscalated)
			return out, nil
		case Continue:
			switchStreak = 0
			s.persist(ctx, c, StateInvestigating)
		case Switch:
			switchStreak++
			if switchStreak >= 2 {
				out.State = StateEscalated
				out.Reason = "SWITCH 连续 ≥2 次强制升级"
				out.FinalReport = delta.Report
				s.persist(ctx, c, StateEscalated)
				return out, nil
			}
			s.persist(ctx, c, StateInvestigating)
		}
	}
	// 轮数硬顶：产出部分结论 + ESCALATE
	out.State = StateEscalated
	out.Reason = fmt.Sprintf("轮数硬顶（%d）", s.cfg.MaxRounds)
	s.persist(ctx, c, StateEscalated)
	return out, nil
}

// verdict 单轮判定。
func (s *Supervisor) verdict(c *DomainCase, d InvestigationDelta, switchStreak int) Verdict {
	if d.Report == nil {
		return Escalate
	}
	if d.Report.NeedsHuman {
		return Escalate
	}
	// 轮数硬顶：仍有未决事项 → 强制升级（部分结论）；干净 → FINISH
	if c.Round >= s.cfg.MaxRounds {
		if len(c.OpenItems) > 0 {
			return Escalate
		}
		return Finish
	}
	// 有新 open item（未解之问/缺口）→ 继续；证据指向他域 → SWITCH
	if len(c.OpenItems) > 0 {
		if skill := s.router.SelectSkill(context.Background(), c.Event, c.Facts); skill != nil &&
			c.Skill != nil && skill.Name != c.Skill.Name {
			return Switch
		}
		return Continue
	}
	return Finish
}

// judgeNeeded Judge 条件触发判定（PLAN §4.2 成本纪律——非每轮评审）：
// max(confidence) < 阈值 || severity==critical || 无根因。
func (s *Supervisor) judgeNeeded(rep *model.DiagnosisReport) bool {
	if rep == nil || len(rep.RootCauses) == 0 {
		return true
	}
	maxConf := 0.0
	for _, rc := range rep.RootCauses {
		if rc.Confidence > maxConf {
			maxConf = rc.Confidence
		}
	}
	return maxConf < s.cfg.JudgeMinConf || rep.Severity == model.SeverityCritical
}

func (s *Supervisor) persist(ctx context.Context, c *DomainCase, state State) {
	row := store.CaseRow{
		ID: c.ID, Fingerprint: c.Fingerprint, State: string(state),
		CurrentSkill: c.SkillName(), Round: c.Round,
		Facts: factsJSON(c.Facts), TokensUsed: c.TokensUsed,
		CreatedAt: c.CreatedAt, UpdatedAt: s.clock.Now(),
	}
	if err := s.st.UpsertCase(ctx, row); err != nil {
		slog.Warn("case 落库失败", "id", c.ID, "err", err)
	}
}
