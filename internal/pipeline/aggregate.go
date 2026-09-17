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
	plugin.RegisterStageFactory("aggregate", newAggregate)
}

// AggregateInfo incident 聚合信息（随 runState 传递给排查内核做上下文）。
type AggregateInfo struct {
	IncidentID  string `json:"incident_id"`
	Occurrences int    `json:"occurrences"` // 本窗口内同指纹告警累计条数
	Escalated   bool   `json:"escalated"`   // 是否因达到阈值升级再排查
}

// SetAggregate 聚合 stage 写入本次运行信息。
func (s *runState) SetAggregate(a *AggregateInfo) { s.aggregate = a }

// aggregate incident 聚合（DESIGN.md §8 关联聚合的 P1/P2 落地）：
// 窗口内同 fingerprint 的重复告警合并进同一 incident（Drop，不重复排查）；
// 累计达到 escalate_at 条时放行并升级为 critical 再排查一次（告警风暴升级信号）。
type aggregate struct {
	window     time.Duration
	escalateAt int

	mu        sync.Mutex
	incidents map[string]*incidentState
}

type incidentState struct {
	id    string
	start time.Time
	count int
}

type aggregateOptions struct {
	Window     string `json:"window"`      // 聚合窗口，默认 10m
	EscalateAt int    `json:"escalate_at"` // 升级阈值（累计条数），默认 10；<=0 关闭升级
}

func newAggregate(opts map[string]any) (plugin.Stage, error) {
	o := aggregateOptions{Window: "10m", EscalateAt: 10}
	if err := DecodeOptions(opts, &o); err != nil {
		return nil, err
	}
	w, err := time.ParseDuration(o.Window)
	if err != nil || w <= 0 {
		return nil, fmt.Errorf("aggregate: 非法 window %q", o.Window)
	}
	return &aggregate{window: w, escalateAt: o.EscalateAt, incidents: map[string]*incidentState{}}, nil
}

func (*aggregate) Name() string { return "aggregate" }

func (a *aggregate) Process(ctx context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	a.mu.Lock()
	now := time.Now()
	for k, inc := range a.incidents {
		if now.Sub(inc.start) > a.window {
			delete(a.incidents, k)
		}
	}
	fp := evt.Fingerprint
	inc, ok := a.incidents[fp]
	if !ok {
		inc = &incidentState{
			// 毫秒粒度：同指纹在窗口边界重开 incident 时，秒级时间戳会撞 ID
			id:    fmt.Sprintf("inc_%s_%d", fp[:12], now.UnixMilli()),
			start: now,
			count: 0,
		}
		a.incidents[fp] = inc
	}
	inc.count++
	n := inc.count
	incID := inc.id
	a.mu.Unlock()

	escalated := a.escalateAt > 0 && n == a.escalateAt
	if st := StateFrom(ctx); st != nil {
		st.SetAggregate(&AggregateInfo{IncidentID: incID, Occurrences: n, Escalated: escalated})
	}

	switch {
	case n == 1:
		return evt, plugin.ActionContinue, nil // incident 首条：正常排查
	case escalated:
		// 告警风暴升级：放行再排查一次，等级提到 critical
		evt.Severity = model.SeverityCritical
		return evt, plugin.ActionContinue, nil
	default:
		// 合并进已有 incident，不重复排查
		return evt, plugin.ActionDrop, nil
	}
}
