// domaincase Case 的内存状态 + Reducer（PLAN Wave 4.2）：
// Reducer 是 CaseState 的唯一合并点——Facts 累积、Tokens 累计、OpenItems 维护。
package caseflow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/cnyup/alert-agent/internal/skills"
	"github.com/cnyup/alert-agent/pkg/model"
)

// DomainCase 内存态 Case。
type DomainCase struct {
	ID          string
	Fingerprint string
	Skill       *skills.Skill // 当前剧本
	Event       *model.AlertEvent
	Facts       []Fact
	OpenItems   []string // 未解之问/缺口（驱动 CONTINUE）
	Extra       string
	Round       int
	TokensUsed  int64
	FinalReport *model.DiagnosisReport
	CreatedAt   time.Time
}

// SkillName 当前剧本名（nil 安全）。
func (c *DomainCase) SkillName() string {
	if c.Skill == nil {
		return ""
	}
	return c.Skill.Name
}

// reduce Reducer：合并一轮 Delta 进 CaseState（幂等语义由 attempt 保证单次）。
func (c *DomainCase) reduce(d InvestigationDelta) {
	if d.Report == nil {
		return
	}
	// 根因假设 → 事实（带证据引用）
	for i, rc := range d.Report.RootCauses {
		if rc.Confidence < 0.5 {
			continue // 低置信不进事实，留 OpenItems
		}
		c.Facts = append(c.Facts, Fact{
			ID:          fmt.Sprintf("F%d-%d", c.Round, i+1),
			Kind:        "inferred",
			Statement:   rc.Hypothesis,
			EvidenceIDs: rc.Evidence,
			Round:       c.Round,
		})
	}
	// 未解之问 → OpenItems
	for _, u := range d.Report.Unresolved {
		c.OpenItems = append(c.OpenItems, u)
	}
	// 轮 token 累计（Case 级预算口径）
	c.TokensUsed += int64(d.Cost.TokensIn) + int64(d.Cost.TokensOut)
}

// judgeGaps Judge 产出的缺口清单（v1 规则式：低置信/无根因即缺口）。
func judgeGaps(rep *model.DiagnosisReport) []string {
	var gaps []string
	if len(rep.RootCauses) == 0 {
		gaps = append(gaps, "无根因结论：证据不足或假设未收敛")
		return gaps
	}
	for i, rc := range rep.RootCauses {
		if rc.Confidence < 0.6 {
			gaps = append(gaps, fmt.Sprintf("根因[%d] 置信 %.2f 低于阈值：需补充证据（当前 %v）",
				i+1, rc.Confidence, rc.Evidence))
		}
	}
	return gaps
}

// snapshotJSON 轮输入快照（崩溃恢复点）。
func snapshotJSON(c *DomainCase) string {
	b, _ := json.Marshal(map[string]any{
		"round":  c.Round,
		"skill":  c.SkillName(),
		"facts":  c.Facts,
		"tokens": c.TokensUsed,
	})
	return string(b)
}

// deltaJSON Delta 序列化（case_rounds.delta）。
func deltaJSON(d InvestigationDelta) string {
	b, _ := json.Marshal(d)
	return string(b)
}

// factsJSON Facts 序列化（cases.facts）。
func factsJSON(f []Fact) string {
	if len(f) == 0 {
		return ""
	}
	b, _ := json.Marshal(f)
	return string(b)
}
