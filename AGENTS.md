# AGENTS.md — Halon 项目开发指南（Agent 必读）

> **任何 AI Agent 或开发者接手本项目时，先完整阅读本文件，再按索引阅读 `docs/` 下的模块文档。**

## 第一条铁律：改代码必须同步更新文档

**修改任何代码后，必须检查并更新受影响的文档，文档更新与代码变更属于同一个提交（或紧随其后的独立 docs 提交），不允许只改代码不改文档。** 具体规则见 [docs/README.md](docs/README.md) 的「文档维护规则」。每个模块文档末尾列出了「修改本模块时必须同步检查的文档清单」。

## 项目简介

Halon 是一个云原生 Go 微服务项目：Gin 网关转发请求并异步落流量日志到 PostgreSQL；网关在上游 gRPC 超时时通过 webhook 推送事件给独立的 **Agent 故障排查服务**；Agent 服务调用 OpenAI 兼容大模型，借助只读/操作两级工具排查 Kubernetes 集群内的故障，并把中文 Markdown 排查报告持久化到数据库。

仓库根目录：`/Users/krypton/Documents/Halon`，Go module：`github.com/oxkrypton/Halon`。

## 架构速览

```text
HTTP Client → Gin Gateway (:8080) ──gRPC──> user-service (:50051)
                  │                               
                  ├─ 异步批量 ─> PostgreSQL (base_logs / error_body / incident_reports)
                  ├─ 504 超时 ──webhook──> Agent 服务 (:8081)
                  └─ 管理接口 (:9090) <── 封禁/限流 ── Agent 操作工具
Agent 服务:事件去重 → 编排器队列 → LLM 排查循环(工具调用) → 报告落库
```

## 模块文档索引

| 模块 | 文档 | 代码位置 |
| --- | --- | --- |
| Gin 网关 | [docs/modules/gateway.md](docs/modules/gateway.md) | `internal/gateway`、`cmd/gateway` |
| 用户服务 | [docs/modules/user-service.md](docs/modules/user-service.md) | `internal/user`、`cmd/user-service`、`api/proto` |
| 流量日志存储 | [docs/modules/trafficlog.md](docs/modules/trafficlog.md) | `internal/trafficlog` |
| 超时事件与故障报告 | [docs/modules/incident.md](docs/modules/incident.md) | `internal/incident` |
| Agent 核心(循环/编排/API/配置) | [docs/modules/agent.md](docs/modules/agent.md) | `internal/agent`、`cmd/agent` |
| Agent 工具层 | [docs/modules/agent-tools.md](docs/modules/agent-tools.md) | `internal/agent/tools` |
| 部署与运维 | [docs/deployment.md](docs/deployment.md) | `Dockerfile`、`Makefile`、`deploy/kubernetes` |
| 开发流程与约定 | [docs/development.md](docs/development.md) | 全仓库 |
| 总体设计(历史设计稿) | [docs/system-architecture.txt](docs/system-architecture.txt) | — |

## 常用命令

```bash
make test            # go test -race ./... && go vet ./...(每次提交前必跑)
make build           # 构建 user-service / gateway / agent 三个二进制
make run / run-gateway / run-agent   # 本地运行(环境变量见各模块文档)
make docker-build    # 构建三个镜像
make minikube-image  # 构建并载入 Minikube
make deploy          # kubectl apply -f deploy/kubernetes/
```

## 代码约定(新增代码必须遵守)

1. **配置**:全部走环境变量,仿照 `internal/gateway/config.go` / `internal/agent/config.go` 的 `LoadConfig()` 模式——集中解析、启动时校验、给出默认值。禁止在业务代码里散落 `os.Getenv`。
2. **错误响应**:HTTP 错误统一 `{error: {code, message}}` 结构(网关实现见 `abortWithError`,Agent 见 `abortWithAPIError`)。
3. **数据库**:使用 golang-migrate 嵌入式版本化 SQL 迁移,禁止 GORM AutoMigrate;写操作参数化,禁止字符串拼接 SQL。
4. **异步队列**:一律有界 channel,满了宁可丢弃并打日志,不许阻塞主流程。
5. **测试**:新代码配单测;存储层用 `sqlmock`,LLM 用 `modelFunc` 桩;测试必须能在 `go test -race` 下通过。
6. **提交**:Conventional Commits(`feat:` / `fix:` / `docs:` / `build:` / `test:`),一次提交只做一件事,主题用英文小写。
7. **安全**:Token 比较用 `subtle.ConstantTimeCompare`;K8s RBAC 最小权限;容器非 root、只读根文件系统。

## 当前已知边界(改动前先读)

- 网关黑名单/限流状态在内存中,网关固定 1 副本;Agent 去重窗口与操作冷却也在内存中,Agent 固定 1 副本。要扩副本需先迁移到共享存储。
- Agent 与网关共用同一个 PostgreSQL 数据库,但使用**独立的迁移版本表**(`incident_schema_migrations`),不要合并。
- gRPC 集群内明文传输;`OPENAI_API_KEY` 等密钥只走 `halon-secrets`,禁止入库或写入代码。
