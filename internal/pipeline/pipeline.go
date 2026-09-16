// Package pipeline 实现确定性线性管道（DESIGN.md §1）：
// stages 顺序由配置装配、可增删；P2 需要图状编排时升级 xdag。
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

// RouteResult 路由阶段的产出：定级/候选剧本/通知目标。
// Severity 为空串表示维持原始定级。
type RouteResult struct {
	Severity  model.Severity `json:"severity,omitempty"`
	Skills    []string       `json:"skills,omitempty"`
	Notifiers []string       `json:"notifiers,omitempty"`
	Matched   string         `json:"matched,omitempty"` // 命中的规则名
}

// StageLog 单阶段执行记录（trace 素材）。
type StageLog struct {
	Stage  string        `json:"stage"`
	Action plugin.Action `json:"action"`
	Err    string        `json:"err,omitempty"`
	At     time.Time     `json:"at"`
}

// Result 管道终态。
type Result struct {
	Event     *model.AlertEvent
	Dropped   bool
	DropBy    string // 作出 Drop 决定的 stage 名
	Route     *RouteResult
	Aggregate *AggregateInfo
	StageLog  []StageLog
}

// runState 单次管道运行的状态，经 ctx 传递（route stage 写入路由结果；
// stage 实例是各装配点独立的，但仍不允许请求级状态落在实例上）。
type runState struct {
	route     *RouteResult
	aggregate *AggregateInfo
}

type stateKey struct{}

// StateFrom 供 stage 实现读写本次运行的路由结果。
func StateFrom(ctx context.Context) *runState {
	if s, ok := ctx.Value(stateKey{}).(*runState); ok {
		return s
	}
	return nil
}

// SetRoute route stage 写入路由结果。
func (s *runState) SetRoute(r *RouteResult) { s.route = r }

// StageSpec 一个装配条目：stage 名 + 其专属配置块。
type StageSpec struct {
	Name    string
	Options map[string]any
}

type entry struct {
	name  string
	stage plugin.Stage
}

// Runner 顺序执行装配好的 stages。每个装配条目独立实例化（plugin.NewStage）。
type Runner struct {
	stages []entry
}

// New 装配管道；stage 未注册或选项非法直接报错（启动期暴露装配错误）。
func New(specs []StageSpec) (*Runner, error) {
	r := &Runner{}
	for _, spec := range specs {
		s, err := plugin.NewStage(spec.Name, spec.Options)
		if err != nil {
			return nil, fmt.Errorf("pipeline: 装配 stage %q 失败: %w", spec.Name, err)
		}
		r.stages = append(r.stages, entry{name: spec.Name, stage: s})
	}
	return r, nil
}

// Names 装配名单（诊断用）。
func (r *Runner) Names() []string {
	out := make([]string, 0, len(r.stages))
	for _, e := range r.stages {
		out = append(out, e.name)
	}
	return out
}

// Run 执行管道。Continue 传递（可能被修改的）事件；Skip 记录后以原事件继续；
// Drop 终止。任一 stage 返回错误视为该阶段失败：记录进 StageLog 并继续
// （管道尽力而为，单阶段故障不阻断告警流转——确定性优先于严格性）。
func (r *Runner) Run(ctx context.Context, evt *model.AlertEvent) *Result {
	state := &runState{}
	ctx = context.WithValue(ctx, stateKey{}, state)
	res := &Result{Event: evt}
	for _, e := range r.stages {
		log := StageLog{Stage: e.name, At: time.Now().UTC()}
		out, action, err := e.stage.Process(ctx, evt)
		if err != nil {
			log.Err = err.Error()
			res.StageLog = append(res.StageLog, log)
			continue
		}
		log.Action = action
		res.StageLog = append(res.StageLog, log)
		switch action {
		case plugin.ActionContinue:
			if out != nil {
				evt = out
				res.Event = evt
			}
		case plugin.ActionSkip:
			// 本阶段跳过，事件原样继续
		case plugin.ActionDrop:
			res.Dropped = true
			res.DropBy = e.name
			res.Route = state.route
			res.Aggregate = state.aggregate
			return res
		}
	}
	res.Route = state.route
	res.Aggregate = state.aggregate
	return res
}

// DecodeOptions 把配置里的 map 选项解码进目标结构（yaml→any→json 往返）。
func DecodeOptions(opts map[string]any, target any) error {
	if len(opts) == 0 {
		return nil
	}
	b, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("pipeline: 编码选项失败: %w", err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		return fmt.Errorf("pipeline: 解码选项失败: %w", err)
	}
	return nil
}
