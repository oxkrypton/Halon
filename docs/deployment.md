# 部署与运维

> 修改 Dockerfile、Makefile、deploy/kubernetes 下的任何清单或 Secret 键后,必须同步更新本文档、根 `README.md` 的部署章节与对应模块文档。

## 构件

| 服务 | 镜像 | 二进制 | 端口 | 角色 |
| --- | --- | --- | --- | --- |
| gateway | `halon/gateway:dev` | `bin/gateway` | 8080 (业务) / 9090 (管理) | **产品**:通用网关 |
| agent | `halon/agent:dev` | `bin/agent` | 8081 (事件 + 报告 API) | **产品**:故障排查 Agent |
| user-service | `halon/user-service-demo:dev` | `bin/user-service-demo` | 8090 | **演示**,非产品 |

`Dockerfile` 是多阶段构建:golang:1.26-alpine 编译 → distroless 非 root 运行,`--build-arg SERVICE_PATH=<目录>` 选择入口(`cmd/gateway`、`cmd/agent` 或 `examples/user-service`)。`Makefile` 目标见 [development.md](development.md)。

## Kubernetes 清单

产品清单在 `deploy/kubernetes/`(`make deploy` 只部署它们):

| 文件 | 内容 |
| --- | --- |
| `postgres.yaml` | StatefulSet + Service + PVC,数据在 Pod 重建后保留 |
| `gateway.yaml` | 1 副本 Deployment + 两个 Service(`gateway` / `gateway-admin`),`ROUTES` 声明上游路由,推送超时事件到 `http://agent:8081/api/v1/incidents/timeout` |
| `agent.yaml` | ServiceAccount + 命名空间级 Role/RoleBinding(最小权限)+ 1 副本 Deployment + Service |

演示清单在 `examples/user-service/deploy.yaml`,需单独 `kubectl apply`。

### 适配任意 K8s 集群

清单不绑定任何发行版,无 Minikube/云厂商专属 API:

1. 构建镜像并推送到集群可访问的镜像仓库;
2. 修改清单中的 `image:`(默认 `halon/*:dev` + `imagePullPolicy: IfNotPresent`,适合本地集群);
3. 确认集群默认 StorageClass 可供给 PVC(postgres.yaml);不行则改用 hostPath 或外部数据库;
4. `kubectl apply`。

### Agent 的 RBAC 权限(agent.yaml)

`pods: get/list/delete`、`pods/log: get`、`events: get/list`、`deployments: get/list/update/patch`。新增需要其他资源的工具时先扩 Role,并在 [modules/agent-tools.md](modules/agent-tools.md) 记录。

### Secret(halon-secrets,手动创建)

| 键 | 用途 | 消费方 |
| --- | --- | --- |
| `postgres-user` / `postgres-password` | 数据库账号 | postgres |
| `database-dsn` | 连接串 | gateway、agent |
| `admin-token` | 网关管理接口 Token | gateway、agent(封禁工具) |
| `ingest-token` | 事件接收/报告查询 Token | agent、gateway(webhook 推送) |
| `openai-api-key` | 大模型凭据 | agent |

创建命令见根 `README.md` 的「部署到 Kubernetes」。**新增密钥键时同步更新本表与 README。**

## 本地运行(无 K8s)

1. Docker 启动 PostgreSQL 17(命令见 README);
2. `make run-demo`(演示后端,:8090);
3. 带 `DATABASE_DSN`/`ADMIN_TOKEN`/`ROUTES` 等环境变量 `make run-gateway`;
4. 可选:带 `INGEST_TOKEN`/`OPENAI_API_KEY`/`OPENAI_MODEL` 等 `make run-agent`。Agent 会回退使用 `~/.kube/config` 访问集群;本地无集群时 K8s 工具会报连接错误,其余功能不受影响(可将操作级别保持 readonly 并忽略 K8s 工具错误)。

## 端到端故障演练(任意集群)

```bash
kubectl port-forward service/gateway 18080:8080
kubectl port-forward service/agent   18081:8081

# 1. 制造超时:演示后端 ?delay= 超过网关 UPSTREAM_TIMEOUT
curl -i 'http://127.0.0.1:18080/api/v1/users/1?delay=5000'     # 预期 504

# 2. 轮询 Agent 报告
curl -s http://127.0.0.1:18081/api/v1/incidents \
  -H 'Authorization: Bearer halon-local-ingest-token'

# 3. 报告也可直接查库
kubectl exec postgres-0 -- psql -U halon -d halon \
  -c 'SELECT event_id, status, steps, total_tokens FROM incident_reports ORDER BY occurred_at DESC LIMIT 5;'
```

## 配置总表

网关与 Agent 的全部环境变量见根 `README.md` 的「网关配置」「Agent 配置」两节;改配置定义(`internal/*/config.go`)时必须同步这两节。
