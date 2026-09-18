// Package webhook 通用 webhook 源：接收 HTTP POST，按配置的解析器归一化为 AlertEvent。
// 零代码接入的主力（DESIGN.md 三层扩展模型第一层）。
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func init() {
	plugin.RegisterSourceFactory("webhook", newSource)
}

// Parser 报文解析器：不同来源格式 → []AlertEvent。
type Parser interface {
	Name() string
	Parse(ctx context.Context, body []byte) ([]*model.AlertEvent, error)
}

// ConfiguredParser 需要用户配置的解析器（jmespath/regex）实现的可选接口：
// source 装配时若注册表未命中则尝试按 parse_options 构造。
type ConfiguredParser interface {
	Parser
	Configure(opts map[string]any) (Parser, error)
}

var (
	parserMu sync.RWMutex
	parsers  = map[string]Parser{}
)

// RegisterParser 注册报文解析器（内置集编译期注册）。
func RegisterParser(p Parser) {
	parserMu.Lock()
	defer parserMu.Unlock()
	if _, dup := parsers[p.Name()]; dup {
		panic(fmt.Sprintf("webhook: 解析器 %q 重复注册", p.Name()))
	}
	parsers[p.Name()] = p
}

// LookupParser 按名查找解析器。
func LookupParser(name string) (Parser, bool) {
	parserMu.RLock()
	defer parserMu.RUnlock()
	p, ok := parsers[name]
	return p, ok
}

var (
	configuredMu sync.RWMutex
	configured   = map[string]ConfiguredParser{}
)

// RegisterConfiguredParser 注册带配置解析器的原型（Configure 产出实例）。
func RegisterConfiguredParser(name string, proto ConfiguredParser) {
	configuredMu.Lock()
	defer configuredMu.Unlock()
	configured[name] = proto
}

// LookupConfigured 按名查找带配置解析器原型。
func LookupConfigured(name string) (ConfiguredParser, bool) {
	configuredMu.RLock()
	defer configuredMu.RUnlock()
	p, ok := configured[name]
	return p, ok
}

type source struct {
	path   string
	parse  string
	parser Parser // 解析器实例：注册表查找，或按 parse_options 构造（jmespath/regex）
	mux    *http.ServeMux
	server *http.Server
}

type sourceOptions struct {
	Path   string `json:"path"`   // 如 /hooks/alertmanager
	Parse string `json:"parse"`  // 解析器名：alertmanager / grafana / jmespath / regex
	// 带配置解析器的构造参数（jmespath: 表达式映射；regex: pattern+捕获组声明），
	// 原样传给 ConfiguredParser 工厂
	ParseOptions map[string]any `json:"parse_options"`
}

// SetMux 注入主服务的路由表（装配期由 main 调用，webhook 源把自己的
// handler 挂上去；框架单进程内共享一个 HTTP server）。
func (s *source) SetMux(mux *http.ServeMux) { s.mux = mux }

func newSource(opts map[string]any) (plugin.Source, error) {
	o := sourceOptions{}
	if err := decode(opts, &o); err != nil {
		return nil, err
	}
	if o.Path == "" {
		return nil, fmt.Errorf("webhook: path 不能为空")
	}
	if o.Parse == "" {
		return nil, fmt.Errorf("webhook: parse 不能为空")
	}
	var parser Parser
	if p, ok := LookupParser(o.Parse); ok {
		parser = p
	} else {
		// 注册表未命中 → 尝试带配置构造（jmespath/regex 等声明式解析器）
		cp, ok := LookupConfigured(o.Parse)
		if !ok {
			return nil, fmt.Errorf("webhook: 解析器 %q 未注册", o.Parse)
		}
		instance, err := cp.Configure(o.ParseOptions)
		if err != nil {
			return nil, fmt.Errorf("webhook: 解析器 %q 构造失败: %w", o.Parse, err)
		}
		parser = instance
	}
	return &source{path: o.Path, parse: o.Parse, parser: parser}, nil
}

func (*source) Name() string { return "webhook" }

func (s *source) Start(ctx context.Context, emit plugin.EmitFunc) error {
	if s.mux == nil {
		return fmt.Errorf("webhook: 未注入 HTTP mux（装配期需 SetMux）")
	}
	s.mux.HandleFunc("POST "+s.path, func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20)) // 1MB 上限
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		events, err := s.parser.Parse(req.Context(), body)
		if err != nil {
			slog.Warn("webhook 报文解析失败", "path", s.path, "parse", s.parse, "err", err)
			http.Error(w, "parse failed", http.StatusBadRequest)
			return
		}
		var firstErr error
		for _, evt := range events {
			if err := evt.Validate(); err != nil {
				slog.Warn("webhook 事件校验失败", "err", err)
				firstErr = err
				continue
			}
			if err := emit(req.Context(), evt); err != nil {
				slog.Error("webhook 事件投递失败", "id", evt.ID, "err", err)
				firstErr = err
			}
		}
		if firstErr != nil {
			// 已有事件进入管道的照常处理；解析部分失败的以 207 语义告知来源方
			w.WriteHeader(http.StatusMultiStatus)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	slog.Info("webhook 源就绪", "path", s.path, "parse", s.parse)
	<-ctx.Done()
	return nil
}

// decode 把配置 map 解码进目标结构（yaml→any→json 往返）。
func decode(opts map[string]any, target any) error {
	if len(opts) == 0 {
		return nil
	}
	b, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("webhook: 编码选项失败: %w", err)
	}
	if err := json.Unmarshal(b, target); err != nil {
		return fmt.Errorf("webhook: 解码选项失败: %w", err)
	}
	return nil
}
