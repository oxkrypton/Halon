# 模块:Agent 工具层(agent/tools)

代码:`internal/agent/tools/`。被 `cmd/agent` 装配进 `agent.Registry`,按 `AGENT_OPERATION_LEVEL` 对应的角色×权限过滤后暴露给 LLM。

## 安全模型

- 工具通过 `Tool.Permission`(`authz.Permission`)声明权限,与网关管理接口共用同一套权限字符串:`tool:observe` / `tool:k8s:restart_pod` / `tool:k8s:scale` / `tool:k8s:update_image` / `gateway:blacklist:write`。角色映射见 [auth.md](auth.md),由 `AGENT_OPERATION_LEVEL`(默认 `readonly`)总开关控制:注册时过滤、执行时再校验一次;低于角色的工具对模型不可见也不可执行;
- 所有工具参数经 `decodeArguments` 严格解码(`DisallowUnknownFields`,且只允许一个 JSON 对象);
- 所有 SQL 参数化;K8s 操作受命名空间白名单(`K8S_NAMESPACES`,`*` 表示全部)约束;
- 变更类操作(重启/扩容/改镜像/封禁)共用 `KubernetesToolset.allowAction` 冷却窗口(`K8S_ACTION_COOLDOWN`,默认 2 分钟),在**资源粒度**上互斥:Deployment 的扩容(`k8s_scale_deployment`)、改镜像(`k8s_update_deployment_image`)以及归属于该 Deployment(经 `ReplicaSet` 溯源)的 Pod 重启(`k8s_restart_pod`)共用锁键 `deployment/<namespace>/<name>`,防止多 Agent 同时对同一工作负载执行冲突变更;StatefulSet/DaemonSet Pod 共用 `statefulset/<ns>/<name>` / `daemonset/<ns>/<name>`;独立 Pod 使用 `pod/<ns>/<pod>`;网关 IP 操作使用规范化后的 `gateway/ip/<ip>`。冷却由 `RedisCooldownStore`(`SET halon:cooldown:<key> NX PX`)原子裁决,所有副本同一窗口内只有一个能执行,未抢到的拿到 `PTTL` 剩余时间;Redis 不可达(或 `Cooldowns` 未配置)时**拒绝变更**(`ErrCooldownUnavailable`,安全功能不侥幸放行),只读工具不受影响;
- 单次只读查询有 15s 超时(`queryTimeout`)。

## 工具清单

### 只读(readonly,`tool:observe`)

| 工具 | 说明 | 关键参数约束 |
| --- | --- | --- |
| `query_error_logs` | 查 PostgreSQL 错误/慢请求报文,优先按 incident 的 request_id | minutes 1..1440,limit 1..100 |
| `query_traffic_stats` | 按路径+状态码聚合请求量、均值、P95、5xx 数 | 同上 |
| `query_top_ips` | 按客户端 IP 聚合请求量与错误数 | 同上 |
| `query_prometheus` | 只读 PromQL 即时查询(`PROMETHEUS_URL` 为空时报配置错误) | query 必填 |
| `k8s_list_pods` | 列 Pod(状态、重启次数、容器状态) | namespace 白名单,limit 1..100 |
| `k8s_get_pod_logs` | 读容器日志,支持 previous 取崩溃前日志 | tail 1..1000 行,since 1..86400 秒,限 1 MiB |
| `k8s_list_events` | 列 K8s 事件,可按对象名/类型过滤 | limit 1..100 |
| `k8s_get_deployment` | Deployment 副本、状态、镜像、更新策略 | namespace + name |

### operator 级变更

| 工具 | 权限 | 说明 | 保护 |
| --- | --- | --- | --- |
| `k8s_restart_pod` | `tool:k8s:restart_pod` | 删除一个故障 Pod 让控制器重建 | 必须提供 reason;先 Get 确认存在并解析所属控制器生成资源锁;冷却 |
| `k8s_scale_deployment` | `tool:k8s:scale` | 调整 Deployment 副本数 | 上限 `K8S_MAX_REPLICAS`;冷却;写入 `halon.dev/last-agent-action` 注解审计 |
| `gateway_block_ip` | `gateway:blacklist:write` | 调网关管理接口把 IP 加入黑名单 | IP 必须 `net.ParseIP` 通过;reason 必填;冷却;网关返回非 2xx 视为失败 |

`gateway_block_ip` 必须携带 `typ=agent` 的 JWT(网关按令牌里的角色放行);仅靠 Agent 进程内的级别过滤已不再足够。

### full 级变更

| 工具 | 权限 | 说明 | 保护 |
| --- | --- | --- | --- |
| `k8s_update_deployment_image` | `tool:k8s:update_image` | 更新 Deployment 中已存在容器的镜像 | 镜像名长度/非法字符校验;容器名必须存在;冷却;审计注解 |

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `observability.go` | 4 个只读工具(3 个 SQL + PromQL);行类型 `ErrorLogRow`/`TrafficStatRow`/`TopIPRow` |
| `kubernetes.go` | `KubernetesToolset`(白名单、副本上限、冷却)与 6 个 K8s 工具 |
| `cooldown.go` | `CooldownStore` 接口(Claim)、`ErrCooldownUnavailable` 与 `RedisCooldownStore`(SET NX PX 裁决 + 后台 Ping 保活指标) |
| `gateway.go` | `NewGatewayActionTool`:封禁 IP,经网关管理接口,复用 K8s 冷却器 |
| `schema.go` | `objectSchema`/`decodeArguments`/`queryContext` 等工具构造函数 |

## 新增一个工具的流程

1. 在合适的文件里定义 `agent.Tool`:唯一 Name、清晰的 Description(模型靠它决策)、JSON Schema 参数、`Permission`(复用 `internal/authz` 常量,不要新建权限字符串)、Handler;
2. Handler 内严格校验参数并遵守只读 15s 超时;变更类必须要求 `reason` 并接入冷却;
3. 在 `cmd/agent/main.go` 的装配处把工具加入列表;
4. 补单测(参数校验、级别过滤、冷却,参考 `kubernetes_test.go` / `observability_test.go`);
5. **更新本文档的工具清单**,并在 [agent.md](agent.md) 系统提示词契约处评估是否需要提示模型。

## 修改本模块时必须同步检查

- 本文档工具清单与根 `README.md` 的 Agent 功能列表;
- [trafficlog.md](trafficlog.md):SQL 工具依赖其表结构;
- `deploy/kubernetes/agent.yaml`:新增 K8s 操作可能需要扩展 RBAC;
- `AGENTS.md` 的已知边界;
- [auth.md](auth.md):新增权限项或改变角色映射时。
