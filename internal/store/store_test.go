package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
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

// --- Wave 1：jobs 持久队列（PLAN-CASE-SUPERVISOR §5 Wave 1.3）---

func TestJobsEnqueueDedup(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id1, err := s.EnqueueJob(ctx, "webhook_event", `{"a":1}`, "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	// 同 dedupKey 重复入队：幂等，不报错不重复
	id2, err := s.EnqueueJob(ctx, "webhook_event", `{"a":2}`, "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.QueueDepth(ctx); n != 1 {
		t.Fatalf("dedupKey 重复入队应仅 1 行，实际 %d", n)
	}
	// ON CONFLICT DO NOTHING 下 id2 应为 0（未插入）——但不同 dedupKey 正常插入
	id3, err := s.EnqueueJob(ctx, "webhook_event", `{"a":3}`, "fp-2")
	if err != nil || id3 == 0 {
		t.Fatalf("不同 dedupKey 应正常插入: %v %d", err, id3)
	}
	if id1 == id2 && id1 == 0 {
		t.Fatal("首次入队应返回真实 ID")
	}
}

func TestJobsConcurrentClaimNoDuplicate(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 3 条 job，8 个 worker 并发抢
	for i := 0; i < 3; i++ {
		if _, err := s.EnqueueJob(ctx, "webhook_event", fmt.Sprintf(`{"i":%d}`, i), fmt.Sprintf("fp-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	claimed := map[int64]string{} // jobID → worker
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				j, err := s.ClaimNextJob(ctx, fmt.Sprintf("w%d", w), []string{"webhook_event"})
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				if prev, ok := claimed[j.ID]; ok {
					t.Errorf("job %d 被重复领取: %s 与 %s", j.ID, prev, j.ClaimedBy)
				}
				claimed[j.ID] = j.ClaimedBy
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	if len(claimed) != 3 {
		t.Fatalf("3 条 job 应各被领取一次，实际 %d 条被领", len(claimed))
	}
}

func TestJobsStaleReclaimAndLateFinishRejected(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 入队并领取（模拟 worker 崩溃：claim 后不 finish）
	if _, err := s.EnqueueJob(ctx, "approval_exec", `{}`, "fp-1"); err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimNextJob(ctx, "w-crashed", nil)
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", err, j)
	}
	// stale 回收：阈值 1ms + 显式越过（SQLite TIMESTAMP 往返精度可能截断到
	// 毫秒以下，同 tick 时 claimed_at < cutoff 不成立——快机器上稳定复现）
	time.Sleep(5 * time.Millisecond)
	ids, err := s.ReclaimStaleJobs(ctx, time.Millisecond, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("应回收 1 条，实际 %v", ids)
	}
	// 原 worker 迟到的 FinishJob 必须被拒（状态已非 running）
	if err := s.FinishJob(ctx, j.ID, "done"); err != nil {
		t.Fatalf("FinishJob 应静默成功（RowsAffected=0），报错则语义不符: %v", err)
	}
	// 验证状态仍是 pending（迟到 done 未生效）
	j2, err := s.ClaimNextJob(ctx, "w-recovery", nil)
	if err != nil || j2 == nil || j2.ID != j.ID {
		t.Fatalf("回收后应可重新领取: %v %v", err, j2)
	}
	if j2.Attempts != 2 {
		t.Fatalf("attempts 应累加到 2，实际 %d", j2.Attempts)
	}
	// 超限后置 failed（显式越过阈值，理由同上）
	time.Sleep(5 * time.Millisecond)
	if _, err := s.ReclaimStaleJobs(ctx, time.Millisecond, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, j2.ID, "done"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	ids2, err := s.ReclaimStaleJobs(ctx, time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = ids2
	// attempts=2 >= maxAttempts=1 → failed，不再可领
	j3, err := s.ClaimNextJob(ctx, "w-3", nil)
	if err != nil || j3 != nil {
		t.Fatalf("超限后不应再可领取: %v %v", err, j3)
	}
}
