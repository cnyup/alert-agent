# alert-agent

通用、完全可插拔的**告警排查 Agent 框架**：任意来源告警 → 确定性管道（聚合/静默/路由）→
LLM 按剧本排查（Skills + MCP 工具，证据链可回放）→ 报告卡片（飞书）→ 人工审批执行/反馈 →
反馈蒸馏回剧本，越用越准。

- 设计文档：[docs/DESIGN.md](docs/DESIGN.md) · 架构图：[docs/diagrams/alert-agent-architecture.html](docs/diagrams/alert-agent-architecture.html)
- 技术栈：Go 1.27 · [Eino](https://github.com/cloudwego/eino)（ReAct 内核）· MCP · SQLite · 飞书长连接

## 快速开始

```bash
# 构建（需 Go 1.27+）
make build && make test

# 配置凭证（不落 git）
cp config.example.yaml config.yaml
cat > .env <<'EOF'
LLM_REASONER_BASE_URL=https://api.siliconflow.cn/v1
LLM_REASONER_MODEL=Qwen/Qwen2.5-72B-Instruct
LLM_REASONER_API_KEY=sk-xxx
FEISHU_APP_ID=cli_xxx
FEISHU_APP_SECRET=xxx
FEISHU_CHAT_ID=oc_xxx
EOF

# 运行（webhook: /hooks/alertmanager ｜ 飞书: @机器人 或 回复报告卡片）
./bin/alert-agent -config config.yaml

# 打一条测试告警
curl -X POST localhost:8080/hooks/alertmanager -H 'Content-Type: application/json' \
  -d '{"alerts":[{"status":"firing","labels":{"severity":"critical","service":"demo","env":"prod"},"annotations":{"summary":"演示告警"}}]}'
```

## 人机闭环（飞书）

- **告警源**：群里 @机器人 发告警文本，或单聊直接发
- **审批**：回复报告卡片 `批准 A1` / `拒绝 A1`（mutating 动作必须审批后才执行）
- **反馈**：回复 `认领` / `误报` / `根因确认`
- **蒸馏**：`./bin/alert-agent -config config.yaml -distill` 把反馈生成剧本修订建议（不自动生效）

## 运维

```bash
./bin/alert-agent -config config.yaml -replay <event_id>   # 回放事件全链路 trace
```

部署：`deploy/alert-agent.service`（systemd）或 `docker build -t alert-agent .`

## 扩展模型（一切可插拔）

| 层 | 方式 |
|---|---|
| 零代码 | webhook 源 + 内置解析器（alertmanager）、配置装配管道 stages |
| 剧本 | `skills/<name>/SKILL.md`（Markdown + frontmatter，与 Agent Skills 生态互通） |
| 工具 | MCP server（配置接入，任意语言） |
| 代码 | `pkg/plugin` 工厂注册（Source/Notifier/Stage），PR 进内置集 |
