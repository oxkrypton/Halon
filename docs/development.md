# 开发流程与约定

> 修改本文档描述的任何流程或约定后,必须同步更新根目录 `AGENTS.md` 和 `README.md`。

## 环境要求

- Go 1.26+
- golangci-lint v2.15+(本地跑 `make lint` 需要,如 `brew install golangci-lint`)
- Docker(构建镜像)
- kubectl + 任意 Kubernetes 集群(仅部署验证需要;本地推荐 Minikube 或 Kind)

## 日常命令

| 命令 | 作用 |
| --- | --- |
| `make test` | `go test -race ./...` + `go vet ./...`,**每次提交前必跑** |
| `make lint` | `golangci-lint run ./...`(配置见 `.golangci.yml`),**每次提交前必跑** |
| `make build` / `make build-demo` | 构建产品二进制 `bin/gateway`、`bin/agent`、`bin/halonctl` 与演示二进制 `bin/user-service-demo` |
| `make run-gateway` / `make run-agent` / `make run-demo` / `make run-web` | 本地运行(环境变量见各模块文档;`run-web` 启动 Vite 前端) |
| `make build-web` / `make docker-build-web` / `make deploy-web` | 前端类型检查/构建、构建 `halon/web:dev` 镜像与部署可选控制台清单 |
| `make docker-build` / `make docker-build-demo` | 构建产品镜像与演示镜像 |
| `make deploy` | `kubectl apply -f deploy/kubernetes/`(适用于任意 K8s 集群) |

## CI 流水线

GitHub Actions 工作流见 `.github/workflows/ci.yml`,在 push 到 `main` 和每个 Pull Request 时运行,两个并行任务:

- **test / vet / build**:gofmt 格式检查 → `go vet` → `go test -race -count=1` → `make build`。
- **golangci-lint**:按 `.golangci.yml` 运行,版本固定为 v2.15(与本地 `make lint` 保持一致;升级时两边同步改)。

规则:`#nosec` 豁免必须写明理由(参见 `internal/agent/tools/observability.go` 中 G201 的用法);不允许用配置整体关闭某个 linter 来让 CI 变绿。

## 目录约定

```text
cmd/<service>/main.go      # 产品服务入口:只做装配(加载配置、打开依赖、启动、优雅退出)
internal/<module>/         # 产品模块全部实现与测试
examples/<name>/           # 演示组件,不属于产品本体;产品代码禁止 import examples
deploy/kubernetes/         # 产品组件清单,每服务一个 yaml
docs/                      # 本文档中心
```

## 代码约定

1. **配置**:每模块一个 `LoadConfig()`(见 `internal/gateway/config.go`、`internal/agent/config.go`),环境变量集中解析、启动时校验、join 所有校验错误一次报出。密钥类配置必须为空时直接拒绝启动。
2. **错误响应**:HTTP 层统一 `{error: {code, message}}`;上游服务的响应(含错误)原样透传,网关不包装。
3. **数据库**:golang-migrate 嵌入式迁移(`//go:embed migrations`),禁止 AutoMigrate;共享同一数据库的不同模块使用**各自独立的迁移版本表**;SQL 全部参数化。
4. **并发**:跨 goroutine 传递用有界 channel;队列满时丢弃并计数告警,不阻塞调用方;共享状态用 `sync.Mutex` 保护;所有测试必须过 `-race`。
5. **安全**:Token 用 `subtle.ConstantTimeCompare` 比较;不信任代理头(`SetTrustedProxies(nil)`);请求体读取一律 `http.MaxBytesReader` 限长;容器 `runAsNonRoot` + 只读根文件系统 + drop ALL capabilities。
6. **优雅退出**:`signal.NotifyContext` 监听 SIGINT/SIGTERM → 停 HTTP(有超时)→ 排空队列 → 关闭数据库。
7. **解耦**:产品组件之间只能通过契约通信(`incident.Event` webhook、管理 HTTP API、PostgreSQL 表结构),禁止共享内部包之外的状态;网关不得出现具体业务服务名。
8. **鉴权**:新增 HTTP 接口或工具必须复用 `internal/authz` 的 `Permission` 常量,网关侧挂 `Require`,Agent 工具侧写 `Tool.Permission`;角色映射只在代码中维护,签发工具为 `cmd/halonctl`,详见 [modules/auth.md](modules/auth.md)。

## 测试约定

- 每个 `internal/` 包都有对应 `_test.go`;新增公开行为必须配测试。
- 存储层用 `github.com/DATA-DOG/go-sqlmock`(参考 `internal/incident/store_test.go`);注意 GORM `Take` 会自动追加 `LIMIT $n` 参数。
- LLM 调用用 `internal/agent` 包内测试的 `modelFunc` 桩;K8s 用 `k8s.io/client-go/kubernetes/fake`(见 `internal/agent/tools/kubernetes_test.go`);HTTP 上游用 `httptest.NewServer`(见 `internal/gateway/server_test.go`)。
- 并发组件(批写器、编排器)测试用「轮询 + 截止时间」而不是固定 sleep 等待结果。

## Git 提交规范

- Conventional Commits:`feat:` / `fix:` / `docs:` / `build:` / `test:` / `refactor:`,主题行英文小写、祈使句。
- 一次提交只做一件事,分步提交;文档更新可与代码同提交,也可紧随其后以 `docs:` 单独提交。
- 提交前确认:`make test` 与 `make lint` 全绿、`gofmt -l` 无输出、`git status` 无遗漏文件。CI 会在 push 和 PR 上重复同样的检查,本地先跑可以少一次往返。
