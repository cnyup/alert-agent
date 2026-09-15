// Package agent 排查内核的装配层：剧本注入、报告解析与校验（DESIGN.md §4）。
// Eino 细节全部隔离在 internal/agent/eino。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	einoengine "github.com/cnyup/alert-agent/internal/agent/eino"
	"github.com/cnyup/alert-agent/internal/skills"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

// Runner 排查内核。零内部可变状态，并发安全。
type Runner struct {
	reasoner model.BaseModel[*schema.Message]
	tools    []tool.BaseTool // P0：MCP 接入前为空集（剧本收权在空集上恒安全）
	maxIter  int
}

// New 构造排查内核。
func New(reasoner model.BaseModel[*schema.Message], tools []tool.BaseTool, maxIter int) *Runner {
	if maxIter <= 0 {
		maxIter = 12
	}
	return &Runner{reasoner: reasoner, tools: tools, maxIter: maxIter}
}

// reportFormat 强制最终回答为可解析的报告 JSON（证据链引用 T<n> 编号）。
const reportFormat = `

## 输出格式（最终回答必须是纯 JSON，不要包裹代码块，不要输出其他文字）

{
  "summary": "一句话结论",
  "severity": "critical|warning|info",
  "root_causes": [{"hypothesis": "根因假设", "confidence": 0.0-1.0, "evidence": ["T1","T2"]}],
  "actions": [{"id": "A1", "title": "建议动作", "risk": "read-only|mutating", "tool": "工具名"}],
  "unresolved": ["未解之问"],
  "needs_human": false
}

硬约束：root_causes 每条必须引用证据链编号（T1、T2…）；无证据的猜测不得写入。`

// Tools 返回排查内核持有的工具集（审批执行器按名查找用）。
func (r *Runner) Tools() []tool.BaseTool { return r.tools }

// Diagnose 对单条告警执行排查：注入剧本全文（第二阶段加载）→ 引擎执行 →
// 解析并校验报告。skill 为 nil 时由调用方先落位通用兜底剧本。
func (r *Runner) Diagnose(ctx context.Context, evt *coremodel.AlertEvent, skill *skills.Skill) (*coremodel.DiagnosisReport, []einoengine.Evidence, error) {
	start := time.Now()

	// 剧本收权：只把剧本声明的工具交给引擎（skill.Tools 为空则不给工具——
	// 通用兜底剧本 P0 无工具，只做告警本身的推理）
	var allowed []tool.BaseTool
	for _, t := range r.tools {
		if skill == nil || len(skill.Tools) == 0 {
			break
		}
		for _, name := range skill.Tools {
			if info, err := t.Info(ctx); err == nil && info != nil && info.Name == name {
				allowed = append(allowed, t)
			}
		}
	}

	evtJSON, _ := json.MarshalIndent(evt, "", "  ")
	out := einoengine.Run(ctx, einoengine.EngineInput{
		Model:   r.reasoner,
		System:  skill.Body + reportFormat,
		User:    "排查以下告警：\n" + string(evtJSON),
		Tools:   allowed,
		MaxIter: r.maxIter,
	})
	if out.Err != nil {
		return nil, out.Evidence, fmt.Errorf("agent: 排查执行失败: %w", out.Err)
	}
	if out.Content == "" {
		return nil, out.Evidence, fmt.Errorf("agent: 引擎未产出最终回答")
	}

	rep, parseErr := parseReport(out.Content)
	if parseErr != nil {
		// 解析失败兜底：原文进 summary，标记需人工
		rep = &coremodel.DiagnosisReport{
			Summary:    "（报告 JSON 解析失败，原文附后）" + truncate(out.Content, 2000),
			NeedsHuman: true,
			Unresolved: []string{"最终回答不是合法报告 JSON: " + parseErr.Error()},
		}
	}
	rep.AlertID = evt.ID
	rep.Refs = evt.Refs
	if skill != nil {
		rep.SkillID = skill.Name
		rep.SkillMatched = skill.Name != "generic"
	}
	if rep.Severity == "" {
		rep.Severity = evt.Severity
	}
	rep.Cost = coremodel.ReportCost{Steps: out.Steps, DurationMs: time.Since(start).Milliseconds()}
	if err := rep.Validate(); err != nil {
		return rep, out.Evidence, fmt.Errorf("agent: 报告未过契约校验: %w", err)
	}
	return rep, out.Evidence, nil
}

// parseReport 解析最终回答。容忍三种包装：```json 围栏、前后缀说明文字
// （实测 Qwen 会输出"### 结论 ### 输出 {…}"式结构）、裸 JSON。
func parseReport(content string) (*coremodel.DiagnosisReport, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx > 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	// 直接解析
	var rep coremodel.DiagnosisReport
	if err := json.Unmarshal([]byte(s), &rep); err == nil {
		return &rep, nil
	}
	// 从首个 { 起提取配平的 JSON 对象（跳过前后缀文字）
	if i := strings.Index(s, "{"); i >= 0 {
		if obj, ok := extractBalancedJSON(s[i:]); ok {
			rep = coremodel.DiagnosisReport{}
			if err := json.Unmarshal([]byte(obj), &rep); err == nil {
				return &rep, nil
			}
		}
	}
	return nil, fmt.Errorf("内容中未找到合法报告 JSON")
}

// extractBalancedJSON 提取首个配平的 {...}（处理字符串内的花括号与转义）。
func extractBalancedJSON(s string) (string, bool) {
	depth, inStr, esc := 0, false, false
	for i, r := range s {
		if esc {
			esc = false
			continue
		}
		switch r {
		case '\\':
			if inStr {
				esc = true
			}
		case '"':
			inStr = !inStr
		case '{':
			if !inStr {
				depth++
			}
		case '}':
			if !inStr {
				depth--
				if depth == 0 {
					return s[:i+1], true
				}
			}
		}
	}
	return "", false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
