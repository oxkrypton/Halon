# 模块:RBAC 与 JWT 鉴权(auth)

代码:`internal/authz/`(角色、权限、Principal、JWT 签发/验证、gin 中间件)+ `internal/identity/`(可选本地用户存储、密码认证、Refresh Token 轮转与清理)+ `cmd/halonctl/`(离线 keygen/token CLI)。

## 设计要点

- **一套角色**:`readonly` < `operator` < `full`,沿用 `AGENT_OPERATION_LEVEL` 和报告 `level` 字符串,不迁移历史数据。人类管理员与 Agent 共用同一套角色。
- **权限字符串**:`资源:动作`,由网关路由和 Agent 工具共同引用;映射表写在代码里(`internal/authz/role.go`),不支持自定义角色或环境变量改映射。
- **凭据并存**:`AUTH_MODE=legacy`(默认,与升级前行为完全一致)/ `hybrid`(JWT 与静态令牌并存)/ `jwt`(管理面只认 JWT,webhook ingest 例外)。
- **签名算法**:仅 Ed25519(EdDSA)。拒绝 `none`、HS 系列和未知 `kid`;`exp` 必填,校验 `iss`/`aud`/`sub`/`jti`,支持 `kid` 轮换与 `AUTH_REVOKED_JTIS` 单点撤销。验签无状态,不需要 Redis。
- **两种运行形态**:默认保持无状态离线密钥模式(`AUTH_LOGIN_ENABLED=false`,不读写用户库,私钥离线由 `halonctl` 保管);如需浏览器/本地账号登录与用户管理,可按需开启 `AUTH_LOGIN_ENABLED=true`。

## 权限矩阵

| 权限 | readonly | operator | full |
| --- | :-: | :-: | :-: |
| `incident:read` | ✓ | ✓ | ✓ |
| `gateway:rules:read` / `gateway:breaker:read` | ✓ | ✓ | ✓ |
| `gateway:metrics:read`(`/metrics` 抓取) | ✓ | ✓ | ✓ |
| `gateway:observability:read`(`/admin/system`、`/admin/observability/*`、`/admin/traces/*`) | ✓ | ✓ | ✓ |
| `tool:observe`(`query_*`、`k8s_*` 只读工具) | ✓ | ✓ | ✓ |
| `gateway:blacklist:write`(`gateway_block_ip` 所需) | | ✓ | ✓ |
| `tool:k8s:restart_pod` / `tool:k8s:scale` | | ✓ | ✓ |
| `gateway:ratelimit:write` | | | ✓ |
| `tool:k8s:update_image` | | | ✓ |
| `auth:user:read` / `auth:user:manage` | | | ✓ |

限流写入与用户管理只给 `full`:限流参数写错会让网关拒绝全部流量,用户管理涉及权限授予。

## JWT claims

```json
{
  "sub": "alice",
  "typ": "human",
  "role": "operator",
  "uid": "11111111-1111-4111-8111-111111111111",
  "pcr": false,
  "aud": ["halon-gateway-admin", "halon-agent"],
  "iss": "halon",
  "exp": 1764000000,
  "jti": "random-hex",
  "kid": "<JWT header, 公钥派生>"
}
```

`aud` 可为单个字符串或数组,元素只能是 `halon-gateway-admin`(网关管理面、Agent 访问网关)或 `halon-agent`(Agent HTTP API 读接口),任一元素命中当前组件即验签通过,跨组件重放仍被拒绝。`uid` 仅在本地登录签发的人类会话中存在;`pcr`(password change required)为 `true` 时,请求会被 `authz.Require` 以 `403 PASSWORD_CHANGE_REQUIRED` 拦截,仅放行 `/auth/me` 与 `/auth/password`。`typ` 只做身份分类,授权完全由 `role` 决定;但 `cmd/agent` 的启动自检额外要求网关凭据 `typ=agent`,`typ=human` 的人类令牌即使在角色上匹配也会被拒绝。

## 公共配置(`internal/authz`)

两端都在自己的 `LoadConfig()` 中调用 `authz.LoadConfig()`(网关在开启 `AUTH_LOGIN_ENABLED` 时调用 `LoadConfigWithExtraPublicKeys` 自动合入本地签名私钥对应的公钥),启动即校验。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTH_MODE` | `legacy` | `legacy` / `hybrid` / `jwt` |
| `AUTH_JWT_PUBLIC_KEYS` | 空 | `kid=base64公钥,...`;`hybrid` 和 `jwt` 模式必填(网关开启 `AUTH_LOGIN_ENABLED` 时可由 `AUTH_JWT_PRIVATE_KEY` 自动派生) |
| `AUTH_JWT_ISSUER` | `halon` | 期望的 `iss` |
| `AUTH_REVOKED_JTIS` | 空 | 逗号分隔的紧急撤销 jti |

`ADMIN_TOKEN` 在 `legacy`/`hybrid` 下仍需配置(≥12 字符),映射为 `full` 主体;`jwt` 模式下可为空。`INGEST_TOKEN` 始终只用于 `POST /api/v1/incidents/timeout`,不进入角色体系:`hybrid` 下兼容报告读取,`jwt` 下读接口必须提供 JWT。

## 本地用户与会话(可选,`AUTH_LOGIN_ENABLED=true`)

开启本地登录需同时满足 `AUTH_MODE=hybrid` 或 `jwt` 并配置 `AUTH_JWT_PRIVATE_KEY`。未开启时 `/auth/*` 与 `/admin/users/*` 路由仍注册,但统一返回 `404 LOGIN_DISABLED`。

### 存储与安全机制(`internal/identity`)

- **表结构与迁移**:使用独立迁移版本表 `auth_schema_migrations`,创建 `auth_users`、`auth_refresh_tokens`、`auth_events` 三张表,与 `schema_migrations`(trafficlog)及 `incident_schema_migrations`(agent)互不干扰。
- **密码哈希与防爆破**:采用 Argon2id(`m=64 MiB, t=3, p=2`),进程内以 `maxHashConcurrency=4` 信号量限流(峰值约 256 MiB,开启本地登录时网关 `limits.memory` 建议 `>= 384Mi`);不存在的用户名使用 `FakePasswordHash()` 平衡耗时,登录失败统一返回 `401 INVALID_CREDENTIALS`。登录失败计数在 SQL 中原子递增并在锁定期满后自动重置计数。
- **Refresh Token 轮转与撤销**:随机 32 字节,库内仅存 SHA-256 摘要。`RotateRefreshToken` 在单个数据库事务内原子执行 `UPDATE ... WHERE used_at IS NULL ... RETURNING` 与新令牌写入(插入失败自动回滚);`10s` 并发重试宽限期外再次使用已消费令牌会吊销整条 family 并记录 `refresh_reuse_detected`。`ChangePassword`、管理员 `ResetPassword` 以及 `UpdateUserGuarded` 禁用用户时都会立即吊销用户全部 Refresh Token。
- **管理员保护与审计**:`UpdateUserGuarded` 通过 `SELECT ... FOR UPDATE` 禁止自我降权/禁用,并阻止降权/禁用最后一个启用的 `full` 管理员;所有认证与用户管理动作写入 `auth_events`(不记录明文密码与令牌),`Cleaner` 每小时按 1000 条/批清理过期 Refresh Token 与超龄审计记录。

### HTTP 接口(网关管理端口 `:9090`)

| 接口 | 权限 / 说明 |
| --- | --- |
| `POST /auth/login` | 用户名密码登录,返回 `access_token` + `refresh_token` + `user`;受 `AUTH_LOGIN_RATE_LIMIT_*` 限流 |
| `POST /auth/refresh` | 轮转 Refresh Token(重新查库生效最新角色/禁用状态);受 `AUTH_LOGIN_RATE_LIMIT_*` 限流 |
| `POST /auth/logout` | 吊销 Refresh Token 所属 family(幂等 204);受 `AUTH_LOGIN_RATE_LIMIT_*` 限流 |
| `GET /auth/me` | 返回当前主体信息;仅挂 `authz.Middleware`,允许 `pcr=true` 令牌 |
| `POST /auth/password` | 自助改密并吊销旧 Refresh Token(仅挂 `authz.Middleware`,允许 `pcr=true` 令牌;静态 Token 无 `uid` 拒绝) |
| `GET /admin/users` / `GET /admin/users/:id` | `auth:user:read` (`:id` 须为合法 UUID) |
| `POST /admin/users` | `auth:user:manage`;未传密码时自动生成 `max(24, AUTH_PASSWORD_MIN_LENGTH)` 位临时密码并置 `must_change_password=true` |
| `PATCH /admin/users/:id` | `auth:user:manage`;改角色/启禁用/改显示名,保护自身与最后管理员 |
| `POST /admin/users/:id/password` | `auth:user:manage`;重置临时密码、置 `must_change_password=true` 并吊销全部旧 Refresh Token |

### 本地登录配置(网关环境变量)

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `AUTH_LOGIN_ENABLED` | `false` | 开启 `/auth/*` 与 `/admin/users/*` |
| `AUTH_JWT_PRIVATE_KEY` | 空 | base64 Ed25519 私钥,`AUTH_LOGIN_ENABLED=true` 时必填 |
| `AUTH_JWT_KID` | 自动派生 | 可选 `kid`,默认由私钥对应的公钥派生 |
| `AUTH_ACCESS_TOKEN_TTL` | `15m` | Access Token 有效期(`1m..1h`),默认签发给 `halon-gateway-admin,halon-agent` |
| `AUTH_REFRESH_TOKEN_TTL` | `168h` | 单次 Refresh Token 滚动有效期 |
| `AUTH_REFRESH_MAX_LIFETIME` | `720h` | 同一次登录 family 的最长寿命(需 `>= AUTH_REFRESH_TOKEN_TTL`) |
| `AUTH_PASSWORD_MIN_LENGTH` | `12` | 创建/重置/改密最小长度(`8..128`) |
| `AUTH_LOGIN_RATE_LIMIT_RPS` | `0.2` | 登录/刷新/登出接口单 IP 速率上限(Redis 键前缀 `halon:ratelimit:login:`) |
| `AUTH_LOGIN_RATE_LIMIT_BURST` | `5` | 登录/刷新/登出接口单 IP 突发容量 |
| `AUTH_LOGIN_MAX_FAILURES` | `10` | 连续失败锁定阈值 |
| `AUTH_LOGIN_LOCK_DURATION` | `5m` | 账号锁定时长 |
| `AUTH_EVENT_RETENTION` | `2160h` | `auth_events` 审计表保留时长(由后台 Cleaner 按小时清理) |
| `AUTH_BOOTSTRAP_ADMIN_USER` | 空 | 首次引导创建的 `full` 管理员用户名(需与密码同设,仅 `auth_users` 为空时生效) |
| `AUTH_BOOTSTRAP_ADMIN_PASSWORD` | 空 | 首次引导管理员初始密码(首次登录强制改密,不打印日志) |

## 各组件强制点

| 位置 | 做法 |
| --- | --- |
| 网关管理接口 `:9090` | `authz.Middleware` + 按路由 `authz.Require(perm)`;无令牌/验签失败 401 `UNAUTHORIZED`,权限不足 403 `FORBIDDEN` / `PASSWORD_CHANGE_REQUIRED`,错误体仍是 `{error:{code,message,request_id}}` |
| Agent HTTP API | ingest 独立于角色体系;`GET /incidents*` 要求 `incident:read` |
| Agent 工具层 | `Tool.Permission` 取代 `Level`;`Registry` 注册时过滤、执行时再校验一次 |
| Agent → 网关 | `GATEWAY_ADMIN_TOKEN` 可为 JWT,启动自检要求 `typ=agent` 且角色与 `AGENT_OPERATION_LEVEL` 一致,否则启动失败。若 `GATEWAY_ADMIN_URL` 已配置且操作级别为 `operator`/`full`,令牌为空也直接启动失败(避免运行时才 401);`GATEWAY_ADMIN_URL` 为空时允许空令牌(由工具自身报「未配置」),但 `jwt` 模式下非空静态令牌仍拒绝。静态令牌在级别低于 `full` 时打 WARN |
| `readonly` Agent | 工具集里没有网关写操作,部署时可不配 `GATEWAY_ADMIN_TOKEN` |
| 网关 `/metrics` | `METRICS_REQUIRE_AUTH=true` 时只挂 `authz.Middleware` + `Require(gateway:metrics:read)`,允许 `readonly` 抓取;抓取不写审计日志、不计 `halon_gateway_admin_requests_total`,避免每次 Prometheus 拉取都产生一条记录 |
| 数据面 `:8080` | 不变 |
| 审计 | 管理接口写/读请求统一结构日志(`sub`/`typ`/`role`/`jti`/`permission`/`result`),指标 `halon_gateway_admin_requests_total{role,permission,result}`;仅 401/403 鉴权拒绝(`authz.DeniedFromContext`)计 `result="deny"`,下游业务错误(400/404/409/500)计 `result="allow"`;`/metrics` 抓取除外 |

## 离线 CLI

```bash
# 1. 生成密钥。未开启本地登录时私钥由管理员离线保管,不进集群。
bin/halonctl keygen

# 2. 签发人类管理员令牌(支持逗号分隔多 aud,默认 halon-gateway-admin)
bin/halonctl token issue \
  --private-key '<base64 private_key>' --sub alice \
  --type human --role operator --aud halon-gateway-admin,halon-agent --ttl 8h

# 3. 签发 Agent 访问网关的令牌(角色须与 AGENT_OPERATION_LEVEL 一致)
bin/halonctl token issue \
  --private-key '<base64 private_key>' --sub agent-1 \
  --type agent --role readonly --ttl 2160h

# 4. 查看/验签令牌(支持多 aud;存在 uid/pcr/audiences 时一并输出)
bin/halonctl token inspect --token '<jwt>' \
  --public-keys '<kid=base64公钥>' --aud halon-gateway-admin
```

`keygen` 输出的 `public_keys_env` 可直接放到 `AUTH_JWT_PUBLIC_KEYS`。Agent 令牌建议 90 天有效期,到期前 14 天告警;Agent 不持有私钥,无法自行升权。

## 升级路径

1. 升级后默认 `legacy`,行为零变化。
2. 把 `AUTH_JWT_PUBLIC_KEYS` 配到两端,切到 `hybrid`,逐步把调用方换成 JWT(若需 Web/账号密码登录,可同时在网关开启 `AUTH_LOGIN_ENABLED=true` 并配置 `AUTH_JWT_PRIVATE_KEY` 与引导管理员)。
3. 确认全部客户端已换完后切到 `jwt`。此时 `INGEST_TOKEN` 仍可 ingest,但不能读报告。

撤销手段:`kid` 轮换(移除旧公钥)、将单个 `jti` 加入 `AUTH_REVOKED_JTIS` 后重启相关组件,或在本地登录模式下调用 `POST /auth/logout` / 禁用用户 / 重置密码吊销 Refresh Token(Access Token 有效期最长 1h)。

## 修改本模块时必须同步检查

- 根 `README.md`:接口表权限列、配置表、项目结构;
- [gateway.md](gateway.md)、[agent.md](agent.md)、[agent-tools.md](agent-tools.md):路由、API 与工具权限;
- `docs/deployment.md` 与 `deploy/kubernetes/{gateway,agent}.yaml`:环境变量、内存限制与 Secret;
- `AGENTS.md`:模块索引与已知边界。

## 已知边界

- 长期有效的 Agent JWT 本质仍是静态凭据,靠 K8s Secret 保护;收益是角色上限与审计。
- 不接 OIDC/JWKS 与自定义角色(需要时可新增 `Authenticator` 实现);`AUTH_LOGIN_ENABLED=false`(默认)时无用户库开销、私钥不进集群,开启 `AUTH_LOGIN_ENABLED=true` 时私钥仅存网关 Secret 用于签发 15m 短生命周期会话令牌。
- `jwt` 模式必须先换令牌再切换,否则 Agent 调不通网关管理接口。
- 本地登录模式下 Access Token 本身无状态(默认 15m),禁用用户或改密/重置密码会立即吊销其 Refresh Token,但已签发的 Access Token 最坏在 TTL 内仍有效(可通过 `AUTH_REVOKED_JTIS` 紧急单点撤销)。
