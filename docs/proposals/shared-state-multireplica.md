# 共享状态外迁与多副本方案 v1(设计稿)

> 状态:**已实施**(2026-11-17,P0–P4 全部落地;参考 Redis 清单位于 `deploy/kubernetes/redis/` 子目录而非主路径单文件,以满足"不进 `make deploy`"的约束)
> 评审修订(2026-11-17):限流算法定为 Lua 令牌桶、与现有语义逐点对齐(§3.4);Redis 参考清单开启 AOF 并补恢复步骤(§6);补半共享配置约束与多部署隔离(§5)、滚动升级混合模式窗口(§10)。
> 范围:`cmd/gateway`、`internal/gateway`、`internal/agent`、`internal/incident`、部署清单与文档
> 前置阅读:AGENTS.md「当前已知边界」第一条、[gateway-observability-robustness.md](gateway-observability-robustness.md) §12 末行
> 目标:**解除网关与 Agent 的 1 副本限制**,使 `replicas: 2+` 时安全、限流、去重、冷却语义与单副本完全一致。

## 1. 背景与目标

当前所有跨请求状态都在进程内存中,因此网关与 Agent 的 Deployment 均被锁定为 `replicas: 1`。内存态清单(以代码为准):

| 状态 | 代码位置 | 性质 | 多副本故障现象 |
| --- | --- | --- | --- |
| IP 黑名单 | `internal/gateway/rules.go` `IPRules.blocked` | **安全功能** | 封禁只在一个副本生效,攻击流量经另一副本放行 |
| 每 IP 限流器 | `internal/gateway/ratelimit.go` `RateLimiter.clients` | 公平性/防护 | 限额 × 副本数,副本越多限流越松 |
| 熔断器/舱壁 | `internal/gateway/resilience.go` `ResilienceSet` | 自我保护 | 各副本独立探测,语义可接受(见 §3.3) |
| 事件去重窗口 | `internal/incident/dedup.go` `Deduper` | 成本优化 | **已有 DB 表决兜底**(见 §3.2),多副本安全 |
| 操作冷却 | `internal/agent/tools/kubernetes.go` `lastActions` | **安全功能** | N 副本可对同一 Pod 执行 N 次变更,冷却形同虚设 |

### 1.1 一个必须杜绝的具体场景

网关 2 副本,攻击者 `1.2.3.4` 打满上游,Agent 调用管理接口封禁:

```text
PUT /admin/rules/blacklist/1.2.3.4 → Service 选中 gateway-a
gateway-a 内存: blocked = {"1.2.3.4"}   ✅
gateway-b 内存: blocked = {}            ❌

攻击者后续请求: 奇数请求 → gateway-a → 403(拦住)
               偶数请求 → gateway-b → 透传上游(放行)
Agent 报告写"已封禁",上游实际仍承受 50% 攻击流量。
```

这不是能力打折,而是**安全功能给出已生效的假象**。冷却同理:两个 Agent 副本各自认为"冷却期已过",同一 Pod 被连续重启两次。

### 1.2 设计原则

> **共享存储是唯一事实源,内存只能做缓存;并发写入由共享存储的原子操作裁决,输家放弃。**

项目里已有该原则的范例:`incident.Store.Claim` 用 `INSERT ... ON CONFLICT DO NOTHING` 裁决同一事件的归属,内存 `Deduper` 只是省一次 DB 往返的快路径。本方案把这一模式推广到其余状态。

## 2. 选型对比与决策

### 2.1 共享存储:Redis ✅(热路径)+ PostgreSQL ✅(已有,冷路径兜底)

| 候选 | 结论 |
| --- | --- |
| **Redis** | **采用**。单命令原子语义(`SET NX PX`、`SADD`、Lua 脚本)与 TTL 天然契合限流/冷却/黑名单;亚毫秒延迟扛得住每请求热路径。 |
| PostgreSQL | **辅助**。已是既有组件,`ON CONFLICT`、 advisory lock 可做裁决(事件去重已在用);但每请求一次的限流计数走 PG 会给日志库增加不必要的争用,不作为热路径方案。 |
| 仅本地内存 + 广播失效 | 否决:副本间无可靠广播通道(需自建 peer 发现),失效窗口内仍不一致,等于没解决。 |

Redis 定为**可选组件**:`REDIS_URL` 为空时保持当前单副本行为(内存态),不为空时启用共享实现。这与"观测栈可选"的既有决策同构——参考栈清单进 `deploy/kubernetes/`,不进主路径的除外规则见 §6。

### 2.2 Redis 客户端:`github.com/redis/go-redis/v9` ✅

事实标准、稳定、支持 `SET NX PX` 与 Lua(`EvalSha`),与项目依赖管理惯例一致。限流算法用 Lua 实现**令牌桶**(与现有 `x/time/rate` 语义逐点对齐),脚本在 Redis 单线程内原子执行,无需客户端锁。

### 2.3 抽象分层

每项状态定义窄接口,内存实现(现状)与共享实现并存,启动时按 `REDIS_URL` 择优装配:

```go
type BlacklistStore interface { Add/Remove/Contains/List }      // 内存 map | Redis SET + 本地缓存
type RateLimitBackend interface { Allow(ip string) bool }       // 内存令牌桶 | Redis Lua 滑动窗口
type CooldownStore interface { Claim(key, ttl) bool }           // 内存 map | Redis SET NX PX
```

接口归各自模块所有(禁止新建 `internal/common` 之类的杂物包),实现差异对业务代码不可见。

## 3. 分项设计

### 3.1 黑名单 → Redis SET + 进程内缓存(必须共享)

- 事实源:Redis 集合 `halon:blacklist`(`SADD`/`SREM`/`SMEMBERS`),管理接口的写操作直写 Redis。
- 读路径:黑名单检查在每请求热路径上,不能每请求一次 Redis RT——进程内缓存 + **两级失效**:Redis pub/sub 频道 `halon:blacklist:invalidate` 即时通知 + 10s TTL 兜底刷新(pub/sub 消息丢失由 TTL 兜住)。
- 失效窗口语义:新增封禁最坏 10s 内在全部副本生效;解封同理。管理接口响应里注明 `propagation: "eventually, <=10s"`,不承诺实时。
- **降级**:Redis 不可达时,各副本退化为使用最后一份缓存 + 打 `halon_gateway_redis_up 0` 指标告警;封禁写操作直接返回 503(安全功能不允许"看似写成功")。

### 3.2 事件去重 → 维持现状,明确语义(已安全,零改动)

`Claim` 的 `ON CONFLICT DO NOTHING` 已是 DB 表决:多副本下同一事件只有一个 `INSERT` 成功,编排器队列归属自然唯一。本方案只做两件事:在 `docs/modules/incident.md` 写明"去重的正确性由 DB 唯一约束保证,内存 Deduper 仅为优化",并给 `Deduper` 加一条注释指向该结论。**不重写任何代码。**

### 3.3 熔断器/舱壁 → 保留 per-replica(有意为之,文档化)

熔断语义是"保护本副本到上游的连接与请求配额":上游全挂时,2 副本各自探测、各自熔断,代价只是探测流量 ×N,不产生错误放行。舱壁同理(每副本并发上限 = 总额/N,可通过调大 `CONCURRENCY_PER_ROUTE` 补偿)。

- 在 `docs/modules/gateway.md` 与 AGENTS.md 边界条目里写明这是**有意的 per-replica 语义**,不是遗漏。
- 共享失败计数(全集群统一熔断)列为未来选项,不在本方案——它需要热度统计外迁,收益不抵复杂度。

### 3.4 限流 → Redis Lua 滑动窗口(必须共享)

- 每请求执行一次 Lua 脚本实现令牌桶:从 `halon:ratelimit:<ip>` 读出剩余令牌与上次补充时间戳 → 按经过时间 × `RATE_LIMIT_RPS` 补充(封顶 `RATE_LIMIT_BURST`)→ 有令牌扣 1 放行,无则拒绝,单次 RT 原子完成。语义与现有 `x/time/rate` 完全一致:存储外迁不改变限流行为,现有边界测试用例直接平移。key 带 TTL(空闲回收),不泄漏内存。
- **降级**:Redis 不可达时 fail-open 并退化到现有内存令牌桶(防护弱于拒绝服务,但网关不可用更糟),同时打指标与 warn 日志。这与"队列满则丢弃不阻塞主流程"的既有取舍一致。
- 管理接口 `PUT /admin/rules/rate-limit` 的参数更新改为写 Redis `halon:ratelimit:config`,各副本读取时缓存 10s——参数变更不再需要进入每个副本进程。

### 3.5 Agent 操作冷却 → Redis `SET NX PX`(必须共享)

- `KubernetesToolset.allowAction` 的内存 map 替换为:`SET halon:cooldown:<action-key> 1 NX PX <cooldown-ms>`——只有抢到的副本执行变更,未抢到返回现有冷却错误文案(含剩余时间,取自 `PTTL`)。
- 裁决语义与 `incident.Claim` 完全一致:原子命令决定唯一执行者。
- **降级**:Redis 不可达时拒绝变更操作(返回 503,安全功能不允许侥幸放行),只读工具不受影响。

## 4. 目标架构

```text
                 ┌──────────── Gateway ×N ────────────┐
 Client ──HTTP──▶│ 黑名单缓存(10s TTL ⟵ pub/sub 失效) │
                 │ 限流 ──Lua──┐      熔断/舱壁(本地)  │
                 └─────────────│───────────────────────┘
                               ▼
                        Redis(可选,共享事实源)
                        ├─ halon:blacklist        (SET)
                        ├─ halon:blacklist:invalidate (pub/sub)
                        ├─ halon:ratelimit:<ip>   (Lua 滑动窗口)
                        ├─ halon:ratelimit:config
                        └─ halon:cooldown:<key>   (SET NX PX)
                               ▲
                 ┌──────────── Agent ×N ───────────────┐
  Gateway ──webhook──▶│ Claim(DB 表决,已有) → 队列      │
                 │ 变更操作 ──冷却裁决(SET NX PX)       │
                 └─────────────────────────────────────┘
 PostgreSQL:事件去重表决(已有) + 全部持久化,职责不变
```

## 5. 新增配置一览

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `REDIS_URL` | 空(内存模式) | 如 `redis://redis:6379/0`;网关与 Agent 各自解析,语义一致 |
| `BLACKLIST_CACHE_TTL` | `10s` | 黑名单进程内缓存兜底刷新间隔 |
| `REDIS_DIAL_TIMEOUT` / `REDIS_OP_TIMEOUT` | `1s` / `500ms` | 热路径超时上限,超时按 §3 各项降级策略处理 |

装配规则(写入 `LoadConfig` 校验):`REDIS_URL` 非空时校验可解析、启动时 Ping 一次(失败**允许启动**,按降级策略运行并打 warn——与"数据库必就绪"不同,Redis 是可选组件)。

配套约束(写入 `docs/deployment.md`):网关与 Agent 的 `REDIS_URL` 必须同时配置或同时留空——一端共享、一端内存的半共享态是配置错误,`LoadConfig` 无法跨组件校验;两端启动日志各打一行 `shared_state_mode=redis|memory` 供核查。多套部署不得共用同一 Redis logical DB(用 `REDIS_URL` 的 db 编号区分),不引入可配 key 前缀。

## 6. 部署变更

- 新增**可选**参考清单 `deploy/kubernetes/redis.yaml`(单副本 Redis + Service,定位与观测栈一致:任意 K8s 可跑的参考件;生产建议用云厂商托管或 Helm HA 部署,文档给说明)。不进 `make deploy` 主路径,提供 `make deploy-redis`。参考清单开启 `appendonly everysec`:Redis 是黑名单的事实源,重启不得静默清空;AOF 最坏丢失 1s 内的写入,`docs/deployment.md` 给出经管理接口重放的恢复步骤。
- `gateway.yaml` / `agent.yaml` 增加注释掉的 `REDIS_URL` env 示例与 `replicas` 说明:启用共享模式后网关可改 `replicas: 2+`,Agent 在 §3.5 落地后可改 `replicas: 2+`。
- `/readyz` **不**加 Redis 检查:降级策略允许网关在 Redis 不可达时继续服务,摘除 Pod 反而中断全部流量;Redis 健康用 `halon_gateway_redis_up` / `halon_agent_redis_up` gauge 表达,告警规则进观测栈。

## 7. 测试计划

- 单元测试:Redis 实现用 `alicebob/miniredis/v2` 在进程内起仿真 Redis,覆盖黑名单增删/缓存失效、限流令牌桶 refill 与突发边界(沿用现有令牌桶测试用例)、冷却抢占与 TTL 到期;`-race` 全绿。
- 降级路径:miniredis 中途 `Close()` 模拟故障,断言黑名单读退化到缓存、限流 fail-open 到内存桶、冷却拒绝变更、封禁写返回 503。
- 多副本语义测试(关键新增):同一进程内起**两个**网关 handler 实例共享一个 miniredis,模拟 Service 轮询——验证"在 a 实例封禁,b 实例在 TTL 内拒绝该 IP"、"两实例合计限流量等于配置值";Agent 侧两个 `KubernetesToolset` 实例并发 `Claim` 同一冷却 key,断言只有一个成功。
- 集成测试(后续阶段):testcontainers 起真 Redis + PostgreSQL,跑双副本端到端。
- 收尾:`make test` 与 `make lint` 全绿。

## 8. 文档同步清单(与代码同提交)

- `docs/modules/gateway.md`:黑名单/限流的共享模式、缓存失效语义、降级行为、新 env、`redis_up` 指标。
- `docs/modules/agent.md` / `docs/modules/agent-tools.md`:冷却裁决机制与降级行为。
- `docs/modules/incident.md`:补"去重正确性由 DB 唯一约束保证"的明确表述。
- `docs/deployment.md`:Redis 参考清单、托管 Redis 说明、多副本切换步骤。
- AGENTS.md「当前已知边界」第一条改写:区分"共享模式(多副本)"与"内存模式(单副本)",注明熔断/舱壁为有意 per-replica。
- 根 `README.md`:「当前边界」与配置表同步。

## 9. 分期实施

| 阶段 | 内容 | 风险 | 预估 |
| --- | --- | --- | --- |
| P0 | Redis 客户端装配(`REDIS_URL`、降级开关、`redis_up` 指标)+ `deploy/kubernetes/redis.yaml` 参考清单 | 低 | 1 天 |
| P1 | 黑名单外迁(§3.1)+ 多副本语义测试 | 中(安全功能语义变化:生效从实时变为 ≤10s) | 2 天 |
| P2 | 限流外迁(§3.4) | 中(热路径引入 Redis RT,需观测延迟) | 2 天 |
| P3 | Agent 冷却外迁(§3.5)+ Agent 双副本验证 | 中 | 1–2 天 |
| P4 | 文档定稿、网关/Agent 清单 `replicas: 2` 示例、灰度说明 | 低 | 1 天 |

每阶段独立 Conventional Commits;go-redis 引入为独立 `build:` 提交。P1–P3 之间任意时刻都可安全回滚到内存模式(`REDIS_URL` 置空)。

## 10. 风险与权衡

| 风险 | 缓解 |
| --- | --- |
| Redis 单点成为新故障域 | 可选组件 + 分项降级策略(§3);参考清单仅供开发,生产文档指向 HA/托管 |
| 热路径增加 Redis RT(限流每请求一次) | Lua 单 RT 合并操作;`REDIS_OP_TIMEOUT` 500ms 硬上限;`redis_op_duration_seconds` 指标观测 |
| pub/sub 失效消息丢失 | 10s TTL 兜底;管理接口响应注明生效窗口,不承诺实时 |
| 黑名单生效从实时变为最终一致(≤10s) | 接受并在文档显式声明;需要即时生效的场景经 `kubectl port-forward` 逐 Pod 调管理接口(操作手册进 `docs/deployment.md`) |
| 滚动升级期间内存/共享副本并存 | 灰度说明写明:≤10s 收敛承诺仅在全部副本进入共享模式后成立,升级窗口内黑名单仍按单副本语义理解 |
| 新增 go-redis / miniredis 依赖 | 均为成熟稳定项目;锁版本,依赖评审随 PR 进行 |
| 双模式(内存/共享)行为分叉,测试面翻倍 | 窄接口 + 两套实现共用契约测试;CI 两个模式都跑 |

## 11. 明确不做的

- 不引入 Redis Cluster/Sentinel 的自建部署(参考清单只给单副本;HA 交给使用者)。
- 不迁移熔断器/舱壁到共享存储(§3.3)。
- 不改事件去重的任何代码(§3.2,仅文档明确语义)。
- 不引入服务网格或 API 网关替代方案来"顺便"解决状态共享。
- 不在本方案做 HPA 自动扩缩(多副本是前置条件,HPA 是后续独立提案)。
- 不处理 Agent 调查报告的副本地缚(副本调查中宕机,queued/processing 报告卡住):既有问题,单副本同样存在,多副本只是暴露面变大,留作后续提案。
