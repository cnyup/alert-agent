package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRegistryBasics(t *testing.T) {
	r := New()
	r.MustCounter("evt_total", "事件数", "source", "status")
	r.MustCounter("inflight", "在途")
	r.MustGauge("g", " gauge 测试", "skill")

	r.Inc("evt_total", "webhook/alertmanager", "firing")
	r.Inc("evt_total", "webhook/alertmanager", "firing")
	r.Inc("evt_total", "feishu/quote", "firing")
	r.Add("evt_total", 5, "webhook/alertmanager", "resolved")
	r.SetGauge("g", 3, "workflow-failed")
	r.SetGauge("g", 0, "workflow-failed") // 覆盖写

	out := r.Render()
	for _, want := range []string{
		`# TYPE evt_total counter`,
		`evt_total{source="webhook/alertmanager",status="firing"} 2`,
		`evt_total{source="webhook/alertmanager",status="resolved"} 5`,
		`evt_total{source="feishu/quote",status="firing"} 1`,
		`# TYPE g gauge`,
		`g{skill="workflow-failed"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺少 %q:\n%s", want, out)
		}
	}
	// 未声明指标安全忽略
	r.Inc("nope")
	// help 转义：反斜杠与换行按规范转义，引号原样
	r.MustCounter("weird", "含\"引号\"与\\反斜杠\n换行")
	if !strings.Contains(r.Render(), `含"引号"与\\反斜杠\n换行`) {
		t.Fatal("help 转义异常")
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := New()
	r.MustCounter("c", "并发", "k")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Inc("c", "x")
			}
		}()
	}
	wg.Wait()
	if !strings.Contains(r.Render(), `c{k="x"} 5000`) {
		t.Fatal("并发累计错误")
	}
}

func TestHandler(t *testing.T) {
	r := New()
	r.MustCounter("h", "处理器")
	r.Inc("h")
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "h 1") {
		t.Fatalf("handler 输出异常: %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type 异常: %s", ct)
	}
}
