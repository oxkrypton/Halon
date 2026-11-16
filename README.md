# Halon

Halon 是一个面向云原生场景的 Go 微服务项目。当前已实现 Gin HTTP 网关、gRPC 用户服务、PostgreSQL 异步流量日志存储，以及完整的 Minikube 部署配置。

## 当前架构

```text
HTTP Client
    |
    v
Gin Gateway (:8080)
    |-- Request ID / Recovery / Access Log
    |-- IP Blacklist / Per-IP Rate Limit
    |
    | gRPC
    v
user-service (:50051)

Gin Gateway -- bounded channel --> Batch Writer --> PostgreSQL
                                                   |-- base_logs
                                                   |-- error_body

Gateway Admin (:9090) -- Bearer Token --> blacklist / rate-limit rules
```

## 已完成功能

### 用户服务

- `GetUser` gRPC 接口、参数校验和标准状态码。
- gRPC 健康检查、服务反射和 SIGINT/SIGTERM 平滑退出。
- 双副本 Deployment、原生 gRPC 探针和资源限制。

### Gin 网关

- `GET /api/v1/users/:id` 将 HTTP 请求转换为 gRPC 调用，复用长连接。
- 统一 JSON 错误响应和 `X-Request-ID` 请求追踪。
- 内存 IP 黑名单及按客户端 IP 的令牌桶限流。
- 独立管理端口 `:9090`，支持动态更新黑名单和限流参数。
- `/healthz` 存活检查和 `/readyz` 依赖就绪检查。
- HTTP 超时、Panic 恢复、平滑退出及非 root 容器运行。

### PostgreSQL 日志存储

- `base_logs`：记录全部 HTTP 请求的 IP、路径、状态码和耗时。
- `error_body`：仅记录状态码不低于 400 或耗时达到阈值的请求报文，默认最多各截取 64 KiB。
- 有界队列异步写入，默认每 500 条或每 1 秒批量提交。
- 基础日志和详情日志在同一事务中写入，失败批次最多重试 3 次。
- 使用嵌入式、版本化 SQL 迁移，不使用自动改表。
- StatefulSet 配合 PersistentVolumeClaim，数据库 Pod 重建后数据仍保留。

## 环境要求

- Go 1.26+
- Protocol Buffers Compiler（`protoc`）
- Docker Desktop
- kubectl、Minikube
- `protoc-gen-go`、`protoc-gen-go-grpc`（仅重新生成接口代码时需要）

安装 Go 代码生成插件：

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
```

运行全部单元测试、竞态检测和静态检查：

```bash
make test
```

## 本地运行

启动 PostgreSQL：

```bash
docker run --rm --name halon-postgres \
  -e POSTGRES_DB=halon \
  -e POSTGRES_USER=halon \
  -e POSTGRES_PASSWORD=halon-local-password \
  -p 5432:5432 \
  -v halon-pgdata:/var/lib/postgresql/data \
  postgres:17-alpine
```

在另一个终端启动用户服务：

```bash
make run
```

再在另一个终端启动网关：

```bash
DATABASE_DSN='postgres://halon:halon-local-password@localhost:5432/halon?sslmode=disable' \
ADMIN_TOKEN='halon-local-admin-token' \
USER_SERVICE_ADDR='localhost:50051' \
make run-gateway
```

调用网关：

```bash
curl -i http://localhost:8080/api/v1/users/1
curl -i http://localhost:8080/readyz
```

内置演示用户为 `1 / Alice`、`2 / Bob`；查询其他 ID 会返回 `NotFound`。

## HTTP 接口

| 接口 | 说明 |
| --- | --- |
| `GET /api/v1/users/:id` | 通过 gRPC 查询用户 |
| `GET /healthz` | 进程存活检查，不依赖外部服务 |
| `GET /readyz` | 检查 PostgreSQL 和用户服务是否可用 |
| `GET /admin/rules/blacklist` | 查询黑名单，需要管理鉴权 |
| `PUT /admin/rules/blacklist/:ip` | 添加黑名单 IP，需要管理鉴权 |
| `DELETE /admin/rules/blacklist/:ip` | 删除黑名单 IP，需要管理鉴权 |
| `GET /admin/rules/rate-limit` | 查询限流参数，需要管理鉴权 |
| `PUT /admin/rules/rate-limit` | 更新限流参数，需要管理鉴权 |

管理接口鉴权头：

```text
Authorization: Bearer <ADMIN_TOKEN>
```

动态更新限流示例：

```bash
curl -X PUT http://localhost:9090/admin/rules/rate-limit \
  -H "Authorization: Bearer halon-local-admin-token" \
  -H 'Content-Type: application/json' \
  -d '{"requests_per_second":10,"burst":20}'
```

主要错误映射：

| 场景 | HTTP 状态码 | 错误代码 |
| --- | --- | --- |
| 参数无效 | 400 | `INVALID_ARGUMENT` |
| 用户不存在 | 404 | `NOT_FOUND` |
| IP 被拉黑 | 403 | `IP_BLOCKED` |
| 请求超过限流阈值 | 429 | `RATE_LIMITED` |
| 用户服务不可用 | 503 | `UPSTREAM_UNAVAILABLE` |
| 用户服务调用超时 | 504 | `UPSTREAM_TIMEOUT` |
| 其他上游错误 | 502 | `UPSTREAM_ERROR` |

## 部署到 Minikube

首次使用创建集群：

```bash
make minikube-start
```

创建本地开发 Secret。以下凭据仅用于本机演示，不应复用于生产环境：

```bash
kubectl create secret generic halon-secrets \
  --from-literal=postgres-user=halon \
  --from-literal=postgres-password=halon-local-password \
  --from-literal=database-dsn='postgres://halon:halon-local-password@postgres:5432/halon?sslmode=disable' \
  --from-literal=admin-token='halon-local-admin-token'
```

构建并载入两个服务镜像，然后先启动数据库：

```bash
make minikube-image
kubectl apply -f deploy/kubernetes/postgres.yaml
kubectl rollout status statefulset/postgres --timeout=180s
```

部署网关和用户服务：

```bash
make deploy
kubectl rollout status deployment/gateway --timeout=180s
kubectl rollout status deployment/user-service --timeout=180s
make status
```

### 验证 HTTP 到 gRPC 调用

```bash
kubectl port-forward service/gateway 18080:8080
```

在另一个终端执行：

```bash
curl -i http://127.0.0.1:18080/readyz
curl -i http://127.0.0.1:18080/api/v1/users/1
curl -i http://127.0.0.1:18080/api/v1/users/404
```

如需测试管理接口，再开启一个端口转发：

```bash
kubectl port-forward service/gateway-admin 19090:9090
```

添加并删除黑名单：

```bash
curl -i -X PUT http://127.0.0.1:19090/admin/rules/blacklist/127.0.0.1 \
  -H 'Authorization: Bearer halon-local-admin-token'
curl -i http://127.0.0.1:18080/api/v1/users/1
curl -i -X DELETE http://127.0.0.1:19090/admin/rules/blacklist/127.0.0.1 \
  -H 'Authorization: Bearer halon-local-admin-token'
```

等待至少一个日志刷新周期后查询 PostgreSQL：

```bash
kubectl exec postgres-0 -- psql -U halon -d halon \
  -c 'SELECT method, path, status_code, latency_ms FROM base_logs ORDER BY id DESC LIMIT 10;'

kubectl exec postgres-0 -- psql -U halon -d halon \
  -c 'SELECT base_log_id, request_body, response_body FROM error_body ORDER BY id DESC LIMIT 5;'
```

修改 Go 代码后重新构建并载入镜像，再重启对应 Deployment：

```bash
make minikube-image
kubectl rollout restart deployment/gateway
kubectl rollout restart deployment/user-service
```

## 网关配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | 业务 HTTP 监听地址 |
| `ADMIN_ADDR` | `:9090` | 管理 HTTP 监听地址 |
| `USER_SERVICE_ADDR` | `localhost:50051` | gRPC 用户服务地址 |
| `DATABASE_DSN` | 必填 | PostgreSQL 连接串 |
| `ADMIN_TOKEN` | 必填，至少 12 字符 | 管理接口 Bearer Token |
| `RATE_LIMIT_RPS` | `20` | 每个 IP 每秒补充的令牌数 |
| `RATE_LIMIT_BURST` | `40` | 每个 IP 的突发容量 |
| `LOG_QUEUE_SIZE` | `1024` | 有界日志队列容量 |
| `LOG_BATCH_SIZE` | `500` | 批量写入条数 |
| `LOG_FLUSH_INTERVAL` | `1s` | 批量写入间隔 |
| `SLOW_REQUEST_THRESHOLD` | `1s` | 保存完整报文的慢请求阈值 |
| `BODY_CAPTURE_BYTES` | `65536` | 请求和响应各自的最大采集字节数 |
| `RPC_TIMEOUT` | `3s` | 单次 gRPC 调用超时 |
| `SHUTDOWN_TIMEOUT` | `10s` | 平滑退出超时 |

## 当前边界

- 黑名单和限流状态保存在网关内存中，因此当前 Deployment 固定为 1 个副本；多副本前应迁移到 Redis 等共享存储。
- 异步日志采用有界、尽力投递队列，数据库长期不可用或队列饱和时会丢弃日志，避免拖垮业务请求。
- 当前 gRPC 使用集群内明文连接；跨集群或不可信网络应启用 TLS。
- PostgreSQL 清单面向本地开发，生产环境还需要高可用、备份、密钥管理和网络策略。

## 项目结构

```text
.
├── api/
│   ├── gen/user/v1/              # 生成的 Go 代码
│   └── proto/user/v1/            # Protobuf 接口定义
├── cmd/
│   ├── gateway/                  # Gin 网关入口
│   ├── user-client/              # gRPC 验证客户端
│   └── user-service/             # 用户服务入口
├── internal/
│   ├── gateway/                  # HTTP、中间件、规则和测试
│   ├── trafficlog/               # PostgreSQL、迁移、批写器和测试
│   └── user/                     # 用户业务实现及测试
├── deploy/kubernetes/
│   ├── gateway.yaml              # 网关 Deployment 和 Service
│   ├── postgres.yaml             # PostgreSQL StatefulSet、Service 和 PVC
│   └── user-service.yaml         # 用户服务 Deployment 和 Service
├── docs/                         # 总体架构设计资料
├── Dockerfile                    # 通过 SERVICE 参数构建不同服务
└── Makefile                      # 常用开发命令
```

详细设计见 `docs/system-architecture.txt`。
