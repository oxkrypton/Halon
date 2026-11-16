# Halon

> **开发者/Agent 注意**:修改代码后必须同步更新文档,规则见 [AGENTS.md](AGENTS.md) 与 [docs/README.md](docs/README.md)。分模块详细文档见 [docs/](docs/README.md)。

Halon 是一个**开源的云原生网关 Agent 项目**:一个通用 HTTP 网关 + 一个由大模型驱动的故障排查 Agent,两者彼此独立、通过契约通信,可部署到**任意 Kubernetes 集群**(Minikube、Kind、ACK、EKS 等)。

- **网关(gateway)**:通用反向代理,按 `ROUTES` 环境变量把路径前缀转发到任意上游 HTTP 服务;自带 IP 黑名单、按 IP 限流、流量日志落库、上游超时事件上报。**不感知任何具体业务服务。**
- **Agent 服务(agent)**:接收网关超时事件,调用 OpenAI 兼容大模型,借助日志/PromQL/K8s 只读工具取证,按权限级别执行 Pod 重启、扩容、IP 封禁等变更,产出中文 Markdown 排查报告并持久化。

`examples/user-service` 是演示用后端(带故障注入),**不属于产品本体**,仅用于本地开发与故障演练。

## 当前架构

```text
HTTP Client
    |
    v
Gin Gateway (:8080)                      # 通用反向代理
    |-- Request ID / Recovery / Access Log
    |-- IP Blacklist / Per-IP Rate Limit
    |
    | ROUTES: prefix=upstream(纯 HTTP)
    v
任意上游服务(如 examples/user-service)

Gin Gateway -- bounded channel --> Batch Writer --> PostgreSQL
                                                   |-- base_logs
                                                   |-- error_body
                                                   |-- incident_reports

Gateway Admin (:9090) -- Bearer Token --> blacklist / rate-limit rules

Gin Gateway -- 504 webhook --> Agent Service (:8081)
                                   |-- 事件去重(TTL + ON CONFLICT)
                                   |-- 编排器队列 --> LLM 排查循环
                                   |-- 只读工具:日志 / PromQL / Pod / 事件
                                   |-- 操作工具(按级别开放):重启 / 扩容 / 封禁
                                   +-- 故障报告持久化 --> incident_reports
```

## 已完成功能

### Gin 网关(通用反向代理)

- `ROUTES` 环境变量声明路由表(`前缀=上游地址`,逗号分隔,最长前缀优先),上游可以是集群内任意 HTTP 服务。
- 上游响应原样透传;上游超时返回 504 `UPSTREAM_TIMEOUT` 并触发故障事件;连接失败返回 502 `UPSTREAM_ERROR`。
- 统一 JSON 错误响应和 `X-Request-ID` 请求追踪(自动透传到上游)。
- 内存 IP 黑名单及按客户端 IP 的令牌桶限流。
- 独立管理端口 `:9090`,支持动态更新黑名单和限流参数,暴露 Prometheus 指标。
- `/healthz` 存活检查和 `/readyz` 数据库就绪检查。
- HTTP 超时、Panic 恢复、平滑退出及非 root 容器运行。

### PostgreSQL 日志存储

- `base_logs`:记录全部 HTTP 请求的 IP、路径、状态码和耗时。
- `error_body`:仅记录状态码不低于 400 或耗时达到阈值的请求报文,默认最多各截取 64 KiB。
- 有界队列异步写入,默认每 500 条或每 1 秒批量提交。
- 基础日志和详情日志在同一事务中写入,失败批次最多重试 3 次。
- 使用嵌入式、版本化 SQL 迁移,不使用自动改表。
- StatefulSet 配合 PersistentVolumeClaim,数据库 Pod 重建后数据仍保留。

### Agent 故障排查服务

- `POST /api/v1/incidents/timeout` 接收网关 504 webhook,Bearer 鉴权 + 事件体严格校验。
- 双重事件去重:内存 TTL 去重器拦截重试投递,`ON CONFLICT DO NOTHING` 保证入库幂等。
- 有界队列 + 可配置 worker 数的编排器,每次排查有独立超时,支持 readonly/operator/full 三级操作权限。
- 只读工具:PostgreSQL 错误日志、流量统计、Top IP,Prometheus PromQL 即时查询,Pod/日志/事件/Deployment 状态。
- 操作工具(operator):Pod 重启、Deployment 扩容(上限保护)、网关 IP 封禁;full 级别额外支持更新容器镜像。
- 命名空间白名单与操作冷却时间,SQL 全部参数化,工具参数严格校验。
- 排查结果(中文 Markdown 报告、工具调用轨迹、Token 用量)持久化到 `incident_reports` 表,可随时通过 API 查询。

## 环境要求

- Go 1.26+
- Docker(构建镜像)
- kubectl + 任意 Kubernetes 集群(本地开发推荐 Minikube 或 Kind)

运行全部单元测试、竞态检测和静态检查:

```bash
make test
```

## 本地运行(不依赖 K8s)

启动 PostgreSQL:

```bash
docker run --rm --name halon-postgres \
  -e POSTGRES_DB=halon \
  -e POSTGRES_USER=halon \
  -e POSTGRES_PASSWORD=halon-local-password \
  -p 5432:5432 \
  -v halon-pgdata:/var/lib/postgresql/data \
  postgres:17-alpine
```

启动演示后端(第二个终端):

```bash
make run-demo    # :8090
```

启动网关(第三个终端):

```bash
DATABASE_DSN='postgres://halon:halon-local-password@localhost:5432/halon?sslmode=disable' \
ADMIN_TOKEN='halon-local-admin-token' \
ROUTES='/api/v1/users/=http://localhost:8090' \
make run-gateway
```

调用网关:

```bash
curl -i http://localhost:8080/api/v1/users/1
curl -i http://localhost:8080/api/v1/users/404
curl -i http://localhost:8080/readyz
```

如需本地验证 Agent 排查闭环,再开一个终端启动 Agent 服务(网关配置 `AGENT_WEBHOOK_URL=http://localhost:8081/api/v1/incidents/timeout` 和相同的 Token 后重启):

```bash
DATABASE_DSN='postgres://halon:halon-local-password@localhost:5432/halon?sslmode=disable' \
INGEST_TOKEN='halon-local-ingest-token' \
OPENAI_API_KEY='<你的 OpenAI 兼容 API Key>' \
OPENAI_MODEL='gpt-4o-mini' \
make run-agent
```

制造一次超时并观察闭环:

```bash
curl -i 'http://localhost:8080/api/v1/users/1?delay=5000'   # 演示后端延迟 5s → 网关 504 → Agent 排查
curl -s http://localhost:8081/api/v1/incidents -H 'Authorization: Bearer halon-local-ingest-token'
```

## HTTP 接口

### 网关(`:8080` 业务 / `:9090` 管理)

| 接口 | 说明 |
| --- | --- |
| `ANY <ROUTES 配置的前缀>` | 反向代理到对应上游,响应原样透传 |
| `GET /healthz` | 进程存活检查,不依赖外部服务 |
| `GET /readyz` | 检查 PostgreSQL 是否可用 |
| `GET /admin/rules/blacklist` | 查询黑名单,需要管理鉴权 |
| `PUT /admin/rules/blacklist/:ip` | 添加黑名单 IP,需要管理鉴权 |
| `DELETE /admin/rules/blacklist/:ip` | 删除黑名单 IP,需要管理鉴权 |
| `GET /admin/rules/rate-limit` | 查询限流参数,需要管理鉴权 |
| `PUT /admin/rules/rate-limit` | 更新限流参数,需要管理鉴权 |
| `GET /metrics` | Prometheus 指标(管理端口) |

管理接口鉴权头:

```text
Authorization: Bearer <ADMIN_TOKEN>
```

动态更新限流示例:

```bash
curl -X PUT http://localhost:9090/admin/rules/rate-limit \
  -H "Authorization: Bearer halon-local-admin-token" \
  -H 'Content-Type: application/json' \
  -d '{"requests_per_second":10,"burst":20}'
```

网关自身错误映射(上游错误原样透传,不在此列):

| 场景 | HTTP 状态码 | 错误代码 |
| --- | --- | --- |
| 无匹配路由 | 404 | `ROUTE_NOT_FOUND` |
| IP 被拉黑 | 403 | `IP_BLOCKED` |
| 请求超过限流阈值 | 429 | `RATE_LIMITED` |
| 上游响应超时 | 504 | `UPSTREAM_TIMEOUT`(同时触发故障事件) |
| 上游连接失败 | 502 | `UPSTREAM_ERROR` |

### Agent 服务(`:8081`,均需 `Authorization: Bearer <INGEST_TOKEN>`)

| 接口 | 说明 |
| --- | --- |
| `POST /api/v1/incidents/timeout` | 接收超时事件并排队排查;重复事件返回 `duplicate: true` |
| `GET /api/v1/incidents` | 列出最近的故障报告,`limit` 参数最大 200 |
| `GET /api/v1/incidents/:event_id` | 查询单个事件的排查报告与工具调用轨迹 |
| `GET /healthz` / `GET /readyz` | 存活检查 / PostgreSQL 就绪检查 |

## 部署到 Kubernetes(任意集群)

部署只依赖 kubectl 与一个可用集群,不绑定特定发行版。

创建 Secret。以下凭据仅用于本机演示,不应复用于生产环境:

```bash
kubectl create secret generic halon-secrets \
  --from-literal=postgres-user=halon \
  --from-literal=postgres-password=halon-local-password \
  --from-literal=database-dsn='postgres://halon:halon-local-password@postgres:5432/halon?sslmode=disable' \
  --from-literal=admin-token='halon-local-admin-token' \
  --from-literal=ingest-token='halon-local-ingest-token' \
  --from-literal=openai-api-key='<你的 OpenAI 兼容 API Key>'
```

构建镜像并推送到集群可访问的镜像仓库,然后把 `deploy/kubernetes/*.yaml` 里的 `image:` 改为你的镜像地址:

```bash
make docker-build
docker tag halon/gateway:dev <registry>/halon/gateway:v1 && docker push <registry>/halon/gateway:v1
docker tag halon/agent:dev   <registry>/halon/agent:v1   && docker push <registry>/halon/agent:v1
```

先启动数据库,再部署产品组件:

```bash
kubectl apply -f deploy/kubernetes/postgres.yaml
kubectl rollout status statefulset/postgres --timeout=180s
make deploy
kubectl rollout status deployment/gateway deployment/agent --timeout=180s
make status
```

**接入你自己的服务**:修改 `deploy/kubernetes/gateway.yaml` 中 `ROUTES` 环境变量,把你的服务加进路由表(如 `/api/v1/orders/=http://order-service:8080`),然后 `kubectl rollout restart deployment/gateway`。

### 使用 Minikube 本地演练(可选)

```bash
make minikube-start
make minikube-image          # 构建并载入网关/Agent/演示镜像
kubectl apply -f deploy/kubernetes/postgres.yaml
kubectl rollout status statefulset/postgres --timeout=180s
make deploy
kubectl apply -f examples/user-service/deploy.yaml   # 可选演示后端
kubectl rollout status deployment/gateway deployment/agent deployment/user-service --timeout=180s
```

### 端到端故障演练

```bash
kubectl port-forward service/gateway 18080:8080
kubectl port-forward service/agent   18081:8081

# 1. 制造超时:演示后端延迟 5s,网关 3s 超时
curl -i 'http://127.0.0.1:18080/api/v1/users/1?delay=5000'   # 预期 504

# 2. 轮询 Agent 报告
curl -s http://127.0.0.1:18081/api/v1/incidents \
  -H 'Authorization: Bearer halon-local-ingest-token'

# 3. 管理接口验证(可选)
kubectl port-forward service/gateway-admin 19090:9090
curl -i -X PUT http://127.0.0.1:19090/admin/rules/blacklist/127.0.0.1 \
  -H 'Authorization: Bearer halon-local-admin-token'
curl -i http://127.0.0.1:18080/api/v1/users/1                # 预期 403
curl -i -X DELETE http://127.0.0.1:19090/admin/rules/blacklist/127.0.0.1 \
  -H 'Authorization: Bearer halon-local-admin-token'

# 4. 直接查库
kubectl exec postgres-0 -- psql -U halon -d halon \
  -c 'SELECT event_id, status, steps, total_tokens FROM incident_reports ORDER BY occurred_at DESC LIMIT 5;'
```

修改 Go 代码后重新构建并载入镜像,再重启对应 Deployment:

```bash
make minikube-image   # 或推送到你的镜像仓库
kubectl rollout restart deployment/gateway
kubectl rollout restart deployment/agent
```

## 网关配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | 业务 HTTP 监听地址 |
| `ADMIN_ADDR` | `:9090` | 管理 HTTP 监听地址 |
| `ROUTES` | 空 | 路由表,`前缀=上游地址` 逗号分隔,如 `/api/v1/users/=http://user-service:8090` |
| `DATABASE_DSN` | 必填 | PostgreSQL 连接串 |
| `ADMIN_TOKEN` | 必填,至少 12 字符 | 管理接口 Bearer Token |
| `RATE_LIMIT_RPS` | `20` | 每个 IP 每秒补充的令牌数 |
| `RATE_LIMIT_BURST` | `40` | 每个 IP 的突发容量 |
| `LOG_QUEUE_SIZE` | `1024` | 有界日志队列容量 |
| `LOG_BATCH_SIZE` | `500` | 批量写入条数 |
| `LOG_FLUSH_INTERVAL` | `1s` | 批量写入间隔 |
| `SLOW_REQUEST_THRESHOLD` | `1s` | 保存完整报文的慢请求阈值 |
| `BODY_CAPTURE_BYTES` | `65536` | 请求和响应各自的最大采集字节数 |
| `UPSTREAM_TIMEOUT` | `3s` | 单次上游调用超时 |
| `AGENT_WEBHOOK_URL` | 空 | Agent 超时事件接收地址,为空则不推送 |
| `AGENT_WEBHOOK_TOKEN` | 空 | Agent 接收接口 Bearer Token,配置 URL 后必填 |
| `SHUTDOWN_TIMEOUT` | `10s` | 平滑退出超时 |

## Agent 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HTTP_ADDR` | `:8081` | 事件接收与报告查询监听地址 |
| `DATABASE_DSN` | 必填 | PostgreSQL 连接串,与网关共用数据库 |
| `INGEST_TOKEN` | 必填,至少 12 字符 | 事件接收与报告查询的 Bearer Token |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | OpenAI 兼容 API 地址 |
| `OPENAI_API_KEY` / `OPENAI_MODEL` | 必填 | 大模型凭据与模型名 |
| `AGENT_OPERATION_LEVEL` | `readonly` | 工具开放级别:`readonly` / `operator` / `full` |
| `K8S_NAMESPACES` | `default` | 允许操作的命名空间,逗号分隔,`*` 表示全部 |
| `K8S_MAX_REPLICAS` | `5` | Deployment 扩容上限 |
| `K8S_ACTION_COOLDOWN` | `2m` | 同一对象两次变更操作的最小间隔 |
| `AGENT_MAX_STEPS` | `12` | 单次排查的最大模型轮数 |
| `AGENT_MAX_TOOL_OUTPUT` | `4096` | 单次工具返回的最大字节数 |
| `AGENT_WORKERS` / `AGENT_QUEUE_SIZE` | `2` / `64` | 排查 worker 数与事件队列容量 |
| `AGENT_RUN_TIMEOUT` | `5m` | 单次排查的超时时间 |
| `AGENT_DEDUP_TTL` | `10m` | 事件去重窗口 |
| `GATEWAY_ADMIN_URL` / `GATEWAY_ADMIN_TOKEN` | 空 | 网关管理接口地址与 Token,供封禁工具使用 |
| `PROMETHEUS_URL` | 空 | Prometheus 查询地址,为空时 PromQL 工具不可用 |
| `SHUTDOWN_TIMEOUT` | `30s` | 平滑退出超时 |

## 当前边界

- 黑名单和限流状态保存在网关内存中,因此当前 Deployment 固定为 1 个副本;多副本前应迁移到 Redis 等共享存储。
- 异步日志采用有界、尽力投递队列,数据库长期不可用或队列饱和时会丢弃日志,避免拖垮业务请求。
- 网关与上游之间、Agent 与网关管理接口之间使用集群内明文 HTTP;跨集群或不可信网络应启用 TLS。
- PostgreSQL 清单面向本地开发,生产环境还需要高可用、备份、密钥管理和网络策略。

## 项目结构

```text
.
├── cmd/
│   ├── agent/                    # Agent 故障排查服务入口
│   └── gateway/                  # 通用网关入口
├── internal/
│   ├── agent/                    # 排查循环、编排器、事件 API 和配置
│   ├── agent/tools/              # 日志/PromQL/K8s/封禁工具及测试
│   ├── gateway/                  # 反向代理、中间件、规则和测试
│   ├── incident/                 # 超时事件、去重、报告持久化及测试
│   └── trafficlog/               # PostgreSQL、迁移、批写器和测试
├── examples/
│   └── user-service/             # 演示后端(非产品):HTTP 服务 + 故障注入 + 部署清单
├── deploy/kubernetes/
│   ├── agent.yaml                # Agent Deployment、Service 和 RBAC
│   ├── gateway.yaml              # 网关 Deployment 和 Service
│   └── postgres.yaml             # PostgreSQL StatefulSet、Service 和 PVC
├── docs/
│   ├── README.md                 # 文档索引与文档维护规则
│   ├── development.md            # 开发流程与约定
│   ├── deployment.md             # 部署与运维手册
│   ├── modules/                  # 分模块详细文档(改代码必须同步更新)
│   └── system-architecture.txt   # 立项时的总体设计稿
├── AGENTS.md                     # Agent 必读:铁律、架构、约定
├── Dockerfile                    # 通过 SERVICE_PATH 参数构建任意服务
└── Makefile                      # 常用开发命令
```

详细模块文档见 [docs/README.md](docs/README.md);总体设计稿见 `docs/system-architecture.txt`。
