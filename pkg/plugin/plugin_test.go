package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/cnyup/alert-agent/pkg/model"
)

// 假 Stage 工厂：记录实例化次数，验证"每配置条目一个实例"
type stageBook struct{ name string }

func (s *stageBook) Name() string { return s.name }
func (s *stageBook) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, Action, error) {
	return evt, ActionContinue, nil
}

func TestFactoryRegistryInstantiate(t *testing.T) {
	RegisterStageFactory("stage-test", func(opts map[string]any) (Stage, error) {
		return &stageBook{name: "stage-test"}, nil
	})
	RegisterStageFactory("stage-err", func(opts map[string]any) (Stage, error) {
		return nil, errors.New("bad options")
	})

	s1, err := NewStage("stage-test", nil)
	if err != nil {
		t.Fatalf("实例化失败: %v", err)
	}
	s2, _ := NewStage("stage-test", nil)
	if s1 == s2 {
		t.Fatal("两次装配应产生两个独立实例")
	}
	if s1.Name() != "stage-test" {
		t.Fatalf("名字不符: %s", s1.Name())
	}
	if _, err := NewStage("stage-err", nil); err == nil {
		t.Fatal("工厂报错应透传")
	}
	if _, err := NewStage("nope", nil); err == nil {
		t.Fatal("未注册的 stage 应报错")
	}
	if len(StageNames()) < 2 {
		t.Fatal("工厂名单不全")
	}
}

func TestFactoryDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("重复注册应 panic")
		}
	}()
	RegisterStageFactory("stage-dup", func(map[string]any) (Stage, error) { return nil, nil })
	RegisterStageFactory("stage-dup", func(map[string]any) (Stage, error) { return nil, nil })
}
