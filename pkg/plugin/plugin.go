// Package plugin 定义三个插件契约（Source/Notifier/Stage）与编译期注册表。
// Go 无动态加载：内置插件集在各自包的 init() 中注册（Terraform/Caddy 模式），
// 用户长尾场景走 sidecar HTTP 对接（DESIGN.md §2.2 三层扩展模型）。
package plugin

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/cnyup/alert-agent/pkg/model"
)

// EmitFunc Source 向框架投递归一化事件的出口。
// 实现方应保证传入的 evt 已通过 Validate。
type EmitFunc func(ctx context.Context, evt *model.AlertEvent) error

// Source 告警来源插件：产出 AlertEvent。
// Start 在框架生命周期内常驻；ctx 取消时必须停止订阅并返回。
type Source interface {
	Name() string
	Start(ctx context.Context, emit EmitFunc) error
}

// Notifier 通知插件：发送诊断报告。
// 报告自带 AlertID 与来源侧 Refs，完成回复/引用关联。
type Notifier interface {
	Name() string
	Notify(ctx context.Context, report *model.DiagnosisReport) error
}

// Action 管道阶段的处理结果——"跳过"是一等状态，不是失败
// （借鉴 xdag 的 Skipped 语义：silence 命中、dedup 窗口内都返回 Skip）。
type Action int

const (
	// ActionContinue 继续流向下一阶段（evt 可被阶段修改后返回）。
	ActionContinue Action = iota
	// ActionSkip 本阶段跳过，事件继续（如静默命中但允许后续告警）。
	ActionSkip
	// ActionDrop 终止该事件的处理（如 dedup 窗口内的重复告警）。
	ActionDrop
)

// Stage 管道阶段插件：顺序由配置装配，可增删。
type Stage interface {
	Name() string
	Process(ctx context.Context, evt *model.AlertEvent) (*model.AlertEvent, Action, error)
}

var (
	mu        sync.RWMutex
	sources   = map[string]Source{}
	notifiers = map[string]Notifier{}
	stages    = map[string]Stage{}
)

// RegisterSource 编译期注册 Source 插件，重名直接 panic（启动期暴露装配错误）。
func RegisterSource(s Source) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := sources[s.Name()]; dup {
		panic(fmt.Sprintf("plugin: Source %q 重复注册", s.Name()))
	}
	sources[s.Name()] = s
}

// RegisterNotifier 编译期注册 Notifier 插件。
func RegisterNotifier(n Notifier) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := notifiers[n.Name()]; dup {
		panic(fmt.Sprintf("plugin: Notifier %q 重复注册", n.Name()))
	}
	notifiers[n.Name()] = n
}

// RegisterStage 编译期注册 Stage 插件。
func RegisterStage(s Stage) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := stages[s.Name()]; dup {
		panic(fmt.Sprintf("plugin: Stage %q 重复注册", s.Name()))
	}
	stages[s.Name()] = s
}

// LookupSource 按名查找（配置装配用），未注册返回 ok=false。
func LookupSource(name string) (Source, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := sources[name]
	return s, ok
}

// LookupNotifier 按名查找。
func LookupNotifier(name string) (Notifier, bool) {
	mu.RLock()
	defer mu.RUnlock()
	n, ok := notifiers[name]
	return n, ok
}

// LookupStage 按名查找。
func LookupStage(name string) (Stage, bool) {
	mu.RLock()
	defer mu.RUnlock()
	s, ok := stages[name]
	return s, ok
}

// SourceNames 已注册的 Source 名单（诊断/启动日志用）。
func SourceNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(sources))
	for n := range sources {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// NotifierNames 已注册的 Notifier 名单。
func NotifierNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(notifiers))
	for n := range notifiers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// StageNames 已注册的 Stage 名单。
func StageNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(stages))
	for n := range stages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
