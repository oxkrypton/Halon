# 网关可观测性与健壮性增强方案 v2(设计稿)

> 状态:**已实施**(2026-11-17,`make test` / `make lint` 全绿) · 日期:2026-11-16
> 范围:`cmd/gateway`、`internal/gateway`、`internal/trafficlog`、部署清单与文档
> 约束:**对用户服务(上游)零侵入**——上游无需改代码、无需装 SDK、无需配合发版。

## 实施说明(2026-11-17,与 v2 设计的偏差)

1. **策略链中去掉了 failsafe timeout policy**:单次尝试的边界由 Transport 的拨号/TLS/响应头超时天然承担(每个 RoundTrip 都是一次尝试),整体仍由 `context.WithTimeout(UPSTREAM_TIMEOUT)` 兜底——少一层策略,语义等价且更简单。
2. **重试不解析 `Retry-After`**:实现只对传输层错误重试(没有任何响应),`Retry-After` 无从谈起;v2 该条作废。
3. `BREAKER_HALF_OPEN_MAX` 落实为 failsafe-go 的 `WithSuccessThreshold`:half-open → closed 所需的连续成功数。
4. `OTEL_EXPORTER_OTLP_ENDPOINT` 格式为 `host:port`(不带 scheme),与 `otlptracegrpc.WithEndpoint` 对齐。
5. 日志采集选 **Grafana Alloy** 而非 Promtail(Promtail 已停止维护)。
6. `TimeoutNotifier` 构造函数增加 `*Metrics` 参数(webhook 失败计数),管理接口路由保持 `/admin/rules/*` 不变,新增 `GET /admin/breakers`。
7. 告警规则与看板落在 `deploy/kubernetes/observability/`(Prometheus rules ConfigMap + 预置 Grafana 看板),见 §11 P3。

## 0. v1 → v2 修订说明

v1 被批评"闭门造车",属实:v1 手写 traceparent 解析/透传、手写熔断状态机、手写退避重试,且完全没有回答"指标/日志/追踪数据存到哪、怎么看、怎么告警"。v2 的核心转变:

> **网关只做两件事:发射遥测(OTLP traces + Prometheus metrics + stdout JSON logs)、执行弹性策略。采集、路由、存储、关联、看板、告警全部交给成熟开源组件。**

| v1 的自建件 | v2 替换为 |
| --- | --- |
| 手写 W3C traceparent 中间件(阶段 A) | OpenTelemetry Go SDK + `otelhttp`(traces/metrics 均已 Stable) |
| 手写熔断状态机(`breaker.go`) | `failsafe-go` circuitbreaker |
| 手写退避重试 RoundTripper | `failsafe-go` retrypolicy + `failsafehttp.NewRoundTripper` |
| 手写并发信号量 | `failsafe-go` bulkhead |
| (无)观测数据后端 | OTel Collector + Prometheus + Loki + Tempo + Grafana + Alertmanager(LGTM),可选部署 |

仍保留自建的部件见 §8(它们要么已是产品差异化能力,要么库无法替代)。

## 1. 背景与目标

网关现状(以代码为准):指标仅 2 个且无 `/metrics` 认证;slog JSON 日志 + `X-Request-ID`;健壮性仅靠全局 `UPSTREAM_TIMEOUT`、每 IP 限流、内存黑名单;`http.Transport` 未调分阶段超时;无重试/熔断/并发上限;**无任何观测数据后端的接入路径**(Agent 的 `query_prometheus` 还假设集群里已有一个 Prometheus)。

目标不变:可观测性三信号(指标/日志/追踪)齐备且可关联;分阶段超时、重试、熔断、并发上限齐备;全部 fail-fast;遵守仓库全部代码约定。

## 2. 选型对比与决策

### 2.1 追踪:OTel SDK + otelhttp ✅(替换手写 traceparent)

- OpenTelemetry Go 的 **Traces 与 Metrics API/SDK 均 Stable**(Logs 仍 Beta,故日志不走 OTel SDK,见 5.4);`otelhttp` 一行包装即可给 Handler/Transport 自动打 span、按 W3C TraceContext 标准自动注入/提取 `traceparent`。上游不识别该头即忽略——零侵入由标准保证,且上游日后自愿接入 OTel 即可端到端串联。
- 手写 traceparent 解析要自行维护格式校验、采样位、b3 兼容等细节,是典型的重复造轮子,删除。
- 备选 OBI(OTel eBPF 无侵入探针):pre-1.0、Linux 限定,仅记录为"观测上游本体"的未来选项(见 §10),不采用。

### 2.2 弹性策略:failsafe-go ✅(替换自建熔断/重试/信号量)

| 候选 | 结论 |
| --- | --- |
| `failsafe-go` | **采用**。retry / circuit breaker(计数或时间窗口)/ timeout / **bulkhead(并发舱壁)** / hedge / fallback 一套库组合,且提供 `failsafehttp.NewRoundTripper`,与 v1 的"在 RoundTripper 层做弹性"设计天然吻合;支持 `Retry-After` DelayFunc、重试预算、事件回调(用于接指标)。 |
| `sony/gobreaker/v2` | 仅熔断,API 干净;但重试(cenkalti/backoff)、舱壁还得另拼,组合性弱。若评审认为 failsafe-go 过重,此为降级选项。 |
| 自建(v1) | 放弃:状态机并发正确性、滑动窗口统计、半开探测都要自己写和测,是本项目没有比较优势的领域。 |

注意:failsafe-go 为 v0.x,API 可能随 minor 版本变动——锁版本、封装在 `internal/gateway/resilience.go` 一处,升级影响面可控(见 §12 风险表)。

### 2.3 指标:保留 client_golang + `/metrics` ✅(不迁 OTel metrics)

理由:K8s 生态 Prometheus 抓取是事实标准;Agent 的 `query_prometheus` 工具已依赖一个 Prometheus 服务;现有 2 个指标及潜在看板继续兼容。OTel metrics 虽 Stable,但双套指标 API 只会增加复杂度。

### 2.4 观测后端:LGTM 开源栈 ✅(v1 完全缺失的部分)

| 信号 | 组件 | 接入方式 |
| --- | --- | --- |
| Traces | **Grafana Tempo** | 网关 → OTLP → **OTel Collector** → Tempo |
| Metrics | **Prometheus**(已有假设;长期存储可选 Mimir) | Prometheus 抓取网关 `/metrics` |
| Logs | **Grafana Loki** | 容器 stdout JSON,由 Promtail/Alloy 采集;`trace_id` 作为 label/字段实现日志↔追踪互跳 |
| 可视化/告警 | **Grafana + Alertmanager** | 预置看板(dashboards-as-code)、告警规则(错误率/P99/熔断/队列丢弃) |

架构原则:**网关只对一个 OTel Collector 说话 + 暴露 `/metrics`**;换后端(如 Tempo 换 Jaeger、上 SaaS)只改 Collector 配置,不动网关代码。整套栈作为**可选**清单放在 `deploy/kubernetes/observability/`,不进 `make deploy` 主路径;集群已有 kube-prometheus-stack 时只复用其 Prometheus/Alertmanager,文档给对接片段。

### 2.5 明确不做的

- 不用 Envoy/Kong 替换网关本体(Halon 自身就是网关项目)。
- 不上 Istio/Linkerd mesh(过重;仅备注:mesh 的 outlier detection 与本方案进程内熔断是互补层)。
- 不自建看板/告警 UI。

## 3. 目标架构

```text
                 ┌──────────────── Gateway (本方案改动全部在此)────────────────┐
 Client ──HTTP──▶│ otelhttp(server span) → RequestID → AccessLog → Rules/Limit │
                 │ → failsafe(bulkhead→breaker→retry→timeout) → otelhttp       │
                 │   (client span, 自动注入 traceparent) → Transport → 上游     │
                 └──────┬───────────────────┬───────────────────┬──────────────┘
                        │ OTLP traces       │ /metrics          │ stdout JSON logs
                        ▼                   ▼                   ▼
                 OTel Collector ──▶ Tempo   Prometheus ──▶ Alertmanager   Promtail/Alloy ──▶ Loki
                        └──────────────▶ Grafana(dashboards + trace_id 三信号互跳)
```

## 4. 网关侧改动

### 4.1 OTel 接入(`cmd/gateway` + `internal/gateway/tracing.go` 新增)

- `cmd/gateway` 初始化 TracerProvider:OTLP gRPC exporter、`resource` 带 `service.name=halon-gateway`、采样 `ParentBased(TraceIDRatioBased(OTEL_TRACES_SAMPLER_RATIO))`。
- 入口:`newHTTPServer` 的 handler 用 `otelhttp.NewHandler(router, "gateway")` 包装(gin 无需感知)。
- 出口:上游 `http.Transport` 用 `otelhttp.NewTransport(...)` 包装,自动产生 client span 并注入 `traceparent`。
- 关闭路径:`OTEL_ENABLED=false`(默认)时完全不初始化 SDK,零开销、无行为变化;部署观测栈后置 `true`。
- 环境变量:`OTEL_ENABLED`、`OTEL_EXPORTER_OTLP_ENDPOINT`(默认 `http://otel-collector:4317`)、`OTEL_TRACES_SAMPLER_RATIO`(默认 `1.0`)。

### 4.2 弹性策略组合(`internal/gateway/resilience.go` 新增)

在 proxy 的 RoundTripper 层按序组合(executor 内层先执行):

```
bulkhead(并发上限) → circuit breaker → retry(仅传输层错误、仅幂等) → per-attempt timeout
```

- **超时**:per-attempt 由 failsafe timeout policy 承担,整体仍由现有 `context.WithTimeout(UPSTREAM_TIMEOUT)` 兜底;504 语义不变。
- **重试**:仅 `GET/HEAD/OPTIONS` 或带 `Idempotency-Key` 的方法;`RETRY_MAX_ATTEMPTS` 默认 1(关);指数退避 + jitter;尊重 `Retry-After`(封顶 1s);Body 缓冲上限 `RETRY_BODY_BUFFER_BYTES`(默认 1 MiB),超限不重试。
- **熔断**:按路由前缀各持有一个 breaker(内存态,符合单副本边界);连续失败阈值、open 时长、半开探测数走环境变量;open 时快速失败 503 + `{error:{code:"UPSTREAM_UNAVAILABLE"}}` + `Retry-After`。
- **并发上限**:bulkhead `CONCURRENCY_PER_ROUTE`(默认 256,0 不限),满则立即 429 + `Retry-After: 1`,不排队。
- **指标钩子**:全部走 failsafe 事件回调(`OnStateChanged`/`OnRetryScheduled`/`OnFailure` 等)更新 §4.3 的 Prometheus 指标——状态机归库管,指标归我们。

### 4.3 指标扩展(`internal/gateway/metrics.go`)

现有 2 个指标保留不动,新增(label 全部有界,禁止 path/IP 入 label):

| 指标 | 类型 | Labels | 来源 |
| --- | --- | --- | --- |
| `halon_gateway_upstream_requests_total` | Counter | `route`,`result`(success/timeout/error) | proxy 错误路径 + failsafe 事件 |
| `halon_gateway_upstream_request_duration_seconds` | Histogram | `route` | otelhttp transport / 自行观测 |
| `halon_gateway_in_flight_requests` | Gauge | — | 中间件 |
| `halon_gateway_upstream_in_flight` | Gauge | `route` | bulkhead 事件 |
| `halon_gateway_rejected_requests_total` | Counter | `reason`(blacklist/rate_limit/concurrency_limit/body_too_large) | 各中间件 |
| `halon_gateway_retries_total` / `halon_gateway_retry_exhausted_total` | Counter | `route` | failsafe retry 事件 |
| `halon_gateway_circuit_breaker_state` | Gauge | `route` | failsafe `OnStateChanged` |
| `halon_gateway_circuit_breaker_trips_total` | Counter | `route` | 同上 |
| `halon_gateway_trafficlog_dropped_total` / `halon_gateway_timeout_events_dropped_total` | Counter | — | accesslog 队列丢弃处 |
| `halon_gateway_webhook_notify_failures_total` | Counter | — | notify |
| `halon_gateway_response_size_bytes` | Histogram | `route` | accesslog |

### 4.4 日志

- `LOG_LEVEL`(debug/info/warn/error,默认 info)注入 `slog.HandlerOptions`。
- AccessLog 增加字段:`route`、`trace_id`(取自 `trace.SpanContextFromContext`,未启用 OTel 时为空串,日志采集侧按空值过滤)、`upstream_result`、重试次数。
- 日志格式不变(JSON stdout),由 Promtail/Alloy 采集进 Loki;`trace_id` 贯通 Loki↔Tempo 互跳。**不引入 OTel Logs SDK(Beta)**。

### 4.5 传输层与分阶段超时(同 v1)

`UPSTREAM_DIAL_TIMEOUT`(2s)、`UPSTREAM_TLS_HANDSHAKE_TIMEOUT`(3s)、`UPSTREAM_RESPONSE_HEADER_TIMEOUT`(默认=`UPSTREAM_TIMEOUT`,启动校验 ≤ 总超时)、`UPSTREAM_MAX_CONNS_PER_HOST`(64);补 `ExpectContinueTimeout: 1s`、`ForceAttemptHTTP2: true`。

### 4.6 其余保留项(同 v1)

- `/metrics` 可选 Bearer 认证:`METRICS_REQUIRE_AUTH`(默认 false)。
- 请求体上限:`MAX_REQUEST_BODY_BYTES`(默认 10 MiB,0 不限),超限 413。
- `GET /admin/breakers`(Bearer 认证):各路由熔断状态,数据源换成 failsafe breaker 的 `State()`。
- `/healthz`、`/readyz` 保持现状;上游健康**不进** readyz(防级联摘除)。

## 5. 观测栈部署(`deploy/kubernetes/observability/`,新增,可选)

- 组件清单:OTel Collector(Deployment,4317/4318 接收,exporter→Tempo)、Tempo、Loki、Grafana(预置 datasource 关联 `trace_id` + 网关看板 JSON)、Prometheus + Alertmanager(若集群已有则跳过,文档给 scrape/告警规则片段)。
- 独立 `make deploy-observability`,不进 `make deploy` 主路径;网关 `OTEL_ENABLED` 默认关,部署栈后在 `gateway.yaml` 打开。
- 资源边界给保守默认值(全部组件单副本 + 小 requests),定位是"任意 K8s 可跑的参考栈",生产扩容留给使用者。
- Agent 侧零改动:`query_prometheus` 继续用 `PROMETHEUS_URL`;后续可给 Agent 增加 Loki/Tempo 查询工具(独立提案,不在本方案)。

## 6. 新增配置一览(`internal/gateway/config.go`)

| 变量 | 默认 | 变量 | 默认 |
| --- | --- | --- | --- |
| `LOG_LEVEL` | `info` | `RETRY_MAX_ATTEMPTS` | `1`(关) |
| `OTEL_ENABLED` | `false` | `RETRY_BODY_BUFFER_BYTES` | `1048576` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://otel-collector:4317` | `BREAKER_ENABLED` | `true` |
| `OTEL_TRACES_SAMPLER_RATIO` | `1.0` | `BREAKER_FAILURE_THRESHOLD` | `5` |
| `METRICS_REQUIRE_AUTH` | `false` | `BREAKER_OPEN_DURATION` | `30s` |
| `UPSTREAM_DIAL_TIMEOUT` | `2s` | `BREAKER_HALF_OPEN_MAX` | `1` |
| `UPSTREAM_TLS_HANDSHAKE_TIMEOUT` | `3s` | `BREAKER_COUNT_5XX` | `true` |
| `UPSTREAM_RESPONSE_HEADER_TIMEOUT` | =`UPSTREAM_TIMEOUT` | `CONCURRENCY_PER_ROUTE` | `256` |
| `UPSTREAM_MAX_CONNS_PER_HOST` | `64` | `MAX_REQUEST_BODY_BYTES` | `10485760` |

## 7. 保留自建的部件及理由

| 部件 | 为什么不用库 |
| --- | --- |
| AccessLog + trafficlog 批量落 PG | 产品差异化能力(Agent 排查的数据源),无现成组件替代 |
| 黑名单/每 IP 限流 | 已存在且简单;failsafe 的 ratelimiter 不提供每 IP 维度 |
| `X-Request-ID` 中间件 | 与 trace_id 互补(对外契约、日志检索键),实现仅 30 行 |
| 超时事件 webhook → Agent | 产品核心契约 |

## 8. 对用户服务的侵入性分析

| 机制 | 上游需要做什么 |
| --- | --- |
| 指标/日志/熔断/重试/并发/超时 | **零改动**,全部在网关进程内 |
| `traceparent` 注入(otelhttp 自动) | 零改动;不识别即忽略;自愿接入 OTel 后可端到端串联 |
| `Retry-After` / 413 / 503 新错误码 | 零改动;标准 HTTP 语义 |
| 观测上游本体的可选项(OBI eBPF / mesh) | 零改动,但属未来选项,不在本方案实施 |

结论不变:**`examples/user-service`、`examples/order-service` 及任何真实上游均无需修改**;网关代码不出现具体上游服务名。

## 9. 测试计划

- `resilience.go`:`httptest` 上游模拟断连/慢响应/5xx,验证重试次数与退避、熔断迁移(open→half-open→closed)、bulkhead 打满立即 429、Body 重放一致性;`-race` 全绿。failsafe 事件回调→指标的断言沿用 `observability_test.go` 的 registry gather 模式。
- OTel:用 SDK 自带的 in-memory exporter(`tracetest`)断言 server/client span 与 `traceparent` 透传;`OTEL_ENABLED=false` 时断言无初始化、无性能路径变化。
- 配置:新环境变量解析、默认值、交叉校验。
- 观测栈清单:`kubectl apply --dry-run=server` 可过;Collector 配置用其内置 config 校验。
- 收尾:`make test` 与 `make lint`(golangci-lint,提交前必跑)全绿。

## 10. 文档同步清单(与代码同提交)

- `docs/modules/gateway.md`:新 env、中间件/策略链、指标清单、熔断/重试语义、新 admin 端点、OTel 开关。
- `docs/deployment.md`:观测栈部署与对接已有 Prometheus 的说明、告警规则样例、Grafana 看板导入。
- `AGENTS.md`「当前已知边界」:熔断/并发为内存态并入单副本条目;新增"观测栈为可选组件"说明。
- `go.mod` 依赖评审说明:`failsafe-go`(v0.x,锁版本)、`go.opentelemetry.io/otel` + contrib(Stable)。

## 11. 分期实施

| 阶段 | 内容 | 风险 | 预估 |
| --- | --- | --- | --- |
| P0 | 配置骨架、4.5 传输层超时、4.3 指标扩展(无库依赖部分)、4.4 日志级别、`/metrics` 可选认证 | 低 | 1–2 天 |
| P1 | 引入 failsafe-go:4.2 重试/熔断/并发上限 + 4.6 请求体上限、`/admin/breakers`、事件→指标 | 中(新依赖、失败路径行为变化) | 2–3 天 |
| P2 | 引入 OTel SDK:4.1 接入 + `trace_id` 日志关联 | 中(新依赖树) | 1–2 天 |
| P3 | `deploy/kubernetes/observability/` 参考栈 + 看板/告警规则样例 | 低(纯部署件) | 1 天 |

每阶段独立 Conventional Commits;P1/P2 的依赖引入各为一个独立 `build:` 提交。

## 12. 风险与权衡

| 风险 | 缓解 |
| --- | --- |
| failsafe-go v0.x API 变动 | 锁版本 + 封装在 `resilience.go` 单文件;升级时只改一处;降级选项 gobreaker |
| OTel 依赖树变大、二进制膨胀 | 仅 P2 引入;`OTEL_ENABLED=false` 时无运行时开销;依赖评审随 PR 进行 |
| Collector 单点/资源占用 | 栈为可选参考件;文档说明生产应 DaemonSet/多副本;网关侧 exporter 有界队列 + 丢弃不阻塞 |
| 重试放大上游压力 | 默认关、仅幂等、硬上限 3、`retries_total` 监控 |
| 熔断误判 | 阈值保守、可禁用、半开自动恢复、`/admin/breakers` 可观测 |
| Loki label 基数 | 文档约定只把 `trace_id`/`route`/`status` 等低基字段做 label,path/IP 仅作日志字段 |
| 内存态弹性策略在多副本下不一致 | 已知边界(单副本),扩副本时统一迁共享存储 |

## 13. 参考依据

- OpenTelemetry Go 信号稳定性(Traces/Metrics Stable,Logs Beta)与 `otelhttp` 现状:OpenTelemetry 官方博客 2025 Go Goals、openobserve.ai 的 OTel Go 指南。
- 弹性库选型:failsafe-go 官方仓库与文档(retrypolicy/circuitbreaker/bulkhead/timeout/failsafehttp)、社区对比(glukhov.org 2026 版 Go CB 对比、failsafe-go.dev/comparisons)。
- LGTM 栈架构:Grafana Labs 生态文档与多篇 2026 年生产实践(Loki 日志 / Tempo 追踪 / Mimir|Prometheus 指标 / Grafana 关联)。
