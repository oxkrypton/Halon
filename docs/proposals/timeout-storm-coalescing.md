# 超时风暴按路由抑制与聚集延迟方案 v2(已实施)

> 状态:**已实施**(2026-11-25)
> v1 → v2 收敛说明:评审后砍掉了 PG 窗口表/报告累加(`occurrences` 写放大)与网关预聚合(网关默认已开熔断器,且 `base_logs` 已完整落库全部 504 供 LLM 工具查询)。最终收敛为 **Agent 侧纯抑制(复用内存/Redis 双模式)+ 15s 聚集延迟**,零数据库表结构改动、零网关与契约改动。
> 范围:`internal/agent`、`cmd/agent`、文档
> 目标:**同一路由在一个冷却窗口内只触发一次 LLM 排查**,多副本下语义一致,并在启动排查前等待日志与 Prometheus 指标就绪。

## 1. 背景与问题

网关为每个 504 生成独立 `EventID`(`internal/gateway/accesslog.go`),而 `Deduper` 与 `incident.Store.Claim` 只按 `event_id` 去重(仅挡 webhook 重投)。上游卡死时,网关熔断跳开前的在途请求会同时超时,在几秒内产生数十条不同 `event_id` 的 504,进而触发多次重复的 LLM 排查并挤占编排器有界队列(默认 64)。

## 2. 选型收敛

- **为什么只做“按路由抑制”,不做“向 `incident_reports` 累加计数”?**
  网关本来就已把全部 504 异步写入 `base_logs` / `error_body`(1s 刷盘),且 Agent 已有 `query_traffic_stats`、`query_error_logs`、`query_prometheus`。等待聚集延迟后,LLM 自己查库即可拿到精确次数、P95、来源 IP 与报错体,无需再为后续 504 写 PG。
- **为什么砍掉网关预聚合?**
  网关默认开启熔断(`BREAKER_ENABLED=true`,5 次失败跳开 30s),跳开后请求快速返回 503 而非 504,不会持续轰炸 webhook;把策略全部收口在 Agent 侧可保持网关极简与 `TimeoutEvent` 契约零改动。
- **为什么窗口存储沿用“内存 / Redis 双模式”而非新建 PG 表?**
  既然不需要把“抢窗”和“累加报告”放进同一个 PG 事务,窗口锁就和黑名单/限流/操作冷却完全同构:单副本内存模式用进程内 map,多副本共享模式用 `cmd/agent` 已有 `redisClient` 的原子 Lua 脚本(`SET halon:incident:route:<route> <event_id> NX PX <ttl>`)。

## 3. 设计细节

### 3.1 路由抑制存储(`internal/agent/suppress.go`)

- 接口 `RouteSuppressor`:`TryAcquire(ctx, route, owner, ttl)`、`SetTTL(ctx, route, owner, ttl)`、`Release(ctx, route, owner)`。
- **owner 守卫**:窗口值为开窗事件的 `event_id`,Redis 的 `SetTTL` 与 `Release` 通过 Lua 脚本原子校验 `GET == owner` 后才执行 `PEXPIRE`/`DEL`,防止过期任务误改新排查的窗口。
- **降级策略**:与 K8s 操作冷却(安全功能,fail-closed)不同,风暴抑制是降噪/成本优化。Redis 不可达时 `RedisRouteSuppressor` 自动 **fail-open 降级到内置 `MemoryRouteSuppressor`**(最坏退化为每副本查 1 次,不阻断故障排查),并经 `onHealth` 刷新 `halon_agent_redis_up`。

### 3.2 时间方案(对齐 Halon 现有时钟)

| 参数 | 默认值 | 对齐依据 |
| --- | --- | --- |
| `INCIDENT_SETTLE_DELAY` | `15s` | 覆盖首波 504 爆发(`UPSTREAM_TIMEOUT=3s`)、日志刷盘(`LOG_FLUSH_INTERVAL=1s`)并**对齐 Prometheus `scrape_interval=15s`**,且小于熔断 `30s`;`0` 表示立即入队 |
| `INCIDENT_COALESCE_COOLDOWN` | `5m` | 排查成功后的同路由静默期(覆盖 `K8S_ACTION_COOLDOWN=2m` 与 Pod 重建恢复期,未恢复则 5m 后复查);**`0` 即关闭抑制** |
| `INCIDENT_RETRY_BACKOFF` | `1m` | 排查失败或队列满后的短退避(刚好跨过 2 个 30s 熔断探测周期),不得大于 cooldown |

窗口生命周期:
1. **开窗**:`TryAcquire` 设初始 TTL = `SETTLE_DELAY + RunTimeout + COOLDOWN`(防止崩溃后永久压制);若已被其他 `event_id` 持有,直接返回 `SubmitSuppressed`(不写 PG、不入队);若 `owner == event.EventID` 则判为 `SubmitDuplicate`。
2. **`Claim` 失败或老事件重投(`!claimed`)**:立即 `Release` 窗口。
3. **聚集等待**:`pending` 表计入队列容量上限(`len(pending)+len(queue) >= cap(queue)` 返回 `ErrQueueFull` 并缩短窗口为 `RETRY_BACKOFF`),保证即使关闭抑制内存仍有界;`Close()` 停止所有挂起定时器、标记报告 failed 并 `Release` 窗口。
4. **运行与结束**:`MarkProcessing` 时刷新 TTL 为 `RunTimeout + COOLDOWN`;`SaveResult` 后成功改写为 `COOLDOWN`(5m),失败改写为 `RETRY_BACKOFF`(1m)。

### 3.3 HTTP 响应与指标

- `POST /api/v1/incidents/timeout`:
  - 新受理:`202 {event_id, incident_id, accepted:true, duplicate:false, suppressed:false}`
  - 重复事件:`200 {event_id, accepted:false, duplicate:true, suppressed:false}`
  - 窗口抑制:`200 {event_id, incident_id, accepted:false, duplicate:false, suppressed:true}`(网关视所有 2xx 为投递成功,不重试)
- Prometheus 指标:`halon_agent_incident_admissions_total{outcome="accepted|duplicate|suppressed"}`。

## 4. 测试与验证

- `internal/agent/suppress_test.go`:内存与 Redis(miniredis)窗口生命周期、owner 守卫、40 并发双副本竞态抢窗(恰 1 个成功)、Redis 宕机 fail-open 降级。
- `internal/agent/orchestrator_test.go`:50 事件风暴只落库/排查 1 次、失败 1m 退避 vs 成功 5m 冷却、Claim 失败释放窗口、聚集延迟与 `Close()` 快速取消不留悬挂行、`pending` 受队列上限约束、双副本共享抑制。
- `internal/agent/server_test.go` / `config_test.go`:API 响应字段、`/metrics` 计数与配置校验。
