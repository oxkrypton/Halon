# 模块:Agent 核心(agent)

代码:`internal/agent/`(核心)+ `cmd/agent/main.go`(服务入口)。
监听:`:8081`(`HTTP_ADDR`)。

## 职责

作为单实例控制面服务，接收网关归一化后的各类异常事件 → 去重 → 创建独立排查 Session → 按事件类型匹配排查画像并驱动 LLM 工具调用循环完成故障排查 → 报告落库。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `types.go` | `OperationLevel`(readonly < operator < full)、消息/工具调用类型、`Tool.Permission`、`RunResult`、`ErrMaxSteps` |
| `profiles.go` | `InvestigationProfile` / `ProfileFor`:按 `incident.EventType` 注册排查画像(标题与针对性取证引导)并提供未知类型的通用兜底画像 |
| `registry.go` | `Registry`:按角色×权限过滤工具、排序定义、执行时再校验一次权限与 JSON 并把错误回传给模型 |
| `loop.go` | `Loop.Run` / `RunWithProgress`:系统提示词 + 画像引导 + 事件 JSON → 多轮 `Complete` → 工具执行 → 最终中文报告;支持步进回调与工具输出双上限截断(UTF-8 安全):喂模型按 `AGENT_MAX_TOOL_OUTPUT`,落库按更宽的 `AGENT_MAX_STORED_TOOL_OUTPUT` 并置 `output_truncated` 标记 |
| `openai.go` | `OpenAIClient`:OpenAI 兼容 `/chat/completions`;429/5xx 重试最多 3 次,尊重 `Retry-After`;4xx 不重试 |
| `session.go` | `SessionState`(`queued`/`running`/`cancelling`)、`SessionSnapshot`(含 `event_type` / `scope_key` / `occurrences` / `current_step`)、`ErrSessionNotFound` |
| `orchestrator.go` | `Orchestrator`:`Submit`(校验→同 `(event_type, scope_key)` 活跃 Session 防重→`Claim`→入队创建 Session)→ worker `investigate`(可取消上下文 + 步进刷新 + 落库)→ `ListSessions` / `CancelSession` / `CancelActive` / `RecoverUnfinished` → `Close` |
| `metrics.go` | Prometheus 指标:`halon_agent_active_sessions`、`halon_agent_incident_admissions_total{type,outcome}` |
| `pods.go` | `PodLogReader` 契约与 K8s 只读代理处理器(`/api/v1/k8s/pods`、`/api/v1/k8s/pods/:pod/logs`,10s 超时与错误映射) |
| `server.go` | HTTP API:通用事件接收与兼容路由(仅 INGEST_TOKEN)、报告查询(`incident:read`,支持 `?type=`)、活跃 Session 查询(`incident:read`)与取消(`incident:manage`)、Pod/日志只读查询(`k8s:pod_logs:read`)、健康探针、`/metrics` |
| `config.go` | `LoadConfig()`:全部环境变量与启动校验 |

工具实现在独立的 [agent-tools](agent-tools.md) 模块。

## 系统提示词与事件画像契约(`DefaultSystemPrompt` / `profiles.go`)

模型被要求:先取证后动手(通过 `read_observability` 查流量日志、错误报文、Top IP 与可用时的 Prometheus 指标,通过可用时的 `read_gateway` 查网关黑名单/限流/熔断/活跃事件归一化窗口状态,并通过 `read_k8s` 查 Pod/事件/Deployment/日志)、区分证据与推断、最小可逆变更、变更后验证、最终输出**中文 Markdown 报告**,固定七个小节:事件摘要、排查过程、根因分析、证据、执行操作、修复验证、后续建议;证据不足必须明说「无法确定根因」。修改排查行为或新增触发类型排查指引时改这里与 `profiles.go`,并保持与文档一致。

## HTTP API

事件体最大 64 KiB。鉴权分两条:`POST /api/v1/incidents` 与 `POST /api/v1/incidents/timeout` 只认 `Authorization: Bearer <INGEST_TOKEN>`(常量时间比较),不进入角色体系;报告与活跃 Session 查询(`incident:read`)、Session 取消(`incident:manage`)与 Pod/容器日志查询(`k8s:pod_logs:read`)在 `AUTH_MODE=hybrid/legacy` 下可用 INGEST_TOKEN 兼容,`jwt` 下必须用 `aud=halon-agent` 的 JWT。详见 [auth.md](auth.md)。

| 接口 | 权限 | 行为 |
| --- | --- | --- |
| `POST /api/v1/incidents` | INGEST_TOKEN | 接收通用 `incident.Event`(校验 `type`/`scope_key` 等)→ `Deduper.Seen` → `Orchestrator.Submit`。新事件 `202 {accepted:true, event_type, incident_id}`;重复 `200 {duplicate:true}`;同 `(event_type, scope_key)` 已有活跃 Session `200 {suppressed:true, incident_id}`;队列满 `503 QUEUE_FULL` |
| `POST /api/v1/incidents/timeout` | INGEST_TOKEN | 兼容入口,缺省 `type` 时自动补为 `upstream_timeout` 后走同一受理链路 |
| `GET /api/v1/incidents?type=&limit=` | `incident:read` | 报告列表(支持 `?type=` 按事件类型过滤;`limit` 1..200,默认 50;非法 `type` 返回 `400 INVALID_EVENT_TYPE`) |
| `GET /api/v1/incidents/:event_id` | `incident:read` | 单条报告;不存在 `404 REPORT_NOT_FOUND` |
| `GET /api/v1/sessions` | `incident:read` | 列出当前排队、运行或取消中的活跃排查 Session 快照(含 `event_type`、`scope_key`、归并次数、当前步数与起止时间) |
| `POST /api/v1/sessions/:event_id/cancel` | `incident:manage` | 手动取消排队或运行中的排查 Session;不存在返回 `404 SESSION_NOT_FOUND` |
| `GET /api/v1/k8s/pods` | `k8s:pod_logs:read` | 列出白名单命名空间内 Pod 状态、容器状态与重启次数(`limit` 1..100,默认 100);越权命名空间 `403 NAMESPACE_FORBIDDEN`,未注入 K8s 客户端 `503 K8S_UNAVAILABLE` |
| `GET /api/v1/k8s/pods/:pod/logs` | `k8s:pod_logs:read` | 读取 Pod 容器有界日志(`tail_lines` 1..1000 默认 200、`since_seconds` 1..86400 默认 900、单次上限 1 MiB、支持 `previous=true`);Pod 不存在 `404 POD_NOT_FOUND`,非法参数或无前序崩溃实例 `400 INVALID_REQUEST` |
| `GET /healthz` / `GET /readyz` | — | 存活 / PostgreSQL 就绪 |
| `GET /metrics` | — | Prometheus 指标(无需鉴权):`halon_agent_active_sessions` 与 `halon_agent_incident_admissions_total{type,outcome="accepted|duplicate|suppressed"}` |

事件去重的正确性由数据库唯一约束保证:`Orchestrator.Submit` → `incident.Store.Claim` 的 `INSERT ... ON CONFLICT DO NOTHING` 保证同一事件只受理一次;内存 `Deduper` 只是省一次 DB 往返的快路径。全集群级异常归一化、突增阈值过滤与冷却由网关侧 Redis 承担,Agent 进程内保留「同 `(event_type, scope_key)` 同一时刻最多一个活跃 Session」的轻量防重。

## Session 编排与启动恢复

- **Session 生命周期**:每次受理事件生成一个以 `event_id` 为标识的 `Session`(`queued → running → completed | failed | cancelled`),绑定独立可取消 context 与步进状态回调;
- **同域防重**:`Submit` 在 `Claim` 前按 `(event_type, scope_key)` 预占活跃槽位(出错或 `!claimed` 时释放),同一路由上的不同故障类型互不阻塞,同类同范围则避免并发重复排查;
- **启动期孤儿回收(`RecoverUnfinished`)**:单实例启动时扫描 `queued` / `processing` 行:
  - `processing` 行(上次崩溃时可能已执行部分变更)直接 `MarkFailed("agent interrupted during investigation")`,避免重复施加重试/扩缩容等副作用;
  - `queued` 行在 `AGENT_RECOVERY_MAX_AGE`(默认 `30m`)内且无同 `(event_type, scope_key)` 已恢复任务时重新创建 Session 入队;超龄或冲突直接 `MarkFailed` 收尾;
- **优雅关闭**:`Close()` 停止接收并等待排空,超过 `SHUTDOWN_TIMEOUT` 后调用 `CancelActive()` 主动中止在途 Session,持久化用独立 10s context 收尾。

## 配置(环境变量)

完整表格见根 `README.md` 的「Agent 配置」。必填:`DATABASE_DSN`、`INGEST_TOKEN`(≥12 字符)、`OPENAI_API_KEY`、`OPENAI_MODEL`。`AGENT_OPERATION_LEVEL` 默认 `readonly`(最安全),生产开放变更操作需谨慎评估。`AUTH_MODE`/`AUTH_JWT_PUBLIC_KEYS` 等鉴权配置见 [auth.md](auth.md)。

## cmd/agent 装配顺序

`LoadConfig` → `incident.OpenStore` + `Migrate` → K8s clientset(优先 in-cluster,回退 `~/.kube/config`)→ `MemoryCooldownStore` 与工具注册(Observability + Kubernetes + Gateway 只读/写工具)→ `Registry`(按级别过滤)→ `OpenAIClient` → `Loop` → `Orchestrator` 启动 + `RecoverUnfinished` → HTTP server。

## 测试

- `loop_test.go`:`modelFunc` 桩覆盖工具调用、错误回传、最大轮数、截断(模型视图与落库双上限)及内置/自定义事件画像引导;
- `openai_test.go`:httptest 假服务器覆盖重试与错误;
- `orchestrator_test.go`:完成/失败/重复/非法事件/队列满/按 `(event_type, scope_key)` 活跃防重/Session 查询与取消/`CancelActive` 优雅中断/启动恢复区分 `queued` 与 `processing`;
- `auth_test.go`:三种 `AUTH_MODE` 下的 ingest、报告/Session 读写与取消、Pod/容器日志接口权限、JWT 与 INGEST_TOKEN 边界;
- `server_test.go`:gin 测试路由覆盖多类型 ingest(含非 HTTP `breaker_open` 与 `/api/v1/incidents/timeout` 兼容)、`?type=` 过滤、去重、抑制响应、`/api/v1/sessions`、`/metrics` 指标、校验、查询、Pod/日志代理接口及探针。

## 修改本模块时必须同步检查

- 根 `README.md`:架构图、Agent 功能列表、Agent 接口表、Agent 配置表;
- [agent-tools.md](agent-tools.md):若改了 `Tool`/`ToolCall` 结构或级别语义;
- [gateway.md](gateway.md):若改了事件接收协议或网关状态查询协议;
- `deploy/kubernetes/agent.yaml`:若改了端口、环境变量、更新策略或 RBAC 需求;
- `AGENTS.md`:若改了架构速览或约定;
- [auth.md](auth.md):若改了 API 权限或启动自检规则。
