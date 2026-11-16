# 模块:Gin 网关(gateway)

代码:`internal/gateway/`(实现)+ `cmd/gateway/main.go`(入口)。
监听:业务端口 `:8080`(`HTTP_ADDR`)、管理端口 `:9090`(`ADMIN_ADDR`)。

**定位:通用 HTTP 反向代理。** 网关不感知任何具体业务服务,上游完全由 `ROUTES` 环境变量声明。

## 职责

1. 按路由表把请求前缀代理到任意上游 HTTP 服务,响应原样透传;
2. IP 黑名单、按 IP 令牌桶限流、`X-Request-ID` 请求追踪(透传到上游)、Panic 恢复;
3. 上游弹性策略:分阶段超时、有预算的重试、每路由熔断、每路由并发上限(基于 `failsafe-go`,见 `resilience.go`);
4. 全量请求日志异步写入 PostgreSQL(见 [trafficlog](trafficlog.md));
5. 上游超时时生成 `incident.TimeoutEvent` 并经 webhook 推送给 Agent 服务;
6. 暴露管理接口(黑名单/限流动态调整、熔断状态)和 Prometheus 指标;可选 OpenTelemetry 追踪(W3C `traceparent` 注入上游,上游零改动)。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `proxy.go` | `RouteTable`:`ParseRoutes` 解析 `前缀=上游` 配置(最长前缀优先、启动时校验);`ProxyHandler` 反向代理(保留完整路径、注入 `X-Forwarded-For`、按请求限时、请求体上限与重试体缓冲);`newUpstreamTransport` 分阶段超时(拨号/TLS/响应头)与连接池;`upstreamErrorHandler` 错误分类(超时 504、熔断 503、并发满 429、体过大 413、其余 502);`routeLabel` 统一指标/事件的路由标签 |
| `resilience.go` | 弹性策略组合(`failsafe-go`):每路由 bulkhead(并发上限)→ circuit breaker(熔断)→ retry(仅传输层错误、仅幂等);`observingTransport` 记录上游调用指标与单次请求结果;`ResilienceSet.BreakerStatuses` 供管理接口查询 |
| `tracing.go` | OpenTelemetry 接入:`OTEL_ENABLED=true` 时初始化 OTLP gRPC exporter 与 TracerProvider;`WrapHandler` 给业务端口打 server span,`WrapTransport` 给上游调用打 client span 并注入 `traceparent`;禁用时全部透传零开销 |
| `server.go` | `NewHTTPHandler`:探针路由 + 把代理链(黑名单 → 限流 → 路由表)挂载到 `NoRoute`(任意前缀无需注册路由);`NewAdminHandler`(`/admin/rules/*` + `/admin/breakers` + `/metrics`,`METRICS_REQUIRE_AUTH` 可给 `/metrics` 加 Bearer 鉴权);`readinessHandler`(只查数据库) |
| `errors.go` | 统一错误响应结构 `abortWithError` 与 `upstream_timeout` 标记键 |
| `accesslog.go` | `AccessLog` 中间件:采集请求/响应体(各自最多 `BODY_CAPTURE_BYTES`),投递日志队列;检测超时标记并投递超时事件队列;日志字段含 `route`、`trace_id`(启用 OTel 时)、`upstream_result`、`retries`;维护在途请求 gauge 与队列丢弃计数 |
| `notify.go` | `TimeoutNotifier`:消费超时事件队列,POST JSON 到 `AGENT_WEBHOOK_URL`,带 Bearer Token,最多 3 次指数退避重试;最终失败计入 `halon_gateway_webhook_notify_failures_total` |
| `rules.go` | `BlacklistStore` 接口(Add/Remove/Contains/List)与 `ErrBlacklistUnavailable`;`IPRules`:内存实现(内存模式),IP 规范化(`canonicalIP`);`BlacklistMiddleware` 拦截并计数拒绝 |
| `rules_redis.go` | `RedisBlacklistStore`:共享实现——Redis 集合 `halon:blacklist` 为事实源,进程内缓存服务读路径;pub/sub 即时失效 + TTL 兜底刷新;写失败返回 `ErrBlacklistUnavailable`(管理接口 503) |
| `ratelimit.go` | `RateLimiter`:每 IP 独立令牌桶(`golang.org/x/time/rate`),`Update` 动态调参,拒绝计数 |
| `metrics.go` | Prometheus 指标与 `/metrics` Handler(独立 Registry,全部方法 nil-safe) |
| `request.go` | `RequestID` 中间件(校验/生成)、`Recovery`、`adminAuthentication`(常量时间比较) |
| `config.go` | `LoadConfig()`:全部环境变量与启动校验(含 `ROUTES` 语法校验、响应头超时 ≤ 总超时的交叉校验) |

## 关键流程

### 路由与代理

```text
请求 → RequestID → AccessLog → Recovery → 无静态路由命中
  → NoRoute:IPRules.Middleware → RateLimiter.Middleware → ProxyHandler
  → 最长前缀匹配 ROUTES → 请求体上限(MaxBytesReader) → 重试资格判定(幂等方法/Idempotency-Key + 体缓冲)
  → httputil.ReverseProxy → failsafe 策略链:bulkhead(并发上限)→ circuit breaker(熔断)→ retry(可选)
  → observingTransport(上游指标/结果) → http.Transport(拨号/TLS/响应头分阶段超时)
  → 上游响应原样返回;错误时 upstreamErrorHandler 写 413/429/502/503/504
```

- 代理挂在 `NoRoute` 是为了支持任意前缀而不与 gin 路由树冲突;`HandleMethodNotAllowed` 必须保持 `false`,否则非 GET 请求会被 405 拦截而进不了代理。
- 指标与超时事件的 `route` 标签 = gin 路由模式,代理请求则为命中的路由前缀。
- **重试只发生在传输层错误**(连接拒绝/重置/各类超时,响应头尚未写出),此时客户端什么都没收到,重放绝对安全;不重试已收到的 5xx 响应。默认 `RETRY_MAX_ATTEMPTS=1` 即关闭。
- **熔断按路由前缀各自独立**(内存态):连续失败(传输错误,`BREAKER_COUNT_5XX` 时含上游 5xx;客户端主动取消不计)达到阈值 → open,期间快速失败 503 + `Retry-After`;open 时长后进入 half-open,探测成功即 closed。客户端取消(context.Canceled)不记为上游失败。
- **并发上限**(bulkhead)满时立即 429 + `Retry-After: 1`,不排队;重试期间持占用并发额度,抑制重试风暴。
- 整体 `UPSTREAM_TIMEOUT` 覆盖含重试在内的全部尝试;单次尝试的边界由传输层分阶段超时承担。

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

### 共享模式(可选,`REDIS_URL` 非空时启用)

黑名单外迁到 Redis,事实源是集合 `halon:blacklist`:

- **读路径零 Redis 往返**:每请求只查进程内缓存;两级失效——pub/sub 频道 `halon:blacklist:invalidate` 即时通知 + `BLACKLIST_CACHE_TTL`(默认 10s)兜底刷新,消息丢失由 TTL 兜住;
- **生效窗口**:新增/解除封禁最坏 10s 内在全部副本生效;管理接口写响应带 `propagation: "eventually, <=10s"`,不承诺实时。需要即时生效时经 `kubectl port-forward` 逐 Pod 调管理接口(见 `docs/deployment.md`);
- **降级**:Redis 不可达时读路径退化为最后一份缓存,`halon_gateway_redis_up` 归 0;写操作返回 503 `BLACKLIST_UNAVAILABLE`(安全功能不允许"看似写成功");
- 滚动升级期间内存/共享副本并存,上述收敛承诺仅在全部副本进入共享模式后成立。

### 优雅退出顺序(`cmd/gateway/main.go`)

停两个 HTTP server → `close(logQueue)` / `close(timeoutQueue)` → 等 BatchWriter 和 TimeoutNotifier 排空 → 关数据库;TracerProvider 随进程退出前刷新(span 有界批量,不阻塞)。顺序不能颠倒,否则在途 handler 会向已关闭的 channel 发送导致 panic。

## 配置(环境变量)

完整表格见根 `README.md` 的「网关配置」。必填:`DATABASE_DSN`、`ADMIN_TOKEN`(≥12 字符);`ROUTES` 为空是合法的(网关只提供探针,业务请求一律 404);设置 `AGENT_WEBHOOK_URL` 时 `AGENT_WEBHOOK_TOKEN` 必填(≥12 字符)。

弹性与可观测性配置要点:

- `UPSTREAM_RESPONSE_HEADER_TIMEOUT` 默认跟随 `UPSTREAM_TIMEOUT` 且不允许超过它(启动校验),保证 504 语义始终由整体 deadline 兜底;
- `RETRY_MAX_ATTEMPTS` 默认 1(不重试),硬上限 3,防止意外放大上游压力;
- `BREAKER_ENABLED` 默认 true;`CONCURRENCY_PER_ROUTE` 默认 256,`0` 表示不限;
- `MAX_REQUEST_BODY_BYTES` 默认 10 MiB,`0` 表示不限;
- `OTEL_ENABLED` 默认 false,开启需配套 `OTEL_EXPORTER_OTLP_ENDPOINT`(host:port,不带 scheme)。

## 错误语义

- 上游响应(包括 4xx/5xx)**原样透传**,网关不包装;
- 网关自身错误统一 `{error:{code,message,request_id}}`:404 `ROUTE_NOT_FOUND`、403 `IP_BLOCKED`、429 `RATE_LIMITED` / `CONCURRENCY_LIMIT`、413 `REQUEST_TOO_LARGE`、503 `UPSTREAM_UNAVAILABLE`(熔断开启,带 `Retry-After`)、503 `BLACKLIST_UNAVAILABLE`(共享模式黑名单写时 Redis 不可达)、504 `UPSTREAM_TIMEOUT`(同时触发超时事件)、502 `UPSTREAM_ERROR`。

## 指标清单(`/metrics`,管理端口)

所有 label 取值有界(`route` 为路由前缀,其余为枚举),禁止把 path/client_ip 作为 label:

| 指标 | 类型 | Labels | 说明 |
| --- | --- | --- | --- |
| `halon_gateway_http_requests_total` | Counter | method, route, status | 请求总数 |
| `halon_gateway_http_request_duration_seconds` | Histogram | method, route | 端到端耗时 |
| `halon_gateway_response_size_bytes` | Histogram | route | 响应体大小 |
| `halon_gateway_in_flight_requests` | Gauge | — | 网关在途请求 |
| `halon_gateway_rejected_requests_total` | Counter | reason(blacklist/rate_limit/concurrency_limit/body_too_large) | 前置拒绝 |
| `halon_gateway_upstream_requests_total` | Counter | route, result(success/timeout/error) | 上游调用尝试 |
| `halon_gateway_upstream_request_duration_seconds` | Histogram | route | 上游调用耗时(与端到端差值 ≈ 网关开销) |
| `halon_gateway_upstream_in_flight` | Gauge | route | 上游在途(bulkhead 占用) |
| `halon_gateway_retries_total` / `halon_gateway_retry_exhausted_total` | Counter | route | 重试发生 / 预算耗尽 |
| `halon_gateway_circuit_breaker_state` | Gauge | route | 0=closed 1=half-open 2=open |
| `halon_gateway_circuit_breaker_trips_total` | Counter | route | 熔断触发次数 |
| `halon_gateway_trafficlog_dropped_total` / `halon_gateway_timeout_events_dropped_total` | Counter | — | 有界队列丢弃 |
| `halon_gateway_webhook_notify_failures_total` | Counter | — | 超时事件推送 Agent 最终失败 |
| `halon_gateway_redis_up` | Gauge | — | 共享模式 Redis 可达性(内存模式恒 1) |

## 测试

`server_test.go`(代理转发/透传/超时/不可达/404、路由表解析、探针、管理接口、限流)、`observability_test.go`(超时事件与指标、webhook 投递)、`resilience_test.go`(重试与体重放、熔断迁移与 503 快速失败、bulkhead 打满 429、请求体上限 413、响应头超时归类 504、`/metrics` 鉴权、`/admin/breakers`;上游传输用桩 RoundTripper 保证确定性)、`tracing_test.go`(OTel 禁用透传、server/client span 同 trace、`traceparent` 透传,用 in-memory span recorder)、`rules_redis_test.go`(共享黑名单增删、外部写入刷新、双副本 pub/sub 传播、TTL 兜底、降级读缓存/写 503;`alicebob/miniredis` 进程内仿真)、`config_test.go`(配置校验)。真实上游一律用 `httptest.NewServer`。

## 修改本模块时必须同步检查

- 根 `README.md`:架构图、「Gin 网关」功能列表、HTTP 接口表、错误映射表、网关配置表;
- `docs/deployment.md`:若改了端口、环境变量或 Secret 键;
- `deploy/kubernetes/gateway.yaml`:若改了端口或新增环境变量;
- [modules/incident.md](incident.md):若改了 `TimeoutEvent` 的生成时机或字段;
- [modules/agent.md](agent.md):若改了 webhook 协议(路径/鉴权/事件体)。

## 已知边界

- 黑名单在共享模式下已外迁 Redis;限流、熔断器与并发舱壁仍是内存态,Deployment 固定 1 副本(多副本待限流外迁,见 `docs/proposals/shared-state-multireplica.md`);
- 日志与超时事件队列均为有界、满则丢弃,保证不拖垮业务请求;
- 与上游之间是明文 HTTP,跨集群或不可信网络应启用 TLS;
- 熔断/限流等弹性策略基于 `failsafe-go`(v0.x,API 可能随版本变动),全部封装在 `resilience.go`,升级依赖时只改这一处;
- 追踪默认关闭;开启后 span 经 OTLP 发往 Collector,Collector 不可达时 span 由 SDK 有界队列丢弃,不影响业务请求;
- `/metrics` 默认无鉴权(便于 Prometheus 抓取),跨不信任网络暴露时设 `METRICS_REQUIRE_AUTH=true`。
