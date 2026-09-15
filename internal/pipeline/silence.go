package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/cnyup/alert-agent/pkg/model"
	"github.com/cnyup/alert-agent/pkg/plugin"
)

func init() {
	plugin.RegisterStageFactory("silence", newSilence)
}

// silence 静默：labels 全部匹配则 Drop（如 env=staging 的告警一律不排查）。
type silence struct {
	match map[string]string
}

type silenceOptions struct {
	Match string `json:"match"` // "env=staging" 或逗号分隔多对 "env=staging,service=foo"
}

func newSilence(opts map[string]any) (plugin.Stage, error) {
	o := silenceOptions{}
	if err := DecodeOptions(opts, &o); err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.Match) == "" {
		return nil, fmt.Errorf("silence: match 不能为空")
	}
	m := map[string]string{}
	for _, pair := range strings.Split(o.Match, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("silence: 非法匹配对 %q（应为 k=v）", pair)
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("silence: match 无有效对")
	}
	return &silence{match: m}, nil
}

func (*silence) Name() string { return "silence" }

func (s *silence) Process(_ context.Context, evt *model.AlertEvent) (*model.AlertEvent, plugin.Action, error) {
	for k, v := range s.match {
		if evt.Labels[k] != v {
			return evt, plugin.ActionContinue, nil
		}
	}
	return evt, plugin.ActionDrop, nil
}
