# 模块:Gin 网关(gateway)

代码:`internal/gateway/`(实现)+ `cmd/gateway/main.go`(入口)。
监听:业务端口 `:8080`(`HTTP_ADDR`)、管理端口 `:9090`(`ADMIN_ADDR`)。

## 职责

1. `GET /api/v1/users/:id` 把 HTTP 请求转换为对 user-service 的 gRPC 调用;
2. IP 黑名单、按 IP 令牌桶限流、`X-Request-ID` 请求追踪、Panic 恢复;
3. 全量请求日志异步写入 PostgreSQL(见 [trafficlog](trafficlog.md));
4. 上游 gRPC 超时时生成 `incident.TimeoutEvent` 并经 webhook 推送给 Agent 服务;
5. 暴露管理接口(黑名单/限流动态调整)和 Prometheus 指标。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `server.go` | `NewHTTPHandler`(业务路由)、`NewAdminHandler`(`/admin/rules/*` + `/metrics`)、`newRouter`(RequestID/Recovery/NoRoute)、`readinessHandler` |
| `handler.go` | `UserHandler.GetUser`;`handleUserRPCError` 把 gRPC 状态码映射为 HTTP;超时时设置 `upstream_timeout` 标记 |
| `accesslog.go` | `AccessLog` 中间件:采集请求/响应体(各自最多 `BODY_CAPTURE_BYTES`),投递日志队列;检测超时标记并投递超时事件队列 |
| `notify.go` | `TimeoutNotifier`:消费超时事件队列,POST JSON 到 `AGENT_WEBHOOK_URL`,带 Bearer Token,最多 3 次指数退避重试 |
| `rules.go` | `IPRules`:内存黑名单,IP 规范化(`canonicalIP`),`Middleware()` 拦截 |
| `ratelimit.go` | `RateLimiter`:每 IP 独立令牌桶(`golang.org/x/time/rate`),`Update` 动态调参 |
| `metrics.go` | Prometheus 指标与 `/metrics` Handler |
| `request.go` | `RequestID` 中间件(校验/生成)、`Recovery`、`adminAuthentication`(常量时间比较) |
| `config.go` | `LoadConfig()`:全部环境变量与启动校验 |

## 关键流程

### 超时事件上报(与 Agent 服务对接)

```text
UserHandler.GetUser → gRPC DeadlineExceeded
  → handleUserRPCError 设置 c.Set("upstream_timeout", true),返回 504
  → AccessLog 中间件检测到标记,构造 incident.TimeoutEvent
     (EventID 新生成,RequestID 复用请求 ID)
  → 非阻塞投递到 TimeoutQueue(满则丢弃并计数告警)
  → TimeoutNotifier.Run() 消费,POST 到 AGENT_WEBHOOK_URL
     (Authorization: Bearer AGENT_WEBHOOK_TOKEN;2xx 成功;4xx 不重试;5xx/网络错误重试 2 次)
```

`AGENT_WEBHOOK_URL` 为空时整个超时事件链路不启用(队列不创建,事件直接不生成)。

### 优雅退出顺序(`cmd/gateway/main.go`)

停两个 HTTP server → `close(logQueue)` / `close(timeoutQueue)` → 等 BatchWriter 和 TimeoutNotifier 排空 → 关数据库。顺序不能颠倒,否则在途 handler 会向已关闭的 channel 发送导致 panic。

## 配置(环境变量)

完整表格见根 `README.md` 的「网关配置」。必填:`DATABASE_DSN`、`ADMIN_TOKEN`(≥12 字符);设置 `AGENT_WEBHOOK_URL` 时 `AGENT_WEBHOOK_TOKEN` 必填(≥12 字符)。

## 错误映射

| gRPC 状态码 | HTTP | 错误代码 |
| --- | --- | --- |
| `InvalidArgument` | 400 | `INVALID_ARGUMENT` |
| `NotFound` | 404 | `NOT_FOUND` |
| `DeadlineExceeded` | 504 | `UPSTREAM_TIMEOUT`(同时触发超时事件) |
| `Unavailable` | 503 | `UPSTREAM_UNAVAILABLE` |
| 其他 | 502 | `UPSTREAM_ERROR` |

## 测试

`server_test.go`(路由/探针)、`observability_test.go`(访问日志与超时事件投递)、`config_test.go`(配置校验)。新增路由或中间件时在同文件补测试。

## 修改本模块时必须同步检查

- 根 `README.md`:架构图、「Gin 网关」功能列表、HTTP 接口表、错误映射表、网关配置表;
- `docs/deployment.md`:若改了端口、环境变量或 Secret 键;
- `deploy/kubernetes/gateway.yaml`:若改了端口或新增环境变量;
- [modules/incident.md](incident.md):若改了 `TimeoutEvent` 的生成时机或字段;
- [modules/agent.md](agent.md):若改了 webhook 协议(路径/鉴权/事件体)。

## 已知边界

- 黑名单与限流是内存态,Deployment 固定 1 副本;
- 日志与超时事件队列均为有界、满则丢弃,保证不拖垮业务请求。
