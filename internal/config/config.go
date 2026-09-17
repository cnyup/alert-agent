// Package config 装载用户配置：配置表达"装配"，代码表达"行为"（DESIGN.md §0）。
// 单文件入口 + ${ENV} 环境变量展开（凭证不落盘）。
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

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
	Command string           `yaml:"command"`
	Args    []string         `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}

// ToolsConfig 内置框架工具（exec/read），与 MCP 工具并列装配。
type ToolsConfig struct {
	Exec struct {
		Enabled        bool              `yaml:"enabled"`
		Backend        string            `yaml:"backend"` // local | docker
		Allow          []string          `yaml:"allow"`   // 二进制白名单（名字或绝对路径）
		Readonly       map[string][]string `yaml:"readonly"` // bin → 词对齐只读子命令前缀；未命中的调用一律按 mutating（宁严勿松）
		Timeout        string            `yaml:"timeout"` // 单命令超时，如 30s
		MaxOutputBytes int               `yaml:"max_output_bytes"`
		Docker         struct {
			Image    string   `yaml:"image"`   // 空 = 组件默认镜像
			Network  *bool    `yaml:"network"` // 默认 true（CLI 需要访问 API）
			MemoryMB int64    `yaml:"memory_mb"`
			CPU      float64  `yaml:"cpu"`
			Timeout  string   `yaml:"timeout"` // 容器内单命令超时
			Env      []string `yaml:"env"`     // 注入容器的环境变量（支持 ${ENV} 展开）
		} `yaml:"docker"`
	} `yaml:"exec"`
	Read struct {
		Enabled bool `yaml:"enabled"` // read 工具：限定剧本目录内读文件（references 按需加载）
	} `yaml:"read"`
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
		Dir     string                     `yaml:"dir"` // 目录式配置：一个 yaml 文件一个 server
		Servers map[string]MCPServerConfig `yaml:"servers"`
	} `yaml:"mcp"`
	Agent struct {
		MaxIterations int `yaml:"max_iterations"` // 排查内核模型轮次上限（含工具调用轮）
	} `yaml:"agent"`
	Tools ToolsConfig `yaml:"tools"`
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
	if c.Agent.MaxIterations <= 0 {
		c.Agent.MaxIterations = 30 // 技能渐进加载 + 资源逐层下钻的合理预算
	}
	if te := &c.Tools.Exec; te.Enabled {
		switch te.Backend {
		case "":
			te.Backend = "local"
		case "local", "docker":
		default:
			return fmt.Errorf("tools.exec.backend 非法: %q（local | docker）", te.Backend)
		}
		if len(te.Allow) == 0 {
			return fmt.Errorf("tools.exec.allow 白名单不能为空（enabled 时必须显式放行二进制）")
		}
		if te.Timeout == "" {
			te.Timeout = "30s"
		}
		if _, err := time.ParseDuration(te.Timeout); err != nil {
			return fmt.Errorf("tools.exec.timeout 非法: %q", te.Timeout)
		}
		if te.MaxOutputBytes <= 0 {
			te.MaxOutputBytes = 64 * 1024
		}
		if td := &te.Docker; td.Timeout == "" {
			td.Timeout = te.Timeout
		}
		if _, err := time.ParseDuration(te.Docker.Timeout); err != nil {
			return fmt.Errorf("tools.exec.docker.timeout 非法: %q", te.Docker.Timeout)
		}
	}
	for i, s := range c.Sources {
		if s.Type == "" {
			return fmt.Errorf("sources[%d].type 不能为空", i)
		}
	}
	for i, s := range c.Pipeline {
		if s.Stage == "" {
			return fmt.Errorf("pipeline[%d].stage 不能为空", i)
		}
	}
	return nil
}
