# 演示服务:order-service(示例,不属于产品本体)

第二个极简 HTTP 演示后端,与 `examples/user-service` 并列,用于演示「一个网关代理多个上游服务」的拓扑。

## 接口

| 接口 | 说明 |
| --- | --- |
| `GET /healthz` | 存活检查 |
| `GET /api/v1/orders/:id` | 返回演示订单(内置 `1001`、`1002`,其余 404) |

## 故障注入(演练用)

与 user-service 相同:`?delay=<毫秒>`(0..30000)延迟响应、`?status=<code>`(100..599)强制状态码。

## 运行

```bash
go run ./examples/order-service          # 默认 :8092,HTTP_ADDR 可改
```

## 部署(任意 K8s 集群)

```bash
docker build --build-arg SERVICE_PATH=examples/order-service -t halon/order-service-demo:dev .
kubectl apply -f examples/order-service/deploy.yaml
```

网关上把订单服务加进路由表(与用户服务并列):

```text
ROUTES=/api/v1/users/=http://user-service:8090,/api/v1/orders/=http://order-service:8092
```
