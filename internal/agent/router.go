// SemanticRouter 剧本语义路由（DESIGN.md §4 三级路由的第二级）：
// 规则匹配（labels/keywords）未命中后，router 小模型按剧本元数据做一次选择，
// 未命中或调用失败回落通用兜底（调用方处理）。入口决策保持确定性可审计：
// 模型只能在候选清单内选名，选不出就明确 none。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/cnyup/alert-agent/internal/skills"
	coremodel "github.com/cnyup/alert-agent/pkg/model"
)

const routerTimeout = 15 * time.Second

type SemanticRouter struct {
	model model.BaseModel[*schema.Message]
	idx   *skills.Index
}

// NewSemanticRouter m 为 router 小模型（llm.New 产物）；idx 为剧本索引。
func NewSemanticRouter(m model.BaseModel[*schema.Message], idx *skills.Index) *SemanticRouter {
	return &SemanticRouter{model: m, idx: idx}
}

// Select 返回语义最匹配的剧本；无合适候选、输出非法或调用失败返回 nil。
// 候选只含带 triggers 的业务剧本——无 triggers 的知识技能（如 db_info）
// 设计上只被业务剧本引用，不参与入口路由。
func (r *SemanticRouter) Select(ctx context.Context, evt *coremodel.AlertEvent) *skills.Skill {
	var sb strings.Builder
	routable := map[string]*skills.Skill{}
	for _, name := range r.idx.Names() {
		s, ok := r.idx.Get(name)
		if !ok || len(s.Triggers.Labels) == 0 && len(s.Triggers.Keywords) == 0 {
			continue
		}
		routable[name] = s
		sb.WriteString(fmt.Sprintf("- %s: %s\n", name, s.Description))
	}
	if sb.Len() == 0 {
		return nil
	}
	labels, _ := json.Marshal(evt.Labels)
	prompt := "你是告警排查系统的剧本路由器。可用排查剧本清单（名称: 描述）：\n" + sb.String() +
		"\n待路由的告警事件：\n标题: " + evt.Title +
		"\n描述: " + truncate(evt.Description, 1500) +
		"\nlabels: " + string(labels) +
		"\n\n判断哪个剧本最适合排查该告警（依据剧本描述与告警内容的相关性）；" +
		"都不相关则选 none，不要勉强。\n" +
		`只输出 JSON：{"skill": "<剧本名或none>", "reason": "<不超过30字>"}（不要包裹代码块）`

	cctx, cancel := context.WithTimeout(ctx, routerTimeout)
	defer cancel()
	resp, err := r.model.Generate(cctx, []*schema.Message{{Role: schema.User, Content: prompt}})
	if err != nil {
		slog.Warn("语义路由调用失败，回落通用兜底", "err", err)
		return nil
	}
	var out struct {
		Skill  string `json:"skill"`
		Reason string `json:"reason"`
	}
	body := strings.TrimSpace(resp.Content)
	if i := strings.Index(body, "{"); i >= 0 {
		if obj, ok := extractBalancedJSON(body[i:]); ok {
			body = obj
		}
	}
	if json.Unmarshal([]byte(body), &out) != nil {
		slog.Warn("语义路由输出不可解析，回落通用兜底", "out", truncate(resp.Content, 120))
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(out.Skill), "none") || out.Skill == "" {
		slog.Info("语义路由未命中（模型判定无相关剧本），回落通用兜底", "reason", out.Reason, "event", evt.ID)
		return nil
	}
	s, ok := routable[out.Skill]
	if !ok {
		slog.Warn("语义路由返回了候选清单外的剧本名，忽略", "skill", out.Skill)
		return nil
	}
	slog.Info("语义路由命中", "skill", s.Name, "reason", out.Reason, "event", evt.ID)
	return s
}
