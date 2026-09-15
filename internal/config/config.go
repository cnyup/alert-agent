// Package config 装载用户配置：配置表达"装配"，代码表达"行为"（DESIGN.md §0）。
// 单文件入口 + ${ENV} 环境变量展开（凭证不落盘）。
package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// ModelRef 一个 LLM 端点：OpenAI 兼容协议，base_url + model 全可配。
type ModelRef struct {
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	APIKey  string `yaml:"api_key"` // 支持 ${ENV} 展开
}

// SourceConfig / NotifierConfig 的 Options 保留插件专属装配项，
// 由各插件自行解码（框架不理解插件的私有配置）。
type SourceConfig struct {
	Type    string            `yaml:"type"`
	Options map[string]any    `yaml:",inline"`
}

type NotifierConfig struct {
	Type    string         `yaml:"type"`
	Options map[string]any `yaml:",inline"`
}

type StageConfig struct {
	Stage   string         `yaml:"stage"`
	Options map[string]any `yaml:",inline"`
}

type MCPServerConfig struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}

// Config 框架全部装配入口。
type Config struct {
	Server struct {
		Addr string `yaml:"addr"` // HTTP 监听地址（webhook/卡片回调/健康检查）
	} `yaml:"server"`
	LLM struct {
		Router   ModelRef `yaml:"router"`   // 分类/选剧本/去重辅助（小模型）
		Reasoner ModelRef `yaml:"reasoner"` // 排查主力（大模型）
	} `yaml:"llm"`
	Sources  []SourceConfig  `yaml:"sources"`
	Pipeline []StageConfig   `yaml:"pipeline"`
	Skills   struct {
		Dir string `yaml:"dir"`
	} `yaml:"skills"`
	MCP struct {
		Servers map[string]MCPServerConfig `yaml:"servers"`
	} `yaml:"mcp"`
	Policy struct {
		Execution string `yaml:"execution"` // suggest-only | approval-required | auto+whitelist
	} `yaml:"policy"`
	Notifiers []NotifierConfig `yaml:"notifiers"`
	Store     struct {
		Path string `yaml:"path"`
	} `yaml:"store"`
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand 展开 ${VAR} 为环境变量值；未定义的变量展开为空串并保留原样可查。
func expand(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
}

// Load 从 YAML 文件装载配置并做基础校验。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	expanded := envRef.ReplaceAllStringFunc(string(raw), func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		return os.Getenv(name)
	})
	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %s 校验失败: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Store.Path == "" {
		c.Store.Path = "data/alert-agent.db"
	}
	if c.Skills.Dir == "" {
		c.Skills.Dir = "skills"
	}
	switch c.Policy.Execution {
	case "", "suggest-only", "approval-required", "auto+whitelist":
	default:
		return fmt.Errorf("policy.execution 非法: %q", c.Policy.Execution)
	}
	if c.Policy.Execution == "" {
		c.Policy.Execution = "suggest-only" // P0 一律仅建议
	}
	for i, s := range c.Sources {
		if s.Type == "" {
			return fmt.Errorf("sources[%d].type 不能为空", i)
		}
		if _, ok := pluginType(s.Type); !ok {
			return fmt.Errorf("sources[%d].type %q 未注册", i, s.Type)
		}
	}
	return nil
}

// pluginType 占位：里程碑3 接入真实注册表后由 plugin.LookupSource 替代。
func pluginType(name string) (struct{}, bool) {
	// P0 骨架阶段：允许任意 type，装配校验在插件注册表落地后收紧
	return struct{}{}, true
}
