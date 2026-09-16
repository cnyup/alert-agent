# 内部主机部署步骤（裸机 / VM / systemd）

适用：一台能出网的 Linux x86_64 机器（arm64 把 Makefile 的 GOARCH 改掉重打包）。
目标机**不需要** Go / git / docker。

## 0. 网络前置检查（目标机上执行）

```bash
# 出网：飞书开放平台 + 长连接 + LLM 网关（按实际 base_url）
curl -sI --max-time 5 https://open.feishu.cn | head -1
curl -sI --max-time 5 https://api.siliconflow.cn | head -1
# 入向：记住本机 IP，告警来源（Alertmanager）要能访问 <本机IP>:8080
hostname -I
```

## 1. 构建交付包（在构建机上，如 yup-dev）

```bash
cd /root/code/alert-agent && git pull
make release          # 产出 dist/alert-agent-<日期>-linux-amd64.tar.gz
```

## 2. 传输与解包（目标机上）

```bash
scp <构建机>:<路径>/dist/alert-agent-*-linux-amd64.tar.gz /tmp/
sudo mkdir -p /opt/alert-agent
sudo tar -C /opt -xzf /tmp/alert-agent-*-linux-amd64.tar.gz
sudo mv /opt/alert-agent-*-linux-amd64/* /opt/alert-agent/ && sudo rmdir /opt/alert-agent-*-linux-amd64
# /opt/alert-agent 结构：bin/ skills/ config.example.yaml alert-agent.service
sudo mkdir -p /opt/alert-agent/{data,mcp}
```

## 3. 配置

```bash
cd /opt/alert-agent
sudo cp config.example.yaml config.yaml
sudo vi config.yaml     # 必改项见下
sudo vi .env            # 凭证，见下
sudo chmod 600 .env
```

**config.yaml 必改**：

| 项 | 说明 |
|---|---|
| `skills.dir` | 默认 `./skills`（/opt/alert-agent/skills） |
| `mcp.dir` | 默认 `./mcp`，`mcp/<name>.yaml` 一个文件一个 server |
| `sources` | webhook 路径/解析器；飞书 app 凭证走 `${}` |
| `pipeline` | aggregate/silence/route 按需调整 |
| `notifiers` | feishu-card 的 chat_id 走 `${FEISHU_CHAT_ID}` |

**`.env` 内容**（config.yaml 里的 `${}` 从这里/环境变量展开）：

```bash
LLM_REASONER_BASE_URL=https://api.siliconflow.cn/v1
LLM_REASONER_MODEL=Qwen/Qwen2.5-72B-Instruct
LLM_REASONER_API_KEY=sk-xxx
FEISHU_APP_ID=cli-xxx
FEISHU_APP_SECRET=xxx
FEISHU_CHAT_ID=oc-xxx
```

## 4. systemd 安装

```bash
sudo cp /opt/alert-agent/alert-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now alert-agent
```

单元文件默认按 `/opt/alert-agent` 部署（含 `ReadWritePaths=/opt/alert-agent/data` 加固），
换了目录要同步改 `WorkingDirectory`/`ExecStart`/`ReadWritePaths` 三处。

## 5. 验证

```bash
systemctl status alert-agent                  # active (running)
journalctl -u alert-agent -n 20               # 应有：剧本索引就绪 / 排查内核就绪 / 飞书长连接就绪 / webhook 源就绪
curl -s localhost:8080/healthz                # ok

# 全链路：打一条测试告警，约 30~60s 后飞书群收到报告卡片
curl -X POST localhost:8080/hooks/alertmanager -H 'Content-Type: application/json' \
  -d '{"alerts":[{"status":"firing","labels":{"severity":"critical","service":"deploy-check","env":"prod"},"annotations":{"summary":"部署验证"}}]}'

# 飞书侧：引用任意告警消息 + @机器人 → 报告卡片；回复报告卡片"认领/误报/批准 A1"验证闭环
```

## 6. 日常运维

| 操作 | 命令 |
|---|---|
| 加剧本 | `mkdir skills/<name> && vi skills/<name>/SKILL.md && systemctl restart alert-agent` |
| 加 MCP | `vi mcp/<name>.yaml`（可执行 server 放 `bin/`，配置里写绝对路径）→ restart |
| 看日志 | `journalctl -u alert-agent -f` |
| 回放事件 | `/opt/alert-agent/bin/alert-agent -config config.yaml -replay <event_id>` |
| 蒸馏反馈 | `... -distill`（产出 data/distill-*.md 修订建议） |
| 升级版本 | 构建机 `make release` → 替换目标机 `bin/` → `systemctl restart alert-agent` |
| 备份 | `data/*.db`（SQLite 单文件，停服拷贝或 sqlite3 .backup） |

## 注意

- **单实例**：一台机器只跑一个实例（SQLite 单写者 + 飞书长连接唯一订阅）；多实例会重复消费消息。
- MCP server 脚本依赖解释器（python3/node）需目标机自备；二进制 server 直接放 `bin/`。
- LLM reasoner 未配置时服务照常起（管道+落库模式，日志有 WARN）。
