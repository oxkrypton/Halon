# AGENTS.md — Halon 项目开发指南（Agent 必读）

> **任何 AI Agent 或开发者接手本项目时，先完整阅读本文件，再按索引阅读 `docs/` 下的模块文档。**

## 第一条铁律：改代码必须同步更新文档

**修改任何代码后，必须检查并更新受影响的文档，文档更新与代码变更属于同一个提交（或紧随其后的独立 docs 提交），不允许只改代码不改文档。** 具体规则见 [docs/README.md](docs/README.md) 的「文档维护规则」。每个模块文档末尾列出了「修改本模块时必须同步检查的文档清单」。

## 项目简介

Halon 是一个**开源的云原生网关 Agent 项目**，适用于任意 Kubernetes 集群（Minikube、Kind、ACK、EKS 等）。两个产品组件彼此独立、通过契约通信：

- **网关（gateway）**：通用 HTTP 反向代理，按 `ROUTES` 环境变量把路径前缀转发到任意上游服务；负责黑名单、限流、流量日志落库、六类异常事件识别/阈值归并上报，以及分阶段超时/重试/熔断/并发上限等弹性策略（failsafe-go）与可选 OpenTelemetry 追踪。**它不感知任何具体业务服务。**
- **Agent 服务（agent）**：接收网关异常事件，按事件类型与范围匹配排查画像并驱动 OpenAI 兼容大模型 + 只读/变更两级工具排查故障，产出中文 Markdown 报告并持久化。

`examples/user-service` 与 `examples/order-service` 是演示用后端，**不属于产品本体**，仅用于本地开发和故障演练。

仓库根目录：`/Users/krypton/Documents/Halon`，Go module：`github.com/oxkrypton/Halon`。

## 架构速览

```text
HTTP Client → Gin Gateway (:8080) ──反向代理(ROUTES)──> 任意上游 HTTP 服务
                  │
                  ├─ 异步批量 ─> PostgreSQL (base_logs / error_body / incident_reports)
                  ├─ Redis ────> 黑名单 / 全局限流 / 异常事件归一化与发件冷却
                  ├─ 异常归并 ─webhook─> Agent 服务 (:8081)
                  └─ 管理接口 (:9090) <── 实时状态查询 / 封禁 ── Agent 工具
Agent 服务:事件去重 → Session 编排器 → LLM 排查循环(工具调用) → 报告落库
```

## 模块文档索引

| 模块 | 文档 | 代码位置 |
| --- | --- | --- |
| Gin 网关 | [docs/modules/gateway.md](docs/modules/gateway.md) | `internal/gateway`、`cmd/gateway` |
| Agent 核心(循环/编排/API/配置) | [docs/modules/agent.md](docs/modules/agent.md) | `internal/agent`、`cmd/agent` |
| Agent 工具层 | [docs/modules/agent-tools.md](docs/modules/agent-tools.md) | `internal/agent/tools` |
| RBAC 与 JWT 鉴权 | [docs/modules/auth.md](docs/modules/auth.md) | `internal/authz`、`internal/identity`、`cmd/halonctl` |
| Web 管理控制台 | [docs/modules/web.md](docs/modules/web.md) | `web/`、`deploy/kubernetes/web` |
| 超时事件与故障报告 | [docs/modules/incident.md](docs/modules/incident.md) | `internal/incident` |
| 流量日志存储 | [docs/modules/trafficlog.md](docs/modules/trafficlog.md) | `internal/trafficlog` |
| 演示后端(示例,非产品) | [examples/user-service/README.md](examples/user-service/README.md)、[examples/order-service/README.md](examples/order-service/README.md) | `examples/` |
| 部署与运维 | [docs/deployment.md](docs/deployment.md) | `Dockerfile`、`Makefile`、`deploy/kubernetes` |
| 开发流程与约定 | [docs/development.md](docs/development.md) | 全仓库 |
| 总体设计(历史设计稿) | [docs/system-architecture.txt](docs/system-architecture.txt) | — |

## 常用命令

```bash
make test            # go test -race ./... && go vet ./...(每次提交前必跑)
make lint            # golangci-lint(每次提交前必跑;CI 在 push/PR 上强制同样检查)
make build           # 构建 gateway / agent / halonctl 三个二进制
make build-demo      # 构建演示后端二进制
make run-gateway / run-agent / run-demo / run-web   # 本地运行(环境变量见各模块文档)
make build-web / docker-build-web / deploy-web     # Web 控制台构建与部署
make docker-build    # 构建网关与 Agent 镜像;make docker-build-demo 构建演示镜像
make deploy          # kubectl apply -f deploy/kubernetes/(适用于任意 K8s)
```

## 代码约定(新增代码必须遵守)

1. **配置**:全部走环境变量,仿照 `internal/gateway/config.go` / `internal/agent/config.go` 的 `LoadConfig()` 模式——集中解析、启动时校验、给出默认值。禁止在业务代码里散落 `os.Getenv`。
2. **错误响应**:HTTP 错误统一 `{error: {code, message}}` 结构(网关实现见 `abortWithError`,Agent 见 `abortWithAPIError`)。
3. **数据库**:使用 golang-migrate 嵌入式版本化 SQL 迁移,禁止 GORM AutoMigrate;写操作参数化,禁止字符串拼接 SQL。
4. **异步队列**:一律有界 channel,满了宁可丢弃并打日志,不许阻塞主流程。
5. **测试**:新代码配单测;存储层用 `sqlmock`,LLM 用 `modelFunc` 桩,HTTP 上游用 `httptest`;测试必须能在 `go test -race` 下通过。
6. **提交**:Conventional Commits(`feat:` / `fix:` / `docs:` / `build:` / `test:`),一次提交只做一件事,主题用英文小写。
7. **安全**:Token 比较用 `subtle.ConstantTimeCompare`;K8s RBAC 最小权限;容器非 root、只读根文件系统。
8. **解耦**:产品代码(`cmd/`、`internal/`)禁止 import `examples/` 下的任何包;网关不得出现具体业务服务的名字。
9. **鉴权**:新增管理接口或工具必须复用 `internal/authz` 的 `Permission` 常量并在两端声明权限要求;角色映射表只在代码中维护,禁止散落角色判断。默认离线私钥签发(运维 CLI 为 `cmd/halonctl`),仅开启 `AUTH_LOGIN_ENABLED=true` 时才由网关 Secret 持有签名私钥。

## 当前已知边界(改动前先读)

- 网关是无状态可扩展数据面，独占 Redis 依赖（`REDIS_URL` 为网关必填项）：黑名单（`internal/gateway/rules_redis.go`）、按 IP 限流（`ratelimit_redis.go`）与跨副本按 `(event_type, scope_key)` 异常事件归一化、突增阈值判定及发件冷却（`incident_coalescer.go`）统一由 Redis 承载，网关支持单副本或 `replicas: 2+`。熔断器/并发舱壁是**有意的 per-replica 语义**（保护本副本到上游的连接与请求配额），不外迁。
- Agent 服务定位为单实例控制面（`replicas: 1`、`strategy.type: Recreate`，不连接 Redis）：每次受理事件启动独立排查 Session（`internal/agent/session.go`、`orchestrator.go`），同 `(event_type, scope_key)` 仅保留一个活跃 Session 防重；进程内通过 `MemoryCooldownStore` 与按资源键串行锁 `ResourceLocker`（`internal/agent/tools/cooldown.go`）防止并发 Session 竞态；事件幂等由数据库唯一约束表决，启动时自动回收未完成行（`queued` 重排或过期收尾，`processing` 标记失败避免重复变更）。Agent 通过网关只读管理接口（`/admin/state/*`）经工具按需查阅网关 Redis 与熔断运行态。
- Agent、网关与本地认证共用同一个 PostgreSQL 数据库,但使用**独立的迁移版本表**(流量日志 `schema_migrations`、事件报告 `incident_schema_migrations`、本地认证 `auth_schema_migrations`),不要合并。
- 网关与上游之间、Agent 与网关管理接口之间均为集群内明文 HTTP;跨集群或不可信网络应启用 TLS。
- `OPENAI_API_KEY` 等密钥只走 `halon-secrets`,禁止入库或写入代码。
- 网关弹性策略基于 failsafe-go(v0.x),封装在 `internal/gateway/resilience.go`;OpenTelemetry 追踪默认关闭(`OTEL_ENABLED`)。
- `deploy/kubernetes/observability/` 是可选观测栈参考件,不在 `make deploy` 主路径内,用 `make deploy-observability` 单独部署。
- 鉴权默认 `AUTH_MODE=legacy`、`AUTH_LOGIN_ENABLED=false`(行为与升级前一致);切到 `hybrid`/`jwt` 前两端都必须配 `AUTH_JWT_PUBLIC_KEYS`(网关开启本地登录时可由私钥派生),且必须先把所有调用方换成 Ed25519 JWT。`INGEST_TOKEN` 永远只用于 webhook ingest,不进入角色体系;只读 Agent 若无需调用网关只读状态工具可不配 `GATEWAY_ADMIN_TOKEN`，如需在 `jwt` 模式下查阅网关状态则需配置 `typ=agent, role=readonly` 的 JWT。详见 `docs/modules/auth.md`。
