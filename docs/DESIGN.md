# alert-agent 系统设计

> 状态：v0.4 设计定稿（2026-09-15，补充 skill 两阶段加载语义与 load_skill）
> 定位：**通用、完全可插拔的告警排查 Agent 框架**。本仓库只提供架构骨架与核心契约，
> 告警来源、排查剧本、工具、通知渠道、执行策略——一切交由用户配置与扩展。

## 0. 已确认的关键决策

| 决策点 | 结论 |
|---|---|
| 语言/栈 | **Go 1.27+**（对齐 xdag 硬性下限）+ net/http + goroutine/channel |
| 架构形态 | **混合**：外层确定性流水线（吸收 DAG 方案优点）+ 内层排查 Agent Loop（Skills + MCP） |
| Agent 内核 | **基于 [Eino](https://github.com/cloudwego/eino)（CloudWeGo，Apache-2.0）**：ChatModel/Tool 抽象 + 内置 ReAct loop + callbacks；自研 DiagnosisRunner 薄封装（剧本加载/收权/预算/证据链），套隔离层，可整体回退自研 loop |
| MCP 接入 | eino-ext `components/tool/mcp` 适配器（MCP server 工具 → Eino Tool），用户配置任意 server |
| 管道 DAG 化 | P2 引入 [xdag](https://github.com/xmapst/xdag)（MIT，零第三方依赖，纯 Go）。**不用 Eino compose.Graph 做管道**——管道与 LLM 框架解耦，换 agent 层不影响管道 |
| 动作边界 | 目标形态：**仅诊断建议 + 人工审批后执行**；P0 一律仅建议 |
| 飞书接入 | 已有自建应用 → 消息事件订阅 + 卡片回调可用，双向闭环无障碍 |
| LLM 接入 | Eino ChatModel 抽象（eino-ext：OpenAI/Claude/Gemini/Ark/Ollama…），OpenAI 兼容协议 `base_url` + `model` 全可配；模型分级（路由小模型 / 排查大模型） |
| 部署 | 单静态二进制（CGO_ENABLED=0，modernc.org/sqlite 纯 Go 无 CGO），容器/裸机皆可 |

### 为什么不是纯 DAG + Function Calling

DAG 的强项是**管道侧**：确定性流转、重试、并行扇出、升级定时器、可视化审计。
但排查本身是观测驱动的分支决策，预画 DAG 要么组合爆炸，要么退化成一个大
"问 LLM" 节点（其内部仍是 agent loop）。因此：

- **管道**（收→归一化→去重→聚合→路由→通知扇出）：stages 顺序可配、可增删。
  P0 用线性 stages + 条件分支；需要图状编排（incident 聚合、多渠道扇出）时
  直接引入 xdag——其状态机（含 Skipped 一等状态）、按次计费的重试、单
  context 取消收敛，正是管道需要的形态，不再自研 DAG 引擎；
- **排查**：agent loop 按剧本（Skill）执行，剧本是用户可写的 Markdown，
  而不是画布上的节点。

### Eino 的取舍（用什么、不用什么）

| Eino 能力 | 取舍 | 理由 |
|---|---|---|
| ChatModel / Tool 抽象 | ✅ 用 | 模型供应商一套抽象全覆盖，OpenAI 兼容 base_url 可配 |
| ChatModelAgent 内置 ReAct loop | ✅ 用 | 替代自研 ~1k 行循环；流式事件迭代器对追问会话有用 |
| eino-ext MCP 适配器 | ✅ 用 | MCP server 工具 → Eino Tool 桥接现成，不自己写 |
| Callbacks（OnStart/OnEnd/OnError） | ✅ 用 | 承载全链路 trace 与证据链记录（工具调用编号） |
| compose.Graph / Workflow | ❌ 不用 | 管道与 LLM 框架解耦；xdag 的进程控制语义（挂起/恢复/按次计费并发）更贴合告警管道 |
| DeepAgent / 中断恢复（HITL） | ❌ 不用 | 我们的审批是异步跨重启的（报告发出→人点卡片可能几分钟后、可能跨进程重启），必须落库做状态机，运行中 interrupt 模型对不上 |

**核心风险与对策**：Eino 的 ReAct loop 是内置的，证据链编号、per-step 预算、
按剧本收权是我们的硬诉求——P0 第一周做 spike 验证（ToolsConfig 按次动态构造 +
callback 记录工具调用），不满足则经隔离层回退自研 loop，契约不动。
Eino 尚未发正式 1.0，go.mod 锁定版本，升级走独立 PR。

## 1. 总体架构

```
飞书(自建应用/@机器人)  Alertmanager  Grafana  自定义webhook  ...
        │                 │            │          │
        └────────┬────────┴─────┬──────┴──────────┘
                 ▼              ▼
          Source 插件 ──► AlertEvent（统一事件模型，落库）
                 │
                 ▼
          Pipeline（可配置 stages，进程内 channel）
          去重 → 聚合 → 静默 → 路由（定级/选剧本/选通知目标）
                 │
                 ▼
          排查 Agent ── Skills（排查剧本，两阶段加载）
                 │      MCP servers（Prometheus/k8s/DB/内部系统…用户接入）
                 ▼ DiagnosisReport（结构化报告 + 证据链）
          策略层：suggest-only | approval-required | auto+whitelist
                 │
                 ▼
          Notifier 插件（飞书卡片/Slack/邮件/webhook）
                 │
                 ▼
          人工反馈（认领/追问/误报/根因确认/审批按钮，飞书卡片回调）
                 │
          反馈库 ──► 剧本(Skill)持续修订（P2：蒸馏成修订建议）
```

排查 Agent 内部（v0.3）：

```
DiagnosisRunner（自研薄封装，隔离层）
├── 剧本装配：路由命中 → 注入剧本全文 + 按 skill.tools 构造 ToolsConfig（收权）
├── 预算控制：max_steps / token 上限（超限产出"部分结论"报告）
├── 证据链：callback handler 记录每次工具调用（编号 + IO），写入 trace
└── Eino ChatModelAgent（内置 ReAct loop）
    ├── ChatModel：eino-ext（OpenAI 兼容，base_url 可配）
    └── Tools：eino-ext MCP 适配器 + 内置工具（httprequest / 历史告警查询）
```

## 2. 核心契约（框架定义死的四样东西）

### 2.1 统一事件模型 AlertEvent

所有源归一化到该模型。这是全系统最重要的契约：

```yaml
id: evt_01H...                 # 内部 ULID
fingerprint: sha256(...)       # 由 labels 规范化生成 → 去重/聚合键
source: feishu-message         # 来源插件标识
severity: critical             # critical | warning | info（映射规则可配）
title: ...
description: ...
labels: { service: order-api, env: prod }
occurred_at: ...
received_at: ...
raw: { ... }                   # 原始报文留底（回放、排查解析问题用）
refs: { feishu_message_id: ... }   # 来源侧关联（回复/引用用）
```

### 2.2 插件接口（Go interface，编译期注册）

```go
// pkg/plugin — 插件作者唯一需要 import 的包
type Source interface {
    Name() string
    Start(ctx context.Context, emit func(context.Context, *model.AlertEvent) error) error
}

type Notifier interface {
    Name() string
    Notify(ctx context.Context, report *model.DiagnosisReport) error
}

// Action 让 stage 能表达"跳过"而非只有成败（借鉴 xdag 的 Skipped 一等状态）
type Action int
const (ActionContinue Action = iota; ActionSkip; ActionDrop)

type Stage interface {
    Name() string
    Process(ctx context.Context, evt *model.AlertEvent) (*model.AlertEvent, Action, error)
}
```

**三层扩展模型**（Go 无动态加载，插件体验按此分层）：

| 层 | 方式 | 适用 |
|---|---|---|
| 零代码 | 声明式配置：webhook 源 + 内置解析器（alertmanager / grafana / jmespath / regex）；飞书源 | 80% 接入场景 |
| 内置集 | 代码插件 PR 进主仓库，`plugin.RegisterSource(...)` 编译期注册（Terraform/Caddy 模式） | 常用源/通知器，生态聚合 |
| out-of-tree | 用户自起进程暴露 HTTP，框架以 webhook 源/通知器对接（sidecar 模式） | 深度定制的长尾场景 |

### 2.3 Skill 规范（对齐 Agent Skills 惯例）

格式与 Claude Code / ZCode 生态互通（YAML frontmatter + Markdown 正文）：

```markdown
---
name: pg-conn-exhaust
description: PostgreSQL 连接耗尽 / too many connections 类告警排查
triggers:
  keywords: ["too many connections"]
  labels: { component: postgres }
severity: [critical, warning]
tools: [prometheus, pg-readonly]    # 本剧本允许用的工具（收权）
---
## 排查步骤
1. 查 pg 连接数与上限趋势（prometheus: pg_stat_activity_count）...
2. 持续高位则按 application_name 分组找 Top 来源...
## 判断标准
- 连接数贴近 max_connections 且 idle 占比高 → 疑似泄漏，建议...
```

### 2.4 配置入口（config.yaml）

```yaml
llm:
  router:   { base_url: ..., model: <小模型> }   # 分类/选剧本/去重辅助
  reasoner: { base_url: ..., model: <大模型> }   # 排查主力

sources:
  - type: feishu-app              # 自建应用事件订阅（已有应用，直接可用）
    app_id: ${FEISHU_APP_ID}
  - type: webhook
    path: /hooks/alertmanager
    parse: alertmanager           # 内置解析器，可换 jmespath/regex 规则

pipeline:
  - { stage: dedup, key: fingerprint, window: 5m }
  - { stage: silence, match: "env=staging" }
  - { stage: route, rules: [...] }   # 定级/选剧本/选通知目标

skills: { dir: ./skills }

mcp:
  servers:
    prometheus: { command: ... }     # 用户接入任意 MCP server
    k8s: { command: ... }

policy: { execution: approval-required }   # suggest-only | approval-required | auto+whitelist

notifiers:
  - type: feishu-card
    chat_id: ...
```

配置文件支持 `${ENV_VAR}` 环境变量展开（凭证不落盘）。

## 3. 技术栈清单

| 组件 | 选型 | 说明 |
|---|---|---|
| HTTP 服务 | net/http（1.22+ 路由）+ chi（如需中间件） | webhook 接入、卡片回调、健康检查 |
| 契约模型 | 结构体 + validator；santhosh-tekuri/jsonschema | Skill frontmatter / LLM 结构化输出校验 |
| Agent 内核 | **Eino**（cloudwego/eino）+ eino-ext | ChatModelAgent ReAct loop、callbacks；DiagnosisRunner 自研薄封装 |
| LLM 接入 | eino-ext model（OpenAI 兼容等） | base_url 可配，覆盖 99% 供应商与内网网关 |
| MCP | eino-ext `components/tool/mcp` | MCP server 工具桥接为 Eino Tool |
| 飞书 | larksuite/oapi-sdk-go/v3（官方） | 事件订阅、卡片回调、发消息 |
| 配置 | gopkg.in/yaml.v3 + env 展开 | 单文件入口，viper 暂不引入 |
| 存储 | modernc.org/sqlite（纯 Go）→ PostgreSQL | 零运维起步；表 events / traces / feedback |
| 日志 | log/slog | 标准库结构化日志 |
| 管道引擎 | P0 自研线性 stages；P2 xdag | xdag：MIT、零依赖、Go 1.27+；不用 Eino compose |
| 并发原语 | context / channel / errgroup | per-alert context 取消收敛（告警 resolved 时取消在途排查） |

## 4. 排查引擎（内核）设计

- **两阶段 skill 加载（按需加载 / progressive disclosure）**：与 Claude Code /
  ZCode 的 Agent Skills 机制同构——常驻上下文的只有目录索引（frontmatter 的
  name + description + triggers，每个约百来 token），路由命中后才注入剧本全文，
  剧本可涨到上百个而不撑爆上下文。设计差异在**决策者**：Claude Code 由模型
  自行浏览目录决定加载哪个 skill（model-driven），alert-agent 的入口决策交给
  路由器（规则匹配 → 小模型语义匹配 → 通用剧本兜底）——告警入口要求确定性、
  可审计、低成本，不能每条告警都让排查大模型先翻一遍目录。
- **load_skill 内置工具（过程按需加载，P1）**：排查中途证据指向变化时，agent
  可主动调用 `load_skill` 把另一个剧本的全文拉进上下文接着查（如 cpu-high
  剧本查到一半证据指向连接池泄漏 → 换 pg-conn-exhaust 剧本）。至此按需加载
  完全体：入口按需（路由器选）+ 过程按需（agent 中途补）。P0 先在
  ToolRegistry 占位注册、不实现，位置很便宜。
- **剧本路由**：规则优先（labels / keywords 精确匹配）→ 未命中用 router 小模型
  语义匹配 → 仍未命中走通用排查剧本兜底，报告中明示"无专用剧本"。
- **工具收权**：每次排查按 `skill.tools` 动态构造 ToolsConfig 注入
  ChatModelAgent——单次排查只能用剧本声明的工具（最小权限）。
- **证据链强制**：callback handler 记录每次工具调用（编号 + 输入输出）写入
  trace；报告中每个结论必须引用工具调用编号——这是 oncall 信任的根基。
- **预算控制**：单告警 max_steps / token 上限，超限产出"部分结论"报告；
  去重前置避免重复排查烧钱。
- **工具风险标注**：所有工具注册时声明 `read-only | mutating`；
  mutating 动作无论策略如何配置，默认必须走审批卡片。
- **取消纪律**：每条告警一个 context 派生链（接收→排查→跟进），resolved
  事件触发取消；所有阻塞点（LLM 调用、MCP、通知）必须尊重 ctx——收敛到
  单一取消通路，避免局部泄漏（xdag 文档中确认的同类经验）。
- **隔离层**：DiagnosisRunner 是 Eino 的唯一接触面；若内置 loop 无法满足
  证据链/预算/收权的细粒度要求，在隔离层内换自研 loop，上层与契约不动。

## 5. 人机闭环（飞书自建应用）

交互卡片按钮：**认领 / 追问（进入会话继续查）/ 误报 / 根因确认 / 批准|拒绝执行**。

- 卡片回调 → 框架事件 → 更新告警状态、触发后续动作或续查会话；
- **审批是异步跨重启的**：报告发出 → 落库（建议动作 + 状态 pending-approval）→
  人点卡片（可能几分钟后、可能跨进程重启）→ 回调匹配待审批记录 → 执行 →
  结果跟进卡片。这是显式状态机，不用运行中 interrupt 模型；
- 所有反馈落库（feedback 表），P2 做"蒸馏"：把误报模式、根因确认沉淀为
  剧本修订建议，用户自然语言确认后生效——系统越用越准，且用户改的是
  剧本不是代码。

## 6. 目录结构规划

```
alert-agent/
├── cmd/alert-agent/main.go   # 入口：配置装载 → 插件注册 → 生命周期
├── internal/
│   ├── pipeline/             # 线性 stages 引擎（P2 升级 xdag）
│   ├── agent/                # DiagnosisRunner：剧本装配/收权/预算/证据链
│   │   └── eino/             # Eino 集成隔离层（ChatModelAgent + callbacks）
│   ├── llm/                  # router/reasoner 双客户端（eino-ext model）
│   ├── mcp/                  # MCP server 装配（eino-ext mcp 适配器）
│   ├── feishu/               # 飞书 source + notifier + 卡片回调
│   ├── webhook/              # 通用 webhook source + 解析器集
│   ├── policy/               # 执行策略层（审批状态机/白名单）
│   └── store/                # SQLite（events / traces / feedback / approvals）
├── pkg/
│   ├── model/                # AlertEvent / DiagnosisReport 契约（公开）
│   └── plugin/               # Source/Notifier/Stage 接口 + 注册函数（公开）
├── config.yaml
├── skills/examples/          # 示例剧本（pg-conn-exhaust 等）
└── Makefile                  # build / test / lint（远程 yup-dev 上执行）
```

`pkg/` 只放插件作者需要引用的契约与接口；其余实现全部 `internal/`。
Eino 相关 import 全部收在 `internal/agent/eino/` 隔离层内。

## 7. 一条告警的完整链路（示例）

1. Alertmanager 打到 `/hooks/alertmanager`（或同事在飞书 @机器人 转发告警消息）；
2. Source 插件解析 → AlertEvent，落库，状态 `firing`；
3. dedup：同 fingerprint 5 分钟窗口合并计数，不重复排查；
4. route：匹配 `service=order-api, env=prod` → critical，候选剧本
   `[http-5xx-spike, k8s-crashloop]`；
5. Agent 加载剧本，按 skill.tools 收权构造工具集，调 MCP 工具查
   Prometheus / k8s / 日志，每次调用经 callback 记入证据链；
6. 产出报告：根因假设（"v1.2.3 发布引入，置信 0.8"）+ 证据链 + 建议（回滚）；
7. 建议动作含写操作 → 策略层发飞书审批卡片（建议动作落库 pending-approval）；
8. 人点「批准」→ 回调 → agent 执行回滚 → 结果跟进卡片；点「误报」→ 落反馈库。

## 8. 风险与对策

| 风险 | 对策 |
|---|---|
| Eino 内置 loop 细粒度控制不足（证据链/预算/收权） | P0 首周 spike 验证；DiagnosisRunner 隔离层可整体回退自研 loop，契约不动 |
| Eino 未发正式 1.0，API 可能变动 | go.mod 锁定版本；升级独立 PR 评审 |
| Go 代码插件无动态加载 | 三层扩展模型（声明式 / 内置集编译期注册 / sidecar）；主扩展路径（配置、剧本、MCP）本就语言无关 |
| 成本/时延 | 去重前置、step/token 预算、模型分级（路由小、排查大） |
| webhook 重复投递 | fingerprint 幂等去重 |
| 凭证安全 | 集中配置 + `${ENV}` 注入，不落盘 |
| 可调试性 | 每告警全链路 trace（stage + agent 每步 IO），`replay` 子命令离线重放 |
| 关联聚合 | incident 聚合比去重难，P1/P2 落地（届时引入 xdag），fingerprint 设计已留余地 |
| 依赖面 | 核心依赖：Eino/eino-ext、oapi-sdk-go、sqlite 驱动；管道引擎零依赖起步 |

## 9. 分期路线图

- **P0（跑通最小闭环）**：webhook 源（alertmanager 解析器）+ AlertEvent 契约 +
  pipeline 骨架（dedup/route）+ **Eino spike**（验证 callbacks 证据链 / ToolsConfig
  收权 / 预算控制，不满足即回退自研 loop）+ 2 个示例剧本 + 1 个 Prometheus MCP +
  飞书卡片单向通知 + SQLite + 全链路 trace + replay。策略层仅 suggest-only。
- **P1（双向闭环）**：飞书自建应用双向（@机器人作为源、卡片按钮回调/审批/追问）、
  silence/aggregate 完整 stage 化、反馈落库、审批执行状态机。
- **P2（越用越准）**：引入 xdag 完成管道 DAG 化（incident 聚合/多渠道扇出）、
  剧本蒸馏（反馈→修订建议）、更多源/通知插件、多租户权限、评估集回归测试。
