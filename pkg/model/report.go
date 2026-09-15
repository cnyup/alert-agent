package model

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ToolRisk 工具风险标注：read-only 在排查中直接执行；
// mutating 无论策略如何配置，一律走飞书审批卡片（DESIGN.md §4）。
type ToolRisk string

const (
	RiskReadOnly  ToolRisk = "read-only"
	RiskMutating  ToolRisk = "mutating"
)

func (r ToolRisk) Valid() bool {
	switch r {
	case RiskReadOnly, RiskMutating:
		return true
	}
	return false
}

// RootCause 根因假设。Evidence 是证据链强制项：
// 每条结论必须引用 trace 中的工具调用编号（如 ["T3","T5"]），可回放。
type RootCause struct {
	Hypothesis string  `json:"hypothesis"`
	Confidence float64 `json:"confidence"` // [0,1]
	Evidence   []string `json:"evidence"`
}

// ActionProposal 建议动作。Risk=mutating 的动作进入审批状态机。
type ActionProposal struct {
	ID           string          `json:"id"` // 如 "A1"，审批回调按此匹配
	Title        string          `json:"title"`
	Risk         ToolRisk        `json:"risk"`
	Tool         string          `json:"tool"`
	Args         json.RawMessage `json:"args,omitempty"`
	RollbackHint string          `json:"rollback_hint,omitempty"`
}

// ReportCost 单次排查的成本核算，预算控制与成本审计的依据。
type ReportCost struct {
	Steps      int   `json:"steps"`       // agent loop 步数
	TokensIn   int   `json:"tokens_in"`   // 输入 token
	TokensOut  int   `json:"tokens_out"`  // 输出 token
	DurationMs int64 `json:"duration_ms"` // 耗时
}

// DiagnosisReport 排查内核的结构化产出。携带 AlertID 与来源侧 Refs，
// Notifier 依赖它完成回复/引用关联，无需额外参数。
type DiagnosisReport struct {
	AlertID      string            `json:"alert_id"`
	Refs         map[string]string `json:"refs,omitempty"`
	SkillID      string            `json:"skill_id"`      // 使用的剧本
	SkillMatched bool              `json:"skill_matched"` // false = 通用剧本兜底
	Severity     Severity          `json:"severity"`     // 复核后的等级（可与原始告警不同）
	Summary      string            `json:"summary"`
	RootCauses   []RootCause       `json:"root_causes,omitempty"`
	Actions      []ActionProposal  `json:"actions,omitempty"`
	Unresolved   []string          `json:"unresolved,omitempty"` // 未解之问，供追问会话
	NeedsHuman   bool              `json:"needs_human"`          // 无法收敛，需人工介入
	Cost         ReportCost        `json:"cost"`
}

// Validate 报告契约校验：证据链强制在此兜底——无证据的结论不允许出厂。
func (r *DiagnosisReport) Validate() error {
	if r == nil {
		return errors.New("model: 报告为空")
	}
	if r.AlertID == "" {
		return errors.New("model: alert_id 不能为空")
	}
	if r.Summary == "" {
		return errors.New("model: summary 不能为空")
	}
	if r.Severity != "" && !r.Severity.Valid() {
		return fmt.Errorf("model: 非法 severity %q", r.Severity)
	}
	for i, rc := range r.RootCauses {
		if rc.Hypothesis == "" {
			return fmt.Errorf("model: root_causes[%d].hypothesis 不能为空", i)
		}
		if rc.Confidence < 0 || rc.Confidence > 1 {
			return fmt.Errorf("model: root_causes[%d].confidence 超出 [0,1]", i)
		}
		if len(rc.Evidence) == 0 {
			return fmt.Errorf("model: root_causes[%d] 缺少证据链引用（结论必须引用工具调用编号）", i)
		}
	}
	for i, a := range r.Actions {
		if a.ID == "" || a.Title == "" || a.Tool == "" {
			return fmt.Errorf("model: actions[%d] 缺少 id/title/tool", i)
		}
		if !a.Risk.Valid() {
			return fmt.Errorf("model: actions[%d] 非法 risk %q", i, a.Risk)
		}
	}
	return nil
}
