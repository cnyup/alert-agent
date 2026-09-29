package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	mu       sync.Mutex
	turns    []turn
	next     int
	firstUser string // 首轮收到的 user 消息原文（时间锚点断言用）
}

func (m *fakeModel) Generate(_ context.Context, msgs []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	if m.next >= len(m.turns) {
		m.mu.Unlock()
		return nil, errors.New("脚本耗尽")
	}
	if len(msgs) > 0 { // 记录首轮 user 消息供断言（时间锚点注入验证；ADK 可能前置 System/Instruction 消息）
		for _, msg := range msgs {
			if msg.Role == schema.User {
				m.firstUser = msg.Content
				break
			}
		}
	}
	t := m.turns[m.next]
	m.next++
	m.mu.Unlock()
	if len(t.calls) > 0 {
		return &schema.Message{Role: schema.Assistant, ToolCalls: t.calls}, nil
	}
	return &schema.Message{Role: schema.Assistant, Content: t.content}, nil
}

// TestDiagnoseInjectsTimeAnchor 验证首轮 user 消息注入当前时间锚点：
// 模型做 epoch 换算时有真实的「现在」可对照（防止年份差一年的系统性错误）。
func TestDiagnoseInjectsTimeAnchor(t *testing.T) {
	fm := &fakeModel{turns: []turn{
		{calls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "prometheus_query", Arguments: `{"q":"up"}`}},
		}},
		{content: `{"summary":"ok","severity":"warning",
			"root_causes":[{"hypothesis":"h","confidence":0.5,"evidence":["T1"]}],
			"needs_human":true}`},
	}}
	skill := loadSkill(t)
	skill = &skills.Skill{Name: skill.Name, Description: skill.Description,
		Triggers: skill.Triggers, Severity: skill.Severity,
		Tools: []string{"prometheus_query"}, Body: skill.Body}

	evt, _ := coremodel.NewEvent("webhook/alertmanager", coremodel.SeverityWarning,
		"测试告警", coremodel.Labels{"a": "b"}, time.Now(), nil, nil)

	r := New(fm, []tool.BaseTool{&fakeTool{name: "prometheus_query"}}, 4, 0)
	if _, _, err := r.Diagnose(context.Background(), evt, skill, ""); err != nil {
		t.Fatalf("排查失败: %v", err)
	}
	u := fm.firstUser
	if u == "" {
		t.Fatal("未捕获首轮 user 消息")
	}
	// 必须含北京时间和 UTC 的当前时间、epoch 秒与毫秒锚点
	now := time.Now()
	for _, want := range []string{
		"当前时间", now.Format("2006-01-02"),
	} {
		if !strings.Contains(u, want) {
			t.Errorf("userMsg 缺少 %q:\n%s", want, u)
		}
	}
	// epoch 毫秒锚点：注入值应与真实当前毫秒在同一分钟量级（Diagnose 时刻取的）
	if !strings.Contains(u, "epoch") {
		t.Error("userMsg 缺少 epoch 时间戳锚点")
	}
	anchorRe := regexp.MustCompile(`epoch 毫秒[：:]?\s*(\d{13})`)
	if m := anchorRe.FindStringSubmatch(u); m == nil {
		t.Error("userMsg 未找到 13 位 epoch 毫秒锚点")
	} else if got, _ := strconv.ParseInt(m[1], 10, 64); abs64(now.UnixMilli()-got) > 5*60*1000 {
		t.Errorf("epoch 毫秒锚点偏差过大: got %d, now %d", got, now.UnixMilli())
	}
}

func abs64(x int64) int64 { if x < 0 { return -x }; return x }

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

	r := New(fm, []tool.BaseTool{&fakeTool{name: "prometheus_query"}, &fakeTool{name: "k8s_get_pod"}}, 10, 0)
	report, evidence, err := r.Diagnose(context.Background(), evt, skill, "")
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
	r := New(fm, nil, 5, 0)
	report, _, err := r.Diagnose(context.Background(), evt, skill, "")
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
	r := New(fm, []tool.BaseTool{&fakeTool{name: "prometheus_query"}}, 3, 0)
	_, _, err := r.Diagnose(context.Background(), evt, skill, "")
	if err == nil {
		t.Fatal("预算触顶应报错")
	}
}

// 实测回归：Qwen 会把报告 JSON 包在 markdown 说明文字里
func TestParseReportProseWrapped(t *testing.T) {
	content := "### 结论\n根据以上排查结果，问题与发布时间吻合。\n\n### 输出\n{\n  \"summary\": \"发布引入\",\n  \"root_causes\": [{\"hypothesis\": \"h\", \"confidence\": 0.9, \"evidence\": [\"T1\"]}]\n}\n以上。"
	rep, err := parseReport(content)
	if err != nil {
		t.Fatalf("说明文字包裹的 JSON 应可解析: %v", err)
	}
	if rep.Summary != "发布引入" || len(rep.RootCauses) != 1 || rep.RootCauses[0].Evidence[0] != "T1" {
		t.Fatalf("解析结果不符: %+v", rep)
	}
	// 字符串内含花括号不干扰配平
	tricky := `前缀 {"summary": "a{b}c{\"k\":1}", "needs_human": false} 后缀}`
	rep2, err := parseReport(tricky)
	if err != nil || rep2.Summary != `a{b}c{"k":1}` {
		t.Fatalf("花括号转义场景失败: %v %+v", err, rep2)
	}
}

// --- Wave 0.3：token 预算 per-调查（PLAN-CASE-SUPERVISOR §5 Wave 0.3 / F5）---

// meteredFakeModel 每次调用固定报 usage（in=400, out=100，合计 500/次）。
type meteredFakeModel struct {
	fakeModel
}

func (m *meteredFakeModel) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	resp, err := m.fakeModel.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		resp.ResponseMeta = &schema.ResponseMeta{
			Usage: &schema.TokenUsage{PromptTokens: 400, CompletionTokens: 100},
		}
	}
	return resp, nil
}

// 连续两次 Diagnose：第二次的 Cost.TokensIn 必须独立计量（现状累计 → RED）。
// 单 Generate/次 的最简形态：第一次 400/100，第二次应仍是 400/100（现状 800/200）。
func TestDiagnoseTokenBudgetPerCall(t *testing.T) {
	one := turn{content: `{"summary":"ok","severity":"warning",
		"root_causes":[{"hypothesis":"h","confidence":0.9,"evidence":["T1"]}],
		"needs_human":false}`}
	skill := loadSkill(t)
	evt, _ := coremodel.NewEvent("t", coremodel.SeverityWarning, "x", nil, time.Now(), nil, nil)
	r := New(&meteredFakeModel{fakeModel{turns: []turn{one, one}}}, nil, 5, 0)

	rep1, _, err := r.Diagnose(context.Background(), evt, skill, "")
	if err != nil {
		t.Fatalf("第一次排查失败: %v", err)
	}
	if rep1.Cost.TokensIn != 400 {
		t.Fatalf("第一次计量应 TokensIn=400，实际 %d", rep1.Cost.TokensIn)
	}
	// 同一 Runner（单例）连续第二次排查，消耗第二个 turn
	rep2, _, err := r.Diagnose(context.Background(), evt, skill, "")
	if err != nil {
		t.Fatalf("第二次排查失败: %v", err)
	}
	if rep2.Cost.TokensIn != 400 {
		t.Fatalf("第二次调查应独立计量 TokensIn=400，实际 %d（跨排查累计缺陷 F5）", rep2.Cost.TokensIn)
	}
	if rep2.Cost.TokensOut != 100 {
		t.Fatalf("TokensOut 应独立计量=100，实际 %d", rep2.Cost.TokensOut)
	}
}

// maxTokens=600：单次调查内两次 Generate 各报 400/100（每次 advance 500，
// 累计 500→1000）：第二次 Generate 前累计未超 600 可调用，调用后累计 1000。
// 第二次排查（新 turn）在 per-call 预算下应正常完成；缺陷实现下开局累计
// 已 1000≥600 直接熔断 → RED。
func TestDiagnoseTokenBudgetIndependentCap(t *testing.T) {
	toolTurn := turn{calls: []schema.ToolCall{
		{ID: "c1", Function: schema.FunctionCall{Name: "prometheus_query", Arguments: "{}"}},
	}}
	finalTurn := turn{content: `{"summary":"ok","severity":"warning",
		"root_causes":[{"hypothesis":"h","confidence":0.9,"evidence":["T1"]}],
		"needs_human":false}`}
	skill := loadSkill(t)
	evt, _ := coremodel.NewEvent("t", coremodel.SeverityWarning, "x", nil, time.Now(), nil, nil)
	r := New(&meteredFakeModel{fakeModel{turns: []turn{toolTurn, finalTurn, toolTurn, finalTurn}}},
		[]tool.BaseTool{&fakeTool{name: "prometheus_query"}}, 5, 600)

	rep1, _, err := r.Diagnose(context.Background(), evt, skill, "")
	if err != nil {
		t.Fatalf("第一次排查失败: %v", err)
	}
	if rep1.Cost.TokensIn != 800 || rep1.Cost.TokensOut != 200 {
		t.Fatalf("第一次计量应 800/200（两次 Generate），实际 %d/%d", rep1.Cost.TokensIn, rep1.Cost.TokensOut)
	}
	// 第二次排查：per-call 预算下从 0 重新计量，应正常完成
	rep2, _, err := r.Diagnose(context.Background(), evt, skill, "")
	if err != nil {
		t.Fatalf("第二次排查失败（预算被第一次跨排查占用，F5）: %v", err)
	}
	if rep2.Cost.TokensIn != 800 {
		t.Fatalf("第二次独立预算下应正常计量 TokensIn=800，实际 %d", rep2.Cost.TokensIn)
	}
}
