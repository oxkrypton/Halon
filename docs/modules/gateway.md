# 模块:Gin 网关(gateway)

代码:`internal/gateway/`(实现)+ `cmd/gateway/main.go`(入口)。
监听:业务端口 `:8080`(`HTTP_ADDR`)、管理端口 `:9090`(`ADMIN_ADDR`)。

**定位:通用 HTTP 反向代理。** 网关不感知任何具体业务服务,上游完全由 `ROUTES` 环境变量声明。

## 职责

1. 按路由表把请求前缀代理到任意上游 HTTP 服务,响应原样透传;
2. IP 黑名单、按 IP 令牌桶限流、`X-Request-ID` 请求追踪(透传到上游)、Panic 恢复;
3. 全量请求日志异步写入 PostgreSQL(见 [trafficlog](trafficlog.md));
4. 上游超时时生成 `incident.TimeoutEvent` 并经 webhook 推送给 Agent 服务;
5. 暴露管理接口(黑名单/限流动态调整)和 Prometheus 指标。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `proxy.go` | `RouteTable`:`ParseRoutes` 解析 `前缀=上游` 配置(最长前缀优先、启动时校验);`ProxyHandler` 反向代理(保留完整路径、注入 `X-Forwarded-For`、按请求限时);`upstreamErrorHandler` 把超时映射为 504、其余失败为 502;`routeLabel` 统一指标/事件的路由标签 |
| `server.go` | `NewHTTPHandler`:探针路由 + 把代理链(黑名单 → 限流 → 路由表)挂载到 `NoRoute`(任意前缀无需注册路由);`NewAdminHandler`(`/admin/rules/*` + `/metrics`);`readinessHandler`(只查数据库) |
| `errors.go` | 统一错误响应结构 `abortWithError` 与 `upstream_timeout` 标记键 |
| `accesslog.go` | `AccessLog` 中间件:采集请求/响应体(各自最多 `BODY_CAPTURE_BYTES`),投递日志队列;检测超时标记并投递超时事件队列 |
| `notify.go` | `TimeoutNotifier`:消费超时事件队列,POST JSON 到 `AGENT_WEBHOOK_URL`,带 Bearer Token,最多 3 次指数退避重试 |
| `rules.go` | `IPRules`:内存黑名单,IP 规范化(`canonicalIP`),`Middleware()` 拦截 |
| `ratelimit.go` | `RateLimiter`:每 IP 独立令牌桶(`golang.org/x/time/rate`),`Update` 动态调参 |
| `metrics.go` | Prometheus 指标与 `/metrics` Handler |
| `request.go` | `RequestID` 中间件(校验/生成)、`Recovery`、`adminAuthentication`(常量时间比较) |
| `config.go` | `LoadConfig()`:全部环境变量与启动校验(含 `ROUTES` 语法校验) |

## 关键流程

### 路由与代理

```text
请求 → RequestID → AccessLog → Recovery → 无静态路由命中
  → NoRoute:IPRules.Middleware → RateLimiter.Middleware → ProxyHandler
  → 最长前缀匹配 ROUTES → httputil.ReverseProxy(保留路径,限时 UPSTREAM_TIMEOUT)
  → 上游响应原样返回;错误时 upstreamErrorHandler 写 502/504
```

- 代理挂在 `NoRoute` 是为了支持任意前缀而不与 gin 路由树冲突;`HandleMethodNotAllowed` 必须保持 `false`,否则非 GET 请求会被 405 拦截而进不了代理。
- 指标与超时事件的 `route` 标签 = gin 路由模式,代理请求则为命中的路由前缀。

### 超时事件上报(与 Agent 服务对接)

```text
ProxyHandler 带 UPSTREAM_TIMEOUT 的 context 调上游
  → 超时:upstreamErrorHandler 设置 c.Set("upstream_timeout", true),返回 504
  → AccessLog 中间件检测到标记,构造 incident.TimeoutEvent
     (EventID 新生成,RequestID 复用请求 ID,Route 为命中的路由前缀)
  → 非阻塞投递到 TimeoutQueue(满则丢弃并计数告警)
  → TimeoutNotifier.Run() 消费,POST 到 AGENT_WEBHOOK_URL
     (Authorization: Bearer AGENT_WEBHOOK_TOKEN;2xx 成功;4xx 不重试;5xx/网络错误重试 2 次)
```

`AGENT_WEBHOOK_URL` 为空时整个超时事件链路不启用(队列不创建,事件直接不生成)。

### 优雅退出顺序(`cmd/gateway/main.go`)

停两个 HTTP server → `close(logQueue)` / `close(timeoutQueue)` → 等 BatchWriter 和 TimeoutNotifier 排空 → 关数据库。顺序不能颠倒,否则在途 handler 会向已关闭的 channel 发送导致 panic。

## 配置(环境变量)

完整表格见根 `README.md` 的「网关配置」。必填:`DATABASE_DSN`、`ADMIN_TOKEN`(≥12 字符);`ROUTES` 为空是合法的(网关只提供探针,业务请求一律 404);设置 `AGENT_WEBHOOK_URL` 时 `AGENT_WEBHOOK_TOKEN` 必填(≥12 字符)。

## 错误语义

- 上游响应(包括 4xx/5xx)**原样透传**,网关不包装;
- 网关自身错误统一 `{error:{code,message,request_id}}`:404 `ROUTE_NOT_FOUND`、403 `IP_BLOCKED`、429 `RATE_LIMITED`、504 `UPSTREAM_TIMEOUT`(同时触发超时事件)、502 `UPSTREAM_ERROR`。

## 测试

`server_test.go`(代理转发/透传/超时/不可达/404、路由表解析、探针、管理接口、限流)、`observability_test.go`(超时事件与指标、webhook 投递)、`config_test.go`(配置校验)。上游一律用 `httptest.NewServer` 起真实 HTTP 服务。

## 修改本模块时必须同步检查

- 根 `README.md`:架构图、「Gin 网关」功能列表、HTTP 接口表、错误映射表、网关配置表;
- `docs/deployment.md`:若改了端口、环境变量或 Secret 键;
- `deploy/kubernetes/gateway.yaml`:若改了端口或新增环境变量;
- [modules/incident.md](incident.md):若改了 `TimeoutEvent` 的生成时机或字段;
- [modules/agent.md](agent.md):若改了 webhook 协议(路径/鉴权/事件体)。

## 已知边界

- 黑名单与限流是内存态,Deployment 固定 1 副本;
- 日志与超时事件队列均为有界、满则丢弃,保证不拖垮业务请求;
- 与上游之间是明文 HTTP,跨集群或不可信网络应启用 TLS。
