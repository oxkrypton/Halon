# 开发流程与约定

> 修改本文档描述的任何流程或约定后,必须同步更新根目录 `AGENTS.md` 和 `README.md`。

## 环境要求

- Go 1.26+
- Docker Desktop、kubectl、Minikube(仅部署验证需要)
- `protoc` + `protoc-gen-go` v1.36.10 + `protoc-gen-go-grpc` v1.5.1(仅修改 proto 时需要)

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.10
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
```

## 日常命令

| 命令 | 作用 |
| --- | --- |
| `make test` | `go test -race ./...` + `go vet ./...`,**每次提交前必跑** |
| `make build` | 构建 `bin/user-service`、`bin/gateway`、`bin/agent` |
| `make run` / `make run-gateway` / `make run-agent` | 本地运行三个服务(环境变量见各模块文档) |
| `make proto` | 重新生成 `api/gen/user/v1` 下的 Go 代码 |
| `make docker-build` / `make minikube-image` / `make deploy` | 镜像构建与部署 |

## 目录约定

```text
cmd/<service>/main.go      # 只做装配:加载配置、打开依赖、启动、优雅退出
internal/<module>/         # 模块全部实现与测试;跨模块引用只允许 internal 内部
api/proto/ + api/gen/      # proto 源文件与生成的代码(生成代码不手改)
deploy/kubernetes/         # 每服务一个 yaml,集中放 Deployment/Service/RBAC
docs/                      # 本文档中心
```

## 代码约定

1. **配置**:每模块一个 `LoadConfig()`(见 `internal/gateway/config.go`、`internal/agent/config.go`),环境变量集中解析、启动时校验、join 所有校验错误一次报出。密钥类配置必须为空时直接拒绝启动。
2. **错误响应**:HTTP 层统一 `{error: {code, message}}`;gRPC 用标准状态码(`InvalidArgument`/`NotFound` 等)。
3. **数据库**:golang-migrate 嵌入式迁移(`//go:embed migrations`),禁止 AutoMigrate;共享同一数据库的不同模块使用**各自独立的迁移版本表**;SQL 全部参数化。
4. **并发**:跨 goroutine 传递用有界 channel;队列满时丢弃并计数告警,不阻塞调用方;共享状态用 `sync.Mutex` 保护;所有测试必须过 `-race`。
5. **安全**:Token 用 `subtle.ConstantTimeCompare` 比较;不信任代理头(`SetTrustedProxies(nil)`);请求体读取一律 `http.MaxBytesReader` 限长;容器 `runAsNonRoot` + 只读根文件系统 + drop ALL capabilities。
6. **优雅退出**:`signal.NotifyContext` 监听 SIGINT/SIGTERM → 停 HTTP(有超时)→ 排空队列 → 关闭数据库。

## 测试约定

- 每个 `internal/` 包都有对应 `_test.go`;新增公开行为必须配测试。
- 存储层用 `github.com/DATA-DOG/go-sqlmock`(参考 `internal/incident/store_test.go`);注意 GORM `Take` 会自动追加 `LIMIT $n` 参数。
- LLM 调用用 `internal/agent` 包内测试的 `modelFunc` 桩;K8s 用 `k8s.io/client-go/kubernetes/fake`(见 `internal/agent/tools/kubernetes_test.go`)。
- 并发组件(批写器、编排器)测试用「轮询 + 截止时间」而不是固定 sleep 等待结果。

## Git 提交规范

- Conventional Commits:`feat:` / `fix:` / `docs:` / `build:` / `test:` / `refactor:`,主题行英文小写、祈使句。
- 一次提交只做一件事,分步提交;文档更新可与代码同提交,也可紧随其后以 `docs:` 单独提交。
- 提交前确认:`make test` 全绿、`gofmt -l` 无输出、`git status` 无遗漏文件。

## 修改 proto 的流程

1. 改 `api/proto/user/v1/user.proto`;
2. `make proto` 重新生成;
3. 同步修改 `internal/user/service.go` 实现与 `cmd/user-client` 验证客户端;
4. 更新 [modules/user-service.md](modules/user-service.md) 与根 `README.md` 接口说明。
