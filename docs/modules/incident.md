# 模块:超时事件与故障报告(incident)

代码:`internal/incident/`。使用方:网关(产生 `TimeoutEvent`)、Agent 服务(去重、报告持久化)。

## 职责

1. 定义网关与 Agent 服务之间的事件协议(`TimeoutEvent`);
2. 提供内存 TTL 事件去重器(`Deduper`);
3. 定义并持久化故障排查报告(`Report` / `Store`)。

## 组件

### TimeoutEvent(`timeout.go`)

网关 504 超时事件的载荷,字段:EventID、RequestID、OccurredAt、Method、Path、Route、ClientIP、StatusCode、LatencyMS。`Validate()` 聚合所有校验错误一次返回。**网关(发送方)与 Agent 服务(接收方)共用这一个结构体,改字段两边同时改。**

### Deduper(`dedup.go`)

- `Seen(eventID)`:TTL 窗口内重复返回 true;新 ID 记录并惰性驱逐过期项;空 ID 永远不算重复也不记录;
- TTL 默认 10 分钟(由 Agent 配置 `AGENT_DEDUP_TTL` 决定);
- 并发安全;驱逐在 size 达到 2 的幂时摊还触发,插入均摊 O(1);
- **内存态,进程重启即失效**——去重的正确性不依赖它:由数据库唯一约束保证(见下),`Deduper` 只是省一次 DB 往返的快路径,多副本下同样成立。

### Report 与状态机(`report.go`)

状态:`queued → processing → completed / failed`。

`Report` 完整记录一次排查:事件字段 + `Status` + `FinalMessage`(中文 Markdown 报告)+ `Steps` + `Executions`(JSONB,工具调用轨迹)+ Token 用量 + `Error` + 起止时间。`JSONB` 类型入库前校验 JSON 合法性。

### Store(`store.go`)

| 方法 | 语义 |
| --- | --- |
| `OpenStore(ctx, dsn)` | 独立连接池(10 开 / 3 闲),启动时 Ping |
| `Migrate()` | iofs 嵌入式迁移,**版本表 `incident_schema_migrations`**(与 trafficlog 隔离,允许同库共存) |
| `Claim(ctx, event)` | 插入 queued 行,`ON CONFLICT DO NOTHING`;返回 false 表示该事件已被认领——这是幂等投递的表决层:多副本下同一事件只有一个 `INSERT` 成功,编排器队列归属由此唯一确定 |
| `MarkProcessing(ctx, eventID, at)` | 仅从 queued 转为 processing(条件更新,不覆盖其他字段) |
| `SaveResult(ctx, report)` | 覆盖写入排查结果;事件不存在返回 `ErrNotFound` |
| `Get` / `List` | 单查 / 按发生时间倒序列表(limit 钳制到 1..200,默认 50) |

### 表结构(迁移 `migrations/000001_create_incident_reports.sql`)

`incident_reports`:主键 `event_id`;`status` 有 CHECK 约束(四种状态);Token 与 steps 非负;`executions JSONB` 可空;`started_at`/`finished_at` 可空;索引 `(occurred_at DESC)`、`(status, occurred_at DESC)`。

## 测试

`dedup_test.go`(注入时钟,确定性)、`store_test.go`(sqlmock;注意 GORM `Take` 自动追加 `LIMIT $n` 参数)。

## 修改本模块时必须同步检查

- [gateway.md](gateway.md) 与 [agent.md](agent.md):`TimeoutEvent` 是两者的协议契约;
- [agent-tools.md](agent-tools.md):无直接依赖,但若改了表名需全局排查;
- 根 `README.md`:架构图(表清单)与 Agent 功能列表;
- `docs/deployment.md`:若改了迁移策略。
