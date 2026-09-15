---
name: http-5xx-spike
description: HTTP 5xx 错误率飙升类告警的通用排查剧本
triggers:
  labels:
    category: http
  keywords: ["5xx", "错误率", "error rate"]
severity: [critical, warning]
tools: [prometheus_query, k8s_get_pod, query_logs]
---
## 排查步骤

1. **确认错误率事实**：查询最近 30 分钟 5xx QPS 与错误率趋势（prometheus_query，
   如 `sum(rate(http_requests_total{code=~"5.."}[5m])) by (service)`），记录是否突增及起点时间。
2. **关联变更**：错误率突增起点前后 30 分钟内是否有发布/配置变更（查发布记录或
   k8s_get_pod 的 pod age / restarts）。
3. **看下游**：错误是自身产生还是下游拖垮——查依赖服务 P99 延迟与连接池状态。
4. **看实例**：按 pod 分组的错误分布是否集中在个别实例（实例级问题重启即可缓解，
   全局性问题需回滚）。

## 判断标准

- 突增起点与发布时间吻合 → 高置信"发布引入"，建议回滚（证据：趋势图 + pod age）
- 个别实例错误占比 > 80% → 疑似节点/pod 异常，建议重启该实例
- 下游 P99 同时飙升 → 根因在下游，转下游服务剧本

## 输出要求

结论必须引用工具调用编号（T1、T2…），无证据的猜测不得写入 root_causes。
