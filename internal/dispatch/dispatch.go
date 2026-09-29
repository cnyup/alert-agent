// dispatch 持久队列 worker 池（PLAN-CASE-SUPERVISOR Wave 2）：
// SQLite jobs 表是真源，channel 仅作唤醒信号（容量 1 非阻塞发）；
// worker 从队列 claim 后执行 handler。入口异步化的根基——HTTP handler
// 入队即返回 200，分钟级排查在 worker ctx 内跑，与 req ctx 彻底解耦（F6）。
package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/cnyup/alert-agent/internal/store"
)

// Handler 单类 job 的执行体。
type Handler func(ctx context.Context, j *store.Job) error

// Options 池配置。
type Options struct {
	Workers     int           // 并发 worker 数（默认 min(4, NumCPU)）
	Kinds       []string      // 认领的 job kind（空 = 不限）
	PollEvery   time.Duration // 轮询兜底间隔（默认 2s）
	StaleAfter  time.Duration // running 判 stale 的时长（默认 10min）
	MaxAttempts int           // 回收重跑上限（默认 3）
	OnJobDone   func(j *store.Job, err error) // 观测钩子（metrics/日志）
}

// Pool 固定 worker 池。
type Pool struct {
	st       *store.Store
	opts     Options
	handlers map[string]Handler
	wake     chan struct{}
	stop     chan struct{}
	stopped  sync.WaitGroup
}

// New 构造池（未启动）。
func New(st *store.Store, opts Options) *Pool {
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers()
	}
	if opts.PollEvery <= 0 {
		opts.PollEvery = 2 * time.Second
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = 10 * time.Minute
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	return &Pool{
		st:       st,
		opts:     opts,
		handlers: map[string]Handler{},
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

func defaultWorkers() int {
	n := runtime.NumCPU()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

// Register 注册某类 job 的 handler（启动前调用）。
func (p *Pool) Register(kind string, h Handler) { p.handlers[kind] = h }

// Kick 发唤醒信号（非阻塞：池已在拉取中则忽略）。
func (p *Pool) Kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Start 启动 worker 池与回收器。ctx 取消时优雅停止。
func (p *Pool) Start(ctx context.Context) {
	// 启动即回收一次（崩溃恢复）
	p.reclaimOnce(ctx)
	n := p.opts.Workers
	for i := 0; i < n; i++ {
		p.stopped.Add(1)
		go p.worker(ctx, i)
	}
	// 回收器：周期扫描 stale running（worker 崩溃的 job 重新入 pending）
	p.stopped.Add(1)
	go p.reclaimer(ctx)
}

// worker 单 worker 循环：claim → handle → finish。
func (p *Pool) worker(ctx context.Context, id int) {
		defer p.stopped.Done()
	workerID := fmt.Sprintf("worker-%d", id)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		default:
		}
		j, err := p.st.ClaimNextJob(ctx, workerID, p.opts.Kinds)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("dispatch claim 失败", "worker", workerID, "err", err)
			p.sleep(ctx, time.Second)
			continue
		}
		if j == nil {
			// 无任务：等唤醒或轮询
			p.waitWake(ctx, p.opts.PollEvery)
			continue
		}
		h, ok := p.handlers[j.Kind]
		if !ok {
			// 无 handler 的 job：直接 failed（fail-closed，不静默丢）
			slog.Error("dispatch job 无 handler，置 failed", "kind", j.Kind, "id", j.ID)
			_ = p.st.FinishJob(context.Background(), j.ID, "failed")
			continue
		}
		// handler 执行用池 ctx（进程生命周期）；单 job 取消走 CaseSupervisor 内部。
		herr := h(ctx, j)
		status := "done"
		if herr != nil {
			status = "failed"
			slog.Warn("dispatch job 执行失败", "kind", j.Kind, "id", j.ID, "err", herr)
		}
		_ = p.st.FinishJob(context.Background(), j.ID, status)
		if p.opts.OnJobDone != nil {
			p.opts.OnJobDone(j, err)
		}
		p.Kick() // 处理完可能还有积压
	}
}

// reclaimer 周期回收 stale running。
func (p *Pool) reclaimer(ctx context.Context) {
	defer p.stopped.Done()
	t := time.NewTicker(p.opts.StaleAfter)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		case <-t.C:
			p.reclaimOnce(ctx)
		}
	}
}

func (p *Pool) reclaimOnce(ctx context.Context) {
	ids, err := p.st.ReclaimStaleJobs(ctx, p.opts.StaleAfter, p.opts.MaxAttempts)
	if err != nil {
		slog.Warn("dispatch stale 回收失败", "err", err)
		return
	}
	if len(ids) > 0 {
		slog.Info("dispatch 回收 stale job", "count", len(ids))
		p.Kick()
	}
}

// waitWake 等唤醒信号或超时。
func (p *Pool) waitWake(ctx context.Context, max time.Duration) {
	t := time.NewTimer(max)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-p.stop:
	case <-p.wake:
	case <-t.C:
	}
}

// sleep 可中断 sleep。
func (p *Pool) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-p.stop:
	case <-t.C:
	}
}

// Stop 停止池并等待所有 worker 退出。
func (p *Pool) Stop() {
	close(p.stop)
	p.stopped.Wait()
}
