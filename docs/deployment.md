# 部署与运维

> 修改 Dockerfile、Makefile、deploy/kubernetes 下的任何清单或 Secret 键后,必须同步更新本文档、根 `README.md` 的部署章节与对应模块文档。

## 构件

| 服务 | 镜像 | 二进制 | 端口 | 角色 |
| --- | --- | --- | --- | --- |
| gateway | `halon/gateway:dev` | `bin/gateway` | 8080 (业务) / 9090 (管理) | **产品**:通用网关 |
| agent | `halon/agent:dev` | `bin/agent` | 8081 (事件 + 报告 API) | **产品**:故障排查 Agent |
| web | `halon/web:dev` | `web/dist` (Nginx) | 8080 (静态 UI + API 反代) | **可选控制台**:观测与网关管理 UI |
| user-service | `halon/user-service-demo:dev` | `bin/user-service-demo` | 8090 | **演示**,非产品 |
| order-service | `halon/order-service-demo:dev` | `bin/order-service-demo` | 8092 | **演示**,非产品 |

`Dockerfile` 是多阶段构建:golang:1.26-alpine 编译 → distroless 非 root 运行,`--build-arg SERVICE_PATH=<目录>` 选择入口(`cmd/gateway`、`cmd/agent` 或 `examples/user-service`)。`Makefile` 目标见 [development.md](development.md)。

## Kubernetes 清单

产品清单在 `deploy/kubernetes/`(`make deploy` 只部署它们):

| 文件 | 内容 |
| --- | --- |
| `postgres.yaml` | StatefulSet + Service + PVC,数据在 Pod 重建后保留 |
| `redis.yaml` | 单副本 Redis 7 + Service(开启 AOF),供开发与演练使用;生产建议托管或 HA Redis |
| `gateway.yaml` | Deployment(支持多副本)+ 两个 Service(`gateway` / `gateway-admin`),`ROUTES` 声明上游路由,归一化后推送异常事件到 `http://agent:8081/api/v1/incidents` |
| `agent.yaml` | ServiceAccount + 命名空间级 Role/RoleBinding(最小权限)+ 单实例 Deployment(`replicas: 1`, `strategy.type: Recreate`)+ Service |

演示清单在 `examples/user-service/deploy.yaml` 与 `examples/order-service/deploy.yaml`,需单独 `kubectl apply`。

可选观测栈在 `deploy/kubernetes/observability/`(`make deploy` **不**包含它):

```bash
make deploy-observability   # kubectl apply -f deploy/kubernetes/observability/
```

| 组件 | 角色 | 访问方式 |
| --- | --- | --- |
| OTel Collector | 接收网关 OTLP traces(4317/4318),转发 Tempo | 集群内服务 |
| Tempo | 追踪存储(单二进制,本地存储,保留 24h) | Web 控制台 Traces 页面(`TEMPO_URL`)或 HTTP API `:3200` |
| Prometheus | 抓取 `gateway-admin:9090/metrics`,评估告警规则 | 集群内服务 |
| Alertmanager | 告警分组路由(默认无 receiver,需自行配置通知渠道) | 集群内服务 |

启用追踪:部署栈后,在 `gateway.yaml` 打开 `OTEL_ENABLED=true`(清单里有注释好的示例)并 `kubectl rollout restart deployment/gateway`。之后网关 JSON 日志行携带 `trace_id`,可据此在 Tempo 中定位对应追踪。若在 `gateway.yaml` 同时设置 `TEMPO_URL=http://tempo.observability.svc.cluster.local:3200`,Web 管理控制台的 Traces 页面也会自动读取 Tempo 分布式链路(未设置时自动降级为基于 PostgreSQL 的网关+上游瀑布流)。

### 可选 Web 管理控制台(`deploy/kubernetes/web/`)

提供 P50/P95/P99 分位数与运行态保护看板、类 Jaeger 链路追踪、请求日志、经 Agent 直连 K8s API 的容器实时日志、AI 故障报告与网关管控界面,`make deploy` **不**包含它:

```bash
make docker-build-web
make deploy-web             # kubectl apply -f deploy/kubernetes/web/
kubectl port-forward service/halon-web 18090:8080
```

容器基于 `nginxinc/nginx-unprivileged`（非 root UID 101、只读根文件系统）,把 `/api/gateway/*` 反代至 `gateway-admin:9090`,把 `/api/agent/*` 反代至 `agent:8081`。

对接已有 Prometheus:集群已有 kube-prometheus-stack 时跳过 `14-prometheus.yaml`/`15-alertmanager.yaml`,把 `14-prometheus.yaml` 里的 `halon-gateway` scrape job 与告警规则组(`prometheus-rules` ConfigMap 内容)并入现有安装即可。

### Redis(`deploy/kubernetes/redis.yaml`)

Redis 是网关独占的状态后端,承载黑名单、按 IP 令牌桶限流以及跨副本按 `(event_type, scope_key)` 异常归一化、突增阈值计数与发件冷却窗口,已包含在 `make deploy` 主路径中。

参考清单为单副本 Redis 7 + Service,开启 `appendonly yes` / `appendfsync everysec`:Redis 是黑名单的事实源,重启不得静默清空;AOF 最坏丢失约 1s 的写入,恢复后经网关管理接口(`PUT /admin/rules/blacklist/:ip`)重放补齐即可。该清单面向开发与演练;生产请改用云厂商托管 Redis 或 Helm 的 HA(Sentinel/Cluster)部署,并把网关的 `REDIS_URL` 指向生产地址。多套部署不要共用同一个 Redis logical DB(用 `REDIS_URL` 的 db 编号区分)。

Redis 健康不进 `/readyz`(降级策略允许网关在 Redis 不可达时继续服务,摘除 Pod 反而中断全部流量),用指标 `halon_gateway_redis_up` 表达,告警规则可并入观测栈。

网关扩缩与 Agent 部署边界:网关的黑名单、限流与异常归一化均由 Redis 裁决,将 `gateway.yaml` 中的 `replicas` 调整为 `2+` 即可直接水平扩容。Agent 固定为单实例控制面(`replicas: 1`、`strategy.type: Recreate`,先停旧 Pod 再起新 Pod,不连接 Redis),通过调整 `AGENT_WORKERS` 控制单进程内并发 Session 数。

黑名单新增/解除通过 Redis pub/sub 即时广播并以 `BLACKLIST_CACHE_TTL`(默认 10s)兜底,最坏 10s 内收敛到全部副本。需要即时生效时逐 Pod 调管理接口:

```bash
for pod in $(kubectl get pods -l app.kubernetes.io/name=gateway -o name); do
  kubectl exec "$pod" -- true 2>/dev/null  # 确认 pod 可用(可选)
  kubectl port-forward "$pod" 19090:9090 &  # 逐个执行:先 port-forward 再 curl,做完杀掉后台任务
done
# 对每个转发的本地端口执行:curl -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" http://localhost:19090/admin/rules/blacklist/1.2.3.4
```

### 适配任意 K8s 集群

清单不绑定任何发行版,无 Minikube/云厂商专属 API:

1. 构建镜像并推送到集群可访问的镜像仓库;
2. 修改清单中的 `image:`(默认 `halon/*:dev` + `imagePullPolicy: IfNotPresent`,适合本地集群);
3. 确认集群默认 StorageClass 可供给 PVC(postgres.yaml);不行则改用 hostPath 或外部数据库;
4. `kubectl apply`。

### Agent 的 RBAC 权限(agent.yaml)

`pods: get/list/delete`、`pods/log: get`、`events: get/list`、`deployments: get/list/update/patch`、`replicasets: get`。新增需要其他资源的工具时先扩 Role,并在 [modules/agent-tools.md](modules/agent-tools.md) 记录。

### Secret(halon-secrets,手动创建)

| 键 | 用途 | 消费方 |
| --- | --- | --- |
| `postgres-user` / `postgres-password` | 数据库账号 | postgres |
| `database-dsn` | 连接串 | gateway、agent |
| `admin-token` | 网关管理接口静态 Token(`full`) | gateway;`legacy`/`hybrid` 下 agent 可选(不推荐) |
| `ingest-token` | webhook 事件接收凭据 | agent、gateway(webhook 推送) |
| `openai-api-key` | 大模型凭据 | agent |
| `agent-gateway-token` | 可选:`typ=agent` 的 JWT,角色须等于 Agent 操作级别 | agent(`operator`/`full` 必填;`readonly` 需调用网关只读状态工具时可选配) |
| `jwt-private-key` | 可选:base64 Ed25519 私钥(`AUTH_LOGIN_ENABLED=true` 时必填) | gateway(本地登录签发会话 JWT) |
| `bootstrap-admin-user` / `bootstrap-admin-password` | 可选:首次引导创建的 `full` 管理员账号与初始密码 | gateway(`AUTH_LOGIN_ENABLED=true` 首次启动时) |

创建命令见根 `README.md` 的「部署到 Kubernetes」。**新增密钥键时同步更新本表与 README。**

### 鉴权配置

默认 `AUTH_MODE=legacy`、`AUTH_LOGIN_ENABLED=false`,清单无需改动。`hybrid`/`jwt` 模式需要:

1. 用 `make build` 产出的 `bin/halonctl keygen` 离线生成密钥;`keygen` 输出的 `public_keys_env` 直接放进 ConfigMap 的 `jwt-public-keys` 键(公钥不是机密)。
2. 两端同时设置 `AUTH_MODE` 与 `AUTH_JWT_PUBLIC_KEYS`(从 ConfigMap `halon-auth` 挂载,清单中已有注释示例),`jwt` 模式下 Agent 读接口不再接受 `INGEST_TOKEN`。
3. 人类管理员用 `halonctl token issue --type human --role <role> --ttl 8h` 签发;Agent 访问网关用 `--type agent --role <与 AGENT_OPERATION_LEVEL 一致>` 签发,写入 `agent-gateway-token`。
4. 若需开启本地账号登录与用户管理(`AUTH_LOGIN_ENABLED=true`),取消 `deploy/kubernetes/gateway.yaml` 中对应环境变量注释并写入 `jwt-private-key`、`bootstrap-admin-user`/`bootstrap-admin-password`(Argon2id 并发上限 4 × 64 MiB,清单已将网关内存上限设为 `384Mi`)。

迁移顺序与撤销手段见 [modules/auth.md](modules/auth.md)。未开启本地登录时私钥不要放进集群。

## 本地运行(无 K8s)

1. Docker 启动 PostgreSQL 17 与 Redis 7(命令见 README);
2. `make run-demo`(演示后端,:8090);
3. 带 `DATABASE_DSN`/`REDIS_URL`/`ADMIN_TOKEN`/`ROUTES` 等环境变量 `make run-gateway`;
4. 可选:带 `DATABASE_DSN`/`INGEST_TOKEN`/`OPENAI_API_KEY`/`OPENAI_MODEL` 等 `make run-agent`。Agent 会回退使用 `~/.kube/config` 访问集群;本地无集群时 K8s 工具会报连接错误,其余功能不受影响(可将操作级别保持 readonly 并忽略 K8s 工具错误)。

## 端到端故障演练(任意集群)

```bash
kubectl port-forward service/gateway 18080:8080
kubectl port-forward service/agent   18081:8081

# 1. 制造超时:演示后端 ?delay= 超过网关 UPSTREAM_TIMEOUT
curl -i 'http://127.0.0.1:18080/api/v1/users/1?delay=5000'     # 预期 504

# 2. 轮询 Agent 报告
curl -s http://127.0.0.1:18081/api/v1/incidents \
  -H 'Authorization: Bearer halon-local-ingest-token'

# 3. 报告也可直接查库
kubectl exec postgres-0 -- psql -U halon -d halon \
  -c 'SELECT event_id, status, steps, total_tokens FROM incident_reports ORDER BY occurred_at DESC LIMIT 5;'
```

## 配置总表

网关与 Agent 的全部环境变量见根 `README.md` 的「网关配置」「Agent 配置」两节;改配置定义(`internal/*/config.go`)时必须同步这两节。鉴权公共配置集中在 `internal/authz/config.go`,说明见 [modules/auth.md](modules/auth.md)。
