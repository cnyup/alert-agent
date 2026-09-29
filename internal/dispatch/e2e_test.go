// e2e Wave 2.5：不 mock 中间层——httptest 起 webhook → 真实 SQLite →
// 真实 dispatch 池 → 仅 Investigator 层 fake（eino 不进测试）。
// 固化 F6 回归：HTTP handler 返回（req ctx 已取消）后，调查仍完整执行。
package dispatch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/internal/dispatch"
	"github.com/cnyup/alert-agent/internal/store"
	_ "github.com/cnyup/alert-agent/internal/webhook" // 注册 webhook 源与解析器（init）
	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

// alertmanager 形态报文（用内置 alertmanager 解析器，零 mock）。
const amPayload = `{"version": "4", "status": "firing", "alerts": [{
  "status": "firing",
  "labels": {"alertname": "HighErrorRate", "service": "order-api", "severity": "warning"},
  "annotations": {"summary": "5xx 飙升"},
  "startsAt": "2026-09-30T08:00:00Z",
  "generatorURL": "http://am.example.com"
}]}`

// TestWebhookEnqueueSurvivesClientDisconnect F6 回归固化：
// handler 返回后立刻取消 req ctx（模拟客户端断连），入队的事件仍被
// dispatch 池完整处理（handleEvent 执行到标记完成）。
func TestWebhookEnqueueSurvivesClientDisconnect(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// “调查完成”观测点：handleEvent 的替代品（本测试只验 dispatch 链路，
	// 不启真实 eino——调查本身属 Wave 3 Investigator 抽象后的 fake 注入点）
	var handled int32
	pool := dispatch.New(st, dispatch.Options{Workers: 2, PollEvery: 20 * time.Millisecond})
	pool.Register("webhook_event", func(_ context.Context, j *store.Job) error {
		var evt model.AlertEvent
		if err := json.Unmarshal([]byte(j.Payload), &evt); err != nil {
			return err
		}
		if evt.Title == "" {
			return fmt.Errorf("事件标题丢失")
		}
		atomic.AddInt32(&handled, 1)
		return nil
	})
	runCtx, cancel := context.WithCancel(ctx)
	pool.Start(runCtx)
	defer func() { cancel(); pool.Stop() }()

	// 装配 webhook 源（真实解析器注册表）+ emit=入队
	emit := func(_ context.Context, evt *model.AlertEvent) error {
		payload, err := json.Marshal(evt)
		if err != nil {
			return err
		}
		if _, err := st.EnqueueJob(ctx, "webhook_event", string(payload), evt.ID); err != nil {
			return err
		}
		pool.Kick()
		return nil
	}
	src, err := plugin.NewSource("webhook", map[string]any{
		"path": "/hooks/am", "parse": "alertmanager",
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if m, ok := src.(interface{ SetMux(*http.ServeMux) }); ok {
		m.SetMux(mux)
	}
	srcCtx, srcCancel := context.WithCancel(ctx)
	defer srcCancel()
	go src.Start(srcCtx, emit)
	// 等 handler 挂载完成（Start 内 mux.HandleFunc 后才可服务）
	time.Sleep(200 * time.Millisecond)

	// 请求 → 200 → 立刻取消请求 ctx（客户端断连语义）
	reqCtx, reqCancel := context.WithCancel(ctx)
	req := httptest.NewRequest(http.MethodPost, "/hooks/am", strings.NewReader(amPayload))
	req = req.WithContext(reqCtx)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("入队成功应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	reqCancel() // ← 客户端断连：F6 场景的核心动作

	// 断言：调查（handler）仍完成
	deadline := time.After(5 * time.Second)
	for atomic.LoadInt32(&handled) == 0 {
		select {
		case <-deadline:
			t.Fatal("req ctx 取消后事件未被处理（F6 回归：排查应存活）")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 队列终态 done
	time.Sleep(100 * time.Millisecond)
	if n, _ := st.QueueDepth(ctx); n != 0 {
		t.Fatalf("job 应已完成，残留 pending %d", n)
	}
}

// TestWebhookQueueFull503 深度超限返回 503（不静默丢）。
func TestWebhookQueueFull503(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "e2e2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 不启 dispatch 池——job 堆积以触发深度超限
	src, err := plugin.NewSource("webhook", map[string]any{
		"path": "/hooks/am", "parse": "alertmanager",
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if m, ok := src.(interface{ SetMux(*http.ServeMux) }); ok {
		m.SetMux(mux)
	}
	srcCtx, srcCancel := context.WithCancel(ctx)
	defer srcCancel()
	emit := func(c context.Context, evt *model.AlertEvent) error {
		payload, _ := json.Marshal(evt)
		_, err := st.EnqueueJobBounded(c, "webhook_event", string(payload), evt.ID, 1)
		return err
	}
	go src.Start(srcCtx, emit)
	time.Sleep(200 * time.Millisecond) // 等 handler 挂载

	// 第一条：占住深度 1
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/hooks/am", strings.NewReader(amPayload))
	mux.ServeHTTP(rec1, req1)
	// 第二条：深度 ≥1 → ErrQueueFull → 503
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/hooks/am", strings.NewReader(amPayload))
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("队列打满应 503，实际 %d: %s", rec2.Code, rec2.Body.String())
	}
}
