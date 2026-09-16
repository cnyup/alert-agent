package fanout

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

type fakeNotifier struct {
	name      string
	failFirst int32 // 前 N 次失败（测试重试）
	calls     atomic.Int32
}

func (f *fakeNotifier) Name() string { return f.name }
func (f *fakeNotifier) Notify(_ context.Context, _ *model.DiagnosisReport) error {
	n := f.calls.Add(1)
	if int32(n) <= f.failFirst {
		return errors.New("flaky")
	}
	return nil
}

func okReport() *model.DiagnosisReport {
	return &model.DiagnosisReport{AlertID: "evt_x", Summary: "s"}
}

func TestFanoutAllSuccess(t *testing.T) {
	a := &fakeNotifier{name: "chan-a"}
	b := &fakeNotifier{name: "chan-b"}
	failed, err := Notify(context.Background(), []plugin.Notifier{a, b}, okReport())
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 0 {
		t.Fatalf("应全部成功: %v", failed)
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatal("各通道应恰好执行一次")
	}
}

func TestFanoutRetryThenSuccess(t *testing.T) {
	// 前 2 次失败，第 3 次成功——xdag RetryPolicy(MaxAttempts=3) 应拉起
	f := &fakeNotifier{name: "flaky-chan", failFirst: 2}
	start := time.Now()
	failed, err := Notify(context.Background(), []plugin.Notifier{f}, okReport())
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 0 {
		t.Fatalf("重试后应成功: %v", failed)
	}
	if f.calls.Load() != 3 {
		t.Fatalf("应执行 3 次（含重试），实得 %d", f.calls.Load())
	}
	// 指数退避：2s + 4s ≥ 6s
	if elapsed := time.Since(start); elapsed < 5*time.Second {
		t.Fatalf("退避时间不足: %v", elapsed)
	}
}

func TestFanoutFailureIsolation(t *testing.T) {
	// 永远失败的通道不影响其他通道成功（扇出隔离）
	bad := &fakeNotifier{name: "bad-chan", failFirst: 100}
	good := &fakeNotifier{name: "good-chan"}
	failed, err := Notify(context.Background(), []plugin.Notifier{bad, good}, okReport())
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0] != "bad-chan" {
		t.Fatalf("应仅 bad-chan 失败: %v", failed)
	}
	if good.calls.Load() != 1 {
		t.Fatal("good 通道应正常送达")
	}
	if bad.calls.Load() != 3 {
		t.Fatalf("失败通道应重试满 3 次，实得 %d", bad.calls.Load())
	}
}
