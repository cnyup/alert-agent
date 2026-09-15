package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
)

func TestEventAndTraceRoundtrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	evt, err := model.NewEvent("webhook/alertmanager", model.SeverityCritical, "订单服务 5xx 飙升",
		model.Labels{"service": "order-api", "env": "prod"}, time.Now(),
		[]byte(`{"raw":true}`), map[string]string{"generator_url": "http://x"})
	if err != nil {
		t.Fatal(err)
	}
	evt.Description = "5 分钟错误率超 5%"

	if err := s.SaveEvent(ctx, evt); err != nil {
		t.Fatal(err)
	}
	// 幂等：同 ID 再存不报错
	if err := s.SaveEvent(ctx, evt); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetEvent(ctx, evt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != evt.Title || got.Severity != evt.Severity || got.Fingerprint != evt.Fingerprint {
		t.Fatalf("事件字段往返不一致: %+v", got)
	}
	if got.Labels["service"] != "order-api" || got.Refs["generator_url"] != "http://x" {
		t.Fatalf("labels/refs 往返丢失: %v %v", got.Labels, got.Refs)
	}
	if got.Description != evt.Description {
		t.Fatalf("description 不一致: %q", got.Description)
	}

	// trace 写入与按序读回
	entries := []TraceEntry{
		{Seq: 0, ID: "S1", Kind: "stage", Name: "dedup", Output: "continue"},
		{Seq: 1, ID: "T1", Kind: "tool", Name: "prometheus_query", Input: `{"q":"up"}`, Output: `{"v":1}`},
		{Seq: 2, ID: "T2", Kind: "model", Name: "reasoner", Output: "下一步查日志"},
	}
	for _, e := range entries {
		if err := s.AppendTrace(ctx, evt.ID, e); err != nil {
			t.Fatal(err)
		}
	}
	trace, err := s.Trace(ctx, evt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace) != 3 || trace[0].ID != "S1" || trace[2].ID != "T2" {
		t.Fatalf("trace 往返异常: %+v", trace)
	}
	if trace[1].Input != `{"q":"up"}` {
		t.Fatalf("工具入参丢失: %+v", trace[1])
	}

	if _, err := s.GetEvent(ctx, "evt_missing"); err == nil {
		t.Fatal("不存在的事件应报错")
	}
}
