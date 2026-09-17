package webhook

import (
	"context"
	"testing"
)

const amGolden = `{
  "version": "4",
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "HighErrorRate",
        "severity": "critical",
        "service": "order-api",
        "env": "prod"
      },
      "annotations": {
        "summary": "订单服务 5xx 飙升",
        "description": "5 分钟错误率超过 5%"
      },
      "startsAt": "2026-09-15T06:00:00.000Z",
      "endsAt": "0001-01-01T00:00:00Z",
      "generatorURL": "http://prom.example.com/graph"
    },
    {
      "status": "resolved",
      "labels": {"alertname": "DiskFull", "severity": "warning"},
      "annotations": {},
      "startsAt": "2026-09-15T05:00:00.000Z",
      "endsAt": "2026-09-15T06:00:00.000Z"
    }
  ]
}`

func TestAlertmanagerParse(t *testing.T) {
	events, err := (&AlertmanagerParser{}).Parse(context.Background(), []byte(amGolden))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("firing+resolved 都应产出，期望 2 条，实得 %d", len(events))
	}
	evt := events[0]
	if evt.Title != "订单服务 5xx 飙升" {
		t.Fatalf("title 应取 annotations.summary: %q", evt.Title)
	}
	if evt.Status != "firing" {
		t.Fatalf("默认状态应为 firing: %q", evt.Status)
	}
	if evt.Severity != "critical" {
		t.Fatalf("severity 映射错误: %q", evt.Severity)
	}
	if evt.Labels["service"] != "order-api" || evt.Labels["env"] != "prod" {
		t.Fatalf("labels 丢失: %v", evt.Labels)
	}
	if evt.Refs["generator_url"] != "http://prom.example.com/graph" {
		t.Fatalf("generatorURL 应进 refs: %v", evt.Refs)
	}
	if evt.Fingerprint == "" || evt.ID == "" {
		t.Fatal("ID/指纹未生成")
	}
	if err := evt.Validate(); err != nil {
		t.Fatalf("事件应通过校验: %v", err)
	}
	if evt.OccurredAt.Year() != 2026 {
		t.Fatalf("occurred_at 应来自 startsAt: %v", evt.OccurredAt)
	}
}

// resolved 事件：Status 标记 + 与同 labels 的 firing 指纹一致（取消关联的前提）。
func TestAlertmanagerResolvedFingerprintStable(t *testing.T) {
	body := `{"version":"4","alerts":[
		{"status":"firing","labels":{"alertname":"X","service":"s"},"startsAt":"2026-09-15T06:00:00Z"},
		{"status":"resolved","labels":{"alertname":"X","service":"s"},"startsAt":"2026-09-15T06:00:00Z","endsAt":"2026-09-15T06:05:00Z"}
	]}`
	events, err := (&AlertmanagerParser{}).Parse(context.Background(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("应产出 2 条: %d", len(events))
	}
	if events[0].Status != "firing" || events[1].Status != "resolved" {
		t.Fatalf("状态标记错误: %s / %s", events[0].Status, events[1].Status)
	}
	if events[0].Fingerprint != events[1].Fingerprint {
		t.Fatal("resolved 与 firing 指纹必须一致（取消关联依赖同指纹）")
	}
}

func TestSeverityNormalization(t *testing.T) {
	cases := map[string]string{
		"critical": "critical", "PAGE": "critical", "sev1": "critical",
		"warning": "warning", "Warn": "warning", "sev2": "warning",
		"info": "info", "": "info", "whatever": "warning",
	}
	for in, want := range cases {
		if got := string(mapSeverity(in)); got != want {
			t.Errorf("mapSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParserRegistry(t *testing.T) {
	if _, ok := LookupParser("alertmanager"); !ok {
		t.Fatal("alertmanager 解析器应已注册")
	}
	if _, ok := LookupParser("nope"); ok {
		t.Fatal("未注册解析器不应查到")
	}
}
