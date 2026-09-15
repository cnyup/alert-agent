// Package skills 排查剧本：SKILL.md（YAML frontmatter + Markdown 正文）。
// 对齐 Agent Skills 惯例（DESIGN.md §2.3），两阶段加载：
// 索引（本包常驻，只有元数据）→ 命中后由排查内核注入剧本全文。
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cnyup/alert-agent/pkg/model"
)

// Triggers 触发条件：labels 精确匹配（子集）或 keywords 子串命中。
type Triggers struct {
	Labels   map[string]string `yaml:"labels,omitempty"`
	Keywords []string          `yaml:"keywords,omitempty"`
}

// Skill 一份排查剧本。
type Skill struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Triggers    Triggers `yaml:"triggers,omitempty"`
	Severity    []string `yaml:"severity,omitempty"` // 空表示不限
	Tools       []string `yaml:"tools,omitempty"`    // 本剧本允许的工具（收权）
	Body        string   `yaml:"-"`                  // Markdown 正文（第二阶段才进上下文）
}

// Index 剧本索引：两阶段加载的第一阶段（元数据常驻，正文按需）。
type Index struct {
	byName map[string]*Skill
	names  []string
}

// Load 扫描目录下所有 */SKILL.md 并解析 frontmatter。
func Load(dir string) (*Index, error) {
	idx := &Index{byName: map[string]*Skill{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil // 空目录合法（全走通用兜底）
		}
		return nil, fmt.Errorf("skills: 读取目录 %s 失败: %w", dir, err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue // _开头为框架内置（如通用兜底剧本），单独加载
		}
		s, err := loadOne(filepath.Join(dir, e.Name(), "SKILL.md"))
		if err != nil {
			return nil, err
		}
		if _, dup := idx.byName[s.Name]; dup {
			return nil, fmt.Errorf("skills: 剧本 %q 重复", s.Name)
		}
		idx.byName[s.Name] = s
		idx.names = append(idx.names, s.Name)
	}
	sort.Strings(idx.names)
	return idx, nil
}

// LoadGeneric 加载通用兜底剧本（_generic/SKILL.md）。
func LoadGeneric(dir string) (*Skill, error) {
	s, err := loadOne(filepath.Join(dir, "_generic", "SKILL.md"))
	if err != nil {
		return nil, err
	}
	return s, nil
}

func loadOne(path string) (*Skill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("skills: 读取 %s 失败: %w", path, err)
	}
	fm, body, err := splitFrontmatter(string(raw))
	if err != nil {
		return nil, fmt.Errorf("skills: %s: %w", path, err)
	}
	var s Skill
	if err := yaml.Unmarshal([]byte(fm), &s); err != nil {
		return nil, fmt.Errorf("skills: %s frontmatter 非法: %w", path, err)
	}
	if s.Name == "" || s.Description == "" {
		return nil, fmt.Errorf("skills: %s 的 name/description 不能为空", path)
	}
	s.Body = body
	return &s, nil
}

func splitFrontmatter(doc string) (frontmatter, body string, err error) {
	doc = strings.TrimLeft(doc, "\n")
	if !strings.HasPrefix(doc, "---") {
		return "", "", fmt.Errorf("缺少 frontmatter（应以 --- 开头）")
	}
	rest := doc[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("frontmatter 未闭合（缺结尾 ---）")
	}
	return strings.TrimSpace(rest[:end]), strings.TrimSpace(rest[end+4:]), nil
}

// Get 按名取剧本。
func (ix *Index) Get(name string) (*Skill, bool) {
	s, ok := ix.byName[name]
	return s, ok
}

// Names 全部剧本名（诊断用）。
func (ix *Index) Names() []string { return append([]string(nil), ix.names...) }

// Match 规则路由（DESIGN.md §4 剧本路由的第一优先级）：
// labels 全对上得 100 分，keywords 每命中一个得 50 分；severity 不符直接排除。
// 返回按得分降序的候选（得分为 0 不入列）。
func (ix *Index) Match(evt *model.AlertEvent) []*Skill {
	type scored struct {
		s *Skill
		n int
	}
	var hits []scored
	for _, name := range ix.names {
		s := ix.byName[name]
		if len(s.Severity) > 0 && !containsFold(s.Severity, string(evt.Severity)) {
			continue
		}
		n := 0
		ok := true
		for k, v := range s.Triggers.Labels {
			if evt.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok && len(s.Triggers.Labels) > 0 {
			n += 100
		}
		hay := strings.ToLower(evt.Title + " " + evt.Description)
		for _, kw := range s.Triggers.Keywords {
			if strings.Contains(hay, strings.ToLower(kw)) {
				n += 50
			}
		}
		if n > 0 {
			hits = append(hits, scored{s, n})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
	out := make([]*Skill, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.s)
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
