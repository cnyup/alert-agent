package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/cnyup/alert-agent/internal/skills"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

// ---- 脚本化假模型（与 spike 同法） ----

type turn struct {
	calls   []schema.ToolCall
	content string
}

type fakeModel struct {
	mu    sync.Mutex
	turns []turn
	next  int
}

func (m *fakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.next >= len(m.turns) {
		return nil, errors.New("脚本耗尽")
	}
	t := m.turns[m.next]
	m.next++
	if len(t.calls) > 0 {
		return &schema.Message{Role: schema.Assistant, ToolCalls: t.calls}, nil
	}
	return &schema.Message{Role: schema.Assistant, Content: t.content}, nil
}

func (*fakeModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, errors.New("未使用")
}

type fakeTool struct{ name string }

func (f *fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "test"}, nil
}
func (f *fakeTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	return fmt.Sprintf(`{"ok":true,"tool":%q}`, f.name), nil
}

func loadSkill(t *testing.T) *skills.Skill {
	t.Helper()
	idx, err := skills.Load("../../skills/examples")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := idx.Get("http-5xx-spike")
	if !ok {
		t.Fatal("示例剧本缺失")
	}
	return s
}

func TestDiagnoseHappyPath(t *testing.T) {
	fm := &fakeModel{turns: []turn{
		{calls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "prometheus_query", Arguments: `{"q":"5xx"}`}},
		}},
		{content: `{"summary":"v1.2.3 发布引入超时","severity":"critical",
			"root_causes":[{"hypothesis":"发布引入连接超时","confidence":0.8,"evidence":["T1"]}],
			"actions":[{"id":"A1","title":"回滚","risk":"mutating","tool":"k8s.rollout_undo"}],
			"needs_human":false}`},
	}}
	skill := loadSkill(t)
	skill = &skills.Skill{Name: skill.Name, Description: skill.Description,
		Triggers: skill.Triggers, Severity: skill.Severity,
		Tools: []string{"prometheus_query"}, Body: skill.Body}

	evt, _ := coremodel.NewEvent("webhook/alertmanager", coremodel.SeverityCritical,
		"订单服务 5xx 飙升", coremodel.Labels{"service": "order-api"}, time.Now(), nil, nil)

	r := New(fm, []tool.BaseTool{&fakeTool{name: "prometheus_query"}, &fakeTool{name: "k8s_get_pod"}}, 10)
	report, evidence, err := r.Diagnose(context.Background(), evt, skill)
	if err != nil {
		t.Fatalf("排查失败: %v", err)
	}
	if report.Summary != "v1.2.3 发布引入超时" {
		t.Fatalf("summary 不符: %q", report.Summary)
	}
	if report.AlertID != evt.ID || report.SkillID != skill.Name || !report.SkillMatched {
		t.Fatalf("报告关联字段缺失: %+v", report)
	}
	if len(evidence) != 1 || evidence[0].ID != "T1" || evidence[0].Tool != "prometheus_query" {
		t.Fatalf("证据链不符: %+v", evidence)
	}
	if report.RootCauses[0].Evidence[0] != "T1" {
		t.Fatal("根因应引用 T1")
	}
	if report.Cost.Steps == 0 || report.Cost.DurationMs < 0 {
		t.Fatalf("成本未记录: %+v", report.Cost)
	}
}

func TestDiagnoseBadJSONFallsBackToHuman(t *testing.T) {
	fm := &fakeModel{turns: []turn{{content: "这不是 JSON"}}}
	skill := loadSkill(t)
	evt, _ := coremodel.NewEvent("t", coremodel.SeverityWarning, "x",
		coremodel.Labels{"a": "b"}, time.Now(), nil, nil)
	r := New(fm, nil, 5)
	report, _, err := r.Diagnose(context.Background(), evt, skill)
	if err != nil {
		t.Fatalf("兜底路径不应报错: %v", err)
	}
	if !report.NeedsHuman {
		t.Fatal("解析失败应标记 needs_human")
	}
	if len(report.Unresolved) == 0 {
		t.Fatal("应记录未解之问")
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("兜底报告应通过校验: %v", err)
	}
}

func TestDiagnoseBudgetCap(t *testing.T) {
	turns := make([]turn, 20)
	for i := range turns {
		turns[i] = turn{calls: []schema.ToolCall{
			{ID: fmt.Sprintf("c%d", i), Function: schema.FunctionCall{Name: "prometheus_query", Arguments: "{}"}},
		}}
	}
	fm := &fakeModel{turns: turns}
	skill := loadSkill(t)
	evt, _ := coremodel.NewEvent("t", coremodel.SeverityCritical, "x", nil, time.Now(), nil, nil)
	r := New(fm, []tool.BaseTool{&fakeTool{name: "prometheus_query"}}, 3)
	_, _, err := r.Diagnose(context.Background(), evt, skill)
	if err == nil {
		t.Fatal("预算触顶应报错")
	}
}
