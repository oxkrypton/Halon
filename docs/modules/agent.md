# 模块:Agent 核心(agent)

代码:`internal/agent/`(核心)+ `cmd/agent/main.go`(服务入口)。
监听:`:8081`(`HTTP_ADDR`)。

## 职责

接收网关超时事件 → 去重 → 排队 → 驱动 LLM 工具调用循环完成故障排查 → 报告落库。是系统的「大脑」服务。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `types.go` | `OperationLevel`(readonly < operator < full)、消息/工具调用类型、`Tool.Permission`、`RunResult`、`ErrMaxSteps` |
| `registry.go` | `Registry`:按角色×权限过滤工具、排序定义、执行时再校验一次权限与 JSON 并把错误回传给模型 |
| `loop.go` | `Loop.Run`:系统提示词 + 事件 JSON → 多轮 `Complete` → 工具执行 → 最终中文报告;工具输出双上限截断(UTF-8 安全):喂模型按 `AGENT_MAX_TOOL_OUTPUT`,落库按更宽的 `AGENT_MAX_STORED_TOOL_OUTPUT` 并置 `output_truncated` 标记 |
| `openai.go` | `OpenAIClient`:OpenAI 兼容 `/chat/completions`;429/5xx 重试最多 3 次,尊重 `Retry-After`;4xx 不重试 |
| `orchestrator.go` | `Orchestrator`:`Submit`(校验→按路由抑制窗口→Claim→聚集延迟/入队)→ worker 池 `investigate`(限时运行 + 落库 + 刷新冷却/退避 TTL)→ `Close` 取消挂起定时器并排空 |
| `suppress.go` | `RouteSuppressor` / `RedisRouteSuppressor`:基于 Redis Lua 的按路由超时风暴抑制(owner 守卫防误改,Redis 不可达时 fail-open) |
| `metrics.go` | Prometheus 指标:`halon_agent_redis_up`、`halon_agent_incident_admissions_total{outcome}` |
| `server.go` | HTTP API:事件接收(仅 INGEST_TOKEN)、报告查询(`incident:read` JWT,兼容期内可用 INGEST_TOKEN)、健康探针、`/metrics` |
| `config.go` | `LoadConfig()`:全部环境变量与启动校验 |

工具实现在独立的 [agent-tools](agent-tools.md) 模块。

## 系统提示词契约(`DefaultSystemPrompt`)

模型被要求:先取证后动手、区分证据与推断、最小可逆变更、变更后验证、最终输出**中文 Markdown 报告**,固定七个小节:事件摘要、排查过程、根因分析、证据、执行操作、修复验证、后续建议;证据不足必须明说「无法确定根因」。修改排查行为时先改这里,并保持与文档一致。

## HTTP API

事件体最大 64 KiB。鉴权分两条:`POST /api/v1/incidents/timeout` 只认 `Authorization: Bearer <INGEST_TOKEN>`(常量时间比较),不进入角色体系;报告查询要求 `incident:read`,即 `AUTH_MODE=hybrid/legacy` 下可用 INGEST_TOKEN 兼容,`jwt` 下必须用 `aud=halon-agent` 的 JWT。详见 [auth.md](auth.md)。

| 接口 | 权限 | 行为 |
| --- | --- | --- |
| `POST /api/v1/incidents/timeout` | INGEST_TOKEN | 校验事件 → `Deduper.Seen`(内存去重)→ `Orchestrator.Submit`(按路由抑制 + 存储幂等)。新事件 `202 {accepted:true, incident_id}`;重复 `200 {duplicate:true}`;同路由窗口内抑制 `200 {suppressed:true, incident_id}`;队列满 `503 QUEUE_FULL` |
| `GET /api/v1/incidents?limit=` | `incident:read` | 报告列表(1..200,默认 50) |
| `GET /api/v1/incidents/:event_id` | `incident:read` | 单条报告;不存在 `404 REPORT_NOT_FOUND` |
| `GET /healthz` / `GET /readyz` | — | 存活 / PostgreSQL 就绪 |
| `GET /metrics` | — | Prometheus 指标(无需鉴权):`halon_agent_redis_up`(Redis 可达性)与 `halon_agent_incident_admissions_total{outcome="accepted|duplicate|suppressed"}` |

去重的正确性由数据库唯一约束保证:`Orchestrator.Submit` → `incident.Store.Claim` 的 `INSERT ... ON CONFLICT DO NOTHING` 是多副本下的表决,同一事件只有一个 `INSERT` 成功;内存 `Deduper` 只是省一次 DB 往返的快路径,不参与正确性。按路由风暴抑制默认开启(`INCIDENT_COALESCE_COOLDOWN=5m`),使用 Redis Lua `SET NX PX` 跨副本裁决(Redis 不可达时 fail-open 放行,不阻断排查)。变更操作冷却同样由 Redis 裁决(见 [agent-tools.md](agent-tools.md) 安全模型)。崩溃副本遗留的 queued/processing 行需租约机制才能在多副本下安全接管回收。

## 编排器行为细节

- 按路由风暴抑制(`INCIDENT_COALESCE_COOLDOWN` 默认 `5m`,设为 `0` 关闭):同路由在窗口内仅受理首个事件并记为 `owner`,后续同路由事件直接返回 `SubmitSuppressed`(不写 PG、不入队);排查成功按 `INCIDENT_COALESCE_COOLDOWN` 静默,排查失败或队列满按 `INCIDENT_RETRY_BACKOFF`(默认 `1m`)短退避;`Claim` 失败或老事件重投立即释放窗口;
- 聚集延迟(`INCIDENT_SETTLE_DELAY` 默认 `15s`,设为 `0` 关闭):新受理的事件延迟入队,等待网关流量日志(`1s`)与 Prometheus(`15s`)落齐后再启动 LLM;等待中的事件计入队列上限(`len(pending)+len(queue) >= QueueSize` 即拒),避免关闭抑制时内存失控;
- 队列满时:`Submit` 先把该事件落库为 `failed`(error = queue full)、把路由窗口缩短为 `INCIDENT_RETRY_BACKOFF`,再返回 `ErrQueueFull`,不留悬挂 queued 行;
- worker 取到任务先 `MarkProcessing` 并刷新路由窗口 TTL,再带 `AGENT_RUN_TIMEOUT` 跑 `Loop.Run`;
- 落库用独立的 10s 超时 context(运行 context 可能已取消);
- `Close()` 取消所有挂起的聚集延迟定时器(将对应报告标记 `failed` 并释放路由窗口),关闭队列并等待排空;入口先停 HTTP 再 `Close`,排空受 `SHUTDOWN_TIMEOUT` 兜底。

## 配置(环境变量)

完整表格见根 `README.md` 的「Agent 配置」。必填:`DATABASE_DSN`、`REDIS_URL`、`INGEST_TOKEN`(≥12 字符)、`OPENAI_API_KEY`、`OPENAI_MODEL`。`AGENT_OPERATION_LEVEL` 默认 `readonly`(最安全),生产开放变更操作需谨慎评估。`AUTH_MODE`/`AUTH_JWT_PUBLIC_KEYS` 等鉴权配置见 [auth.md](auth.md)。

## cmd/agent 装配顺序

`LoadConfig` → `incident.OpenStore` + `Migrate` → Redis 客户端初始化与 Ping 探测 → K8s clientset(优先 in-cluster,回退 `~/.kube/config`)→ `RedisCooldownStore` 与工具注册(Observability + Kubernetes + GatewayAction)→ `Registry`(按级别过滤)→ `OpenAIClient` → `Loop` → `RedisRouteSuppressor` + `Orchestrator` → HTTP server。

## 测试

- `loop_test.go`:`modelFunc` 桩覆盖工具调用、错误回传、最大轮数、截断(模型视图与落库双上限);
- `openai_test.go`:httptest 假服务器覆盖重试与错误;
- `suppress_test.go`:Redis(miniredis)路由窗口生命周期、owner 守卫、40 并发双副本抢窗、Redis 故障 fail-open 降级;
- `orchestrator_test.go`:完成/失败/重复/非法事件/队列满/风暴抑制/成功冷却与失败退避/聚集延迟与 `Close()` 清理/双副本共享抑制;
- `auth_test.go`:三种 `AUTH_MODE` 下的 ingest 与报告读取、JWT 与 INGEST_TOKEN 边界;
- `server_test.go`:gin 测试路由覆盖鉴权、去重、抑制响应、`/metrics` 准入计数、校验、查询、探针。

## 修改本模块时必须同步检查

- 根 `README.md`:架构图、Agent 功能列表、Agent 接口表、Agent 配置表;
- [agent-tools.md](agent-tools.md):若改了 `Tool`/`ToolCall` 结构或级别语义;
- [gateway.md](gateway.md):若改了事件接收协议;
- `deploy/kubernetes/agent.yaml`:若改了端口、环境变量或 RBAC 需求;
- `AGENTS.md`:若改了架构速览或约定;
- [auth.md](auth.md):若改了 API 权限或启动自检规则。
