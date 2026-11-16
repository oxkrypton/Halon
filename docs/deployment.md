# 部署与运维

> 修改 Dockerfile、Makefile、deploy/kubernetes 下的任何清单或 Secret 键后,必须同步更新本文档、根 `README.md` 的部署章节与对应模块文档。

## 构件

| 服务 | 镜像 | 二进制 | 端口 | 角色 |
| --- | --- | --- | --- | --- |
| gateway | `halon/gateway:dev` | `bin/gateway` | 8080 (业务) / 9090 (管理) | **产品**:通用网关 |
| agent | `halon/agent:dev` | `bin/agent` | 8081 (事件 + 报告 API) | **产品**:故障排查 Agent |
| user-service | `halon/user-service-demo:dev` | `bin/user-service-demo` | 8090 | **演示**,非产品 |
| order-service | `halon/order-service-demo:dev` | `bin/order-service-demo` | 8092 | **演示**,非产品 |

`Dockerfile` 是多阶段构建:golang:1.26-alpine 编译 → distroless 非 root 运行,`--build-arg SERVICE_PATH=<目录>` 选择入口(`cmd/gateway`、`cmd/agent` 或 `examples/user-service`)。`Makefile` 目标见 [development.md](development.md)。

## Kubernetes 清单

产品清单在 `deploy/kubernetes/`(`make deploy` 只部署它们):

| 文件 | 内容 |
| --- | --- |
| `postgres.yaml` | StatefulSet + Service + PVC,数据在 Pod 重建后保留 |
| `gateway.yaml` | 1 副本 Deployment + 两个 Service(`gateway` / `gateway-admin`),`ROUTES` 声明上游路由,推送超时事件到 `http://agent:8081/api/v1/incidents/timeout` |
| `agent.yaml` | ServiceAccount + 命名空间级 Role/RoleBinding(最小权限)+ 1 副本 Deployment + Service |

演示清单在 `examples/user-service/deploy.yaml` 与 `examples/order-service/deploy.yaml`,需单独 `kubectl apply`。

可选观测栈在 `deploy/kubernetes/observability/`(`make deploy` **不**包含它):

```bash
make deploy-observability   # kubectl apply -f deploy/kubernetes/observability/
```

| 组件 | 角色 | 访问方式 |
| --- | --- | --- |
| OTel Collector | 接收网关 OTLP traces(4317/4318),转发 Tempo | 集群内服务 |
| Tempo | 追踪存储(单二进制,本地存储,保留 24h) | 经 Grafana 查询 |
| Loki | 日志存储(单二进制,保留 72h) | 经 Grafana 查询 |
| Alloy | 每节点日志采集(Kubernetes API,只采 `app.kubernetes.io/part-of=halon` 的 Pod) | DaemonSet |
| Prometheus | 抓取 `gateway-admin:9090/metrics`,评估告警规则 | 集群内服务 |
| Alertmanager | 告警分组路由(默认无 receiver,需自行配置通知渠道) | 集群内服务 |
| Grafana | 看板与三信号关联查询,预置「Halon Gateway」看板 | `kubectl port-forward -n observability service/grafana 3000:3000`,默认 admin / `halon-admin`(生产必改) |

启用追踪:部署栈后,在 `gateway.yaml` 打开 `OTEL_ENABLED=true`(清单里有注释好的示例)并 `kubectl rollout restart deployment/gateway`。之后:Loki 日志行的 `trace_id` 可一键跳到 Tempo 追踪,Grafana Explore 三信号互通。

对接已有 Prometheus:集群已有 kube-prometheus-stack 时跳过 `14-prometheus.yaml`/`15-alertmanager.yaml`,把 `14-prometheus.yaml` 里的 `halon-gateway` scrape job 与告警规则组(`prometheus-rules` ConfigMap 内容)并入现有安装即可。

Loki 基数约定:`trace_id`、`route`、`status` 可做 label 或查询条件;`path`、`client_ip` 只作为日志字段检索,严禁进 label(Loki 高基数会显著拖慢)。

### 可选共享状态 Redis(`deploy/kubernetes/redis/`)

多副本前置组件,`make deploy` **不**包含它:

```bash
make deploy-redis   # kubectl apply -f deploy/kubernetes/redis/
```

参考清单为单副本 Redis 7 + Service,开启 `appendonly yes` / `appendfsync everysec`:共享模式下 Redis 是黑名单的事实源,重启不得静默清空;AOF 最坏丢失约 1s 的写入,恢复后经网关管理接口(`PUT /admin/rules/blacklist/:ip`)重放补齐即可。该清单面向开发与演练;生产请改用云厂商托管 Redis 或 Helm 的 HA(Sentinel/Cluster)部署。

启用共享模式:给网关与 Agent 同时设置 `REDIS_URL`(参考清单对应 `redis://redis:6379/0`,清单里有注释好的示例)并滚动重启。**两端必须同时配置或同时留空**——一端共享、一端内存的"半共享态"属于配置错误;两端启动日志都会打 `shared_state_mode=redis|memory` 供核查。多套部署不要共用同一个 Redis logical DB(用 `REDIS_URL` 的 db 编号区分)。

Redis 健康不进 `/readyz`(降级策略允许网关在 Redis 不可达时继续服务,摘除 Pod 反而中断全部流量),用指标表达:网关 `halon_gateway_redis_up`、Agent `halon_agent_redis_up`(新增 `/metrics` 端点),告警规则可并入观测栈。

切换到多副本的步骤:

1. `make deploy-redis`(或指向托管/HA Redis);
2. 网关与 Agent 清单同时打开 `REDIS_URL`,先把两端滚动到共享模式——滚动期间内存/共享副本并存,黑名单"≤10s 全副本收敛"的承诺只在全部副本进入共享模式后成立,升级窗口内按单副本语义理解;
3. 确认两端启动日志均为 `shared_state_mode=redis` 且 `halon_*_redis_up` 为 1;
4. 再把 `replicas` 提到 2+。回滚:两端同时去掉 `REDIS_URL` 并把 `replicas` 降回 1 即可,任意阶段都安全。

共享模式下黑名单新增/解除最坏 10s 收敛全部副本。需要即时生效时逐 Pod 调管理接口:

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

`pods: get/list/delete`、`pods/log: get`、`events: get/list`、`deployments: get/list/update/patch`。新增需要其他资源的工具时先扩 Role,并在 [modules/agent-tools.md](modules/agent-tools.md) 记录。

### Secret(halon-secrets,手动创建)

| 键 | 用途 | 消费方 |
| --- | --- | --- |
| `postgres-user` / `postgres-password` | 数据库账号 | postgres |
| `database-dsn` | 连接串 | gateway、agent |
| `admin-token` | 网关管理接口 Token | gateway、agent(封禁工具) |
| `ingest-token` | 事件接收/报告查询 Token | agent、gateway(webhook 推送) |
| `openai-api-key` | 大模型凭据 | agent |

创建命令见根 `README.md` 的「部署到 Kubernetes」。**新增密钥键时同步更新本表与 README。**

## 本地运行(无 K8s)

1. Docker 启动 PostgreSQL 17(命令见 README);
2. `make run-demo`(演示后端,:8090);
3. 带 `DATABASE_DSN`/`ADMIN_TOKEN`/`ROUTES` 等环境变量 `make run-gateway`;
4. 可选:带 `INGEST_TOKEN`/`OPENAI_API_KEY`/`OPENAI_MODEL` 等 `make run-agent`。Agent 会回退使用 `~/.kube/config` 访问集群;本地无集群时 K8s 工具会报连接错误,其余功能不受影响(可将操作级别保持 readonly 并忽略 K8s 工具错误)。

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

网关与 Agent 的全部环境变量见根 `README.md` 的「网关配置」「Agent 配置」两节;改配置定义(`internal/*/config.go`)时必须同步这两节。
