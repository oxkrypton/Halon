# 模块:RBAC 与 JWT 鉴权(auth)

代码:`internal/authz/`(角色、权限、Principal、JWT 签发/验证、gin 中间件)+ `cmd/halonctl/`(离线 keygen/token CLI)。

## 设计要点

- **一套角色**:`readonly` < `operator` < `full`,沿用 `AGENT_OPERATION_LEVEL` 和报告 `level` 字符串,不迁移历史数据。人类管理员与 Agent 共用同一套角色。
- **权限字符串**:`资源:动作`,由网关路由和 Agent 工具共同引用;映射表写在代码里(`internal/authz/role.go`),不支持自定义角色或环境变量改映射。
- **凭据并存**:`AUTH_MODE=legacy`(默认,与升级前行为完全一致)/ `hybrid`(JWT 与静态令牌并存)/ `jwt`(管理面只认 JWT,webhook ingest 例外)。
- **签名算法**:仅 Ed25519(EdDSA)。拒绝 `none`、HS 系列和未知 `kid`;`exp` 必填,校验 `iss`/`aud`/`sub`/`jti`,支持 `kid` 轮换与 `AUTH_REVOKED_JTIS` 单点撤销。验签无状态,内存/共享模式都不需要 Redis。
- **不建用户库、不做登录页**:身份来自签发时填写的 `sub`;验证器在 `*authz.Authenticator` 后面,未来可替换为 OIDC/JWKS 实现。

## 权限矩阵

| 权限 | readonly | operator | full |
| --- | :-: | :-: | :-: |
| `incident:read` | ✓ | ✓ | ✓ |
| `gateway:rules:read` / `gateway:breaker:read` | ✓ | ✓ | ✓ |
| `gateway:metrics:read`(`/metrics` 抓取) | ✓ | ✓ | ✓ |
| `tool:observe`(`query_*`、`k8s_*` 只读工具) | ✓ | ✓ | ✓ |
| `gateway:blacklist:write`(`gateway_block_ip` 所需) | | ✓ | ✓ |
| `tool:k8s:restart_pod` / `tool:k8s:scale` | | ✓ | ✓ |
| `gateway:ratelimit:write` | | | ✓ |
| `tool:k8s:update_image` | | | ✓ |

限流写入只给 `full`:参数写错会让网关拒绝全部流量。

## JWT claims

```json
{
  "sub": "alice",
  "typ": "human",
  "role": "operator",
  "aud": "halon-gateway-admin",
  "iss": "halon",
  "exp": 1764000000,
  "jti": "random-hex",
  "kid": "<JWT header, 公钥派生>"
}
```

`aud` 只能是 `halon-gateway-admin`(网关管理面、Agent 访问网关)或 `halon-agent`(Agent HTTP API 读接口)。跨组件重放会被拒绝。`typ` 只做身份分类,授权完全由 `role` 决定;但 `cmd/agent` 的启动自检额外要求网关凭据 `typ=agent`,`typ=human` 的人类令牌即使在角色上匹配也会被拒绝。

## 配置

两端都在自己的 `LoadConfig()` 中调用 `authz.LoadConfig()`,启动即校验。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTH_MODE` | `legacy` | `legacy` / `hybrid` / `jwt` |
| `AUTH_JWT_PUBLIC_KEYS` | 空 | `kid=base64公钥,...`;`hybrid` 和 `jwt` 模式必填,公钥可放 ConfigMap |
| `AUTH_JWT_ISSUER` | `halon` | 期望的 `iss` |
| `AUTH_REVOKED_JTIS` | 空 | 逗号分隔的紧急撤销 jti |

`ADMIN_TOKEN` 在 `legacy`/`hybrid` 下仍需配置(≥12 字符),映射为 `full` 主体;`jwt` 模式下可为空。`INGEST_TOKEN` 始终只用于 `POST /api/v1/incidents/timeout`,不进入角色体系:`hybrid` 下兼容报告读取,`jwt` 下读接口必须提供 JWT。

## 各组件强制点

| 位置 | 做法 |
| --- | --- |
| 网关管理接口 `:9090` | `authz.Middleware` + 按路由 `authz.Require(perm)`;无令牌/验签失败 401 `UNAUTHORIZED`,权限不足 403 `FORBIDDEN`,错误体仍是 `{error:{code,message,request_id}}` |
| Agent HTTP API | ingest 独立于角色体系;`GET /incidents*` 要求 `incident:read` |
| Agent 工具层 | `Tool.Permission` 取代 `Level`;`Registry` 注册时过滤、执行时再校验一次 |
| Agent → 网关 | `GATEWAY_ADMIN_TOKEN` 可为 JWT,启动自检要求 `typ=agent` 且角色与 `AGENT_OPERATION_LEVEL` 一致,否则启动失败。若 `GATEWAY_ADMIN_URL` 已配置且操作级别为 `operator`/`full`,令牌为空也直接启动失败(避免运行时才 401);`GATEWAY_ADMIN_URL` 为空时保持放行,由工具自身报「未配置」。静态令牌在级别低于 `full` 时打 WARN |
| `readonly` Agent | 工具集里没有网关写操作,部署时可不配 `GATEWAY_ADMIN_TOKEN` |
| 网关 `/metrics` | `METRICS_REQUIRE_AUTH=true` 时只挂 `authz.Middleware` + `Require(gateway:metrics:read)`,允许 `readonly` 抓取;抓取不写审计日志、不计 `halon_gateway_admin_requests_total`,避免每次 Prometheus 拉取都产生一条记录 |
| 数据面 `:8080` | 不变 |
| 审计 | 管理接口写/读请求统一结构日志(`sub`/`typ`/`role`/`jti`/`permission`/`result`),指标 `halon_gateway_admin_requests_total{role,permission,result}`;`/metrics` 抓取除外;不新增数据库表 |

## 离线 CLI

```bash
# 1. 生成密钥。私钥由管理员离线保管,不进集群。
bin/halonctl keygen

# 2. 签发人类管理员令牌(默认 aud=halon-gateway-admin)
bin/halonctl token issue \
  --private-key '<base64 private_key>' --sub alice \
  --type human --role operator --ttl 8h

# 3. 签发 Agent 访问网关的令牌(角色须与 AGENT_OPERATION_LEVEL 一致)
bin/halonctl token issue \
  --private-key '<base64 private_key>' --sub agent-1 \
  --type agent --role readonly --ttl 2160h

# 4. 查看/验签令牌
bin/halonctl token inspect --token '<jwt>' \
  --public-keys '<kid=base64公钥>' --aud halon-gateway-admin
```

`keygen` 输出的 `public_keys_env` 可直接放到 `AUTH_JWT_PUBLIC_KEYS`。Agent 令牌建议 90 天有效期,到期前 14 天告警;Agent 不持有私钥,无法自行升权。

## 升级路径

1. 升级后默认 `legacy`,行为零变化。
2. 把 `AUTH_JWT_PUBLIC_KEYS` 配到两端,切到 `hybrid`,逐步把调用方换成 JWT。
3. 确认全部客户端已换完后切到 `jwt`。此时 `INGEST_TOKEN` 仍可 ingest,但不能读报告。

撤销手段:`kid` 轮换(移除旧公钥),或把单个 `jti` 加入 `AUTH_REVOKED_JTIS` 后重启相关组件。

## 修改本模块时必须同步检查

- 根 `README.md`:接口表权限列、配置表、项目结构;
- [gateway.md](gateway.md)、[agent.md](agent.md)、[agent-tools.md](agent-tools.md):路由、API 与工具权限;
- `docs/deployment.md` 与 `deploy/kubernetes/{gateway,agent}.yaml`:环境变量与 Secret;
- `AGENTS.md`:模块索引与已知边界。

## 已知边界

- 长期有效的 Agent JWT 本质仍是静态凭据,靠 K8s Secret 保护;收益是角色上限与审计。
- 不接 OIDC/JWKS、用户库、登录页、自定义角色;需要时新增 `Authenticator` 实现。
- `jwt` 模式必须先换令牌再切换,否则 Agent 调不通网关管理接口。
- 私钥不应放进集群。
