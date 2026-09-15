package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func init() {
	plugin.RegisterStageFactory("dedup", newDedup)
}

// dedup 指纹去重：窗口内同 fingerprint 的重复告警直接 Drop（不重复排查）。
type dedup struct {
	window time.Duration
	mu     sync.Mutex
	seen   map[string]time.Time
}

type dedupOptions struct {
	Key    string `json:"key"`    // 预留：暂只支持 fingerprint（契约唯一稳定键）
	Window string `json:"window"` // 如 "5m"
}

func newDedup(opts map[string]any) (plugin.Stage, error) {
	o := dedupOptions{Window: "5m"}
	if err := DecodeOptions(opts, &o); err != nil {
		return nil, err
	}
	w, err := time.ParseDuration(o.Window)
	if err != nil || w <= 0 {
		return nil, fmt.Errorf("dedup: 非法 window %q", o.Window)
	}
	if o.Key != "" && o.Key != "fingerprint" {
		return nil, fmt.Errorf("dedup: 暂只支持 key=fingerprint，得到 %q", o.Key)
	}
	return &dedup{window: w, seen: make(map[string]time.Time)}, nil
}

func (*dedup) Name() string { return "dedup" }

func (d *dedup) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	// 惰性清理过期键，防止 map 无界增长
	for k, ts := range d.seen {
		if now.Sub(ts) > d.window {
			delete(d.seen, k)
		}
	}
	if ts, ok := d.seen[evt.Fingerprint]; ok && now.Sub(ts) <= d.window {
		return evt, plugin.ActionDrop, nil
	}
	d.seen[evt.Fingerprint] = now
	return evt, plugin.ActionContinue, nil
}
