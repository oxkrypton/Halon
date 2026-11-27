# Halon

[![CI](https://github.com/oxkrypton/Halon/actions/workflows/ci.yml/badge.svg)](https://github.com/oxkrypton/Halon/actions/workflows/ci.yml)

> **开发者/Agent 注意**:修改代码后必须同步更新文档,规则见 [AGENTS.md](AGENTS.md) 与 [docs/README.md](docs/README.md)。分模块详细文档见 [docs/](docs/README.md)。

Halon 是一个**开源的云原生网关 Agent 项目**:一个通用 HTTP 网关 + 一个由大模型驱动的故障排查 Agent,两者彼此独立、通过契约通信,可部署到**任意 Kubernetes 集群**(Minikube、Kind、ACK、EKS 等)。

- **网关(gateway)**:通用反向代理,按 `ROUTES` 环境变量把路径前缀转发到任意上游 HTTP 服务;自带 IP 黑名单、按 IP 限流、流量日志落库、六类网关异常识别与阈值归并上报。**不感知任何具体业务服务。**
- **Agent 服务(agent)**:接收网关归一化异常事件,按事件类型与范围匹配排查画像并调用 OpenAI 兼容大模型,借助日志/PromQL/网关状态/K8s 只读工具取证,按权限级别执行 Pod 重启、扩容、IP 封禁等变更,产出中文 Markdown 排查报告并持久化。

`examples/user-service` 与 `examples/order-service` 是演示用后端(带故障注入),**不属于产品本体**,仅用于本地开发与故障演练。

## 当前架构

```text
HTTP Client
    |
    v
Gin Gateway (:8080)                      # 通用反向代理
    |-- Request ID / Recovery / Access Log
    |-- IP Blacklist / Per-IP Rate Limit
    |-- Resilience: bulkhead --> circuit breaker --> retry (failsafe-go)
    |
    | ROUTES: prefix=upstream(纯 HTTP,可选 OTel traceparent 透传)
    v
任意上游服务(如 examples/ 下的 user-service、order-service 演示后端)

Gin Gateway -- bounded channel --> Batch Writer --> PostgreSQL
                                                   |-- base_logs
                                                   |-- error_body
                                                   |-- incident_reports

Gin Gateway -- /metrics + OTLP traces --> 可选观测栈
                                         (Prometheus / Tempo)

Gateway Admin (:9090) -- Bearer Token --> blacklist / rate-limit / breakers

Gin Gateway -- 异常阈值归一化 webhook --> Agent Service (:8081, 单实例)
                                          |-- 事件去重(TTL + ON CONFLICT)
                                          |-- Session 编排器(排队/取消/启动恢复)
                                          |-- 只读工具:日志 / PromQL / 网关状态 / Pod / 事件
                                          |-- 操作工具(按级别开放):重启 / 扩容 / 封禁
                                          +-- 故障报告持久化 --> incident_reports
```

## 已完成功能

### Gin 网关(通用反向代理)

- `ROUTES` 环境变量声明路由表(`前缀=上游地址`,逗号分隔,最长前缀优先),上游可以是集群内任意 HTTP 服务。
- 上游弹性策略(基于 [failsafe-go](https://github.com/failsafe-go/failsafe-go)):拨号/TLS/响应头分阶段超时、幂等请求的有预算重试(默认关)、每路由熔断(503 + `Retry-After` 快速失败)、每路由并发上限(429 快速失败)、请求体大小上限。
- 上游响应原样透传;识别 6 类网关异常(`upstream_timeout` 504、`upstream_error` 502、`breaker_open` 熔断跳闸、`upstream_5xx` 上游 5xx 突增、`concurrency_limit` 429 并发打满、`rate_limit_abuse` 429 单 IP 限流异常),按类型可配置开启与阈值条件上报。
- 统一 JSON 错误响应和 `X-Request-ID` 请求追踪(自动透传到上游)。
- 基于 Redis 的 IP 黑名单(进程内缓存 + pub/sub 失效与 TTL 兜底)、按客户端 IP 的 Redis Lua 令牌桶限流,以及跨副本按 `(event_type, scope_key)` 异常归一化、突增阈值判定与发件冷却(`INCIDENT_SETTLE_DELAY` 默认 `15s` 聚集、`INCIDENT_COALESCE_COOLDOWN` 默认 `5m` 冷却、`INCIDENT_RETRY_BACKOFF` 默认 `1m` 退避)。
- 独立管理端口 `:9090`,支持动态更新黑名单和限流参数、查询各路由熔断状态与只读 Redis 运行态(`/admin/state/*`),暴露 Prometheus 指标(全指标清单见 [docs/modules/gateway.md](docs/modules/gateway.md))。
- 可选 OpenTelemetry 追踪(`OTEL_ENABLED`):自动注入/透传 W3C `traceparent`,上游零改动;`trace_id`、`route`、`upstream_latency_ms` 写入 PostgreSQL `base_logs` 并打印到结构化日志。配套可选观测栈见 `deploy/kubernetes/observability/`。
- 可选 Web 管理控制台(`web/`):Google Cloud Console 风格 React 前端,内置 P50/P95/P99 分位数与运行态保护/队列健康看板、类 Jaeger 链路追踪瀑布流(支持 Tempo 或 PG 降级)、请求/错误日志、基于 K8s API 的实时容器日志、Agent AI 故障报告与熔断/黑名单/限流/用户管控,详见 [docs/modules/web.md](docs/modules/web.md)。
- `/healthz` 存活检查和 `/readyz` 数据库就绪检查。
- HTTP 超时、Panic 恢复、平滑退出及非 root 容器运行。

### PostgreSQL 日志存储

- `base_logs`:记录全部 HTTP 请求的 IP、路径、路由、状态码、总耗时、上游耗时、上游结果、重试次数与可选 `trace_id`。
- `error_body`:仅记录状态码不低于 400 或耗时达到阈值的请求报文,默认最多各截取 64 KiB。
- 有界队列异步写入,默认每 500 条或每 1 秒批量提交。
- 基础日志和详情日志在同一事务中写入,失败批次最多重试 3 次。
- 使用嵌入式、版本化 SQL 迁移,不使用自动改表。
- StatefulSet 配合 PersistentVolumeClaim,数据库 Pod 重建后数据仍保留。

### Agent 故障排查服务

- `POST /api/v1/incidents` 接收网关归一化后的通用异常事件 webhook(同时兼容 `POST /api/v1/incidents/timeout`),Bearer 鉴权 + 事件体严格校验。
- 事件去重与 Session 编排:内存 TTL 去重器 + `ON CONFLICT DO NOTHING` 保证事件幂等;单实例服务每次受理启动独立排查 Session(`GET /api/v1/sessions` 可查、`POST /api/v1/sessions/:event_id/cancel` 可取消),按 `(event_type, scope_key)` 对并发请求防重,重启时自动回收未完成行(`queued` 重排或过期收尾、`processing` 标记失败)。
- 有界队列 + 可配置 worker 数的编排器,每次排查有独立超时与可取消上下文,支持 readonly/operator/full 三级操作权限。
- 只读工具:支持按域组合查询的三大只读工具——`read_observability`(PostgreSQL 错误日志/流量统计/Top IP 与条件开放的 Prometheus PromQL 即时查询)、`read_gateway`(网关 Redis/黑名单/限流/熔断/活跃异常窗口状态,在配置管理地址与凭据后注册)、`read_k8s`(Pod/日志/事件/Deployment 状态)。
- 操作工具(operator):Pod 重启、Deployment 扩容(上限保护)、网关 IP 封禁;full 级别额外支持更新容器镜像。
- 命名空间白名单、进程内资源级串行锁(`ResourceLocker`)与冷却(`MemoryCooldownStore`),SQL 全部参数化,工具参数严格校验。
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

启动 PostgreSQL 与 Redis:

```bash
docker run --rm --name halon-postgres \
  -e POSTGRES_DB=halon \
  -e POSTGRES_USER=halon \
  -e POSTGRES_PASSWORD=halon-local-password \
  -p 5432:5432 \
  -v halon-pgdata:/var/lib/postgresql/data \
  postgres:17-alpine

docker run --rm --name halon-redis -p 6379:6379 redis:7-alpine
```

启动演示后端(第三个终端):

```bash
make run-demo    # 用户服务 :8090;订单演示:go run ./examples/order-service(:8092)
```

启动网关(第四个终端):

```bash
DATABASE_DSN='postgres://halon:halon-local-password@localhost:5432/halon?sslmode=disable' \
REDIS_URL='redis://localhost:6379/0' \
ADMIN_TOKEN='halon-local-admin-token' \
ROUTES='/api/v1/users/=http://localhost:8090,/api/v1/orders/=http://localhost:8092' \
make run-gateway
```

调用网关:

```bash
curl -i http://localhost:8080/api/v1/users/1
curl -i http://localhost:8080/api/v1/users/404
curl -i http://localhost:8080/readyz
```

如需本地验证 Agent 排查闭环,再开一个终端启动 Agent 服务(网关配置 `AGENT_WEBHOOK_URL=http://localhost:8081/api/v1/incidents` 和相同的 Token 后重启):

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

| 接口 | 权限 | 说明 |
| --- | --- | --- |
| `ANY <ROUTES 配置的前缀>` | — | 反向代理到对应上游,响应原样透传 |
| `GET /healthz` | — | 进程存活检查,不依赖外部服务 |
| `GET /readyz` | — | 检查 PostgreSQL 是否可用 |
| `GET /admin/rules/blacklist` | `gateway:rules:read` | 查询黑名单 |
| `PUT /admin/rules/blacklist/:ip` | `gateway:blacklist:write` | 添加黑名单 IP |
| `DELETE /admin/rules/blacklist/:ip` | `gateway:blacklist:write` | 删除黑名单 IP |
| `GET /admin/rules/rate-limit` | `gateway:rules:read` | 查询限流参数 |
| `PUT /admin/rules/rate-limit` | `gateway:ratelimit:write` | 更新限流参数 |
| `GET /admin/breakers` | `gateway:breaker:read` | 查询各路由熔断器状态 |
| `GET /admin/state/gateway` | `gateway:rules:read` | 只读查询网关 Redis 健康、黑名单列表、全局限流与各路由熔断状态 |
| `GET /admin/state/rate-limit/:ip` | `gateway:rules:read` | 只读查询指定 IP 黑名单命中与 Redis 令牌桶余量(不扣令牌) |
| `GET /admin/state/incident-windows` | `gateway:observability:read` | 只读查询异常归一化窗口(支持 `?type=` / `?scope=`)计数、样本路径/IP 与冷却剩余 TTL |
| `GET /admin/system` | `gateway:observability:read` | 查询当前登录主体、路由表、能力开关与 Redis 状态 |
| `GET /admin/observability/{overview,timeseries,routes,logs,logs/:id,top-ips}` | `gateway:observability:read` | 查询 PostgreSQL 精确 P50/P95/P99、时序、路由排行、请求报文与 Top IP |
| `GET /admin/traces` / `GET /admin/traces/:trace_id` | `gateway:observability:read` | 查询分布式链路列表与 Span 瀑布流(支持 Tempo 优先与 PostgreSQL 降级) |
| `POST /auth/login` / `refresh` / `logout` | — | 本地登录、Refresh Token 轮转与登出(仅 `AUTH_LOGIN_ENABLED=true`,否则 `404 LOGIN_DISABLED`) |
| `GET /auth/me` / `POST /auth/password` | Bearer JWT | 查看当前主体与自助改密(允许 `pcr=true` 令牌;仅 `AUTH_LOGIN_ENABLED=true`) |
| `GET /admin/users` / `GET /admin/users/:id` | `auth:user:read` | 查询本地用户列表或单个用户(仅 `AUTH_LOGIN_ENABLED=true`) |
| `POST /admin/users` / `PATCH /admin/users/:id` / `POST /admin/users/:id/password` | `auth:user:manage` | 创建用户、修改角色/启禁用/显示名、重置临时密码(仅 `AUTH_LOGIN_ENABLED=true`) |
| `GET /metrics` | — | Prometheus 指标(管理端口;`METRICS_REQUIRE_AUTH=true` 时需 `gateway:metrics:read` 的 Bearer 令牌,抓取不写审计日志) |

管理接口鉴权头(Bearer JWT 或兼容模式下的静态 Token):

```text
Authorization: Bearer <JWT | ADMIN_TOKEN>
```

无令牌/验签失败 `401 UNAUTHORIZED`,权限不足 `403 FORBIDDEN`。角色、claim 与签发工具见 [docs/modules/auth.md](docs/modules/auth.md)。

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
| 路由并发额度打满 | 429 | `CONCURRENCY_LIMIT`(带 `Retry-After`) |
| 请求体超过上限 | 413 | `REQUEST_TOO_LARGE` |
| 路由熔断器开启 | 503 | `UPSTREAM_UNAVAILABLE`(带 `Retry-After`) |
| 上游响应超时 | 504 | `UPSTREAM_TIMEOUT`(同时触发故障事件) |
| 上游连接失败 | 502 | `UPSTREAM_ERROR` |

### Agent 服务(`:8081`)

| 接口 | 凭据 | 说明 |
| --- | --- | --- |
| `POST /api/v1/incidents` | `INGEST_TOKEN` | 接收通用异常事件(`incident.Event`)并排队排查;重复事件返回 `duplicate: true`,同 `(event_type, scope_key)` 存在活跃 Session 时返回 `suppressed: true` |
| `POST /api/v1/incidents/timeout` | `INGEST_TOKEN` | 兼容入口,缺省 `type` 自动补为 `upstream_timeout` |
| `GET /api/v1/incidents` | `incident:read` | 列出最近的故障报告,支持 `?type=` 按事件类型过滤,`limit` 参数最大 200 |
| `GET /api/v1/incidents/:event_id` | `incident:read` | 查询单个事件的排查报告与工具调用轨迹 |
| `GET /api/v1/sessions` | `incident:read` | 列出当前排队、运行或取消中的活跃排查 Session 快照 |
| `POST /api/v1/sessions/:event_id/cancel` | `incident:manage` | 手动取消活跃排查 Session |
| `GET /api/v1/k8s/pods` | `k8s:pod_logs:read` | 列出白名单命名空间下的 Pod 状态、Ready 与重启次数(`namespace`/`label_selector`/`limit`) |
| `GET /api/v1/k8s/pods/:pod/logs` | `k8s:pod_logs:read` | 读取 Pod 容器有界日志(`namespace`/`container`/`tail_lines`/`since_seconds`/`previous`,单次上限 1 MiB) |
| `GET /healthz` / `GET /readyz` | — | 存活检查 / PostgreSQL 就绪检查 |
| `GET /metrics` | — | Prometheus 指标(`halon_agent_active_sessions`、`halon_agent_incident_admissions_total`,无需鉴权) |

`legacy`/`hybrid` 下 `INGEST_TOKEN` 也可读报告(兼容),`jwt` 下读接口只接受 `aud=halon-agent` 的 JWT。

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

先启动数据库,再部署产品组件(`make deploy` 会同时部署 Redis、网关与 Agent):

```bash
kubectl apply -f deploy/kubernetes/postgres.yaml
kubectl rollout status statefulset/postgres --timeout=180s
make deploy
kubectl rollout status deployment/redis deployment/gateway deployment/agent --timeout=180s
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
kubectl apply -f examples/order-service/deploy.yaml
kubectl rollout status deployment/gateway deployment/agent deployment/user-service deployment/order-service --timeout=180s
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
| `ROUTES` | 空 | 路由表,`前缀=上游地址` 逗号分隔,如 `/api/v1/users/=http://user-service:8090,/api/v1/orders/=http://order-service:8092` |
| `DATABASE_DSN` | 必填 | PostgreSQL 连接串 |
| `ADMIN_TOKEN` | `legacy`/`hybrid` 下必填,至少 12 字符 | 管理接口静态 Token,映射为 `full` 主体;`jwt` 下可为空 |
| `AUTH_MODE` | `legacy` | `legacy` / `hybrid` / `jwt`,见 [auth.md](docs/modules/auth.md) |
| `AUTH_JWT_PUBLIC_KEYS` | 空 | `kid=base64公钥,...`;`hybrid`/`jwt` 下必填 |
| `AUTH_JWT_ISSUER` | `halon` | JWT 期望 issuer |
| `AUTH_REVOKED_JTIS` | 空 | 逗号分隔的紧急撤销 jti |
| `AUTH_LOGIN_ENABLED` | `false` | 是否启用 `/auth/*` 与 `/admin/users/*` 本地登录与用户管理(需 `AUTH_MODE=hybrid/jwt`) |
| `AUTH_JWT_PRIVATE_KEY` / `AUTH_JWT_KID` | 空 / 自动派生 | 本地登录签发用的 base64 Ed25519 私钥与可选 `kid`(`AUTH_LOGIN_ENABLED=true` 时私钥必填) |
| `AUTH_ACCESS_TOKEN_TTL` / `AUTH_REFRESH_TOKEN_TTL` / `AUTH_REFRESH_MAX_LIFETIME` | `15m` / `168h` / `720h` | 本地会话 Access Token 有效期(`1m..1h`)、Refresh Token TTL 与 family 最长寿命 |
| `AUTH_PASSWORD_MIN_LENGTH` | `12` | 密码策略最小长度(`8..128`) |
| `AUTH_LOGIN_RATE_LIMIT_RPS` / `AUTH_LOGIN_RATE_LIMIT_BURST` | `0.2` / `5` | `/auth/{login,refresh,logout}` 单 IP 限流参数 |
| `AUTH_LOGIN_MAX_FAILURES` / `AUTH_LOGIN_LOCK_DURATION` | `10` / `5m` | 连续登录失败锁定阈值与锁定时长 |
| `AUTH_EVENT_RETENTION` | `2160h` | `auth_events` 审计记录保留时长 |
| `AUTH_BOOTSTRAP_ADMIN_USER` / `AUTH_BOOTSTRAP_ADMIN_PASSWORD` | 空 | 首次启动(`auth_users` 为空)原子引导的 `full` 管理员账号与初始密码(需成对设置) |
| `RATE_LIMIT_RPS` | `20` | 每个 IP 每秒补充的令牌数 |
| `RATE_LIMIT_BURST` | `40` | 每个 IP 的突发容量 |
| `LOG_QUEUE_SIZE` | `1024` | 有界日志队列容量 |
| `LOG_BATCH_SIZE` | `500` | 批量写入条数 |
| `LOG_FLUSH_INTERVAL` | `1s` | 批量写入间隔 |
| `SLOW_REQUEST_THRESHOLD` | `1s` | 保存完整报文的慢请求阈值 |
| `BODY_CAPTURE_BYTES` | `65536` | 请求和响应各自的最大采集字节数 |
| `UPSTREAM_TIMEOUT` | `3s` | 上游调用总超时(覆盖含重试的全部尝试) |
| `UPSTREAM_DIAL_TIMEOUT` | `2s` | 上游 TCP 拨号超时 |
| `UPSTREAM_TLS_HANDSHAKE_TIMEOUT` | `3s` | 上游 TLS 握手超时 |
| `UPSTREAM_RESPONSE_HEADER_TIMEOUT` | 同 `UPSTREAM_TIMEOUT` | 等待上游响应头的超时,不得超过 `UPSTREAM_TIMEOUT` |
| `UPSTREAM_MAX_CONNS_PER_HOST` | `64` | 单个上游的最大连接数 |
| `MAX_REQUEST_BODY_BYTES` | `10485760` (10 MiB) | 请求体上限,超限 413;`0` 表示不限 |
| `RETRY_MAX_ATTEMPTS` | `1` | 幂等请求的最大尝试次数(含首次),`1` 即不重试,硬上限 3 |
| `RETRY_BODY_BUFFER_BYTES` | `1048576` (1 MiB) | 可重试请求的体缓冲上限,超限不重试 |
| `BREAKER_ENABLED` | `true` | 每路由熔断器开关 |
| `BREAKER_FAILURE_THRESHOLD` | `5` | 熔断的连续失败阈值 |
| `BREAKER_OPEN_DURATION` | `30s` | 熔断 open 持续时长(之后进入 half-open 探测) |
| `BREAKER_HALF_OPEN_MAX` | `1` | half-open 转 closed 需要的连续成功数 |
| `BREAKER_COUNT_5XX` | `true` | 是否把上游 5xx 计入熔断失败 |
| `CONCURRENCY_PER_ROUTE` | `256` | 每路由并发上限,满则 429;`0` 表示不限 |
| `LOG_LEVEL` | `info` | 日志级别:`debug` / `info` / `warn` / `error` |
| `METRICS_REQUIRE_AUTH` | `false` | `/metrics` 是否需要管理 Bearer 鉴权 |
| `OTEL_ENABLED` | `false` | OpenTelemetry 追踪开关 |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `otel-collector.observability.svc.cluster.local:4317` | OTLP gRPC 端点(host:port,不带 scheme) |
| `OTEL_TRACES_SAMPLER_RATIO` | `1.0` | 采样比例(0–1,父 span 优先) |
| `TEMPO_URL` | 空 | 可选 Tempo HTTP 查询地址(如 `http://tempo.observability.svc.cluster.local:3200`) |
| `TEMPO_TIMEOUT` | `5s` | Tempo 查询 HTTP 超时 |
| `AGENT_WEBHOOK_URL` | 空 | Agent 异常事件接收地址,为空则不推送 |
| `AGENT_WEBHOOK_TOKEN` | 空 | Agent 接收接口 Bearer Token,配置 URL 后必填 |
| `INCIDENT_QUEUE_SIZE` | `128` | 有界异常事件队列容量(兼容读取 `TIMEOUT_QUEUE_SIZE`) |
| `INCIDENT_ENABLED_TYPES` | 全部 6 类内置类型 | 逗号分隔的启用异常类型(或 `none` 全部关闭) |
| `INCIDENT_SETTLE_DELAY` | `15s` | 即开型事件首次开窗后的聚集等待时长(`0` 表示立即投递);突增型聚集取 `min(10s, INCIDENT_SETTLE_DELAY)` |
| `INCIDENT_BREAKER_SETTLE_DELAY` | `5s` | `breaker_open` 开窗后的聚集等待时长 |
| `INCIDENT_COALESCE_COOLDOWN` | `5m` | 常规异常成功投递后的归一化冷却期,`0` 表示关闭冷却 |
| `INCIDENT_RETRY_BACKOFF` | `1m` | webhook 投递失败后的短退避(默认上限为 cooldown,不可超过 cooldown) |
| `INCIDENT_5XX_THRESHOLD` / `INCIDENT_5XX_WINDOW` | `10` / `30s` | `upstream_5xx` 在检测窗口内达标触发排查的累计阈值与窗口长度 |
| `INCIDENT_CONCURRENCY_THRESHOLD` / `INCIDENT_CONCURRENCY_WINDOW` | `10` / `30s` | `concurrency_limit` 突增触发的阈值与检测窗口 |
| `INCIDENT_RATELIMIT_THRESHOLD` / `INCIDENT_RATELIMIT_WINDOW` / `INCIDENT_RATELIMIT_COOLDOWN` | `50` / `60s` / `10m` | `rate_limit_abuse`(按客户端 IP 归并)的触发阈值、检测窗口与冷却期 |
| `REDIS_URL` | 必填 | Redis 连接串(如 `redis://redis:6379/0`),承载黑名单、限流与异常归一化状态 |
| `BLACKLIST_CACHE_TTL` | `10s` | 黑名单进程内缓存的兜底刷新间隔 |
| `REDIS_DIAL_TIMEOUT` | `1s` | Redis 拨号超时 |
| `REDIS_OP_TIMEOUT` | `500ms` | Redis 单次操作超时(同时作为读写 socket 超时) |
| `SHUTDOWN_TIMEOUT` | `10s` | 平滑退出超时 |

## Agent 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HTTP_ADDR` | `:8081` | 事件接收与报告查询监听地址 |
| `DATABASE_DSN` | 必填 | PostgreSQL 连接串,与网关共用数据库 |
| `INGEST_TOKEN` | 必填,至少 12 字符 | webhook 事件接收凭据;兼容期内也可读报告,`jwt` 下仅用于 ingest |
| `AUTH_MODE` / `AUTH_JWT_PUBLIC_KEYS` / `AUTH_JWT_ISSUER` / `AUTH_REVOKED_JTIS` | `legacy` / 空 / `halon` / 空 | 与网关同名的鉴权配置,见 [auth.md](docs/modules/auth.md) |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | OpenAI 兼容 API 地址 |
| `OPENAI_API_KEY` / `OPENAI_MODEL` | 必填 | 大模型凭据与模型名 |
| `AGENT_OPERATION_LEVEL` | `readonly` | 工具开放级别:`readonly` / `operator` / `full` |
| `K8S_NAMESPACES` | `default` | 允许操作的命名空间,逗号分隔,`*` 表示全部 |
| `K8S_MAX_REPLICAS` | `5` | Deployment 扩容上限 |
| `K8S_ACTION_COOLDOWN` | `2m` | 同一对象两次变更操作的最小间隔 |
| `AGENT_MAX_STEPS` | `12` | 单次排查的最大模型轮数 |
| `AGENT_MAX_TOOL_OUTPUT` | `4096` | 喂回模型的单次工具输出上限(字节),保护模型上下文 |
| `AGENT_MAX_STORED_TOOL_OUTPUT` | `262144`(256 KiB) | 工具输出落库(`incident_reports.executions`)的上限(字节),超出部分截断并置 `output_truncated` |
| `AGENT_WORKERS` / `AGENT_QUEUE_SIZE` | `2` / `64` | 排查 worker 数与事件队列容量 |
| `AGENT_RUN_TIMEOUT` | `5m` | 单次排查的超时时间 |
| `AGENT_RECOVERY_MAX_AGE` | `30m` | 启动恢复时允许重新排队的 `queued` 事件最大年龄,超龄直接收尾为 `failed` |
| `AGENT_DEDUP_TTL` | `10m` | 事件去重窗口 |
| `GATEWAY_ADMIN_URL` / `GATEWAY_ADMIN_TOKEN` | 空 | 网关管理接口地址与凭据;两者均非空时才注册 `read_gateway` 与 `gateway_block_ip`。`readonly` 若无需查网关状态可不配,需查网关状态且为 `jwt` 模式时配 `role=readonly` 的 Agent JWT;非 `readonly` 在 `jwt` 模式下必须配 `typ=agent` 且角色与操作级别一致的 JWT(启动自检) |
| `PROMETHEUS_URL` | 空 | Prometheus 查询地址,为空时 `read_observability` 不暴露 `prometheus` 子数据源 |
| `SHUTDOWN_TIMEOUT` | `30s` | 平滑退出超时 |

## 当前边界

- 网关黑名单/限流/跨副本按 `(event_type, scope_key)` 异常归一化与突增阈值判定统一基于 Redis(`REDIS_URL` 为网关必填项;限流与异常归一化在 Redis 不可达时降级本地兜底,黑名单写返回 503),网关支持单副本或 2+ 副本(熔断器与并发舱壁是有意的 per-replica 语义,不外迁)。
- Agent 服务为单实例控制面(`replicas: 1`、`strategy.type: Recreate`,不连接 Redis),每次受理启动独立 Session,进程内通过资源串行锁与内存冷却防止并发 Session 竞争;事件去重由数据库唯一约束表决,启动时自动回收未完成行(`queued` 重排或过期收尾、`processing` 标记失败避免重复变更)。Redis 部署说明见 `docs/deployment.md`。
- 异步日志采用有界、尽力投递队列,数据库长期不可用或队列饱和时会丢弃日志,避免拖垮业务请求。
- 网关与上游之间、Agent 与网关管理接口之间使用集群内明文 HTTP;跨集群或不可信网络应启用 TLS。
- 弹性策略依赖 failsafe-go(v0.x,API 可能变动),全部封装在 `internal/gateway/resilience.go`;追踪依赖 OpenTelemetry Go SDK,默认关闭。
- 鉴权默认 `AUTH_MODE=legacy` 且 `AUTH_LOGIN_ENABLED=false`(行为不变);切到 `hybrid`/`jwt` 需两端同时配公钥,并先把调用方换成 JWT。`INGEST_TOKEN` 不进入角色体系;长期有效的 Agent JWT 靠 K8s Secret 保护。开启 `AUTH_LOGIN_ENABLED=true` 时使用独立迁移表 `auth_schema_migrations`,详见 [docs/modules/auth.md](docs/modules/auth.md)。
- 观测栈(`deploy/kubernetes/observability/`)是可选参考件,单副本小资源配置,生产使用需自行扩容与加固。
- PostgreSQL 清单面向本地开发,生产环境还需要高可用、备份、密钥管理和网络策略。

## 项目结构

```text
.
├── cmd/
│   ├── agent/                    # Agent 故障排查服务入口
│   ├── gateway/                  # 通用网关入口
│   └── halonctl/                 # 离线密钥生成与 JWT 签发/查看 CLI
├── internal/
│   ├── agent/                    # 排查循环、Session 编排器、事件 API 和配置
│   ├── agent/tools/              # 日志/PromQL/网关状态/K8s/封禁工具及测试
│   ├── gateway/                  # 反向代理、弹性策略(resilience.go)、OTel(tracing.go)、异常归一化、中间件、规则和测试
│   ├── authz/                    # 角色×权限、Ed25519 JWT 验证、gin 中间件
│   ├── identity/                 # 可选本地用户、Argon2id 密码策略、Refresh Token 轮转与清理
│   ├── incident/                 # 通用异常事件、去重、报告持久化及测试
│   └── trafficlog/               # PostgreSQL、迁移、批写器和测试
├── examples/
│   ├── user-service/             # 演示后端(非产品):HTTP 服务 + 故障注入 + 部署清单
│   └── order-service/            # 演示后端(非产品):订单接口 + 故障注入 + 部署清单
├── deploy/kubernetes/
│   ├── agent.yaml                # Agent Deployment、Service 和 RBAC
│   ├── gateway.yaml              # 网关 Deployment 和 Service
│   ├── postgres.yaml             # PostgreSQL StatefulSet、Service 和 PVC
│   ├── redis.yaml                # Redis Deployment 和 Service(开发参考清单)
│   ├── web/                      # 可选 Web 管理控制台 Deployment 和 Service
│   └── observability/            # 可选观测栈:OTel Collector、Tempo、Prometheus、Alertmanager
├── web/                          # Google 风格 React + TypeScript 管理控制台(Vite + ECharts + Nginx)
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
