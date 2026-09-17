// Package model 定义全系统最核心的契约：统一事件模型与诊断报告。
// 契约一旦冻结，所有插件、管道与排查内核都向它对齐（DESIGN.md §2）。
package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severity 告警等级（映射规则由各 Source 决定，进入系统后统一为这三档）。
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// Labels 告警标签。fingerprint 由它规范化生成，因此来源应把
// 所有可区分告警维度的信息（service/env/component 等）放进 labels。
type Labels map[string]string

// Canonical 返回排序稳定的 k=v 串，与 map 迭代顺序无关。
func (l Labels) Canonical() string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(l[k])
	}
	return b.String()
}

// EventStatus 告警生命周期状态（来源侧归一化；不影响 fingerprint——
// resolved 与 firing 是同一指纹的两个相位）。
type EventStatus string

const (
	StatusFiring   EventStatus = "firing"
	StatusResolved EventStatus = "resolved"
)

// AlertEvent 统一事件模型：任何来源的告警都必须归一化到该结构（DESIGN.md §2.1）。
type AlertEvent struct {
	ID          string            `json:"id"`
	Fingerprint string            `json:"fingerprint"` // labels 规范化生成 → 去重/聚合键
	Source      string            `json:"source"`      // 来源插件标识
	Status      EventStatus       `json:"status"`      // firing | resolved（resolved 不进管道，只做取消）
	Severity    Severity          `json:"severity"`
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	Labels      Labels            `json:"labels,omitempty"`
	OccurredAt  time.Time         `json:"occurred_at"`
	ReceivedAt  time.Time         `json:"received_at"`
	Raw         json.RawMessage   `json:"raw,omitempty"` // 原始报文留底（回放、排查解析问题用）
	Refs        map[string]string `json:"refs,omitempty"` // 来源侧关联（回复/引用用）
}

// NewEvent 构造事件：生成时间有序 ID、计算 fingerprint、补齐时间戳。
// occurredAt 为零值时视为"来源未提供发生时间"，回退为接收时间。
func NewEvent(source string, sev Severity, title string, labels Labels,
	occurredAt time.Time, raw json.RawMessage, refs map[string]string) (*AlertEvent, error) {

	if source == "" {
		return nil, errors.New("model: source 不能为空")
	}
	if !sev.Valid() {
		return nil, fmt.Errorf("model: 非法 severity %q", sev)
	}
	if title == "" {
		return nil, errors.New("model: title 不能为空")
	}
	id, err := newEventID(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if occurredAt.IsZero() {
		occurredAt = now
	}
	return &AlertEvent{
		ID:          id,
		Fingerprint: labels.Fingerprint(),
		Source:      source,
		Status:      StatusFiring,
		Severity:    sev,
		Title:       title,
		Labels:      labels,
		OccurredAt:  occurredAt,
		ReceivedAt:  now,
		Raw:         raw,
		Refs:        refs,
	}, nil
}

// Fingerprint 由 labels 规范化生成的去重/聚合键（sha256 hex，64 字符）。
// labels 为空时退化为空 labels 的哈希，调用方应尽量保证 labels 有区分度。
func (l Labels) Fingerprint() string {
	sum := sha256.Sum256([]byte("v1:" + l.Canonical()))
	return hex.EncodeToString(sum[:])
}

// Validate 事件契约校验：入管道前的最后一道防线。
func (e *AlertEvent) Validate() error {
	if e == nil {
		return errors.New("model: 事件为空")
	}
	if e.ID == "" {
		return errors.New("model: id 不能为空")
	}
	if len(e.Fingerprint) != sha256.Size*2 {
		return fmt.Errorf("model: fingerprint 长度非法: %d", len(e.Fingerprint))
	}
	if _, err := hex.DecodeString(e.Fingerprint); err != nil {
		return errors.New("model: fingerprint 非十六进制")
	}
	if e.Source == "" {
		return errors.New("model: source 不能为空")
	}
	if !e.Severity.Valid() {
		return fmt.Errorf("model: 非法 severity %q", e.Severity)
	}
	if e.Title == "" {
		return errors.New("model: title 不能为空")
	}
	if e.ReceivedAt.IsZero() {
		return errors.New("model: received_at 不能为零值")
	}
	if e.OccurredAt.IsZero() {
		return errors.New("model: occurred_at 不能为零值")
	}
	return nil
}

// newEventID 生成时间有序的事件 ID：unix 毫秒(6B) + 随机(10B)，base32 无填充。
func newEventID(t time.Time) (string, error) {
	var b [16]byte
	ms := t.UnixMilli()
	for i := 0; i < 6; i++ {
		b[5-i] = byte(ms >> (8 * i))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("model: 生成事件 ID 失败: %w", err)
	}
	return "evt_" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}
