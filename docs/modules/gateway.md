# 模块:Gin 网关(gateway)

代码:`internal/gateway/`(实现)+ `cmd/gateway/main.go`(入口)。
监听:业务端口 `:8080`(`HTTP_ADDR`)、管理端口 `:9090`(`ADMIN_ADDR`)。

**定位:通用 HTTP 反向代理。** 网关不感知任何具体业务服务,上游完全由 `ROUTES` 环境变量声明。

## 职责

1. 按路由表把请求前缀代理到任意上游 HTTP 服务,响应原样透传;
2. IP 黑名单、按 IP 令牌桶限流、`X-Request-ID` 请求追踪(透传到上游)、Panic 恢复;
3. 上游弹性策略:分阶段超时、有预算的重试、每路由熔断、每路由并发上限(基于 `failsafe-go`,见 `resilience.go`);
4. 全量请求日志异步写入 PostgreSQL(见 [trafficlog](trafficlog.md));
5. 识别并归一化六类网关异常(`upstream_timeout` / `upstream_error` / `breaker_open` / `upstream_5xx` / `concurrency_limit` / `rate_limit_abuse`),通过 webhook 推送给 Agent 服务;
6. 暴露管理接口(黑名单/限流动态调整、熔断状态、只读 Redis 与异常窗口检查)和 Prometheus 指标;可选 OpenTelemetry 追踪(W3C `traceparent` 注入上游,上游零改动)。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `proxy.go` | `RouteTable`:`ParseRoutes` 解析 `前缀=上游` 配置(最长前缀优先、启动时校验);`ProxyHandler` 反向代理(保留完整路径、注入 `X-Forwarded-For`、按请求限时、请求体上限与重试体缓冲);`newUpstreamTransport` 分阶段超时(拨号/TLS/响应头)与连接池;`upstreamErrorHandler` 错误分类(超时 504、熔断 503、并发满 429、体过大 413、其余 502 并标记对应异常类型,排除客户端主动取消);`routeLabel` 统一指标/事件的路由标签 |
| `resilience.go` | 弹性策略组合(`failsafe-go`):每路由 bulkhead(并发上限)→ circuit breaker(熔断,`OnOpen` 向 `IncidentSink` 上报 `breaker_open`)→ retry(仅传输层错误、仅幂等);`observingTransport` 记录上游调用指标、耗时与单次请求结果;`ResilienceSet.BreakerStatuses` 供管理接口查询 |
| `tracing.go` | OpenTelemetry 接入:`OTEL_ENABLED=true` 时初始化 OTLP gRPC exporter 与 TracerProvider;`WrapHandler` 给业务端口打 server span,`WrapTransport` 给上游调用打 client span 并注入 `traceparent`;禁用时全部透传零开销 |
| `tempo.go` | 只读 Tempo HTTP 客户端(`/api/search` 与 `/api/traces/:id`),归一化 OTLP JSON Span 树;未配置 `TEMPO_URL` 时通过 `synthesizeTraceFromLogs` 从 PG 日志构造「网关+上游」两级瀑布流 |
| `observability_handlers.go` | 管理面观测聚合与系统信息接口:`/admin/system`、`/admin/observability/{overview,timeseries,routes,logs,logs/:id,top-ips}`(其中 `overview` 附带当前副本 `runtime` 运行态指标快照) |
| `trace_handlers.go` | 管理面链路追踪接口:`/admin/traces` 与 `/admin/traces/:trace_id`(支持 Tempo 优先 + PG 降级与关联报文) |
| `server.go` | `NewHTTPHandler`:探针路由 + 把代理链(黑名单 → 限流 → 路由表)挂载到 `NoRoute`(任意前缀无需注册路由);`NewAdminHandler`(`/admin/rules/*` + `/admin/breakers` + `/admin/state/*` + `/admin/system` + `/admin/observability/*` + `/admin/traces/*` + `/metrics` + 可选 `/auth/*` 与 `/admin/users/*`,`authz.Middleware` 认证 + 按路由 `authz.Require` 授权,写/读请求统一审计日志与指标);`readinessHandler`(只查数据库) |
| `state_handlers.go` | 只读 Redis 与运行态查询处理器:`GET /admin/state/gateway`、`/admin/state/rate-limit/:ip`(只读不扣令牌)、`/admin/state/incident-windows`(支持 `?type=` / `?scope=`) |
| `incident_coalescer.go` | `IncidentCoalescer` / `RedisIncidentCoalescer`:基于 Redis Lua 的跨副本按 `(event_type, scope_key)` 异常归一化、突增阈值判定(`pending -> active`)、活跃索引(`halon:incident:active`)与冷却窗口(Redis 不可达时降级到进程内窗口) |
| `incidents.go` | `IncidentSink`:nil-safe 非阻塞异常生产器,负责填充默认事件 ID/时间戳/`ScopeKey`、按 `INCIDENT_ENABLED_TYPES` 过滤并在队列满时丢弃计数 |
| `notify.go` | `IncidentNotifier`:消费异常事件队列,按类型策略经 `IncidentCoalescer` 进行阈值晋升/去重与 `settle_delay` 聚集,随后携带归并次数与样本 POST JSON 到 `AGENT_WEBHOOK_URL`(带 Bearer Token,最多 3 次指数退避重试);成功刷新类型对应 `Cooldown`,失败缩短为 `RetryBackoff`;退出时立即冲刷挂起窗口 |
| `auth_handlers.go` | 本地登录/刷新/登出/自助改密(`/auth/login`、`/auth/refresh`、`/auth/logout`、`/auth/me`、`/auth/password`,均受体大小上限与 `/auth/{login,refresh,logout}` 限流保护);未开启 `AUTH_LOGIN_ENABLED` 时返回 `404 LOGIN_DISABLED` |
| `user_handlers.go` | 管理员用户管理接口(`/admin/users`、`POST /admin/users`、`/admin/users/:id`、`PATCH /admin/users/:id`、`POST /admin/users/:id/password`),校验 UUID 与字段长度,保护自身与最后管理员 |
| `errors.go` | 统一错误响应结构 `abortWithError` 与请求异常分类键 `gateway_incident_type` / `gateway_incident_detail` |
| `accesslog.go` | `AccessLog` 中间件:采集请求/响应体(各自最多 `BODY_CAPTURE_BYTES`),连同 `trace_id`、`route`、`upstream_latency_ms`、`upstream_result`、`retries` 投递日志队列;读取异常分类或上游透传 5xx(`UpstreamResultSuccess`)并经 `IncidentSink.Emit` 非阻塞入队;维护在途请求 gauge 与队列丢弃计数 |
| `rules.go` | `BlacklistStore` 接口(Add/Remove/Contains/List)、`ErrBlacklistUnavailable`、IP 规范化(`canonicalIP`)与 `BlacklistMiddleware` 拦截计数 |
| `rules_redis.go` | `RedisBlacklistStore`:Redis 集合 `halon:blacklist` 为事实源,进程内缓存服务读路径;pub/sub 即时失效 + TTL 兜底刷新;写失败返回 `ErrBlacklistUnavailable`(管理接口 503) |
| `ratelimit.go` | `RateLimitBackend` 接口(Allow/Update/Settings)、`ErrRateLimitUnavailable`、`RateLimitMiddleware` 拦截计数并标记 `rate_limit_abuse` |
| `ratelimit_redis.go` | `RedisRateLimiter`:每 IP 一个令牌桶存 Redis,Lua 脚本原子 refill/扣减(时钟取 Redis `TIME`,副本间无漂移),参数存 `<prefix>config`(默认 `halon:ratelimit:config`,登录限流通过 `WithRateLimitKeyPrefix("halon:ratelimit:login:")` 隔离),进程内缓存 10s;Redis 不可达时读路径 fail-open 放行,写失败返回 `ErrRateLimitUnavailable`(管理接口 503) |
| `metrics.go` | Prometheus 指标、`RuntimeSnapshot()` 运行态快照聚合与 `/metrics` Handler(独立 Registry,全部方法 nil-safe) |
| `request.go` | `RequestID` 中间件(校验/生成)、`Recovery`;Bearer 鉴权已迁到 `internal/authz` |
| `config.go` | `LoadConfig()` 与 `IncidentPolicies()`:全部环境变量解析、校验与异常策略构造 |

## 关键流程

### 路由与代理

```text
请求 → RequestID → AccessLog → Recovery → 无静态路由命中
  → NoRoute:BlacklistMiddleware → RateLimitMiddleware → ProxyHandler
  → 最长前缀匹配 ROUTES → 请求体上限(MaxBytesReader) → 重试资格判定(幂等方法/Idempotency-Key + 体缓冲)
  → httputil.ReverseProxy → failsafe 策略链:bulkhead(并发上限)→ circuit breaker(熔断)→ retry(可选)
  → observingTransport(上游指标/结果) → http.Transport(拨号/TLS/响应头分阶段超时)
  → 上游响应原样返回;错误时 upstreamErrorHandler 写 413/429/502/503/504
```

- 代理挂在 `NoRoute` 是为了支持任意前缀而不与 gin 路由树冲突;`HandleMethodNotAllowed` 必须保持 `false`,否则非 GET 请求会被 405 拦截而进不了代理。
- 指标与路由类事件的 `route` 标签 = gin 路由模式,代理请求则为命中的路由前缀。
- **重试只发生在传输层错误**(连接拒绝/重置/各类超时,响应头尚未写出),此时客户端什么都没收到,重放绝对安全;不重试已收到的 5xx 响应。默认 `RETRY_MAX_ATTEMPTS=1` 即关闭。
- **熔断按路由前缀各自独立**(内存态):连续失败(传输错误,`BREAKER_COUNT_5XX` 时含上游 5xx;客户端主动取消不计)达到阈值 → open,期间快速失败 503 + `Retry-After`;open 时长后进入 half-open,探测成功即 closed。客户端取消(context.Canceled)不记为上游失败。
- **并发上限**(bulkhead)满时立即 429 + `Retry-After: 1`,不排队;重试期间持占用并发额度,抑制重试风暴。
- 整体 `UPSTREAM_TIMEOUT` 覆盖含重试在内的全部尝试;单次尝试的边界由传输层分阶段超时承担。

### 异常事件检测、归一化与上报(与 Agent 服务对接)

| `event_type` | 触发来源 | `scope_key` | 默认触发与冷却策略 |
| --- | --- | --- | --- |
| `upstream_timeout` | `upstreamErrorHandler` 504 | `route` | 即开(`min=1`),聚集 `15s`,冷却 `5m` |
| `upstream_error` | `upstreamErrorHandler` 502(排除 `context.Canceled`) | `route` | 即开(`min=1`),聚集 `15s`,冷却 `5m` |
| `breaker_open` | `resilience.go` 熔断器 `OnOpen` 回调 | `route` | 即开(`min=1`),聚集 `5s`,冷却 `5m` |
| `upstream_5xx` | `AccessLog` 上游直接返回的 5xx(`UpstreamResultSuccess`) | `route` | 阈值(`10 次 / 30s`),聚集 `10s`,冷却 `5m` |
| `concurrency_limit` | `upstreamErrorHandler` 429 舱壁满 | `route` | 阈值(`10 次 / 30s`),聚集 `10s`,冷却 `5m` |
| `rate_limit_abuse` | `RateLimitMiddleware` 429 限流拒绝 | `client_ip` | 阈值(`50 次 / 60s`),聚集 `10s`,冷却 `10m` |

```text
异常触发点(ProxyHandler / RateLimitMiddleware / Resilience OnOpen / AccessLog 上游 5xx)
  → IncidentSink.Emit:校验开启类型、补齐 EventID/ScopeKey，非阻塞入队(满则丢弃计数)
  → IncidentNotifier.Run() 消费,调用 RedisIncidentCoalescer.Record:
     - 即开型(min=1):直接创建 active 窗(halon:incident:win:<type>:<scope_key>),写入活跃索引 ZSET,等待 SettleDelay 后投递
     - 阈值型(min>1):先创建 pending 计数窗(TTL=DetectWindow);达标瞬间原子晋升 active 并触发 SettleDelay 聚集;未达标自然过期清零
     - active 窗口内后续事件只原子累加计数与有界样本,计入 halon_gateway_incident_events_coalesced_total
     - 投递 2xx 成功:TTL 设为 Cooldown;投递失败:TTL 缩短为 RetryBackoff
     - 优雅退出:关闭前立即冲刷尚在聚集等待中的挂起窗口
```

`AGENT_WEBHOOK_URL` 为空时整个事件上报链路不启用(队列与 `IncidentSink` 不创建)。Redis 不可达时 `RedisIncidentCoalescer` 降级到进程内本地窗口,避免网关放大风暴。

### Redis 状态后端(黑名单、限流与异常归一化)

黑名单存储在 Redis 集合 `halon:blacklist`:

- **读路径零 Redis 往返**:每请求只查进程内缓存;两级失效——pub/sub 频道 `halon:blacklist:invalidate` 即时通知 + `BLACKLIST_CACHE_TTL`(默认 10s)兜底刷新,消息丢失由 TTL 兜住;
- **生效窗口**:新增/解除封禁最坏 10s 内在全部副本生效;管理接口写响应带 `propagation: "eventually, <=10s"`,不承诺实时。需要即时生效时经 `kubectl port-forward` 逐 Pod 调管理接口(见 `docs/deployment.md`);
- **降级**:Redis 不可达时读路径退化为最后一份缓存,`halon_gateway_redis_up` 归 0;写操作返回 503 `BLACKLIST_UNAVAILABLE`(安全功能不允许"看似写成功")。

限流由 Redis Lua 令牌桶承载,多副本合计限流量严格等于配置值:

- **Lua 令牌桶**:每请求一次脚本往返,refill/扣减在 Redis 内原子完成,时钟取 Redis `TIME`;key 带 TTL(补满空桶时间 + 60s),空闲自动回收;
- **参数动态调整**:`PUT /admin/rules/rate-limit` 改写 `halon:ratelimit:config`,各副本 10s 配置缓存内生效,无需逐副本进进程;
- **降级**:Redis 不可达时 `Allow` fail-open 放行并打告警日志,`halon_gateway_redis_up` 归 0;参数更新返回 503 `RATELIMIT_UNAVAILABLE`。

### 优雅退出顺序(`cmd/gateway/main.go`)

停两个 HTTP server → `close(logQueue)` / `close(incidentQueue)` → 等 BatchWriter 和 IncidentNotifier 排空 → 若启用本地登录则 `cleaner.Close()` 停止过期认证数据清理器 → 关数据库;TracerProvider 随进程退出前刷新(span 有界批量,不阻塞)。顺序不能颠倒,否则在途 handler 会向已关闭的 channel 发送导致 panic。

## 配置(环境变量)

完整表格见根 `README.md` 的「网关配置」。必填:`DATABASE_DSN`、`REDIS_URL`;`ADMIN_TOKEN`(≥12 字符)在 `legacy`/`hybrid` 下必填,`jwt` 模式下可为空。`ROUTES` 为空是合法的(网关只提供探针,业务请求一律 404);设置 `AGENT_WEBHOOK_URL` 时 `AGENT_WEBHOOK_TOKEN` 必填(≥12 字符)。鉴权模式与公钥见 [auth.md](auth.md)。

弹性与可观测性配置要点:

- `UPSTREAM_RESPONSE_HEADER_TIMEOUT` 默认跟随 `UPSTREAM_TIMEOUT` 且不允许超过它(启动校验),保证 504 语义始终由整体 deadline 兜底;
- `RETRY_MAX_ATTEMPTS` 默认 1(不重试),硬上限 3,防止意外放大上游压力;
- `BREAKER_ENABLED` 默认 true;`CONCURRENCY_PER_ROUTE` 默认 256,`0` 表示不限;
- `MAX_REQUEST_BODY_BYTES` 默认 10 MiB,`0` 表示不限;
- `OTEL_ENABLED` 默认 false,开启需配套 `OTEL_EXPORTER_OTLP_ENDPOINT`(host:port,不带 scheme)。
- `TEMPO_URL` 默认空(可选),配置为 Tempo HTTP 地址(如 `http://tempo.observability.svc.cluster.local:3200`)后 `/admin/traces*` 将查询 Tempo 分布式链路;`TEMPO_TIMEOUT` 默认 `5s`。

## 错误语义

- 上游响应(包括 4xx/5xx)**原样透传**,网关不包装;
- 网关自身错误统一 `{error:{code,message,request_id}}`:401 `UNAUTHORIZED` / 403 `FORBIDDEN`(管理接口鉴权,见 [auth.md](auth.md))、404 `ROUTE_NOT_FOUND`、403 `IP_BLOCKED`、429 `RATE_LIMITED` / `CONCURRENCY_LIMIT`、413 `REQUEST_TOO_LARGE`、503 `UPSTREAM_UNAVAILABLE`(熔断开启,带 `Retry-After`)、503 `BLACKLIST_UNAVAILABLE` / `RATELIMIT_UNAVAILABLE`(Redis 不可达时的写操作)、504 `UPSTREAM_TIMEOUT`、502 `UPSTREAM_ERROR`。

## 指标清单(`/metrics`,管理端口)

所有 label 取值有界(`route` 为路由前缀或 `none`/`unmatched`,`type` 为内置事件类型或 `other`,其余为枚举),禁止把 path/client_ip/scope_key 作为 label:

| 指标 | 类型 | Labels | 说明 |
| --- | --- | --- | --- |
| `halon_gateway_http_requests_total` | Counter | method, route, status | 请求总数 |
| `halon_gateway_http_request_duration_seconds` | Histogram | method, route | 端到端耗时 |
| `halon_gateway_response_size_bytes` | Histogram | route | 响应体大小 |
| `halon_gateway_in_flight_requests` | Gauge | — | 网关在途请求 |
| `halon_gateway_rejected_requests_total` | Counter | reason(blacklist/rate_limit/concurrency_limit/body_too_large) | 前置拒绝 |
| `halon_gateway_admin_requests_total` | Counter | role, permission, result(allow/deny) | 管理接口鉴权决策(仅 401/403 鉴权拒绝计 `deny`,下游业务错误计 `allow`;`/metrics` 抓取不计) |
| `halon_gateway_upstream_requests_total` | Counter | route, result(success/timeout/error) | 上游调用尝试 |
| `halon_gateway_upstream_request_duration_seconds` | Histogram | route | 上游调用耗时(与端到端差值 ≈ 网关开销) |
| `halon_gateway_upstream_in_flight` | Gauge | route | 上游在途(bulkhead 占用) |
| `halon_gateway_retries_total` / `halon_gateway_retry_exhausted_total` | Counter | route | 重试发生 / 预算耗尽 |
| `halon_gateway_circuit_breaker_state` | Gauge | route | 0=closed 1=half-open 2=open |
| `halon_gateway_circuit_breaker_trips_total` | Counter | route | 熔断触发次数 |
| `halon_gateway_trafficlog_dropped_total` | Counter | — | 流量日志队列丢弃 |
| `halon_gateway_incident_events_dropped_total` | Counter | type | 异常事件队列丢弃 |
| `halon_gateway_incident_events_coalesced_total` | Counter | type, route | 被合并进活跃归一化窗口的原始异常事件数 |
| `halon_gateway_incidents_emitted_total` | Counter | type, route | 成功投递给 Agent 的归一化异常事件数 |
| `halon_gateway_webhook_notify_failures_total` | Counter | — | 异常事件推送 Agent 最终失败 |
| `halon_gateway_redis_up` | Gauge | — | Redis 可达性(1=可达,0=降级) |

## 测试

`auth_test.go`(管理接口角色×端点矩阵、legacy/hybrid/jwt 模式、审计指标)、`auth_handlers_test.go`(登录/刷新/登出/改密/限流/禁用态)、`user_handlers_test.go`(用户管理角色矩阵、UUID 与字段校验、脱敏与错误映射)、`server_test.go`(代理转发/透传/超时/不可达/404、路由表解析、探针、管理接口、限流)、`observability_test.go`(六类异常事件触发、客户端取消排除与 webhook 投递)、`observability_handlers_test.go`(`/admin/system`、`/admin/observability/*`、`RuntimeSnapshot()` 计数聚合、`/admin/traces/*` 与参数/权限校验)、`state_handlers_test.go`(`/admin/state/{gateway,rate-limit/:ip,incident-windows}` 与 RBAC/参数/503 校验)、`incident_coalescer_test.go`(双副本 miniredis 40 并发抢窗、突增阈值晋升与未达标过期、按 type/scope 查询、owner 守卫、Redis 故障本地降级、关闭冲刷)、`tempo_test.go`(Tempo 搜索与 OTLP JSON 解码 + PG 降级瀑布流合成)、`resilience_test.go`(重试与体重放、熔断迁移与 503 快速失败、bulkhead 打满 429、请求体上限 413、响应头超时归类 504、`/metrics` 鉴权、`/admin/breakers`;上游传输用桩 RoundTripper 保证确定性)、`tracing_test.go`(OTel 禁用透传、server/client span 同 trace、`traceparent` 透传,用 in-memory span recorder)、`rules_redis_test.go`(黑名单增删、IPv6 规范化、外部写入刷新、双副本 pub/sub 传播、TTL 兜底、降级读缓存/写 503;`alicebob/miniredis` 进程内仿真)、`ratelimit_redis_test.go`(Lua 令牌桶 refill/突发边界、双副本合计限额、参数跨副本传播、降级 fail-open/写 503)、`config_test.go`(配置与策略校验)。真实上游一律用 `httptest.NewServer`。

## 修改本模块时必须同步检查

- 根 `README.md`:架构图、「Gin 网关」功能列表、HTTP 接口表、错误映射表、网关配置表;
- `docs/deployment.md`:若改了端口、环境变量或 Secret 键;
- `deploy/kubernetes/gateway.yaml`:若改了端口或新增环境变量;
- [modules/incident.md](incident.md):若改了 `Event` / `EventType` 的生成时机或字段;
- [modules/agent.md](agent.md):若改了 webhook 协议(路径/鉴权/事件体);
- [modules/auth.md](auth.md):若改了管理接口权限项、鉴权模式或审计字段。

## 已知边界

- 黑名单、限流与跨副本 `(event_type, scope_key)` 异常归一化/突增阈值判定由 Redis 承载,支持网关多副本;熔断器与并发舱壁是有意的 per-replica 语义(保护本副本到上游的连接与请求配额),不外迁;
- 日志与异常事件队列均为有界、满则丢弃,保证不拖垮业务请求;
- 与上游之间是明文 HTTP,跨集群或不可信网络应启用 TLS;
- 熔断/限流等弹性策略基于 `failsafe-go`(v0.x,API 可能随版本变动),全部封装在 `resilience.go`,升级依赖时只改这一处;
- 追踪默认关闭;开启后 span 经 OTLP 发往 Collector,Collector 不可达时 span 由 SDK 有界队列丢弃,不影响业务请求;
- `/metrics` 默认无鉴权(便于 Prometheus 抓取),跨不信任网络暴露时设 `METRICS_REQUIRE_AUTH=true`:此时只要求 `gateway:metrics:read`(`readonly` 即可),不挂审计中间件,每次抓取不写审计日志也不计 `halon_gateway_admin_requests_total`。
