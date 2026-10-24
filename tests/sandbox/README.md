# LOCAL SANDBOX — 真实 Keycloak + 真实 Mailpit（无 Docker）

本目录提供一套**只绑定 loopback**的本地沙箱，用真实 Keycloak（OIDC / Admin API / SMTP）
和真实 Mailpit（SMTP sink + HTTP API）替代 Docker Compose，供 E2E 测试与手工联调使用。

> **LOCAL SANDBOX**：仅机器本地、仅测试凭据、仅 `127.0.0.1`。
> 不是生产环境，不是预发布环境；`moonbyte-test` realm 里的任何用户/密码/令牌都不可用于任何真实系统。

- Keycloak 26.8.1（dev 模式、dev-file H2）：`http://127.0.0.1:8180`，管理/健康接口 `http://127.0.0.1:9000`
- Mailpit v1.32.0：SMTP `127.0.0.1:1025`，Web/API `http://127.0.0.1:8025`
- Realm `moonbyte-test`，OIDC 公共客户端 `onboarding-test`（Authorization Code + PKCE S256）
- 所有运行时数据、下载物、生成的测试凭据都在 `<repo>/.sandbox/`（已被根 `.gitignore` 忽略）

## 1. 前置条件

| 依赖 | 要求 |
|---|---|
| 平台 | macOS arm64（脚本按 `darwin-arm64` 取 Mailpit；其它平台可用 `SB_PLATFORM`/`KC_*` 覆盖，未验证） |
| JDK | `/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home`（系统 Java 25 不用于 Keycloak） |
| 命令 | `curl` `tar` `lsof` `jq` `openssl` `md5` `base64`（macOS 自带，除 `jq`） |
| 网络 | 首次 `up.sh` 下载 Keycloak（166 MB）与 Mailpit（9.5 MB），之后离线可复跑 |
| 端口 | `8180`/`9000`/`1025`/`8025` 空闲；**不要占 8080/8081**（其它项目） |

## 2. 快速开始

```bash
# 1) 安装 + 启动 + 自检（首次会下载，之后约 15 秒）
bash tests/sandbox/up.sh

# 2) 健康与配置总览（退出码 0 = 全部正常）
bash tests/sandbox/status.sh

# 3) 全链路 E2E 验证（建用户 → 发验证邮件 → Mailpit 取信 → 点链接 → OIDC code+PKCE → userinfo → 删用户）
bash tests/sandbox/verify.sh

# 3b) R2 五身份门禁：一次创建并完整验证 N 个身份，打印 PASS 表格后自动删除
bash tests/sandbox/verify.sh --count 5

# 4) 停服务（保留数据库与邮件）
bash tests/sandbox/down.sh

# 5) 清空运行时数据并重新导入 realm（改了 realm JSON 后用它）
bash tests/sandbox/reset.sh

# 6) 连下载物和凭据一起清掉，完全重建
bash tests/sandbox/reset.sh --full
```

脚本均可重复执行：`up.sh` 在已运行时只做校验；`down.sh` 在已停止时直接退出 0；
任何失败都会打印具体原因并以非 0 退出（不会静默通过）。

| 脚本 | 作用 |
|---|---|
| `up.sh` | 下载/解压（幂等）→ 启动 Mailpit → 启动 Keycloak（JDK 21）→ 准备 service-account 客户端并授权 → 校验 realm/SMTP/client/loopback。参数：`--install-only`、`--force-install`、`--no-verify` |
| `down.sh` | 停 Keycloak（先）与 Mailpit；`--purge` 同时删除 H2 库、邮件库、日志 |
| `reset.sh` | `down --purge` + `up`；`--full` 追加删除 `.sandbox/{dist,bin,keycloak,secrets}` |
| `status.sh` | 只读检查：版本、监听地址、HTTP 健康、admin token、service-account token、realm/client/SMTP 配置；`--no-token` 跳过凭据检查；不健康时退出 1 |
| `env.sh` | `source` 后导出全部端点与测试凭据；直接执行只打印（密码/secret 打码） |
| `verify.sh` | 16 项断言的单身份端到端验证（默认）或 `--count N` 多身份门禁；写 `.sandbox/logs/verify-<ts>.log` 记录；`--keep-user` 保留测试用户 |
| `lib.sh` | 公共变量与函数（被上述脚本 source；含 `client_credentials_token` 等助手） |

## 3. 端点清单（全部仅 127.0.0.1）

| 用途 | 地址 |
|---|---|
| Keycloak base | `http://127.0.0.1:8180` |
| OIDC issuer | `http://127.0.0.1:8180/realms/moonbyte-test` |
| Discovery | `http://127.0.0.1:8180/realms/moonbyte-test/.well-known/openid-configuration` |
| Authorization | `http://127.0.0.1:8180/realms/moonbyte-test/protocol/openid-connect/auth` |
| Token | `http://127.0.0.1:8180/realms/moonbyte-test/protocol/openid-connect/token` |
| Userinfo | `http://127.0.0.1:8180/realms/moonbyte-test/protocol/openid-connect/userinfo` |
| JWKS | `http://127.0.0.1:8180/realms/moonbyte-test/protocol/openid-connect/certs` |
| Admin token（master） | `http://127.0.0.1:8180/realms/master/protocol/openid-connect/token`（`grant_type=password`，`client_id=admin-cli`） |
| Admin API | `http://127.0.0.1:8180/admin/realms/moonbyte-test` |
| Keycloak 健康探针 | `http://127.0.0.1:9000/health/ready`、`http://127.0.0.1:9000/health/live` |
| Mailpit Web UI | `http://127.0.0.1:8025` |
| Mailpit API | `http://127.0.0.1:8025/api/v1`（`/messages`、`/message/{ID}`、`/search?query=`、`DELETE /messages`） |
| Mailpit SMTP | `127.0.0.1:1025`（无认证、无 TLS） |
| 应用回调（被测服务） | `http://127.0.0.1:8788/oidc/callback`、`http://127.0.0.1:8788/ui/oauth/callback` |

Realm 关键配置：`registrationAllowed=false`（禁止自助注册，身份只能由 Admin API 创建）、
`verifyEmail=true`、`loginWithEmailAllowed=true`、SMTP 指向 `127.0.0.1:1025`、
发件人 `no-reply@moonbyte.test`、`sslRequired=none`（本地 http）。

客户端 `onboarding-test`：public（无 secret）、Standard Flow、**PKCE 强制 S256**
（不带 `code_challenge` 的授权请求会被拒绝）、Direct Access Grants 打开（便于诊断）。

客户端 `onboarding-admin`：**confidential service-account 客户端**（
`serviceAccountsEnabled=true`、`standardFlowEnabled=false`、`directAccessGrantsEnabled=false`），
其 service-account 用户已被授予 `realm-management` 的 `manage-users`、`view-users`、`query-users`、
`view-realm` 角色。**自动化测试推荐优先用它拿 Admin API token**（client_credentials，TTL 600s），
避免 master realm 管理员 token 只有 60s 的刷新问题。

## 4. 测试凭据

由 `up.sh` 首次运行时生成，写入 `.sandbox/secrets/sandbox.env`（`chmod 600`，已在 gitignore 内）：

| 变量 | 说明 |
|---|---|
| `SANDBOX_ADMIN_USER` | master realm 测试管理员（默认 `kcadmin`），password grant 备用通道 |
| `SANDBOX_ADMIN_PASSWORD` | 随机生成 |
| `SANDBOX_TEST_USER_PASSWORD` | 随机生成，`verify.sh` 创建测试用户时使用 |
| `SANDBOX_ADMIN_CLIENT_ID` | service-account 客户端 id（`onboarding-admin`） |
| `SANDBOX_ADMIN_CLIENT_SECRET` | 该客户端 secret，`up.sh` 每次启动会将其固定写入 Keycloak |

加载方式（供 Go 测试或 shell 使用）：

```bash
source tests/sandbox/env.sh
# 导出：KEYCLOAK_URL / KEYCLOAK_REALM / KEYCLOAK_CLIENT_ID / KEYCLOAK_OIDC_ISSUER /
#       MAILPIT_HTTP_URL / MAILPIT_API_URL / MAILPIT_SMTP_ADDR /
#       SANDBOX_ADMIN_USER / SANDBOX_ADMIN_PASSWORD / SANDBOX_TEST_USER_PASSWORD /
#       SANDBOX_ADMIN_CLIENT_ID / SANDBOX_ADMIN_CLIENT_SECRET /
#       ONBOARDING_BASE_URL / ONBOARDING_OIDC_REDIRECT_URI ...
```

### 获取 admin token（两种方式）

**A. 推荐：service-account client_credentials（TTL 600s，无 60s 刷新问题）**

```bash
source tests/sandbox/env.sh
ADMIN_TOKEN=$(curl -sS -X POST \
  "$KEYCLOAK_URL/realms/moonbyte-test/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=client_credentials' \
  --data-urlencode "client_id=$SANDBOX_ADMIN_CLIENT_ID" \
  --data-urlencode "client_secret=$SANDBOX_ADMIN_CLIENT_SECRET" | jq -r .access_token)
curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$KEYCLOAK_URL/admin/realms/moonbyte-test/users?briefRepresentation=true" | jq length
```

在 shell 里也可以直接调用 lib 助手（`env.sh` 会 source `lib.sh`）：

```bash
source tests/sandbox/env.sh
ADMIN_TOKEN=$(client_credentials_token)     # 失败时返回空并打印原因
```

**B. 备用：master realm 管理员 password grant（TTL 60s，仅用于诊断/引导）**

```bash
source tests/sandbox/env.sh
ADMIN_TOKEN=$(curl -sS -X POST \
  "$KEYCLOAK_URL/realms/master/protocol/openid-connect/token" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=password' -d 'client_id=admin-cli' \
  -d "username=$SANDBOX_ADMIN_USER" \
  --data-urlencode "password=$SANDBOX_ADMIN_PASSWORD" | jq -r .access_token)
```

> master 管理员 token TTL 60s；service-account token TTL 600s（该客户端设置了
> `access.token.lifespan=600`）。5 身份 E2E 建议用 A。

## 5. 常用操作（curl 片段）

```bash
source tests/sandbox/env.sh
ADMIN_TOKEN=$(client_credentials_token)          # 或按上面方式 B 取
AUTH=(-H "Authorization: Bearer $ADMIN_TOKEN")

# 创建用户（emailVerified=false，密码不过期）
curl -sS -X POST "$KEYCLOAK_URL/admin/realms/moonbyte-test/users" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"username":"e2e-1","email":"e2e-1@moonbyte.test","firstName":"E2E","lastName":"One",
       "enabled":true,"emailVerified":false,
       "credentials":[{"type":"password","value":"'"$SANDBOX_TEST_USER_PASSWORD"'","temporary":false}]}'
# -> 201 Created（Location 头里是用户 URL；用 username 查询拿 id）

# 触发验证邮件（VERIFY_EMAIL action token）
curl -sS -o /dev/null -w '%{http_code}\n' -X PUT \
  "$KEYCLOAK_URL/admin/realms/moonbyte-test/users/$USER_ID/execute-actions-email?client_id=onboarding-test&lifespan=3600" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '["VERIFY_EMAIL"]'
# -> 204；等价备用：PUT .../users/$USER_ID/send-verify-email

# 读邮件：列出 + 正文
curl -sS "$MAILPIT_API_URL/messages?limit=50" | jq -r '.messages[] | "\(.ID) \(.To[0].Address) \(.Subject)"'
curl -sS "$MAILPIT_API_URL/message/$MSG_ID" | jq -r '.Text'

# 查询 emailVerified
curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$KEYCLOAK_URL/admin/realms/moonbyte-test/users/$USER_ID" | jq -c '{username,email,emailVerified,requiredActions}'

# 清空邮箱（每个测试用例之间建议清一次）
curl -sS -o /dev/null -X DELETE "$MAILPIT_API_URL/messages"

# 删除用户
curl -sS -o /dev/null -w '%{http_code}\n' -X DELETE \
  "$KEYCLOAK_URL/admin/realms/moonbyte-test/users/$USER_ID" -H "Authorization: Bearer $ADMIN_TOKEN"
```

### ⚠️ 验证邮件链接是"两步"

邮件正文里的链接形如
`http://127.0.0.1:8180/realms/moonbyte-test/login-actions/action-token?key=<JWT>`。
第一次 GET 只返回确认页（"Perform the following action(s): Verify Email » Click here to proceed"），
**必须再 GET 页面里那个 `href`（带 `client_id`、`tab_id`、`client_data` 参数）才真正执行验证**，
之后 Admin API 才会显示 `emailVerified=true`。浏览器里是"用户再点一次"，自动化驱动需要模拟第二次 GET。
`verify.sh` 的第 5 步就是这么做的，可直接抄。

## 6. Go 集成说明（R2 五身份 E2E 需要的调用）

被测服务（`127.0.0.1:8788`）与沙箱的全部交互只有下面这些 REST 调用，
全部只需标准库（`net/http`、`crypto/sha256`、`encoding/base64`、`encoding/json`）：

| # | 动作 | 请求 | 期望 |
|---|---|---|---|
| 1 | admin token（推荐） | `POST {KEYCLOAK_URL}/realms/moonbyte-test/protocol/openid-connect/token`，form：`grant_type=client_credentials&client_id=onboarding-admin&client_secret=…` | 200，`access_token`（TTL 600s，`azp=onboarding-admin`） |
| 1b | admin token（备用/引导） | `POST {KEYCLOAK_URL}/realms/master/protocol/openid-connect/token`，form：`grant_type=password&client_id=admin-cli&username=…&password=…` | 200，`access_token`（TTL 60s） |
| 2 | 创建用户 | `POST {KEYCLOAK_URL}/admin/realms/moonbyte-test/users`，Bearer admin token，JSON：`username/email/enabled/emailVerified=false/credentials[password, temporary=false]` | 201 |
| 3 | 触发验证邮件 | `PUT .../admin/realms/moonbyte-test/users/{id}/execute-actions-email?client_id=onboarding-test&lifespan=3600`，body `["VERIFY_EMAIL"]`（或 `PUT .../users/{id}/send-verify-email`） | 204 |
| 4 | 查询验证状态 | `GET .../admin/realms/moonbyte-test/users/{id}` | 200，`.emailVerified` |
| 5 | 读取验证邮件 | `GET {MAILPIT_API_URL}/messages?limit=50` 按 `To[].Address` 过滤 → `GET {MAILPIT_API_URL}/message/{ID}`，取 `.Text`/`.HTML` 里的 `action-token` 链接 | 200 |
| 6 | 走 OIDC | `GET /realms/moonbyte-test/protocol/openid-connect/auth?client_id=onboarding-test&response_type=code&scope=openid profile email&redirect_uri=…&state=…&nonce=…&code_challenge=<S256>&code_challenge_method=S256` → POST 登录表单 → 302 回调拿 `code` | 302 |
| 7 | 换 token | `POST /realms/moonbyte-test/protocol/openid-connect/token`，form：`grant_type=authorization_code&code=…&redirect_uri=…&client_id=onboarding-test&code_verifier=…` | 200，`access_token`/`id_token` |
| 8 | userinfo | `GET /realms/moonbyte-test/protocol/openid-connect/userinfo`，`Authorization: Bearer <access_token>` | 200，`sub` == Keycloak 用户 id，`email_verified=true` |
| 9 | 清理 | `DELETE .../admin/realms/moonbyte-test/users/{id}` | 204 |

实现注意：

- **redirect_uri 必须与注册值完全一致**（`http://127.0.0.1:8788/oidc/callback`）。
  如果 Go E2E 用 `httptest.NewServer`（随机端口），需要先把该端口加进客户端：
  ```bash
  CID=$(curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
    "$KEYCLOAK_URL/admin/realms/moonbyte-test/clients?clientId=onboarding-test" | jq -r '.[0].id')
  curl -sS -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    "$KEYCLOAK_URL/admin/realms/moonbyte-test/clients/$CID" \
    -d "$(jq -nc --arg cb "$CALLBACK" '{redirectUris:["http://127.0.0.1:8788/oidc/callback","http://127.0.0.1:8788/ui/oauth/callback",$cb]}')"
  ```
  （或在 `reset.sh` 之前改 `tests/sandbox/realm/moonbyte-test-realm.json`）
- PKCE：`code_verifier` 用 43–128 字符 `[A-Za-z0-9-._~]`，`code_challenge = base64url(sha256(verifier))
  去 padding`；失败（错 verifier）Keycloak 返回 `400 invalid_grant`，并且该 code 作废（一次性）。
- `access_token` 的 `aud=account` 是 Keycloak 26 `basic` scope 的默认行为，`id_token` 的 `aud=onboarding-test`；
  userinfo 用 access token 即可，`sub` 与 Admin API 的用户 `id` 一致。
- 邮件正文是 CRLF，按行提取链接时记得 `tr -d '\r'`（`verify.sh` 已处理，踩过坑）。
- 5 个身份并行时：每个身份独立 email（`e2e-<n>@moonbyte.test`）与独立用户；Mailpit 按 `To` 过滤；
  用 service-account token（600s）避免 60s 刷新；`DELETE /api/v1/messages` 可在每轮开始前清空。
- `bash tests/sandbox/verify.sh --count 5` 可直接跑通"建 5 个身份 → 各自验证邮箱 → 各自 OIDC+PKCE →
  userinfo 校验 → 删除"的完整序列；Go 侧实现可把它当参考行为（wire-level 断言）。

### 邮件发件人（供引擎 sender allowlist）

realm SMTP 的 `from` 与 SMTP envelope 发件人一致（真实抓包验证，`Return-Path` 头）：

| 项 | 值 |
|---|---|
| SMTP envelope / `Return-Path` | `no-reply@moonbyte.test` |
| `From` 头 | `"MoonByte Test (LOCAL SANDBOX)" <no-reply@moonbyte.test>` |
| `Reply-To` 头 | `MoonByte Test Support <support@moonbyte.test>` |
| 验证邮件主题（execute-actions-email） | `Update Your Account` |

## 7. 目录结构与运行状态

```
.sandbox/                      # gitignored，全部运行态在这里
├── dist/                      # 下载的 tar.gz（keycloak-26.8.1.tar.gz, mailpit-darwin-arm64.tar.gz）
├── keycloak/                  # 解压后的 Keycloak 发行版；运行时数据在 keycloak/data（H2 + import）
├── bin/mailpit               # Mailpit 可执行文件
├── data/mailpit/mailpit.db    # 邮件库（文件型）
├── logs/{keycloak,mailpit}.log
├── run/{keycloak,mailpit}.pid
├── secrets/sandbox.env        # 生成的测试凭据（600）
├── evidence/                  # 本任务的手工证据（up/down/status/verify 日志）
└── tmp/                       # 脚本中间产物（verify.sh 每次运行一个子目录）
```

PID 与停止方式：`cat .sandbox/run/*.pid`；停止用 `bash tests/sandbox/down.sh`
（脚本会校验命令行再发 `SIGTERM`，不会误杀 8080 上的其它进程）。

## 8. 故障排查

| 现象 | 处理 |
|---|---|
| `port 8180 is already in use by [...]` | 用 `lsof -nP -iTCP:8180 -sTCP:LISTEN` 查占用者；本沙箱的用 `down.sh` 停 |
| Keycloak 起不来 / `UnsupportedClassVersion` | 确认 `--version` 用的 JDK 21；`SB_JAVA_HOME` 可覆盖 |
| realm 不存在 / `emailVerified` 一直 false | `bash tests/sandbox/reset.sh` 重新导入（改了 realm JSON 必须 reset） |
| 收不到邮件 | `curl -sS http://127.0.0.1:8025/api/v1/messages`；再查 realm SMTP 是否 127.0.0.1:1025；`grep -i smtp .sandbox/logs/keycloak.log` |
| admin token 401/400 | 凭据在 `.sandbox/secrets/sandbox.env`；`reset.sh --full` 会重新生成（旧值失效） |
| service-account token 失败（`unauthorized_client`/400） | 跑一次 `bash tests/sandbox/up.sh`：它会重新固定 client secret 并补齐 realm-management 角色映射；仍失败则 `reset.sh` |
| `--count N` 有身份 FAIL | 看该行 `↳ detail`；常见原因：Mailpit 里同一邮箱旧邮件干扰（换 email 或 `DELETE /api/v1/messages`）、realm 被 reset 过 |
| 下载慢/失败 | GitHub Release 直连在本网络会卡在 ~142 KB；`up.sh` 已内置 gh-proxy 镜像与 Maven Central（阿里云镜像）回退，并校验 md5 |
| 想看 Keycloak 内部日志 | `tail -f .sandbox/logs/keycloak.log`（dev 模式，INFO） |
| 健康探针 404 | 健康接口在管理端口 `127.0.0.1:9000/health/ready`，不在 8180 |

## 9. 安全与边界

- 仅 loopback：脚本启动前会检查端口占用，启动后校验监听地址必须是 `127.0.0.1`（不是 `0.0.0.0`）。
- 仅测试数据：`moonbyte-test` realm、`onboarding-test` 客户端、随机生成的测试密码；禁止用于真实系统。
- 不提交凭据：`.sandbox/` 已被根 `.gitignore` 忽略；`tests/sandbox/**` 里没有任何密钥/密码。
- 不做的事：不使用 Docker、不 `git push`、不联系真实 Cloudflare/GitHub/WorkBuddy、不改生产配置。
- Keycloak dev 模式仅用于本地测试：无 HTTPS、H2 单机库、dev 内存缓存，不能照搬到生产。
