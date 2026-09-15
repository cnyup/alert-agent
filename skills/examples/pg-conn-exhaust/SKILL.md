---
name: pg-conn-exhaust
description: PostgreSQL 连接耗尽 / too many connections 类告警排查剧本
triggers:
  labels:
    component: postgres
  keywords: ["too many connections", "connection refused", "连接耗尽", "连接池"]
severity: [critical, warning]
tools: [prometheus_query, psql_readonly]
---
## 排查步骤

1. **连接数事实**：查 `pg_stat_activity_count` 与 `max_connections` 的比值趋势
   （prometheus_query），确认是否贴近上限以及起点。
2. **连接来源**：若持续高位，按 `application_name` / `state` 分组找 Top 来源
   （psql_readonly 查 pg_stat_activity）。
3. **区分泄漏与流量**：idle 占比高且来源集中 → 疑似应用侧连接泄漏；
   active 高且 QPS 同步涨 → 真实流量问题。

## 判断标准

- 连接数贴近 max_connections 且 idle 比例 > 60% → 高置信连接泄漏，建议定位
  泄漏服务并扩容连接池上限/重启该服务
- active 占主导且 QPS 突增 → 容量问题，建议扩容或限流

## 输出要求

结论必须引用工具调用编号（T1、T2…），无证据的猜测不得写入 root_causes。
