# alert-agent 系统设计

> 状态：v0.6（2026-09-18，P0/P1/P2 实测验收 + 内置工具/语义路由/风险分级/取消/评测/可观测性落地）
> 定位：**通用、完全可插拔的告警排查 Agent 框架**。本仓库只提供架构骨架与核心契约，
> 告警来源、排查剧本、工具、通知渠道、执行策略——一切交由用户配置与扩展。
> 用户目标形态：**只编写 SKILL.md + 做配置，零代码**（详见 §2.2 三层扩展模型与 §10 零代码路线）。

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
| 零代码 | 声明式配置：SKILL.md 编写 + webhook 源（内置解析器：**alertmanager 已实现**；grafana / jmespath / regex **规划中未实现**，见 §10 N3）+ 飞书源/通知 + MCP server / CLI 白名单接入 + 评测集 evals YAML | **目标形态**：Alertmanager→飞书 场景已全链路零代码可用 |
| 内置集 | 代码插件 PR 进主仓库，`plugin.RegisterSource/Notifier/Stage(...)` 编译期注册（Terraform/Caddy 模式） | 新告警源（钉钉/企微）、新通知渠道、新解析器——照 feishu/webhook 包各一文件（§10 N1/N2/N3） |
| out-of-tree | 用户自起进程暴露 HTTP，框架以 webhook 源/通知器对接（sidecar 模式）；或提供 MCP server 纯配置接入 | 深度定制的长尾场景 |

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
- **工具双通道（v0.6 落地形态）**：能力接入两条并列通道，混用合法，均受剧本
  `tools:` 收权——
  1. **exec/read 内置工具**（skills→CLI 生态，主通道）：剧本/技能文档即调用规范，
     `exec` 按白名单 argv 直传执行（stdin 支持，经 local 宿主机或 docker 沙箱
     ——官方 CLI 镜像、一事件一容器、runtime token 注入、凭证不落镜像层）；
     `read` 限定剧本目录内按需加载技能文档与 references（渐进加载的运行时载体）。
  2. **MCP server**（eino-ext 适配器）：任意 stdio server 的工具桥接，目录式
     `mcp/<name>.yaml` + 内联配置；失败降级不打挂服务。
  前身 `load_skill` 占位由 `read` 实现（过程按需加载：证据指向变化时 agent 主动
  read 另一技能文档接着查）。
- **剧本路由（三级，v0.6 全部落地）**：route 规则指定 → 规则匹配（labels +100 /
  keywords +50，severity 白名单）→ **router 小模型语义匹配**（候选清单内选择，
  none/清单外/烂 JSON/调用失败全部 fail-closed 回落 generic，reason 记日志可审计）
  → 通用兜底。router 未配置时自动降级两级。
- **工具收权**：每次排查按 `skill.tools` 动态构造 ToolsConfig 注入
  ChatModelAgent——单次排查只能用剧本声明的工具（最小权限）。
- **证据链强制（v0.6 机制升级：labeledTool）**：证据编号在工具调用处分配并
  **写进返回内容开头**（"[T5] {...}"）——模型看得见自己的证据编号，报告 evidence
  才能引用真实数据步（早期编号只在落库侧生成，模型只能瞎猜，已在剧本层与
  机制层双重修复）。每步 IO 落 trace，`-replay` 可回放。
- **预算控制（v0.6 完整落地）**：`agent.max_iterations`（模型轮次）+ 
  `agent.max_tokens`（in+out 累计，tokenModel 包装器计量 ResponseMeta.Usage），
  超限熔断产出"部分结论"报告；去重前置避免重复排查烧钱。
- **工具风险分级（v0.6 落地，fail-closed）**：`tools.exec.readonly` 按
  bin→词对齐前缀声明只读子命令；未命中（含裸二进制）一律按变更拒绝执行。
  排查阶段持只读环境（Acquire），审批通过后持全量环境（AcquireFull）执行；
  落审批前做风险归一化（配置分类覆盖模型自报）。报告 mutating 动作必须携带
  可执行 argv 且与技能文档逐字一致——文档没有的能力写成人工路径建议，严禁
  虚构子命令（执行前另有子命令存在性探测兜底）。
  ⚠️ 未竟项：MCP 工具目前只有剧本收权，readonly/mutating 声明与审批执行
  尚未对齐 exec 水位（§10 路线 N4）。
- **取消纪律（v0.6 落地）**：AlertEvent.Status 相位字段（firing/resolved，
  不进 labels 保指纹一致）；inflight 指纹注册表——resolved 到达按指纹取消全部
  在途排查（预管道分支，绕过 dedup 同指纹丢弃），被取消的排查静默退出不发失败卡。
  所有阻塞点（LLM、工具、通知）尊重 ctx；沙箱清理用独立 ctx（排查取消不得
  阻断容器回收）。
- **隔离层**：DiagnosisRunner 是 Eino 的唯一接触面；若内置 loop 无法满足
  证据链/预算/收权的细粒度要求，在隔离层内换自研 loop，上层与契约不动。

## 5. 人机闭环（飞书自建应用）

交互卡片按钮：**认领 / 追问（进入会话继续查）/ 误报 / 根因确认 / 批准|拒绝执行**。

- 卡片回调 → 框架事件 → 更新告警状态、触发后续动作或续查会话；
- **审批是异步跨重启的**：报告发出 → 落库（建议动作 + 状态 pending-approval）→
  人点卡片（可能几分钟后、可能跨进程重启）→ 回调匹配待审批记录 → 执行 →
  结果跟进卡片。这是显式状态机，不用运行中 interrupt 模型；
- 闭环指令（v0.6）：认领 / 误报 / 根因确认 / 批准|拒绝 / **重查**（走追问续查
  通道：带原事件上下文、当前时刻回溯，比全新排查省预算）；**回复与引用两个
  入口都接指令**（被引内容含事件 ID 即分发——用户未必分得清飞书的回复/引用）；
  同卡重复引用被 dedup 合并时回提示卡（上次结论摘要 + 事件 ID + 重查引导），
  引用是人的主动动作不允许沉默。空引用（仅@占位）回引导卡不产出废告警。
- 所有反馈落库（feedback 表），蒸馏（`-distill`）：把误报模式、根因确认沉淀为
  剧本修订建议，用户自然语言确认后生效——系统越用越准，且用户改的是
  剧本不是代码。合并建议后用评测集（§9）回归验证不退化。

### 引用卡片提取器插件化（后续优化项，暂缓）

引用消息排查目前内置一个**宽松格式**提取器：容忍 title 为字符串/对象、
elements 任意嵌套，展平为可读文本——不绑定任何平台，但也不理解卡片结构
（字段全部进 description 正文，labels 只有 via/chat_id）。

若后续出现需要**结构化归一化**的场景（把卡片里的 对象类型/级别/业务线 等
字段映射为 AlertEvent.labels，让 labels 规则路由与聚合指纹生效），则把提取
器抽为 feishu 包内的注册制扩展点（对齐 §2.2 三层扩展模型：内置集编译期注册）：

```go
// internal/feishu：QuotedCardExtractor 按消息类型注册，source 装配时选用
type QuotedCardExtractor interface {
    Match(msgType, content string) bool
    Extract(msgType, content string) (title, desc string, labels model.Labels)
}
```

触发条件（满足其一才做，避免过度设计）：出现第 2 种非标准卡片格式无法
宽松兼容；或业务要求卡片字段进 labels 参与路由/聚合。在此之前，宽松提取器
够用且零维护。

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
- **P2（越用越准）**：aggregate incident 聚合（窗口合并+风暴阈值升级+上下文注入）、
  剧本蒸馏（`-distill`：反馈→LLM 修订建议，不自动生效）、通知并行扇出+重试。
  **xdag 决策（2026-09-16 二次修订）**：通知扇出已改造为 xdag 任务图
  （internal/fanout：每通道一个任务，RetryPolicy 指数退避按次计费，取消单通路收敛；
  实测通过重试/隔离/退避三用例，注意点：Execute 返回的 error 是任务级失败聚合，
  基础设施判定以 States() 为准）。该包同时是未来 incident 级编排的接入模板。
- **v0.6（2026-09-17/18，工具生态与闭环强化，全部实弹验证）**：exec/read 内置
  工具（local/docker 双后端，docker=官方 CLI 镜像一事件一容器）；labeledTool
  证据编号进返回内容（修复并发回填错位与模型瞎编证据号）；语义路由；工具
  风险分级（readonly 词对齐 fail-closed + 排查/审批双环境）+ 审批执行器重写
  （exec argv 直达 + 子命令存在性探测拦截臆造命令）；inflight resolved 取消；
  追问续查 + 重查指令 + 引用双入口 + dedup 提示卡；评测集（`-eval` route/full
  两档 + `-eval-add` 从真实事件沉淀）；`/metrics`（事件/路由分级/排查结果/
  耗时/步数/token/在途 gauge/通知）；时区纪律（事件 UTC、SQL 字面量北京时间）。
  实弹战果：引用→命中剧本→沙箱查生产库→带真实证据编号根因全链路多次验证，
  `needs_human=false` 自动收敛案例产出。

## 10. 零代码路线（用户目标形态：只写 SKILL.md + 配置）

| # | 项 | 现状 → 目标 |
|---|-----|------------|
| N1 | 通知渠道插件集（钉钉/企业微信/webhook-out） | 仅飞书+log → 照 feishu 包各一文件，配置即用 |
| N2 | 告警源插件集（钉钉/企微机器人入向） | 仅飞书+webhook → source 插件 |
| N3 | 解析器补齐（grafana / jmespath / regex） | 仅 alertmanager（§2.2 承诺兑现） |
| N4 | MCP 工具风险分级与审批执行对齐 exec 水位 | MCP 仅剧本收权，readonly/mutating 声明与批准后执行未覆盖 |
| U0 | 配置 API 化 + 热加载（配置中心前置） | 现为 YAML 启动加载一次 → REST CRUD + 校验复用 validate |
| U1 | Web 配置中心（skills/工具/入口/管道/运行视图） | 依赖 U0；Skill 管理与评测按钮（U2）基于 §9 评测引擎 |

遗留（外部协调）：Agent 专用 token（个人凭证需替换）、远程部署（linux-x64
镜像 + 正式配置）、GitLab 技能库回写、蒸馏闭环实测（feedback 攒量）。
