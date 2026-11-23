# Halon 文档中心

本目录是 Halon 项目的权威文档。接手开发前请先读根目录 [AGENTS.md](../AGENTS.md)。

## ⚠️ 文档维护规则(强制执行)

1. **代码变更与文档更新必须同步**:任何修改了模块行为、接口、配置、数据结构、部署清单或开发流程的提交,必须在同一提交(或紧随的独立 `docs:` 提交)中更新对应模块文档。
2. **新增模块** = 新增 `docs/modules/<module>.md` + 更新本索引 + 更新 `AGENTS.md` 索引 + 更新根 `README.md` 项目结构。
3. **修改接口/配置/表结构**时,除了模块文档,还要同步检查:根 `README.md`(接口表、配置表)、`docs/deployment.md`(清单、Secret)、`AGENTS.md`(约定与边界)。
4. 文档中禁止出现与代码不一致的描述——评审时发现文档与实现不符,以实现为准修正文档,并追溯补上遗漏的更新。
5. 每个模块文档末尾的「修改本模块时必须同步检查」清单是强制检查项。

## 索引

### 开发指南

- [项目阅读指南](reading-guide.md) — 接手本项目的高效阅读方针:先文档后实验、带问题追代码、修改前检查项
- [开发流程与约定](development.md) — 环境要求、命令、代码约定、测试与提交规范
- [部署与运维](deployment.md) — Dockerfile、Makefile、K8s 清单、Secret、本地与 Minikube 运行

### 模块文档

| 模块 | 文档 | 一句话职责 |
| --- | --- | --- |
| Gin 网关 | [modules/gateway.md](modules/gateway.md) | 通用反向代理、黑名单限流、流量日志与超时事件上报 |
| 流量日志存储 | [modules/trafficlog.md](modules/trafficlog.md) | 请求日志的批量异步写入与 PostgreSQL 表结构 |
| 超时事件与故障报告 | [modules/incident.md](modules/incident.md) | 事件模型、去重、报告表与存储层 |
| Agent 核心 | [modules/agent.md](modules/agent.md) | LLM 循环、工具注册、编排器、事件 API、服务配置 |
| Agent 工具层 | [modules/agent-tools.md](modules/agent-tools.md) | 日志/PromQL/K8s/封禁工具的清单、权限与安全约束 |
| RBAC 与 JWT 鉴权 | [modules/auth.md](modules/auth.md) | 角色×权限矩阵、Ed25519 JWT、AUTH_MODE、halonctl 与升级路径 |
| 演示后端(非产品) | [../examples/user-service/README.md](../examples/user-service/README.md)、[../examples/order-service/README.md](../examples/order-service/README.md) | 演练用 HTTP 服务与故障注入 |

### 历史设计稿

- [system-architecture.txt](system-architecture.txt) — 立项时的总体设计,部分设想(如 ReverseProxy 转发、K8s Watcher 自动发现)尚未实现,以模块文档描述的当前实现为准。
