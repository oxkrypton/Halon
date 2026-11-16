# Halon

Halon 是一个面向云原生场景的 Go 微服务项目。当前阶段已实现基础 gRPC 后端服务，并将其部署到本地 Minikube 集群；后续将在此之上接入 Gin 网关、流量日志和智能运维 Agent。

## 已完成功能

- `user-service` 提供 `GetUser` gRPC 接口，包含参数校验及标准状态码。
- 支持 gRPC 健康检查、服务反射和 SIGINT/SIGTERM 平滑退出。
- 提供自动化测试和命令行验证客户端。
- 使用多阶段 Dockerfile 构建精简镜像，容器以非 root 用户运行。
- Kubernetes Deployment 默认运行 2 个副本，配置原生 gRPC 探针、资源限制和安全上下文。
- 通过 ClusterIP Service 在集群内暴露 `50051` 端口。

## 环境要求

- Go 1.26+
- Protocol Buffers Compiler（`protoc`）
- Docker Desktop
- kubectl、Minikube
- `protoc-gen-go`、`protoc-gen-go-grpc`（仅重新生成接口代码时需要）

安装 Go 代码生成插件：

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
```

## 本地运行

```bash
# 可选：Protobuf 定义变更后重新生成代码
make proto

# 运行单元测试、竞态检测和静态检查
make test

# 启动服务，默认监听 :50051
make run
```

在另一个终端调用服务：

```bash
go run ./cmd/user-client -addr localhost:50051 -id 1
```

预期返回健康状态 `SERVING` 和用户 `Alice`。内置演示用户为 `1 / Alice`、`2 / Bob`；查询其他 ID 会返回 `NotFound`。

## 部署到 Minikube

```bash
# 首次使用创建集群，已运行时跳过
make minikube-start

# 构建镜像并载入 Minikube
make minikube-image

# 创建 Deployment 和 Service
make deploy

# 等待两个副本就绪并查看状态
kubectl rollout status deployment/user-service --timeout=180s
make status
```

验证集群中的 gRPC 服务：

```bash
go build -o bin/user-client ./cmd/user-client
kubectl port-forward service/user-service 15051:50051
```

保持端口转发终端运行，在另一个终端执行：

```bash
./bin/user-client -addr 127.0.0.1:15051 -id 2
```

验证完成后，在端口转发终端按 `Ctrl+C`。修改服务代码后，重新执行 `make minikube-image`，再运行 `kubectl rollout restart deployment/user-service` 让 Pod 使用新镜像。

## gRPC 接口

Protobuf 定义位于 `api/proto/user/v1/user.proto`。

```protobuf
service UserService {
  rpc GetUser(GetUserRequest) returns (GetUserResponse);
}
```

| 场景 | gRPC 状态码 |
| --- | --- |
| 查询成功 | `OK` |
| 用户 ID 为空或全为空白字符 | `InvalidArgument` |
| 用户不存在 | `NotFound` |

## 项目结构

```text
.
├── api/
│   ├── gen/user/v1/        # 生成的 Go 代码
│   └── proto/user/v1/      # Protobuf 接口定义
├── cmd/
│   ├── user-client/        # 健康检查与调用验证客户端
│   └── user-service/       # 服务入口
├── internal/user/          # 业务实现及测试
├── deploy/kubernetes/      # Kubernetes 资源清单
├── docs/                   # 架构设计资料
├── Dockerfile              # 多阶段容器构建
└── Makefile                # 常用开发命令
```

详细设计见 `docs/system-architecture.txt`。
