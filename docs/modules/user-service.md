# 模块:用户服务(user-service)

代码:`internal/user/service.go`(实现)+ `cmd/user-service/main.go`(入口)+ `cmd/user-client/main.go`(验证客户端)。
接口定义:`api/proto/user/v1/user.proto`,生成代码在 `api/gen/user/v1/`(**不手改**)。
监听:`:50051`(gRPC)。

## 职责

演示用后端微服务,为网关提供上游依赖,也是故障注入(超时/错误)的实验对象。

## 接口

```proto
service UserService {
  rpc GetUser(GetUserRequest) returns (GetUserResponse);
}
```

- 入参 `id` 为空 → `InvalidArgument`;
- 内置演示用户:`1 / Alice`、`2 / Bob`;其他 ID → `NotFound`;
- 服务开启 gRPC 健康检查(`grpc.health.v1`)与反射。

## 实现要点

- `NewService()` 返回内存用户表的 `Service`;
- `cmd/user-service` 注册服务、健康检查、反射,SIGINT/SIGTERM 触发 `GracefulStop`;
- `cmd/user-client` 是一次性冒烟客户端,按顺序调用正常/异常参数验证状态码映射。

## 修改接口的流程

改 proto → `make proto` 重新生成 → 改实现与客户端 → 更新本文档与根 `README.md`。详细流程见 [../development.md](../development.md)。

## 修改本模块时必须同步检查

- 根 `README.md`:「用户服务」功能列表;
- [gateway.md](gateway.md):若改了 gRPC 方法签名或错误语义;
- `deploy/kubernetes/user-service.yaml`:若改了端口或探针。
