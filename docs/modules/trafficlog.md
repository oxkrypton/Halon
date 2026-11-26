# 模块:流量日志存储(trafficlog)

代码:`internal/trafficlog/`。使用方:网关(写入)、Agent 工具层(读取,经 `*sql.DB` 只读查询)。

## 职责

把网关全量 HTTP 请求日志异步、批量、事务化地写入 PostgreSQL,并为错误/慢请求保存完整报文。

## 表结构(迁移 `migrations/000001_create_traffic_logs.sql`、`000002_add_trace_and_route.sql`,版本表 `schema_migrations`)

| 表 | 内容 | 写入条件 |
| --- | --- | --- |
| `base_logs` | request_id、trace_id、client_ip、method、path、route、status_code、latency_ms、upstream_latency_ms、upstream_result、retries、created_at | 每个请求一行 |
| `error_body` | base_log_id(外键,级联删除)、request_body、response_body | 状态码 ≥400 或耗时 ≥ 慢请求阈值 |

索引:`created_at`、`(client_ip, created_at)`、`(path, status_code, created_at)`、`(trace_id) WHERE trace_id <> ''`、`(route, created_at DESC)`,均按 Agent 排查与控制台观测的查询模式设计。

## 组件

- `Repository`(`postgres.go`):GORM 连接池(20 开 / 5 闲)、`Migrate()`(iofs 嵌入式迁移)、`CreateBatch()`(**同一事务**写 base 行 + detail 行)、`Ping`、`SQLDB`。
- `query.go`:面向管理控制台的只读聚合与分页查询(`OverviewStats`、`TimeSeries`、`RouteStats`、`ListLogs`、`GetLogByID`、`GetLogByRequestID`、`GetLogsByTraceID`、`TopClients`),全部参数化 SQL + 10s 查询超时 + 7d 窗口上限。
- `BatchWriter`(`writer.go`):从有界 channel 读日志,攒够 `LOG_BATCH_SIZE`(默认 500)或每 `LOG_FLUSH_INTERVAL`(默认 1s)批量提交;失败批次指数退避最多重试 3 次后丢弃并打日志;channel 关闭后排空退出。
- 模型(`model.go`):`Entry{Base, Detail *ErrorBody}` 是队列与批写器的传输单元。

## 与 incident 模块的共存约定

Agent 服务的 `incident_reports` 表与 `base_logs` 同库,但迁移走独立的版本表 `incident_schema_migrations`(见 [incident.md](incident.md))。**不要把两个模块的迁移合并进同一个版本表。**

## 测试

`writer_test.go`(攒批/定时/重试三个场景,repository 用桩)、`query_test.go`(使用 `sqlmock` 覆盖总览分位数、`date_bin` 时序、路由排行、多维日志筛选、详情与 Top IP)。

## 修改本模块时必须同步检查

- 根 `README.md`:「PostgreSQL 日志存储」功能列表、网关配置表(队列/批量参数);
- [agent-tools.md](agent-tools.md):`query_error_logs` / `query_traffic_stats` / `query_top_ips` 直接依赖这里的表结构——**改表结构必须同步改工具 SQL**;
- [gateway.md](gateway.md):若改了 `Entry` 结构或写入条件。
