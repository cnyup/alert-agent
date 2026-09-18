// Package metrics 自身可观测性（C3）：进程内指标 + Prometheus 文本格式渲染。
// 手写 exposition 而非引 client_golang——本项目依赖面克制优先；标签基数
// 均为有限集合（skill/source/result 等），无高基数风险。
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry 指标集。所有方法并发安全。
type Registry struct {
	mu       sync.Mutex
	counters map[string]*counterVec
	gauges   map[string]*gaugeVec
	ord      []string // 声明序，渲染稳定
}

type counterVec struct {
	name, help string
	labelKeys  []string
	vals       map[string]*int64
}

type gaugeVec struct {
	name, help string
	labelKeys  []string
	vals       map[string]*int64
}

// New 注册表。
func New() *Registry {
	return &Registry{counters: map[string]*counterVec{}, gauges: map[string]*gaugeVec{}}
}

// MustCounter 声明 counter（labelKeys 为标签名集合；空 = 无标签标量）。
func (r *Registry) MustCounter(name, help string, labelKeys ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.counters[name]; dup {
		return
	}
	sorted := append([]string(nil), labelKeys...)
	sort.Strings(sorted)
	r.counters[name] = &counterVec{name: name, help: help, labelKeys: sorted, vals: map[string]*int64{}}
	r.ord = append(r.ord, name)
}

// MustGauge 声明 gauge。
func (r *Registry) MustGauge(name, help string, labelKeys ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.gauges[name]; dup {
		return
	}
	sorted := append([]string(nil), labelKeys...)
	sort.Strings(sorted)
	r.gauges[name] = &gaugeVec{name: name, help: help, labelKeys: sorted, vals: map[string]*int64{}}
	r.ord = append(r.ord, name)
}

// Inc counter +1（labelValues 按 labelKeys 字典序给值）。
func (r *Registry) Inc(name string, labelValues ...string) {
	r.Add(name, 1, labelValues...)
}

// Add counter 累加。
func (r *Registry) Add(name string, v int64, labelValues ...string) {
	r.mu.Lock()
	cv := r.counters[name]
	if cv == nil {
		r.mu.Unlock()
		return
	}
	key := labelKey(cv.labelKeys, labelValues)
	p, ok := cv.vals[key]
	if !ok {
		p = new(int64)
		cv.vals[key] = p
	}
	r.mu.Unlock()
	atomic.AddInt64(p, v)
}

// SetGauge 设置 gauge 值。
func (r *Registry) SetGauge(name string, v int64, labelValues ...string) {
	r.mu.Lock()
	gv := r.gauges[name]
	if gv == nil {
		r.mu.Unlock()
		return
	}
	key := labelKey(gv.labelKeys, labelValues)
	p, ok := gv.vals[key]
	if !ok {
		p = new(int64)
		gv.vals[key] = p
	}
	r.mu.Unlock()
	atomic.StoreInt64(p, v)
}

func labelKey(keys []string, vals []string) string {
	var b strings.Builder
	for i, k := range keys {
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteByte('"')
		b.WriteString(escape(v))
		b.WriteByte('"')
		b.WriteByte(',')
	}
	return b.String()
}

func escape(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) }

// escapeHelp HELP 行按规范只转义反斜杠与换行。
func escapeHelp(s string) string { return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s) }

// Render Prometheus 文本格式。
func (r *Registry) Render() string {
	var b strings.Builder
	r.mu.Lock()
	defer r.mu.Unlock()
	names := append([]string(nil), r.ord...)
	sort.Strings(names)
	for _, name := range names {
		if cv := r.counters[name]; cv != nil {
			renderVec(&b, cv.name, cv.help, "counter", cv.labelKeys, cv.vals)
		}
		if gv := r.gauges[name]; gv != nil {
			renderVec(&b, gv.name, gv.help, "gauge", gv.labelKeys, gv.vals)
		}
	}
	return b.String()
}

func renderVec(b *strings.Builder, name, help, typ string, keys []string, vals map[string]*int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, escapeHelp(help), name, typ)
	if len(keys) == 0 {
		var v int64
		if p := vals[""]; p != nil {
			v = atomic.LoadInt64(p)
		}
		fmt.Fprintf(b, "%s %d\n", name, v)
		return
	}
	type row struct {
		key string
		val int64
	}
	var rows []row
	for k, p := range vals {
		rows = append(rows, row{k, atomic.LoadInt64(p)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })
	for _, rw := range rows {
		fmt.Fprintf(b, "%s{%s} %d\n", name, strings.TrimSuffix(rw.key, ","), rw.val)
	}
}

// Handler HTTP 处理器（挂 /metrics）。
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.Render()))
	})
}
