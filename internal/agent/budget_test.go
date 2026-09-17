package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/cnyup/alert-agent/internal/skills"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

// ---- A2: token 计量与预算熔断 ----

type usageFakeModel struct {
	turns  []string // 每轮返回内容
	usage  [][2]int // 每轮 (prompt, completion)
	calls  int
}

func (m *usageFakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	i := m.calls
	if i >= len(m.turns) {
		i = len(m.turns) - 1
	}
	m.calls++
	msg := &schema.Message{Role: schema.Assistant, Content: m.turns[i]}
	if i < len(m.usage) {
		msg.ResponseMeta = &schema.ResponseMeta{Usage: &schema.TokenUsage{
			PromptTokens:     m.usage[i][0],
			CompletionTokens: m.usage[i][1],
			TotalTokens:      m.usage[i][0] + m.usage[i][1],
		}}
	}
	return msg, nil
}

func (*usageFakeModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("unused")
}

func TestTokenCostAccumulated(t *testing.T) {
	report := `{"summary":"s","root_causes":[{"hypothesis":"h","confidence":0.9,"evidence":["T1"]}]}`
	fm := &usageFakeModel{turns: []string{report}, usage: [][2]int{{100, 20}}}
	evt, _ := coremodel.NewEvent("t", coremodel.SeverityWarning, "x", coremodel.Labels{"a": "b"}, time.Now(), nil, nil)
	r := New(fm, nil, 5, 0)
	rep, _, err := r.Diagnose(context.Background(), evt, &skills.Skill{Name: "generic", Body: "排查"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cost.TokensIn != 100 || rep.Cost.TokensOut != 20 {
		t.Fatalf("token 未累计: %+v", rep.Cost)
	}
}

func TestTokenBudgetExhausted(t *testing.T) {
	report := `{"summary":"s","root_causes":[{"hypothesis":"h","confidence":0.9,"evidence":["T1"]}]}`
	// 第一轮耗掉 900，预算 900 → 第二轮 Generate 前熔断（边界：已用量 >= 预算）
	fm := &usageFakeModel{turns: []string{report}, usage: [][2]int{{900, 0}}}
	tm := newTokenModel(fm, 900)
	if _, err := tm.Generate(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.Generate(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "预算") {
		t.Fatalf("应熔断: %v", err)
	}
	if in, out := tm.usage(); in != 900 || out != 0 {
		t.Fatalf("用量异常: %d/%d", in, out)
	}
	// maxTokens<=0 不限
	tm2 := newTokenModel(&usageFakeModel{turns: []string{report}, usage: [][2]int{{900, 0}}}, 0)
	for i := 0; i < 5; i++ {
		if _, err := tm2.Generate(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
}
