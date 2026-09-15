package plugin

import (
	"context"
	"testing"

	"github.com/cnyup/alert-agent/pkg/model"
)

// 测试用假插件
type fakeSource struct{ name string }

func (f *fakeSource) Name() string { return f.name }
func (f *fakeSource) Start(_ context.Context, _ EmitFunc) error { return nil }

type fakeStage struct{ name string }

func (f *fakeStage) Name() string { return f.name }
func (f *fakeStage) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, Action, error) {
	return evt, ActionContinue, nil
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	// 用未注册的名字先清理，避免与其他测试并行注册互相污染
	RegisterSource(&fakeSource{name: "src-test"})
	RegisterStage(&fakeStage{name: "stage-test"})

	if _, ok := LookupSource("src-test"); !ok {
		t.Fatal("注册后的 Source 应可查到")
	}
	if _, ok := LookupSource("nope"); ok {
		t.Fatal("未注册的 Source 不应查到")
	}
	if _, ok := LookupStage("stage-test"); !ok {
		t.Fatal("注册后的 Stage 应可查到")
	}
	if len(SourceNames()) == 0 || len(StageNames()) == 0 {
		t.Fatal("名单不应为空")
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("重复注册应 panic")
		}
	}()
	RegisterSource(&fakeSource{name: "src-dup"})
	RegisterSource(&fakeSource{name: "src-dup"})
}
