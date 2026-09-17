// exec/read 两个内置工具：模型面向的接口与后端无关，
// 执行环境从 ctx 取（handleEvent 每事件 Acquire 注入）。
package execenv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// NewExecTool 构造 exec 工具：argv 直传白名单内的 CLI，返回 stdout/stderr/exit_code。
func NewExecTool() (tool.InvokableTool, error) {
	type input struct {
		Argv  []string `json:"argv" jsonschema:"required" jsonschema_description:"命令及参数，argv[0] 为白名单内的可执行文件名，如 [\"tt-devops-cli\", \"databases\", \"resources\", \"+search\", \"--input\", \"-\"]"`
		Stdin string   `json:"stdin,omitempty" jsonschema_description:"需要从标准输入喂给命令的内容（如 --input - 要求的 JSON 文档），一般留空"`
	}
	return utils.InferTool("exec",
		"执行白名单内的运维 CLI 命令（如 tt-devops-cli）。按剧本/技能文档的调用规范构造 argv，不经 shell；协议要求 `--input -` 时把 JSON 文档放进 stdin 参数。返回 stdout、stderr 与退出码；退出码非零时先读 stderr 再决定重试或换路径。注意：排查阶段仅允许只读子命令（按 tools.exec.readonly 配置分级），变更类命令会被拒绝——变更操作不要重试，应写成报告中 risk=mutating 的动作（tool=exec，args 携带完整 argv 与 stdin），经人工审批后执行。不要尝试白名单外的解释器（python3/sh 等）。",
		func(ctx context.Context, in input) (Result, error) {
			env, ok := FromContext(ctx)
			if !ok {
				return Result{}, fmt.Errorf("当前上下文无执行环境（exec 未启用或装配缺失）")
			}
			return env.Run(ctx, in.Argv, in.Stdin)
		})
}

// NewReadTool 构造 read 工具：限定在剧本目录内读文件（SKILL.md、references/*.md）。
func NewReadTool(skillsDir string) (tool.InvokableTool, error) {
	root, err := filepath.Abs(skillsDir)
	if err != nil {
		return nil, err
	}
	type output struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	return utils.InferTool("read",
		"读取剧本目录内的文件内容（相对路径，如 tt-devops-shared/SKILL.md 或 tt-telemetry-log/references/logs-query.md）。用于按需加载技能文档与 references；目录外的路径一律拒绝。",
		func(_ context.Context, in struct {
			Path string `json:"path" jsonschema:"required" jsonschema_description:"相对剧本目录的文件路径"`
		}) (output, error) {
			p := filepath.Clean(in.Path)
			// 预期内失败一律作为正常输出返回（软失败），不打挂引擎
			if filepath.IsAbs(p) || strings.HasPrefix(p, "..") {
				return output{Content: "错误：只接受剧本目录内的相对路径: " + in.Path}, nil
			}
			full := filepath.Join(root, p)
			// Clean 后再校验一次前缀，堵住符号链接外的构造绕过
			rel, err := filepath.Rel(root, full)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return output{Content: "错误：路径越界: " + in.Path}, nil
			}
			b, err := os.ReadFile(full)
			if err != nil {
				return output{Content: "错误：读取失败: " + err.Error()}, nil
			}
			const maxRead = 96 * 1024
			if len(b) > maxRead {
				return output{Content: string(b[:maxRead]), Truncated: true}, nil
			}
			return output{Content: string(b)}, nil
		})
}
