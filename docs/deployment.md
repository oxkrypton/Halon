# 部署与运维

> 修改 Dockerfile、Makefile、deploy/kubernetes 下的任何清单或 Secret 键后,必须同步更新本文档、根 `README.md` 的部署章节与对应模块文档。

## 构件

| 服务 | 镜像 | 二进制 | 端口 |
| --- | --- | --- | --- |
| user-service | `halon/user-service:dev` | `bin/user-service` | 50051 (gRPC) |
| gateway | `halon/gateway:dev` | `bin/gateway` | 8080 (业务) / 9090 (管理) |
| agent | `halon/agent:dev` | `bin/agent` | 8081 (事件 + 报告 API) |

`Dockerfile` 是多阶段构建:golang:1.26-alpine 编译 → distroless 非 root 运行,`--build-arg SERVICE=<name>` 选择入口。`Makefile` 目标见 [development.md](development.md)。

## Kubernetes 清单(deploy/kubernetes/)

| 文件 | 内容 |
| --- | --- |
| `postgres.yaml` | StatefulSet + Service + PVC,数据在 Pod 重建后保留 |
| `user-service.yaml` | 2 副本 Deployment + Service,原生 gRPC 探针 |
| `gateway.yaml` | 1 副本 Deployment + 两个 Service(`gateway` / `gateway-admin`),推送超时事件到 `http://agent:8081/api/v1/incidents/timeout` |
| `agent.yaml` | ServiceAccount + 命名空间级 Role/RoleBinding(最小权限)+ 1 副本 Deployment + Service |

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

创建命令见根 `README.md` 的「部署到 Minikube」。**新增密钥键时同步更新本表与 README。**

## 本地运行(无 K8s)

1. Docker 启动 PostgreSQL 17(命令见 README);
2. `make run`(user-service);
3. 带 `DATABASE_DSN`/`ADMIN_TOKEN` 等环境变量 `make run-gateway`;
4. 可选:带 `INGEST_TOKEN`/`OPENAI_API_KEY`/`OPENAI_MODEL` 等 `make run-agent`,Agent 会回退使用 `~/.kube/config` 访问集群(本地无集群时 K8s 工具会报连接错误,其余功能不受影响——若需完全离线可将操作级别保持 readonly 并忽略 K8s 工具错误)。

## Minikube 部署顺序

```bash
make minikube-start
kubectl create secret generic halon-secrets ...   # 见 README
make minikube-image                                # 三个镜像都载入
kubectl apply -f deploy/kubernetes/postgres.yaml
kubectl rollout status statefulset/postgres --timeout=180s
make deploy                                        # 其余全部清单
kubectl rollout status deployment/gateway deployment/user-service deployment/agent --timeout=180s
```

改代码后:`make minikube-image` + `kubectl rollout restart deployment/<服务>`。

## 端到端故障演练(验证 Agent 闭环)

```bash
kubectl port-forward service/gateway 18080:8080
kubectl port-forward service/agent   18081:8081

# 1. 制造超时:把网关 RPC_TIMEOUT 调小或给 user-service 注入延迟后调用
curl -i http://127.0.0.1:18080/api/v1/users/1     # 预期 504

# 2. 轮询 Agent 报告
curl -s http://127.0.0.1:18081/api/v1/incidents \
  -H 'Authorization: Bearer halon-local-ingest-token'

# 3. 报告也可直接查库
kubectl exec postgres-0 -- psql -U halon -d halon \
  -c 'SELECT event_id, status, steps, total_tokens FROM incident_reports ORDER BY occurred_at DESC LIMIT 5;'
```

## 配置总表

网关与 Agent 的全部环境变量见根 `README.md` 的「网关配置」「Agent 配置」两节;改配置定义(`internal/*/config.go`)时必须同步这两节。
