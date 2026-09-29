// caseflow Case 级调查编排的领域层（PLAN-CASE-SUPERVISOR §6）：
// 零 eino import——Eino 细节全部隔离在 internal/agent 适配器内。
package caseflow

import (
	"context"
	"time"

	"github.com/cnyup/alert-agent/internal/skills"
	"github.com/cnyup/alert-agent/pkg/model"
)

// Fact Reducer 累积的已确认事实（跨轮持久，进 CaseState）。
type Fact struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"` // observed | inferred
	Statement   string   `json:"statement"`
	EvidenceIDs []string `json:"evidence_ids"`
	Round       int      `json:"round"`
}

// EvidenceRef traces 行引用（不复制内容——Case 视角是 traces 的投影）。
type EvidenceRef struct {
	ID    string `json:"id"`    // T<n> 编号
	Tool  string `json:"tool"`
	Round int    `json:"round"`
}

// InvestigationInput 单轮调查输入。
type InvestigationInput struct {
	CaseID string
	Round  int
	Skill  *skills.Skill // 已收权剧本
	Event  *model.AlertEvent
	Facts  []Fact // Reducer 累积事实
	Extra  string // incident/追问等附加上下文
}

// InvestigationDelta 单轮调查产出（校验与合并的最小单元）。
type InvestigationDelta struct {
	Report   *model.DiagnosisReport
	Evidence []EvidenceRef
	Cost     model.ReportCost
}

// Investigator 领域接口：一次剧本内调查 → Delta。
// 实现在 internal/agent（现有 Diagnose 的适配）。
type Investigator interface {
	Investigate(ctx context.Context, in InvestigationInput) (InvestigationDelta, error)
}

// Router 能力路由：入口三级路由 + 轮间重选（候选清单内选择，fail-closed）。
type Router interface {
	SelectSkill(ctx context.Context, evt *model.AlertEvent, facts []Fact) *skills.Skill
}

// Validator Delta 校验：schema + evidence 编号真实存在 + 不变量。
type Validator interface {
	Validate(d InvestigationDelta) error
}

// Clock 可注入时间（测试用）。
type Clock interface {
	Now() time.Time
}
