package inflight

import (
	"sync"
	"testing"
)

func TestRegistryCancel(t *testing.T) {
	r := New()
	var mu sync.Mutex
	cancelled := 0
	mk := func() func() {
		return func() { mu.Lock(); cancelled++; mu.Unlock() }
	}

	remove1 := r.Add("fp1", mk())
	r.Add("fp1", mk()) // 同指纹两个在途
	r.Add("fp2", mk())
	if r.Len() != 3 {
		t.Fatalf("在途数应为 3: %d", r.Len())
	}

	// 注销一个后再取消：只取消剩余的
	remove1()
	if n := r.Cancel("fp1"); n != 1 {
		t.Fatalf("应取消 1 个: %d", n)
	}
	mu.Lock()
	if cancelled != 1 {
		t.Fatalf("实际取消数: %d", cancelled)
	}
	mu.Unlock()

	// 已清空的指纹再取消为 0；重复注销无害
	if n := r.Cancel("fp1"); n != 0 {
		t.Fatal("清空后再取消应为 0")
	}
	remove1()
	if r.Len() != 1 {
		t.Fatalf("应剩 fp2 一个: %d", r.Len())
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rm := r.Add("fp", func() {})
			rm()
		}()
	}
	wg.Wait()
	if r.Len() != 0 {
		t.Fatalf("并发注册/注销后应为 0: %d", r.Len())
	}
}
