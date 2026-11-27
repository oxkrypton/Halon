# 模块:Web 管理控制台(web)

代码:`web/`。部署清单:`deploy/kubernetes/web/console.yaml`。

## 定位与技术栈

Google Cloud Console / Cloud Trace 风格的轻量级管理控制台，将 P50/P95/P99 分位数观测、类 Jaeger 链路追踪、慢/错请求日志、Agent AI 故障排查报告与网关管控（熔断、黑名单、限流、本地用户）整合在同一套 RBAC 界面中。

- **前端**:React 19 + TypeScript + Vite 6 + React Router 7 + TanStack Query v5 + Tailwind CSS v4 + Radix UI + Apache ECharts + `react-markdown`。
- **同源反向代理**:
  - 开发态(`make run-web`):Vite `server.proxy` 将 `/api/gateway/*` 转发至本机 `:9090`、`/api/agent/*` 转发至本机 `:8081`。
  - 生产态(`web/Dockerfile` + `web/nginx.conf`):`nginxinc/nginx-unprivileged`（非 root UID 101，只读根文件系统）提供静态资源并反代 `gateway-admin.default.svc.cluster.local:9090` 与 `agent.default.svc.cluster.local:8081`。

## 核心页面

| 页面 | 路径 | 能力与数据源 |
| --- | --- | --- |
| 登录页 | — | 支持账号密码登录（`AUTH_LOGIN_ENABLED=true`，含 `must_change_password` 强制改密流程）与 Bearer Token 登录（`legacy` `ADMIN_TOKEN` 或 `halonctl` 签发的 JWT） |
| 概览看板 | `/` | 总请求/QPS、P50/P95/P99 延迟、5xx 错误率、当前副本熔断状态与运行态保护/队列健康卡片(在途请求、前置拒绝、重试与熔断触发、队列丢弃、Webhook 失败);ECharts 分位数曲线与状态码分布图;Top 慢路由榜与最近 AI 故障报告 |
| 链路追踪 (Traces) | `/traces` | 类 Jaeger 筛选条与耗时散点图，自研 Google Cloud Trace 风格 Span 瀑布流与右侧属性/报文检查器；配置 `TEMPO_URL` 时查询 Tempo 分布式链路，否则自动降级为基于 PostgreSQL `base_logs` 的网关+上游瀑布流 |
| 路由与 P99 分析 | `/routes` | 按路由前缀或精确路径聚合 Avg / P50 / P95 / P99 / Max 与 5xx 率，点击行下钻时序图与样本请求 |
| 请求与错误日志 | `/logs` | 多条件筛选（route/path/request_id/trace_id/client_ip/状态码/最小耗时）与分页，点击行查看美化后的 Request/Response Body 并支持跳转 Trace |
| 容器日志 | `/pod-logs` | 查询 Agent `GET /api/v1/k8s/pods*`，支持命名空间与 Halon 组件筛选、Pod/容器切换、尾部行数与时间窗口、上次崩溃实例 (`previous`) 与关键字高亮过滤 |
| AI 故障报告 | `/incidents` | 查询 Agent `GET /api/v1/incidents*`，渲染中文 Markdown 报告与工具调用轨迹时间线，支持联动关联日志与 Trace |
| 网关管控 | `/gateway` | 路由与每副本熔断器状态、Top IP 榜与 Redis 共享黑名单管理（`operator+`，显示 `<=10s` 传播窗口提示）、Redis Lua 全局限流配置（`full`） |
| 用户与权限 | `/users` | `full` 角色在 `AUTH_LOGIN_ENABLED=true` 下管理本地账号、角色与临时密码重置 |

## 鉴权与安全

- Access Token 仅保存在内存；Refresh Token 仅用于单飞（single-flight）并发去重刷新，避免并发 `401` 触发后端 Refresh Token 重用吊销。
- 界面按 `GET /admin/system` 返回的 `principal.role` 动态控制写操作入口（后端仍强制执行 `authz.Require` 鉴权）。

## 本地开发与构建

```bash
make run-web            # 启动 Vite 开发服务器 (:5173)
make build-web          # TypeScript 类型检查 + 生产构建 (web/dist)
make docker-build-web   # 构建 halon/web:dev 非 root 容器镜像
make deploy-web         # 部署可选控制台清单 deploy/kubernetes/web/
```

## 修改本模块时必须同步检查

- [docs/modules/gateway.md](gateway.md):若新增或修改 `/admin/system`、`/admin/observability/*`、`/admin/traces/*` 响应字段；
- [docs/modules/auth.md](auth.md):若修改登录/刷新/RBAC 权限判定；
- [docs/deployment.md](../deployment.md) 与根 `README.md`:若修改端口、Nginx 反代路径或构建命令。
