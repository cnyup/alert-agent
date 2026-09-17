package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/cnyup/alert-agent/internal/skills"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

type routerFakeModel struct {
	content string
	err     error
	calls   int
}

func (m *routerFakeModel) Generate(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return &schema.Message{Role: schema.Assistant, Content: m.content}, nil
}

func (*routerFakeModel) Stream(context.Context, []*schema.Message, ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	panic("unused")
}

func writeSkill(t *testing.T, dir, name, desc, keywords string) {
	t.Helper()
	body := "---\nname: " + name + "\ndescription: " + desc + "\n"
	if keywords != "" {
		body += "triggers:\n  keywords: [" + keywords + "]\n"
	}
	body += "---\n\n# " + name + "\n"
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkRouterIdx(t *testing.T) *skills.Index {
	t.Helper()
	dir := t.TempDir()
	// 两个可路由业务剧本 + 一个无 triggers 的知识技能（不应进候选）
	writeSkill(t, dir, "http-5xx-spike", "HTTP 5xx 激增排查", "5xx")
	writeSkill(t, dir, "workflow-failed", "短剧工作流失败排查", "工作流失败")
	writeSkill(t, dir, "db_info", "库表知识，供业务剧本引用", "")
	idx, err := skills.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func mkRouterEvt(t *testing.T, title string) *coremodel.AlertEvent {
	t.Helper()
	evt, err := coremodel.NewEvent("test", coremodel.SeverityWarning, title,
		coremodel.Labels{"service": "svc"}, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return evt
}

func TestSemanticRouterSelect(t *testing.T) {
	idx := mkRouterIdx(t)
	evt := mkRouterEvt(t, "出海短剧批量转码失败告警")

	// 正常选中
	m := &routerFakeModel{content: `{"skill": "workflow-failed", "reason": "短剧工作流失败"}`}
	got := NewSemanticRouter(m, idx).Select(context.Background(), evt)
	if got == nil || got.Name != "workflow-failed" {
		t.Fatalf("应选中 workflow-failed: %+v", got)
	}
	if m.calls != 1 {
		t.Fatalf("应恰好一次模型调用: %d", m.calls)
	}

	// 前后缀噪声容忍
	m2 := &routerFakeModel{content: "### 结论\n{\"skill\": \"http-5xx-spike\", \"reason\": \"网关 5xx\"}\n以上"}
	got2 := NewSemanticRouter(m2, idx).Select(context.Background(), evt)
	if got2 == nil || got2.Name != "http-5xx-spike" {
		t.Fatalf("带前后缀应解析成功: %+v", got2)
	}

	// none / 清单外 / 空串 / 烂 JSON → nil（回落 generic）
	for _, c := range []string{
		`{"skill": "none", "reason": "不相关"}`,
		`{"skill": "db_info", "reason": "知识技能不在候选"}`,
		`{"skill": "不存在的剧本", "reason": "x"}`,
		`{"skill": ""}`,
		`这不是 JSON`,
	} {
		if s := NewSemanticRouter(&routerFakeModel{content: c}, idx).Select(context.Background(), evt); s != nil {
			t.Fatalf("输出 %q 应回落 nil，却返回 %v", c, s.Name)
		}
	}

	// 模型调用失败 → nil
	if s := NewSemanticRouter(&routerFakeModel{err: errors.New("boom")}, idx).Select(context.Background(), evt); s != nil {
		t.Fatalf("调用失败应回落 nil")
	}
}

// 无可路由候选（全部技能无 triggers）时不应调用模型。
func TestSemanticRouterNoCandidates(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "db_info", "知识技能", "")
	idx, err := skills.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &routerFakeModel{content: `{"skill": "db_info"}`}
	if s := NewSemanticRouter(m, idx).Select(context.Background(), mkRouterEvt(t, "x")); s != nil || m.calls != 0 {
		t.Fatalf("无候选不应调模型: s=%v calls=%d", s, m.calls)
	}
}
