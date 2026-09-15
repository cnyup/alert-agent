package skills

import (
	"testing"
	"time"

	"github.com/cnyup/alert-agent/pkg/model"
)

// 用仓库自带示例剧本做真实验收
const exampleDir = "../../skills/examples"

func TestLoadExamplesAndMatch(t *testing.T) {
	idx, err := Load(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Names()) != 2 { // _generic 不入普通索引
		t.Fatalf("应加载 2 个剧本，实得 %v", idx.Names())
	}

	// labels 精确命中
	evtPG, _ := model.NewEvent("test", model.SeverityCritical, "PG 连接报错",
		model.Labels{"component": "postgres"}, time.Time{}, nil, nil)
	hits := idx.Match(evtPG)
	if len(hits) == 0 || hits[0].Name != "pg-conn-exhaust" {
		t.Fatalf("postgres 告警应命中 pg 剧本: %+v", hits)
	}

	// keywords 命中
	evtHTTP, _ := model.NewEvent("test", model.SeverityCritical, "订单服务 5xx 飙升",
		model.Labels{"service": "order-api"}, time.Time{}, nil, nil)
	hits = idx.Match(evtHTTP)
	if len(hits) == 0 || hits[0].Name != "http-5xx-spike" {
		t.Fatalf("5xx 告警应命中 http 剧本: %+v", hits)
	}

	// 无命中 → 空（调用方走通用兜底）
	evtOther, _ := model.NewEvent("test", model.SeverityInfo, "磁盘满了",
		model.Labels{"host": "n1"}, time.Time{}, nil, nil)
	if hits := idx.Match(evtOther); len(hits) != 0 {
		t.Fatalf("无关联告警不应命中: %+v", hits)
	}

	// 通用兜底可加载且带正文
	g, err := LoadGeneric(exampleDir)
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "generic" || g.Body == "" {
		t.Fatalf("通用剧本异常: %+v", g)
	}
}

func TestSkillToolsScopingField(t *testing.T) {
	idx, _ := Load(exampleDir)
	s, _ := idx.Get("pg-conn-exhaust")
	if len(s.Tools) != 2 || s.Tools[0] != "prometheus_query" {
		t.Fatalf("剧本应声明收权工具: %v", s.Tools)
	}
	// 两阶段：索引阶段的 Skill 已带 Body（进程内缓存），但注入上下文由内核按需进行
	if s.Body == "" {
		t.Fatal("正文未加载")
	}
}
