// Package fanout 用 xdag 任务图做通知扇出（P2 技术验证，DESIGN.md §9）。
// 每个通知器一个无依赖任务 → 调度器自动并行；重试交给 xdag RetryPolicy
// （按次计费、指数退避）；ctx 取消走调度器的单通路收敛。
// 该包同时是未来 incident 级编排（回滚→验证→通知）的接入模板。
package fanout

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/xmapst/xdag"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

// notifyTask 一个通知通道 = 一个 xdag 任务。
type notifyTask struct {
	name     string
	notifier plugin.Notifier
	report   *model.DiagnosisReport
}

func (t *notifyTask) Name() string          { return t.name }
func (t *notifyTask) Dependencies() []string { return nil }

func (*notifyTask) RetryPolicy() *xdag.RetryPolicy {
	return &xdag.RetryPolicy{
		MaxAttempts: 3, // 总执行次数：1 次执行 + 2 次重试
		Interval:    2 * time.Second,
		Multiplier:  2.0,
		MaxInterval: 10 * time.Second,
	}
}

func (t *notifyTask) PreExecution(_ context.Context, attempt int64, _ map[string]any) {
	if attempt > 1 {
		slog.Warn("通知重试", "notifier", t.name, "attempt", attempt)
	}
}

func (t *notifyTask) Execute(ctx context.Context, _ int64, _ map[string]any) (map[string]any, error) {
	if err := t.notifier.Notify(ctx, t.report); err != nil {
		return nil, fmt.Errorf("notify %s: %w", t.name, err)
	}
	return map[string]any{"sent": true}, nil
}

func (*notifyTask) PostExecution(context.Context, int64, map[string]any, error) {}

// Notify 并行扇出到全部通知通道。返回失败通道名单（全部成功为空）。
func Notify(ctx context.Context, targets []plugin.Notifier, report *model.DiagnosisReport) ([]string, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	tasks := make(map[string]xdag.ITask, len(targets))
	for _, n := range targets {
		tasks[n.Name()] = &notifyTask{name: n.Name(), notifier: n, report: report}
	}
	dag, err := xdag.New(tasks)
	if err != nil {
		return nil, fmt.Errorf("fanout: 构图失败: %w", err)
	}
	// Execute 返回的 error 是全部任务错误的聚合（errors.Join）——
	// 任务级失败属预期信号，以 States() 为准；仅 ctx 取消视为扇出异常
	if _, err := dag.Execute(ctx); err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("fanout: 执行被取消: %w", err)
	}
	var failed []string
	for name, state := range dag.States() {
		if state != xdag.StateSuccess {
			failed = append(failed, name)
		}
	}
	return failed, nil
}
