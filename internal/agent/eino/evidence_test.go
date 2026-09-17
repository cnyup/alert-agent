package eino

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type echoTool struct{ name string }

func (e *echoTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: e.name}, nil
}
func (e *echoTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	return e.name + ":" + args, nil
}

// 并发工具调用下证据编号与结果不得错位（回归：旧 callback 按 entries[last]
// 回填，并行时互相覆盖致 trace 丢输出；labeledTool 按索引 settle 修复）。
func TestLabeledToolConcurrentEvidence(t *testing.T) {
	st := &evidenceState{}
	const n = 50
	tools := make([]tool.InvokableTool, n)
	for i := range tools {
		tools[i] = &labeledTool{InvokableTool: &echoTool{name: "t" + strconv.Itoa(i)}, col: st}
	}
	var wg sync.WaitGroup
	for i := range tools {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := tools[i].InvokableRun(context.Background(), strconv.Itoa(i))
			if err != nil || !strings.HasPrefix(out, "[T") || !strings.HasSuffix(out, " t"+strconv.Itoa(i)+":"+strconv.Itoa(i)) {
				t.Errorf("输出异常: %q err=%v", out, err)
			}
		}(i)
	}
	wg.Wait()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.entries) != n {
		t.Fatalf("证据条数应为 %d: %d", n, len(st.entries))
	}
	seen := map[string]bool{}
	for _, e := range st.entries {
		if want := e.Tool + ":" + e.Args; e.Result != want {
			t.Fatalf("证据错位: id=%s tool=%s args=%s result=%s", e.ID, e.Tool, e.Args, e.Result)
		}
		if seen[e.ID] {
			t.Fatalf("编号重复: %s", e.ID)
		}
		seen[e.ID] = true
	}
}
