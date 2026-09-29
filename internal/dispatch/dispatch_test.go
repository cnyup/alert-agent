package dispatch

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/internal/store"
)

// 池基础语义：入队 → Kick → worker 领取执行 → done。
func TestPoolRunsJob(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var ran int32
	p := New(st, Options{Workers: 2, PollEvery: 20 * time.Millisecond})
	p.Register("test", func(_ context.Context, j *store.Job) error {
		if j.Kind != "test" || j.Payload != `{"x":1}` {
			t.Errorf("job 内容不符: %+v", j)
		}
		atomic.AddInt32(&ran, 1)
		return nil
	})
	runCtx, cancel := context.WithCancel(ctx)
	p.Start(runCtx)
	defer func() { cancel(); p.Stop() }()

	if _, err := st.EnqueueJob(ctx, "test", `{"x":1}`, "dk-1"); err != nil {
		t.Fatal(err)
	}
	p.Kick()

	deadline := time.After(3 * time.Second)
	for atomic.LoadInt32(&ran) == 0 {
		select {
		case <-deadline:
			t.Fatal("job 未在期限内执行")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// 无 handler 的 job fail-closed：置 failed 不静默丢。
func TestPoolNoHandlerFails(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	p := New(st, Options{Workers: 1, PollEvery: 20 * time.Millisecond})
	p.Register("known", func(context.Context, *store.Job) error { return nil })
	runCtx, cancel := context.WithCancel(ctx)
	p.Start(runCtx)
	defer func() { cancel(); p.Stop() }()

	if _, err := st.EnqueueJob(ctx, "unknown-kind", "{}", "dk-2"); err != nil {
		t.Fatal(err)
	}
	p.Kick()
	deadline := time.After(3 * time.Second)
	for {
		n, _ := st.QueueDepth(ctx)
		if n == 0 { // 已被领取处理
			break
		}
		select {
		case <-deadline:
			t.Fatal("job 未被领取")
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(100 * time.Millisecond) // 等终态落库
	// 无 handler 的 job 应已置 failed，不再可领
	j, err := st.ClaimNextJob(ctx, "verify", nil)
	if err != nil {
		t.Fatal(err)
	}
	if j != nil {
		t.Fatalf("无 handler 的 job 不应残留可领: %+v", j)
	}
}

// 并发多 job 全部执行，无遗漏。
func TestPoolDrainsBacklog(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var mu sync.Mutex
	done := map[int64]bool{}
	p := New(st, Options{Workers: 3, PollEvery: 20 * time.Millisecond})
	p.Register("bulk", func(_ context.Context, j *store.Job) error {
		mu.Lock()
		done[j.ID] = true
		mu.Unlock()
		return nil
	})
	runCtx, cancel := context.WithCancel(ctx)
	p.Start(runCtx)
	defer func() { cancel(); p.Stop() }()

	const N = 10
	for i := 0; i < N; i++ {
		if _, err := st.EnqueueJob(ctx, "bulk", "{}", ""); err != nil {
			t.Fatal(err)
		}
	}
	p.Kick()
	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(done)
		mu.Unlock()
		if n == N {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("积压未清空: %d/%d", n, N)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
