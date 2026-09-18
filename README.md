# alert-agent

通用、完全可插拔的**告警排查 Agent 框架**：任意来源告警 → 确定性管道（去重/聚合/静默/路由）→
LLM 按剧本排查（三级路由 + 双通道工具，证据链强制可回放）→ 报告卡片（飞书）→
人工闭环（认领/误报/根因确认/审批/重查）→ 反馈蒸馏回剧本，越用越准。

**用户目标形态：只编写 SKILL.md + 做配置，零代码。** 当前 Alertmanager→飞书 场景已全链路可用；
钉钉/企微等渠道见[扩展模型](#扩展模型一切可插拔)与 [DESIGN §10 零代码路线](docs/DESIGN.md)。

- 设计文档：[docs/DESIGN.md](docs/DESIGN.md)（v0.6）· 架构图：[docs/diagrams/alert-agent-architecture.html](docs/diagrams/alert-agent-architecture.html)
- 技术栈：Go 1.27 · [Eino](https://github.com/cloudwego/eino)（ReAct 内核）· MCP · SQLite · 飞书长连接 · Docker 沙箱

## 核心机制

- **三级剧本路由**：route 规则 → labels/keywords 规则匹配 → router 小模型语义匹配（fail-closed）→ 通用兜底
- **双通道工具**（剧本 `tools:` 统一收权）：
  - `exec`/`read` 内置工具：技能文档即调用规范——白名单 CLI 按 argv 执行（stdin 支持），宿主机或 **Docker 沙箱**（一事件一容器，凭证 runtime 注入不落镜像层）；`read` 按需加载技能 references
  - MCP server：`mcp/<name>.yaml` 目录式接入任意 stdio server
- **风险分级（fail-closed）**：`tools.exec.readonly` 词对齐前缀声明只读子命令，未声明一律按变更拒绝；变更动作（mutating）必须携带与技能文档逐字一致的 argv，经审批后执行（含子命令存在性探测，拦截臆造命令）
- **证据链强制**：每次工具调用带 `[T<n>]` 编号返回，报告结论必须引用产出数据的编号；全链路落 SQLite，`-replay` 回放
- **预算与取消**：`max_iterations` / `max_tokens` 超限产出部分结论；告警恢复（resolved）按指纹取消在途排查
- **人机闭环**：回复/引用报告卡 `认领` / `误报` / `根因确认` / `批准 A1` / `重查`（带上下文续查）；追问续查；同卡重复引用回提示卡不沉默
- **越用越准**：`-distill` 把人工反馈蒸馏为剧本修订建议（不自动生效）；**评测集**回归验证剧本修订不退化

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
LLM_ROUTER_BASE_URL=https://api.siliconflow.cn/v1     # 可选：语义路由小模型
LLM_ROUTER_MODEL=Qwen/Qwen2.5-7B-Instruct
LLM_ROUTER_API_KEY=sk-xxx
FEISHU_APP_ID=cli_xxx
FEISHU_APP_SECRET=xxx
FEISHU_CHAT_ID=oc_xxx
EOF

# 运行（webhook: /hooks/alertmanager ｜ 飞书: 引用告警卡/@机器人）
./bin/alert-agent -config config.yaml

# 打一条测试告警
curl -X POST localhost:8080/hooks/alertmanager -H 'Content-Type: application/json' \
  -d '{"alerts":[{"status":"firing","labels":{"severity":"critical","service":"demo","env":"prod"},"annotations":{"summary":"演示告警"}}]}'

# 观测
curl localhost:8080/metrics      # Prometheus 指标（事件/路由分级/排查结果/耗时/token/在途）
curl localhost:8080/healthz
```

### 编写你的第一个剧本

```markdown
---
name: mysql-conn-exhaust
description: MySQL 连接耗尽告警排查（查连接数分布、定位来源应用）
triggers:
  keywords: ["too many connections", "连接耗尽"]
severity: [critical, warning]
tools: [exec, read]        # 本剧本允许的工具
---
## 排查步骤
1. read mysql-knowhow/SKILL.md 获取连接池排查知识
2. exec 执行只读查询（按你的运维 CLI 规范），按 (user, host) 分组统计连接数
## 判断标准
- 连接数贴近 max_connections 且 sleep 占比高 → 疑似泄漏，定位来源应用
```

放到 `skills/mysql-conn-exhaust/SKILL.md`，重启即生效。评测防退化：

```bash
make eval-route   # 路由档，零 LLM 成本，常跑
make eval-full    # 完整档，每 case 一次真实排查，改剧本时跑
```

## 人机闭环（飞书）

- **告警入口**：Alertmanager webhook（`/hooks/alertmanager`）；或群里 @机器人 / **引用告警卡片**触发排查
- **闭环指令**（回复或引用报告卡，精确匹配）：`批准 A1` / `拒绝 A1` / `认领` / `误报` / `根因确认` / `重查`
- **追问续查**：回复报告卡任意自由文本，带原事件上下文继续排查
- 同卡 5 分钟内重复引用会被去重合并（回提示卡，回复 `重查` 可带上下文重查）
- **蒸馏**：`./bin/alert-agent -config config.yaml -distill` 把反馈生成剧本修订建议（不自动生效）

## 运维

```bash
./bin/alert-agent -config config.yaml -replay <event_id>   # 回放事件全链路 trace
./bin/alert-agent -config config.yaml -eval-add <event_id> # 从真实事件沉淀评测 case
./bin/alert-agent -config config.yaml -eval route|full     # 剧本评测
```

沙箱镜像（docker 后端）：`deploy/sandbox/Dockerfile`（官方 CLI 装入 debian slim，`TT_YW_AUTHORIZATION` 运行时注入）。
部署：`deploy/alert-agent.service`（systemd）或 `Dockerfile`。

## 扩展模型（一切可插拔）

| 层 | 方式 |
|---|---|
| 零代码 | SKILL.md 编写；Alertmanager webhook + 飞书源/通知；MCP server / CLI 白名单接入；评测集 YAML |
| 剧本 | `skills/<name>/SKILL.md`（Markdown + frontmatter，与 Agent Skills 生态互通；渐进加载 references） |
| 工具 | MCP server（`mcp/<name>.yaml` 配置接入任意语言）；CLI 白名单（`tools.exec`） |
| 代码 | `pkg/plugin` 工厂注册（Source/Notifier/Stage），PR 进内置集——钉钉/企微渠道、新解析器照 feishu/webhook 包各一文件 |

> 状态与路线：解析器目前内置 alertmanager（grafana/jmespath/regex、钉钉/企微渠道、MCP 风险分级对齐、配置中心 UI 见 DESIGN §10）。
