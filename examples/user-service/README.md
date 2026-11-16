# 演示服务:user-service(示例,不属于产品本体)

这是一个**故意保持极简的 HTTP 后端**,仅用于:

1. 本地开发时给 Halon 网关提供一个真实的上游;
2. 在任意 K8s 集群里演练「网关 → 超时事件 → Agent 排查 → 报告」的完整闭环。

## 接口

| 接口 | 说明 |
| --- | --- |
| `GET /healthz` | 存活检查 |
| `GET /api/v1/users/:id` | 返回演示用户(内置 `1 / Alice`、`2 / Bob`,其余 404) |

## 故障注入(演练用)

| 参数 | 效果 |
| --- | --- |
| `?delay=<毫秒>` | 延迟响应(0..30000ms),配合网关 `UPSTREAM_TIMEOUT=3s` 可制造 504 |
| `?status=<code>` | 强制返回指定状态码(100..599) |

## 运行

```bash
go run ./examples/user-service           # 默认 :8090,HTTP_ADDR 可改
```

## 部署(任意 K8s 集群)

```bash
docker build --build-arg SERVICE_PATH=examples/user-service -t halon/user-service-demo:dev .
# 推送到你的镜像仓库后,替换 deploy.yaml 里的 image
kubectl apply -f examples/user-service/deploy.yaml
```

网关上把演示服务挂进路由表:`ROUTES=/api/v1/users/=http://user-service:8090`。
