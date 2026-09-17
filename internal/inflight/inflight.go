// Package inflight 在途排查注册表：fingerprint → 取消函数集合。
// resolved 事件到达时按指纹取消所有在途排查（DESIGN.md §4 取消纪律的
// 运行时载体）；排查侧 defer 注销防泄漏。
package inflight

import "sync"

type entry struct{ cancel func() }

type Registry struct {
	mu      sync.Mutex
	cancels map[string][]*entry
}

func New() *Registry {
	return &Registry{cancels: map[string][]*entry{}}
}

// Add 登记一个在途排查的取消函数，返回注销句柄（排查结束时调用）。
func (r *Registry) Add(fingerprint string, cancel func()) (remove func()) {
	e := &entry{cancel: cancel}
	r.mu.Lock()
	r.cancels[fingerprint] = append(r.cancels[fingerprint], e)
	r.mu.Unlock()
	return func() { r.remove(fingerprint, e) }
}

func (r *Registry) remove(fingerprint string, e *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.cancels[fingerprint]
	for i, x := range list {
		if x == e {
			r.cancels[fingerprint] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(r.cancels[fingerprint]) == 0 {
		delete(r.cancels, fingerprint)
	}
}

// Cancel 取消该指纹下所有在途排查，返回取消数量。
func (r *Registry) Cancel(fingerprint string) int {
	r.mu.Lock()
	list := r.cancels[fingerprint]
	delete(r.cancels, fingerprint)
	r.mu.Unlock()
	for _, e := range list {
		e.cancel()
	}
	return len(list)
}

// Len 当前在途排查总数（诊断/观测用）。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.cancels {
		n += len(l)
	}
	return n
}
