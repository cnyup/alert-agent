// adapter Investigator 适配器（PLAN Wave 3.2）：现有 Runner.Diagnose 包装为
// caseflow.Investigator——eino 类型不越过本包边界。
package agent

import (
	"context"
	"fmt"

	"github.com/cnyup/alert-agent/internal/caseflow"
)

// investigatorAdapter 持有 Runner 与轮上下文（轮数进 Extra 文本，Evidence 带轮标记）。
type investigatorAdapter struct {
	r *Runner
}

// NewInvestigator 构造 caseflow 视角的调查者。对 caseflow 只暴露本工厂，
// eino 参数类型（reasoner/tools）收在 agent 包内。
func NewInvestigator(r *Runner) caseflow.Investigator {
	return &investigatorAdapter{r: r}
}

// Investigate 实现 caseflow.Investigator：把 Facts 拼进附加上下文，
// Evidence 结构做 caseflow 视角映射（traces 引用不复制内容语义由调用方落库）。
func (a *investigatorAdapter) Investigate(ctx context.Context, in caseflow.InvestigationInput) (caseflow.InvestigationDelta, error) {
	extra := in.Extra
	if len(in.Facts) > 0 {
		facts := "\n\n## 前序轮次已确认事实（Reducer 累积）\n"
		for _, f := range in.Facts {
			facts += fmt.Sprintf("- [%s] %s（证据 %v，第 %d 轮）\n",
				f.Kind, f.Statement, f.EvidenceIDs, f.Round)
		}
		extra += facts
	}
	rep, evidence, err := a.r.Diagnose(ctx, in.Event, in.Skill, extra)
	delta := caseflow.InvestigationDelta{Report: rep}
	for _, e := range evidence {
		delta.Evidence = append(delta.Evidence, caseflow.EvidenceRef{
			ID: e.ID, Tool: e.Tool, Round: in.Round,
		})
	}
	if rep != nil {
		delta.Cost = rep.Cost
	}
	if err != nil {
		return delta, fmt.Errorf("investigate round %d: %w", in.Round, err)
	}
	return delta, nil
}
