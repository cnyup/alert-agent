package parsers

import (
	"context"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
)

func TestGrafanaAlertsArray(t *testing.T) {
	body := []byte(`{
	  "title": "[Alerting] CPU high",
	  "ruleId": 42,
	  "ruleName": "CPU usage",
	  "state": "alerting",
	  "alerts": [
	    {"status": "firing", "labels": {"severity": "critical", "host": "node-1"},
	     "annotations": {"summary": "CPU 92%"}, "startsAt": "2026-09-18T06:00:00Z",
	     "generatorURL": "http://g.example.com/a/42", "valueString": "[var=92]"},
	    {"status": "resolved", "labels": {"severity": "warning", "host": "node-2"},
	     "annotations": {}, "startsAt": "2026-09-18T05:00:00Z", "endsAt": "2026-09-18T06:01:00Z"}
	  ]
	}`)
	events, err := (&GrafanaParser{}).Parse(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("应产出 2 条: %d", len(events))
	}
	e1 := events[0]
	if e1.Title != "[Alerting] CPU high" || e1.Severity != model.SeverityCritical {
		t.Fatalf("e1 异常: %q %s", e1.Title, e1.Severity)
	}
	if e1.Labels["host"] != "node-1" || e1.Labels["rule_name"] != "CPU usage" || e1.Labels["rule_id"] != "42" {
		t.Fatalf("e1 labels 异常: %v", e1.Labels)
	}
	if e1.Status != model.StatusFiring || e1.Refs["generator_url"] == "" {
		t.Fatalf("e1 状态/refs 异常: %+v", e1.Refs)
	}
	if events[1].Status != model.StatusResolved {
		t.Fatalf("e2 应为 resolved: %s", events[1].Status)
	}
	// 指纹一致性（resolved 取消依赖）
	if e1.Fingerprint == events[1].Fingerprint {
		t.Fatal("不同 labels 的事件指纹不应相同")
	}
}

func TestGrafanaTopLevelSingle(t *testing.T) {
	body := []byte(`{"title":"磁盘告警","state":"ok","ruleName":"disk",
		"labels":{"severity":"warning"},"startsAt":"2026-09-18T06:00:00Z"}`)
	events, err := (&GrafanaParser{}).Parse(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Status != model.StatusResolved || events[0].Title != "磁盘告警" {
		t.Fatalf("顶层形态异常: %+v", events[0])
	}
}

func TestJMESPathParser(t *testing.T) {
	proto := &JMESPathPrototype{}
	inst, err := proto.Configure(map[string]any{
		"title":        "alerts[0].name",
		"description":  "alerts[0].msg",
		"severity":     "alerts[0].level",
		"occurred_at":  "alerts[0].ts",
		"labels":       map[string]any{"service": "alerts[0].svc", "env": "alerts[0].env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"alerts":[{"name":"订单服务错误率飙升","msg":"5xx 6%","level":"critical",
		"ts":"2026-09-18T06:00:00Z","svc":"order-api","env":"prod"}]}`)
	events, err := inst.Parse(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	e := events[0]
	if e.Title != "订单服务错误率飙升" || e.Severity != model.SeverityCritical {
		t.Fatalf("投影异常: %q %s", e.Title, e.Severity)
	}
	if e.Description != "5xx 6%" || e.Labels["service"] != "order-api" {
		t.Fatalf("字段异常: %q %v", e.Description, e.Labels)
	}
	if !e.OccurredAt.Equal(time.Date(2026, 9, 18, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("时间投影异常: %v", e.OccurredAt)
	}
	// 缺 title 表达式构造失败
	if _, err := proto.Configure(map[string]any{"title": ""}); err == nil {
		t.Fatal("空 title 应失败")
	}
	// 烂表达式
	if _, err := proto.Configure(map[string]any{"title": "a["}); err == nil {
		t.Fatal("烂表达式应在构造期失败")
	}
}

func TestRegexParser(t *testing.T) {
	proto := &RegexPrototype{}
	inst, err := proto.Configure(map[string]any{
		"pattern": `(?P<title>[\w\s]+) on (?P<host>\S+): (?P<description>.+)`,
		"labels":  []string{"host"},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := inst.Parse(context.Background(),
		[]byte("HighErrorRate on node-1: error rate 6% exceeded"))
	if err != nil {
		t.Fatal(err)
	}
	e := events[0]
	if e.Title != "HighErrorRate" || e.Labels["host"] != "node-1" {
		t.Fatalf("捕获组映射异常: %q %v", e.Title, e.Labels)
	}
	if e.Description != "error rate 6% exceeded" {
		t.Fatalf("描述异常: %q", e.Description)
	}
	// 不匹配报文
	if _, err := inst.Parse(context.Background(), []byte("完全无关文本")); err == nil {
		t.Fatal("不匹配应报错")
	}
	// 缺 title 捕获组的 pattern 拒绝
	if _, err := proto.Configure(map[string]any{"pattern": `(?P<x>.+)`}); err == nil {
		t.Fatal("缺 title 组应拒绝")
	}
}

// 数字/时间投影的边界。
func TestJMESPathScalars(t *testing.T) {
	inst, err := (&JMESPathPrototype{}).Configure(map[string]any{"title": "name", "occurred_at": "ms_ts"})
	if err != nil {
		t.Fatal(err)
	}
	events, err := inst.Parse(context.Background(), []byte(`{"name":"x","ms_ts":1789699200000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !events[0].OccurredAt.Equal(time.UnixMilli(1789699200000)) {
		t.Fatalf("毫秒时间戳解析异常: %v", events[0].OccurredAt)
	}
}
