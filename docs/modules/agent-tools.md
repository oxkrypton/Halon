# 模块:Agent 工具层(agent/tools)

代码:`internal/agent/tools/`。被 `cmd/agent` 装配进 `agent.Registry`,按 `AGENT_OPERATION_LEVEL` 对应的角色×权限过滤后暴露给 LLM。

## 安全模型

- 工具通过 `Tool.Permission`(`authz.Permission`)声明权限,与网关管理接口共用同一套权限字符串:`tool:observe` / `tool:k8s:restart_pod` / `tool:k8s:scale` / `tool:k8s:update_image` / `gateway:blacklist:write`。角色映射见 [auth.md](auth.md),由 `AGENT_OPERATION_LEVEL`(默认 `readonly`)总开关控制:注册时过滤、执行时再校验一次;低于角色的工具对模型不可见也不可执行;
- 条件注册:仅当 `PROMETHEUS_URL` 非空时,`read_observability` 才开放 `prometheus` 子数据源及 `query`/`time` 字段;仅当 `GATEWAY_ADMIN_URL` 与 `GATEWAY_ADMIN_TOKEN` 均非空时才注册 `read_gateway` 与 `gateway_block_ip`,避免向模型暴露未配置数据源的无效工具;
- 所有工具参数经 `decodeArguments` 严格解码(`DisallowUnknownFields`,且只允许一个 JSON 对象);三个只读域工具(`read_observability`、`read_gateway`、`read_k8s`)统一接收 `queries` 数组(1..8 项),逐项按 `source` 二次严格解码并在出错时返回允许字段列表;部分子查询成功时返回 `{"results":[...]}`(失败项带 `error`),全部失败时返回顶层错误;
- 所有 SQL 参数化;K8s 操作受命名空间白名单(`K8S_NAMESPACES`,`*` 表示全部)约束;
- 变更类操作(重启/扩容/改镜像/封禁)共用 `KubernetesToolset.withResourceAction` 保护:在**资源粒度**上通过进程内 `ResourceLocker` 串行互斥,并通过 `MemoryCooldownStore` 强制冷却窗口(`K8S_ACTION_COOLDOWN`,默认 2 分钟)。Deployment 扩容(`k8s_scale_deployment`)、改镜像(`k8s_update_deployment_image`)以及归属该 Deployment(经 `ReplicaSet` 溯源)的 Pod 重启(`k8s_restart_pod`)共用锁键 `deployment/<namespace>/<name>`,防止并发 Session 同时对同一工作负载执行冲突变更;StatefulSet/DaemonSet Pod 共用 `statefulset/<ns>/<name>` / `daemonset/<ns>/<name>`;独立 Pod 使用 `pod/<ns>/<pod>`;网关 IP 操作使用规范化后的 `gateway/ip/<ip>`。若注入了自定义冷却存储且发生不可用错误,操作按 fail-closed 拒绝(`ErrCooldownUnavailable`);
- `k8s_update_deployment_image` 在获取资源锁和消耗冷却窗口前先只读校验目标 Deployment 与容器名存在,避免误传参数锁死后续修复;
- 单次只读查询有 15s 超时(`queryTimeout`)。

## 工具清单

### 只读(readonly,`tool:observe`)

只读工具统一接受 `{"queries": [{"source": "...", ...}]}`(1..8 项)组合查询:

| 工具 | 子数据源 (`source`) | 说明 | 关键参数约束 |
| --- | --- | --- | --- |
| `read_observability` | `error_logs` | 查 PostgreSQL 错误/慢请求报文,优先按 incident 的 `request_id` | `request_id`、`path`、`minutes` 1..1440、`limit` 1..100 |
| | `traffic_stats` | 按路径+状态码聚合请求量、均值、P95、P99、5xx 数 | `path`、`minutes` 1..1440 |
| | `top_ips` | 按客户端 IP 聚合请求量与错误数 | `path`、`minutes` 1..1440、`limit` 1..50 |
| | `prometheus` | 只读 PromQL 即时查询(仅 `PROMETHEUS_URL` 非空时暴露) | `query` 必填(1..4096 字符)、`time`(RFC3339) |
| `read_gateway` | `state` | 调网关 `GET /admin/state/gateway` 查 Redis 健康、黑名单列表、全局限流配置与熔断状态 | 无额外参数(仅 `GATEWAY_ADMIN_URL` 与 `GATEWAY_ADMIN_TOKEN` 均非空时注册) |
| | `ip_status` | 调网关 `GET /admin/state/rate-limit/:ip` 查指定 IP 黑名单命中与 Redis 令牌桶余量(只读不扣令牌) | `ip` 必填且合法 |
| | `incident_windows` | 调网关 `GET /admin/state/incident-windows` 查网关 Redis 异常归一化窗口状态(pending/active)、累计次数、样本路径/IP 与冷却剩余 TTL | `type`、`scope`(或 `route` 别名)可选 |
| `read_k8s` | `pods` | 列 Pod(状态、重启次数、容器状态) | `namespace` 白名单、`label_selector`、`limit` 1..100 |
| | `pod_logs` | 读容器日志,支持 `previous` 取崩溃前日志 | `namespace`、`pod` 必填、`container`、`tail_lines` 1..1000、`since_seconds` 1..86400、`previous`(上限 1 MiB) |
| | `events` | 列 K8s 事件,可按对象名/类型过滤 | `namespace`、`involved_object`、`type`、`limit` 1..100 |
| | `deployment` | Deployment 副本、状态、镜像、更新策略 | `namespace`、`name` 必填 |

### operator 级变更

| 工具 | 权限 | 说明 | 保护 |
| --- | --- | --- | --- |
| `k8s_restart_pod` | `tool:k8s:restart_pod` | 删除一个故障 Pod 让控制器重建 | 必须提供 reason;先 Get 确认存在并解析所属控制器生成资源锁;串行锁 + 冷却 |
| `k8s_scale_deployment` | `tool:k8s:scale` | 调整 Deployment 副本数 | 上限 `K8S_MAX_REPLICAS`;串行锁 + 冷却;写入 `halon.dev/last-agent-action` 注解审计 |
| `gateway_block_ip` | `gateway:blacklist:write` | 调网关管理接口把 IP 加入黑名单(仅 `GATEWAY_ADMIN_URL` 与 `GATEWAY_ADMIN_TOKEN` 均非空时注册) | IP 必须 `net.ParseIP` 通过;reason 必填;串行锁 + 冷却;网关返回非 2xx 视为失败 |

网关只读/写工具都通过 `GATEWAY_ADMIN_URL` + `GATEWAY_ADMIN_TOKEN` 访问网关管理面;`jwt` 模式下凭据须为 `typ=agent` 且角色与 `AGENT_OPERATION_LEVEL` 一致的 JWT。

### full 级变更

| 工具 | 权限 | 说明 | 保护 |
| --- | --- | --- | --- |
| `k8s_update_deployment_image` | `tool:k8s:update_image` | 更新 Deployment 中已存在容器的镜像 | 先校验 Deployment 与容器名存在;镜像名长度/非法字符校验;串行锁 + 冷却;审计注解 |

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `observability.go` | `read_observability` 组合只读工具(3 个 SQL 子源 + 条件暴露的 `prometheus` 子源);行类型 `ErrorLogRow`/`TrafficStatRow`/`TopIPRow` |
| `kubernetes.go` | `KubernetesToolset`(白名单、副本上限、资源锁与冷却)与 4 个 K8s 工具(`read_k8s` 组合只读 + 3 个变更工具);同时实现 `agent.PodLogReader`(`AllowedNamespaces`、`ListPods`、`GetPodLogs`)供 Agent HTTP API 复用 |
| `cooldown.go` | `CooldownStore` 接口、`ErrCooldownUnavailable`、`MemoryCooldownStore`(进程内过期表)与 `ResourceLocker`(按资源键引用计数互斥锁) |
| `gateway.go` | `NewGatewayTools`(条件注册 `read_gateway` 组合只读工具与 `gateway_block_ip`)与 `NewGatewayActionTool`,共用冷却与资源锁 |
| `schema.go` | `objectSchema`/`batchQuerySchema`/`decodeArguments`/`decodeSourceQuery`/`runBatchQueries`/`queryContext` 等工具构造函数 |

## 新增一个工具的流程

1. 在合适的文件里定义 `agent.Tool`:唯一 Name、清晰的 Description(模型靠它决策)、JSON Schema 参数、`Permission`(复用 `internal/authz` 常量,不要新建权限字符串)、Handler;
2. Handler 内严格校验参数并遵守只读 15s 超时;变更类必须要求 `reason` 并接入 `withResourceAction`;
3. 在 `cmd/agent/main.go` 的装配处把工具加入列表;
4. 补单测(参数校验、级别过滤、资源锁与冷却,参考 `kubernetes_test.go` / `observability_test.go`);
5. **更新本文档的工具清单**,并在 [agent.md](agent.md) 系统提示词契约处评估是否需要提示模型。

## 修改本模块时必须同步检查

- 本文档工具清单与根 `README.md` 的 Agent 功能列表;
- [trafficlog.md](trafficlog.md):SQL 工具依赖其表结构;
- `deploy/kubernetes/agent.yaml`:新增 K8s 操作可能需要扩展 RBAC;
- `AGENTS.md` 的已知边界;
- [auth.md](auth.md):新增权限项或改变角色映射时。
