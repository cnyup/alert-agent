// Package plugin 定义三个插件契约（Source/Notifier/Stage）与工厂注册表。
// Go 无动态加载：内置插件集在各自包的 init() 中注册工厂（Terraform/Caddy 模式），
// 用户长尾场景走 sidecar HTTP 对接（DESIGN.md §2.2 三层扩展模型）。
//
// 注册的是工厂而非实例：每个配置条目（如两个不同 path 的 webhook 源）
// 各自实例化，插件选项在工厂内做强类型解码——插件实例不得依赖全局状态。
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
// （借鉴 xdag 的 Skipped 语义：silence 命中、dedup 窗口内都返回 Skip/Drop）。
type Action int

const (
	// ActionContinue 继续流向下一阶段（evt 可被阶段修改后返回）。
	ActionContinue Action = iota
	// ActionSkip 本阶段跳过，事件继续（如静默命中但允许后续告警）。
	ActionSkip
	// ActionDrop 终止该事件的处理（如 dedup 窗口内的重复告警）。
	ActionDrop
)

// Stage 管道阶段插件：顺序由配置装配，可增删。实例必须并发安全。
type Stage interface {
	Name() string
	Process(ctx context.Context, evt *model.AlertEvent) (*model.AlertEvent, Action, error)
}

// 工厂：opts 来自配置文件中该条目的专属装配块（map 形式），
// 由各插件自行做强类型解码——框架不理解插件的私有配置。
type SourceFactory func(opts map[string]any) (Source, error)
type NotifierFactory func(opts map[string]any) (Notifier, error)
type StageFactory func(opts map[string]any) (Stage, error)

var (
	mu        sync.RWMutex
	sources   = map[string]SourceFactory{}
	notifiers = map[string]NotifierFactory{}
	stages    = map[string]StageFactory{}
)

func register[T any](reg map[string]T, kind, name string, f T) {
	if name == "" {
		panic(fmt.Sprintf("plugin: %s 工厂名不能为空", kind))
	}
	if _, dup := reg[name]; dup {
		panic(fmt.Sprintf("plugin: %s 工厂 %q 重复注册", kind, name))
	}
	reg[name] = f
}

// RegisterSourceFactory 注册 Source 工厂。
func RegisterSourceFactory(name string, f SourceFactory) {
	mu.Lock()
	defer mu.Unlock()
	register(sources, "Source", name, f)
}

// RegisterNotifierFactory 注册 Notifier 工厂。
func RegisterNotifierFactory(name string, f NotifierFactory) {
	mu.Lock()
	defer mu.Unlock()
	register(notifiers, "Notifier", name, f)
}

// RegisterStageFactory 注册 Stage 工厂。
func RegisterStageFactory(name string, f StageFactory) {
	mu.Lock()
	defer mu.Unlock()
	register(stages, "Stage", name, f)
}

// NewSource 按注册名与配置实例化 Source。
func NewSource(name string, opts map[string]any) (Source, error) {
	mu.RLock()
	f, ok := sources[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("plugin: Source %q 未注册", name)
	}
	return f(opts)
}

// NewNotifier 按注册名与配置实例化 Notifier。
func NewNotifier(name string, opts map[string]any) (Notifier, error) {
	mu.RLock()
	f, ok := notifiers[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("plugin: Notifier %q 未注册", name)
	}
	return f(opts)
}

// NewStage 按注册名与配置实例化 Stage。
func NewStage(name string, opts map[string]any) (Stage, error) {
	mu.RLock()
	f, ok := stages[name]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("plugin: Stage %q 未注册", name)
	}
	return f(opts)
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SourceNames 已注册的 Source 工厂名单。
func SourceNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	return sortedKeys(sources)
}

// NotifierNames 已注册的 Notifier 工厂名单。
func NotifierNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	return sortedKeys(notifiers)
}

// StageNames 已注册的 Stage 工厂名单。
func StageNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	return sortedKeys(stages)
}
