# Halon

Halon 是一个面向云原生场景的 Go 微服务项目。当前阶段先实现基础 gRPC 后端服务，再将其部署到本地 Minikube 集群；后续将在此之上接入 Gin 网关、流量日志和智能运维 Agent。

## 当前目标

1. 实现可通过容器运行的 `user-service` gRPC 微服务。
2. 使用 Kubernetes Deployment 和 Service 将其部署到 Minikube。
3. 验证服务健康状态及集群内 gRPC 调用。

## 项目结构

```text
.
├── api/                    # Protobuf 接口及生成代码
├── cmd/user-service/       # 服务入口
├── internal/user/          # 业务实现
├── deploy/kubernetes/      # Kubernetes 资源清单
├── docs/                   # 架构设计资料
└── Makefile                # 常用开发命令
```

详细设计见 `docs/system-architecture.txt`。
