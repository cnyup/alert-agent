package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func mkEvent(t *testing.T, labels model.Labels) *model.AlertEvent {
	t.Helper()
	evt, err := model.NewEvent("test", model.SeverityWarning, "t", labels, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return evt
}

func TestRunnerContinueSkipDropSemantics(t *testing.T) {
	RegisterTestStages(t)
	r, err := New([]StageSpec{
		{Name: "test-continue"},
		{Name: "test-skip"},
		{Name: "test-err"},
		{Name: "test-drop"},
		{Name: "test-continue"}, // Drop 之后不应到达
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Run(context.Background(), mkEvent(t, model.Labels{"a": "1"}))
	if !res.Dropped || res.DropBy != "test-drop" {
		t.Fatalf("应在 test-drop 终止: %+v", res)
	}
	// continue/skip/err/drop 四个阶段各有日志
	if len(res.StageLog) != 4 {
		t.Fatalf("应有 4 条阶段日志，实得 %d: %+v", len(res.StageLog), res.StageLog)
	}
	if res.StageLog[2].Err == "" {
		t.Fatal("test-err 的错误应记录在日志")
	}
}

func TestDedupWindow(t *testing.T) {
	r, err := New([]StageSpec{{Name: "dedup", Options: map[string]any{"window": "100ms"}}})
	if err != nil {
		t.Fatal(err)
	}
	evt := mkEvent(t, model.Labels{"service": "x"})
	if res := r.Run(context.Background(), evt); res.Dropped {
		t.Fatal("首次不应去重")
	}
	if res := r.Run(context.Background(), mkEvent(t, model.Labels{"service": "x"})); !res.Dropped {
		t.Fatal("窗口内同指纹应去重")
	}
	time.Sleep(120 * time.Millisecond)
	if res := r.Run(context.Background(), mkEvent(t, model.Labels{"service": "x"})); res.Dropped {
		t.Fatal("窗口过期后应放行")
	}
	if _, err := New([]StageSpec{{Name: "dedup", Options: map[string]any{"window": "abc"}}}); err == nil {
		t.Fatal("非法 window 应装配失败")
	}
}

func TestSilenceMatch(t *testing.T) {
	r, err := New([]StageSpec{{Name: "silence", Options: map[string]any{"match": "env=staging, service=foo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res := r.Run(context.Background(), mkEvent(t, model.Labels{"env": "staging", "service": "foo"})); !res.Dropped {
		t.Fatal("全部匹配应静默")
	}
	if res := r.Run(context.Background(), mkEvent(t, model.Labels{"env": "staging", "service": "bar"})); res.Dropped {
		t.Fatal("部分匹配不应静默")
	}
}

func TestRouteRules(t *testing.T) {
	r, err := New([]StageSpec{{Name: "route", Options: map[string]any{"rules": []any{
		map[string]any{
			"name":     "order-prod",
			"match":    map[string]any{"service": "order-api", "env": "prod"},
			"severity": "critical",
			"skills":   []any{"http-5xx-spike", "k8s-crashloop"},
			"notifiers": []any{"feishu-card"},
		},
	}}}})
	if err != nil {
		t.Fatal(err)
	}

	res := r.Run(context.Background(), mkEvent(t, model.Labels{"service": "order-api", "env": "prod"}))
	if res.Route == nil || res.Route.Matched != "order-prod" {
		t.Fatalf("应命中规则: %+v", res.Route)
	}
	if res.Event.Severity != model.SeverityCritical {
		t.Fatal("定级应被规则覆盖为 critical")
	}
	if len(res.Route.Skills) != 2 {
		t.Fatalf("候选剧本不符: %v", res.Route.Skills)
	}

	res2 := r.Run(context.Background(), mkEvent(t, model.Labels{"service": "other"}))
	if res2.Route != nil {
		t.Fatal("未命中应无路由结果（内核走通用兜底）")
	}

	// 非法 severity 应装配失败
	if _, err := New([]StageSpec{{Name: "route", Options: map[string]any{"rules": []any{
		map[string]any{"name": "bad", "match": map[string]any{"a": "b"}, "severity": "sev9"},
	}}}}); err == nil {
		t.Fatal("非法 severity 应装配失败")
	}
}

// ---- 测试用 stage（工厂注册） ----

func RegisterTestStages(t *testing.T) {
	t.Helper()
	plugin.RegisterStageFactory("test-continue", func(map[string]any) (plugin.Stage, error) {
		return contStage{}, nil
	})
	plugin.RegisterStageFactory("test-skip", func(map[string]any) (plugin.Stage, error) {
		return skipStage{}, nil
	})
	plugin.RegisterStageFactory("test-err", func(map[string]any) (plugin.Stage, error) {
		return errStage{}, nil
	})
	plugin.RegisterStageFactory("test-drop", func(map[string]any) (plugin.Stage, error) {
		return dropStage{}, nil
	})
}

type contStage struct{}

func (contStage) Name() string { return "test-continue" }
func (contStage) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	return evt, plugin.ActionContinue, nil
}

type skipStage struct{}

func (skipStage) Name() string { return "test-skip" }
func (skipStage) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	return evt, plugin.ActionSkip, nil
}

type errStage struct{}

func (errStage) Name() string { return "test-err" }
func (errStage) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	return evt, plugin.ActionContinue, errors.New("boom")
}

type dropStage struct{}

func (dropStage) Name() string { return "test-drop" }
func (dropStage) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	return evt, plugin.ActionDrop, nil
}

func TestAggregateIncident(t *testing.T) {
	r, err := New([]StageSpec{{Name: "aggregate", Options: map[string]any{"window": "200ms", "escalate_at": 3}}})
	if err != nil {
		t.Fatal(err)
	}
	fp := model.Labels{"service": "agg-svc"}

	// 首条：通过，incident 建立
	res := r.Run(context.Background(), mkEvent(t, fp))
	if res.Dropped || res.Aggregate == nil || res.Aggregate.Occurrences != 1 {
		t.Fatalf("首条应通过且计数 1: %+v", res.Aggregate)
	}
	incID := res.Aggregate.IncidentID

	// 第 2 条：合并丢弃，同 incident
	res2 := r.Run(context.Background(), mkEvent(t, fp))
	if !res2.Dropped || res2.DropBy != "aggregate" || res2.Aggregate.IncidentID != incID || res2.Aggregate.Occurrences != 2 {
		t.Fatalf("第 2 条应合并: dropped=%v agg=%+v", res2.Dropped, res2.Aggregate)
	}

	// 第 3 条：达到阈值升级放行，severity 提升 critical
	res3 := r.Run(context.Background(), mkEvent(t, fp))
	if res3.Dropped || !res3.Aggregate.Escalated {
		t.Fatalf("阈值应升级放行: %+v", res3.Aggregate)
	}
	if res3.Event.Severity != model.SeverityCritical {
		t.Fatalf("升级后等级应为 critical: %s", res3.Event.Severity)
	}

	// 第 4 条：继续合并（不再重复升级）
	if res4 := r.Run(context.Background(), mkEvent(t, fp)); !res4.Dropped {
		t.Fatal("超过阈值后应继续合并")
	}

	// 不同指纹互不影响
	resOther := r.Run(context.Background(), mkEvent(t, model.Labels{"service": "other"}))
	if resOther.Dropped || resOther.Aggregate.Occurrences != 1 {
		t.Fatal("不同指纹应独立成 incident")
	}

	// 窗口过期重置
	time.Sleep(220 * time.Millisecond)
	resNew := r.Run(context.Background(), mkEvent(t, fp))
	if resNew.Dropped || resNew.Aggregate.Occurrences != 1 || resNew.Aggregate.IncidentID == incID {
		t.Fatal("窗口过期应开新 incident")
	}
}
