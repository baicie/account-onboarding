# R2-infra 验收报告 — 本地真实 Keycloak + Mailpit 沙箱（无 Docker）

- 报告时间：`2026-10-24T21:15+08:00`（首轮）／`2026-10-24T21:22+08:00`（补充 service-account 客户端与 5 身份门禁后更新；Asia/Shanghai，本机 CST）
- 任务：task-2（R2 infra: local Keycloak + Mailpit sandbox without Docker）
- 负责人：sandbox-infra
- 结论：**PASS** — 真实 Keycloak 26.8.1 与真实 Mailpit v1.32.0 已在 loopback 上运行；任务要求的
  (a) admin token、(b) Admin API 建用户、(c) 触发 VERIFY_EMAIL 邮件、(d) 从 Mailpit API 取信并提取链接、
  (e) 跟随链接确认 `emailVerified=true`、(f) OIDC Authorization Code + PKCE S256 换 token、
  (g) userinfo `sub` 与所建用户一致，**全部实机执行并通过**（单身份 16 项断言，0 失败，退出码 0），
  随后删除测试用户并重启 Keycloak 验证数据持久化。
  补充交付：confidential service-account 客户端 `onboarding-admin`（`client_credentials`，TTL 600s）
  已验证可建/删用户；`verify.sh --count 5` 五身份门禁 **5/5 PASS**（10 s）。
- 测试等级（按 `docs/11-testing-acceptance.md` 约定）：**LOCAL SANDBOX / LIVE（本机真实组件）**。
  真实调用对象仅限本机 `127.0.0.1` 上的 Keycloak 与 Mailpit；**未**调用真实 Cloudflare / GitHub /
  WorkBuddy / 任何生产服务；未使用任何生产凭据；未执行 `git push`；未使用 Docker。

> **LOCAL SANDBOX** —— 本报告中的端点、密码、令牌只存在于本机 `.sandbox/`（gitignored），
> 仅用于自动化测试，禁止用于任何真实系统。

---

## 1. 结论与交付物

| 交付物 | 路径 | 状态 |
|---|---|---|
| 沙箱脚本（启停/重置/状态/环境/自检/多身份） | `tests/sandbox/{up,down,reset,status,env,lib,verify}.sh` | PASS（可重复执行） |
| Realm 导入 JSON（两个客户端 + 15 个内置 client scope） | `tests/sandbox/realm/moonbyte-test-realm.json` | PASS |
| service-account 客户端 `onboarding-admin`（client_credentials，TTL 600s） | 由 `up.sh` 幂等创建/固定 secret/授权 | PASS（§7.1） |
| 五身份门禁 `verify.sh --count 5` | `tests/sandbox/verify.sh` | PASS 5/5（§7.2） |
| 使用说明（端点/凭据/Go 集成/发件人） | `tests/sandbox/README.md` | 完成 |
| 运行态（下载物、H2 库、邮件库、日志、凭据） | `.sandbox/`（gitignored） | 运行中 |
| 本报告 | `docs/acceptance/R2-infra.md` | 完成 |

未修改任何 Go 代码、`go.mod`/`go.sum`、`Makefile`、根 `.gitignore`、`docs/` 下他人文档；
写入范围严格限制在 `tests/sandbox/**`、`docs/acceptance/R2-infra.md`、`.sandbox/**`。

## 2. 环境

| 项 | 值 |
|---|---|
| 工作目录 | `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding` |
| 操作系统 / 架构 | macOS 15.7.7（Build 24G720），arm64 |
| JDK（Keycloak 用） | Homebrew OpenJDK `21.0.12.1` `/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home`（系统默认 java 为 25，未用于 Keycloak） |
| Keycloak | 26.8.1（Quarkus 3.40.1），`start-dev`，dev-file H2，dev 配置 |
| Mailpit | v1.32.0（go1.27.3 darwin/arm64），文件库 `bbolt` |
| 端口（仅 loopback） | `127.0.0.1:8180` Keycloak HTTP；`127.0.0.1:9000` Keycloak 管理/健康接口；`127.0.0.1:1025` Mailpit SMTP；`127.0.0.1:8025` Mailpit HTTP/API |
| 未触碰 | 其它项目的 `127.0.0.1:8080` / `8081` 监听 |
| 工具 | `curl 8.7.1`、`jq 1.7.1`、`openssl 3.3.6 (LibreSSL)`、`lsof 4.91`、`bash 3.2.57` |
| 网络 | 仅制品下载出网（GitHub Release / gh-proxy 镜像 / Maven Central 阿里云镜像）；沙箱运行期全 loopback |
| Docker | **未使用**（daemon 不可用，本方案不依赖 Docker） |

## 3. 制品获取与完整性

首次 `up.sh` 下载两件制品，全部落在 `.sandbox/dist/`，解压到 `.sandbox/`。

| 制品 | 版本/大小 | 来源（最终成功） | 校验 |
|---|---|---|---|
| Keycloak 发行版 | 26.8.1 / 166 MB | Maven Central（阿里云镜像）`https://maven.aliyun.com/repository/public/org/keycloak/keycloak-quarkus-dist/26.8.1/keycloak-quarkus-dist-26.8.1.tar.gz` | **md5 = `f2d35a0dfb22cf8a2e824a1cd39913b9`，与官方 GitHub Release 的 `.md5` 一致**；sha256 `4e2740b181faa2ea623a1c2ecf00855935311e546046e2f857615a47ed24cbe8` |
| Mailpit 二进制 | v1.32.0 / 9.5 MB | GitHub Release 经 `gh-proxy.com` 镜像 | 官方未发布 sha256 sidecar（404）；以 `tar -tzf` 完整性 + `mailpit version` 验证 |

下载失败与回退（真实记录，已写入 `up.sh` 的回退链，避免其他人再踩）：

1. `github.com/keycloak/...` 与 `github.com/axllent/...` 直连：稳定卡在 **142 KB** 后无速率，`curl` 退出码 **18**（partial file）。
2. `gh-proxy.com` 镜像：Mailpit 成功（2m08s）；Keycloak 仅 ~700 KB 后停滞，放弃。
3. Maven Central 直连 `repo1.maven.org`：约 8 KB/s（不可用）。
4. **阿里云 Maven 镜像：166 MB / 约 40 s（约 3 MB/s），且 md5 与官方 Release 完全一致 → 采用**。

`up.sh` 内置回退顺序：GitHub Release → gh-proxy 镜像 → Maven Central（阿里云镜像）→ Maven Central 官方；
下载后校验 md5（失败即删除并报错退出），解压前再做 `tar -tzf` 完整性检查。

## 4. Realm / 客户端配置（摘要）

`tests/sandbox/realm/moonbyte-test-realm.json` 由 `up.sh` 拷入 `.sandbox/keycloak/data/import/`，
Keycloak 以 `--import-realm` 首次启动导入。

| 配置 | 值 |
|---|---|
| realm | `moonbyte-test`（`verifyEmail=true`，`registrationAllowed=false`，`sslRequired=none`） |
| SMTP | host `127.0.0.1`、port `1025`、from `no-reply@moonbyte.test`、`auth=false`、`starttls=false`、`ssl=false` |
| SMTP 发件人（实测头部） | `Return-Path: <no-reply@moonbyte.test>`；`From: "MoonByte Test (LOCAL SANDBOX)" <no-reply@moonbyte.test>`；`Reply-To: MoonByte Test Support <support@moonbyte.test>`；验证邮件主题 `Update Your Account` |
| 客户端 `onboarding-test` | public（无 secret）、Standard Flow=true、**PKCE 强制 S256**（`pkce.code.challenge.method=S256`）、Direct Access Grants=true、Implicit Flow=false、Service Accounts=false |
| 客户端 `onboarding-admin` | **confidential**（secret 在 `.sandbox/secrets/sandbox.env`）、`serviceAccountsEnabled=true`、Standard Flow=false、Direct Access Grants=false、`access.token.lifespan=600` |
| `onboarding-admin` service-account 角色 | `realm-management` 的 `manage-users`、`view-users`、`query-users`、`view-realm`（由 `up.sh` 幂等授权） |
| redirectUris | `http://127.0.0.1:8788/oidc/callback`、`http://127.0.0.1:8788/ui/oauth/callback` |
| webOrigins | `http://127.0.0.1:8788` |
| 内置 client scope | 15 个（profile/email/roles/basic/web-origins/acr/offline_access/…），客户端默认挂 `web-origins acr profile roles basic email` |
| 用户 | 空（身份只能由 Admin API 创建，禁止注册） |

> 踩坑记录：若 realm JSON 不带内置 client scope 定义，导入后 realm 里只剩 `offline_access`，
> 客户端拿到 0 个默认 scope，**access token / userinfo 里没有 `email`、`preferred_username`**。
> 本 JSON 已补齐 15 个内置 scope（导出样式），导入后客户端默认 scope 与真实 realm 一致。

## 5. 实际执行的命令与退出码

所有命令在仓库根目录执行；证据文件见 §10。

| # | 命令 | 退出码 | 结果摘要 |
|---|---|---:|---|
| 1 | `bash tests/sandbox/up.sh`（首次，含下载+解压+启动+自检） | **0** | Keycloak 21.3 s 启动就绪；realm 导入成功；SMTP/client/PKCE/loopback 全部校验通过 |
| 2 | `bash tests/sandbox/status.sh` | **0** | 版本/监听/健康/admin token/realm/client 全部 `[ ok ]` |
| 3 | `bash tests/sandbox/verify.sh` | **0** | **16 项断言全 PASS，0 失败，7 s**（§6 逐步证据） |
| 4 | `bash tests/sandbox/up.sh`（服务已在运行，重复执行） | **0** | 幂等：识别已运行，仅重做配置校验；2 s |
| 5 | `bash tests/sandbox/down.sh` | **0** | 先停 Keycloak 再停 Mailpit；PID 与命令行校验后才发 SIGTERM；3 s |
| 6 | `bash tests/sandbox/down.sh`（已停止，重复执行） | **0** | 幂等："is not running"；1 s |
| 7 | `bash tests/sandbox/status.sh --no-token`（服务已停止） | **1** | 失败必须响亮：6 项 `[FAIL]`（8180/8025/1025 未监听、健康检查 `000`），符合预期 |
| 8 | `bash tests/sandbox/up.sh` | **0** | 数据级重启：`Realm 'moonbyte-test' already exists. Import skipped`；14 s |
| 9 | `bash tests/sandbox/reset.sh` | **0** | 清运行时数据 + 重新导入 realm + 重启；28 s |
| 10 | `bash tests/sandbox/verify.sh`（reset 之后） | **0** | 16 项断言全 PASS（全新数据库）；7 s |
| 11 | 手工：建用户 → 发验证邮件 → 点两步链接 → `down.sh` → `up.sh` → 复查 | **0** | 重启后用户仍在且 `emailVerified=true`；realm/client 配置仍在；随后 DELETE 204 / GET 404 |
| 12 | `bash tests/sandbox/status.sh`（最终状态） | **0** | 见 §7 |
| 13 | `bash tests/sandbox/reset.sh`（新增 `onboarding-admin` 客户端后重新导入 + 授权） | **0** | 30 s；`client_credentials -> GET /admin/realms/moonbyte-test/users 200`；service-account 角色 `manage-users,query-users,view-realm,view-users` |
| 14 | service-account 证据脚本（`client_credentials` → GET/POST/DELETE 用户） | **0** | token 1405 字符、`azp=onboarding-admin`、`exp_delta=599s`；GET users 200、POST 201、DELETE 204、复查 404（§7.1） |
| 15 | `bash tests/sandbox/status.sh`（含 service-account 检查） | **0** | 新增两条 `[ ok ]`：admin client 存在、client_credentials 可列用户（§7.1） |
| 16 | `bash tests/sandbox/verify.sh --count 5`（五身份门禁） | **0** | **5/5 PASS**，10 s；每行 create 201 / mail ok / verified true / token 200 / userinfo ok / delete 204（§7.2） |
| 17 | `bash tests/sandbox/verify.sh`（单身份回归，多身份改动后） | **0** | 16 项断言全 PASS，7 s；realm 内无残留用户 |
| 18 | `bash tests/sandbox/up.sh`（已运行 + 新增 service-account 步骤） | **0** | 幂等：1 s，`admin service account 'onboarding-admin' ready` |

## 6. 验证流程（任务要求的 a–g，逐步证据）

证据来源：`.sandbox/evidence/G-verify.log`（reset 后全新库的一次完整运行）与 `.sandbox/logs/verify-20261024-211231.log`。
下列输出均为真实终端输出，仅对令牌/密码/action-token JWT 做脱敏。

### 6.0 预检（服务与 realm）

```console
$ curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8180/realms/moonbyte-test/.well-known/openid-configuration
-> HTTP 200
$ curl -s -o /dev/null -w '%{http_code}' 'http://127.0.0.1:8025/api/v1/messages?limit=1'
-> HTTP 200
```

### (a) admin token —— master / admin-cli / password grant

```console
$ curl -sS -X POST http://127.0.0.1:8180/realms/master/protocol/openid-connect/token \
    -d grant_type=password -d client_id=admin-cli \
    -d username=$SANDBOX_ADMIN_USER --data-urlencode password=$SANDBOX_ADMIN_PASSWORD
-> HTTP 200
token_type=Bearer expires_in=60s scope=email profile refresh_expires_in=1800s
access_token: eyJhbGciOiJS…(750 chars, redacted)
[ PASS ] admin token acquired for 'kcadmin' (master realm, realm-management rights)
```

### (b) Admin API 创建用户（emailVerified=false）

```console
$ curl -sS -X POST http://127.0.0.1:8180/admin/realms/moonbyte-test/users \
    -H 'Authorization: Bearer <admin>' -H 'Content-Type: application/json' \
    -d '{"username":"e2e-…","email":"e2e.…@moonbyte.test","enabled":true,"emailVerified":false,
         "attributes":{"sandbox":["local-e2e"]},
         "credentials":[{"type":"password","value":"<redacted>","temporary":false}]}'
-> HTTP 201 (201 = created)

$ curl -sS -H 'Authorization: Bearer <admin>' \
    'http://127.0.0.1:8180/admin/realms/moonbyte-test/users?username=e2e-…&exact=true'
id=3594576b-73b0-4ce5-a186-b1d8a9792117
username=e2e-1792847551
email=e2e.1792847551@moonbyte.test
enabled=true
emailVerified=false
requiredActions=
[ PASS ] user id=3594576b-…, emailVerified=false (as expected before verification)
```

### (c) 触发 VERIFY_EMAIL 邮件

```console
$ curl -sS -X PUT 'http://127.0.0.1:8180/admin/realms/moonbyte-test/users/3594576b-…/execute-actions-email?client_id=onboarding-test&lifespan=3600' \
    -H 'Authorization: Bearer <admin>' -H 'Content-Type: application/json' -d '["VERIFY_EMAIL"]'
-> HTTP 204 (204 = email accepted for delivery)
[ PASS ] verification email requested (HTTP 204)
```

### (d) Mailpit API 收信并提取验证链接

```console
$ curl -sS 'http://127.0.0.1:8025/api/v1/messages?limit=100'
-> polled 1 time(s), matched message id: 3rQPMSJtH6AxZx6xsEJcZv
From: MoonByte Test (LOCAL SANDBOX) <no-reply@moonbyte.test>
To: e2e.1792847551@moonbyte.test
Subject: Update Your Account
Size: 3499 bytes
verification link (JWT redacted):
http://127.0.0.1:8180/realms/moonbyte-test/login-actions/action-token?key=<JWT-REDACTED>
[ PASS ] email contains a Keycloak action-token link (VERIFY_EMAIL)
```

### (e) 跟随链接 → `emailVerified=true`（注意：链接是两步）

第一步 GET 只返回确认页；第二步 GET 页面里的 `href` 才真正执行验证：

```console
$ curl -sS -c jar -b jar -L 'http://127.0.0.1:8180/realms/moonbyte-test/login-actions/action-token?key=<JWT-REDACTED>'
-> final HTTP 200
-> final page title: Sign in to MoonByte Test Realm (LOCAL SANDBOX — NOT PRODUCTION)
-> interstitial confirmation page detected: performing the "Click here to proceed" step
$ curl -sS -c jar -b jar -L 'http://127.0.0.1:8180/realms/moonbyte-test/login-actions/action-token?key=<JWT-REDACTED>&client_id=onboarding-test&tab_id=…&client_data=…'
-> HTTP 200

admin API: emailVerified=true, requiredActions=
[ PASS ] emailVerified=true confirmed via Admin API
```

> 浏览器中这一步是用户点击"Click here to proceed"；自动化 E2E 必须模拟第二次 GET。
> 页面文案为 `Perform the following action(s): Verify Email » Click here to proceed`。

### (f) OIDC Authorization Code + PKCE S256（curl + cookie jar）

```console
code_verifier  : SVFQtsjoYQBiTl5QUQ3ai3bJivU-K-INhU8f5DKPFqsbpPmTPrTj2wJPTxpXlgKBfT7Jqgbeju6bj429
code_challenge : h4CWfnDmZNF3CIYgtYLiahB0P5RMA3Qe3v_ZuQR5vUw  (base64url(sha256(verifier)), method=S256)
state          : a03afd36c4fd1f5bc3f7cb89
nonce          : 47f1d39517ac3a51452c9bf9

$ curl -sS -c jar -b jar -G 'http://127.0.0.1:8180/realms/moonbyte-test/protocol/openid-connect/auth' \
    --data-urlencode client_id=onboarding-test --data-urlencode response_type=code \
    --data-urlencode 'scope=openid profile email' --data-urlencode redirect_uri=http://127.0.0.1:8788/oidc/callback \
    --data-urlencode state=… --data-urlencode nonce=… \
    --data-urlencode code_challenge=… --data-urlencode code_challenge_method=S256
-> HTTP 200
-> login page title: Sign in to MoonByte Test Realm (LOCAL SANDBOX — NOT PRODUCTION)
-> cookie jar: 6 line(s) (AUTH_SESSION_ID set)
login form action: http://127.0.0.1:8180/realms/moonbyte-test/login-actions/authenticate?session_code=<REDACTED>&execution=<REDACTED>&client_id=onboarding-test&tab_id=…

$ curl -sS -c jar -b jar -X POST '<login form action>' \
    --data-urlencode username=e2e.…@moonbyte.test --data-urlencode password=<redacted> --data-urlencode credentialId=
-> HTTP 302 (302 = credentials accepted, redirect to the callback)
redirect: http://127.0.0.1:8788/oidc/callback?state=a03afd36c4fd1f5bc3f7cb89&session_state=…&iss=…&code=<CODE-REDACTED>
authorization code: 611c35eb-7e2…(98 chars, redacted)
[ PASS ] authorization code issued, state echoed back correctly

$ curl -sS -X POST http://127.0.0.1:8180/realms/moonbyte-test/protocol/openid-connect/token \
    -d grant_type=authorization_code -d code=<code> \
    -d redirect_uri=http://127.0.0.1:8788/oidc/callback -d client_id=onboarding-test \
    -d code_verifier=<verifier>
-> HTTP 200
token_type=Bearer
expires_in=300s
scope=openid email profile
access_token: eyJhbGciOiJS…(1383 chars, redacted)
id_token    : eyJhbGciOiJS…(1167 chars, redacted)
access token claims: {"iss":"http://127.0.0.1:8180/realms/moonbyte-test","aud":"account","azp":"onboarding-test",
                     "typ":"Bearer","scope":"openid email profile","preferred_username":"e2e-…",
                     "email":"e2e.…@moonbyte.test","email_verified":true}
id_token claims:     {"iss":"…/realms/moonbyte-test","aud":"onboarding-test","azp":"onboarding-test",
                     "sub":"3594576b-73b0-4ce5-a186-b1d8a9792117","nonce":"47f1d39517ac3a51452c9bf9", …}
[ PASS ] token response 200 (Bearer, 300s), id_token nonce matches
```

PKCE 负向校验（证明 S256 真的生效、code 一次性）：

```console
$ # 新 code + 错误 code_verifier
-> HTTP 400 {"error":"invalid_grant","error_description":"PKCE verification failed: Code mismatch"}
[ PASS ] invalid code_verifier rejected (HTTP 400 invalid_grant, PKCE S256 enforced)

$ # 重放已用过的 code
-> HTTP 400 {"error":"invalid_grant","error_description":"Code not valid"}
[ PASS ] replayed authorization code rejected (single-use, HTTP 400 invalid_grant)

$ # 授权请求不带 code_challenge
-> HTTP 302
-> redirect: http://127.0.0.1:8788/oidc/callback?error=invalid_request&error_description=Missing+parameter%3A+code_challenge_method&…
[ PASS ] authorization request without code_challenge rejected (PKCE required)
```

### (g) userinfo：`sub` 与所建用户一致

```console
$ curl -sS -H 'Authorization: Bearer <access_token>' \
    http://127.0.0.1:8180/realms/moonbyte-test/protocol/openid-connect/userinfo
-> HTTP 200
{
  "sub": "3594576b-73b0-4ce5-a186-b1d8a9792117",
  "preferred_username": "e2e-1792847551",
  "email": "e2e.1792847551@moonbyte.test",
  "email_verified": true,
  "given_name": "E2E",
  "family_name": "Sandbox",
  "name": "E2E Sandbox"
}
[ PASS ] userinfo sub == created Keycloak user id (3594576b-…), email=…, email_verified=true
```

### (h) 收尾删除用户

```console
$ curl -sS -X DELETE http://127.0.0.1:8180/admin/realms/moonbyte-test/users/3594576b-73b0-4ce5-a186-b1d8a9792117
-> HTTP 204 (204 = deleted)
-> follow-up GET user: HTTP 404 (404 = gone)
[ PASS ] test user deleted (DELETE 204, subsequent GET 404)

VERIFICATION PASSED — 16 checks, 0 failed, 7s
```

## 7. 重启持久化与最终状态

在 `down.sh` + `up.sh` 前后各查一次同一个已验用户（`.sandbox/evidence/H-durability-*.log`）：

```console
=== BEFORE restart ===
{"username":"durability-probe","email":"durability.…@moonbyte.test","emailVerified":true,"enabled":true,"requiredActions":[]}

=== restart: down.sh → up.sh ===
[   ok  ] keycloak stopped
[   ok  ] mailpit stopped
[   ok  ] keycloak already installed: …/.sandbox/keycloak (26.8.1)
[   ok  ] keycloak ready (pid 61689, log …/.sandbox/logs/keycloak.log)
[   ok  ] LOCAL SANDBOX IS UP

=== AFTER restart ===
{"username":"durability-probe","email":"durability.…@moonbyte.test","emailVerified":true,"enabled":true,
 "createdTimestamp":1792847570568}
{"realm":"moonbyte-test","verifyEmail":true,"registrationAllowed":false,"smtpHost":"127.0.0.1","smtpPort":"1025"}
{"clientId":"onboarding-test","publicClient":true,"pkce":"S256",
 "redirectUris":["http://127.0.0.1:8788/oidc/callback","http://127.0.0.1:8788/ui/oauth/callback"]}

=== teardown ===
DELETE probe user -> 204
GET probe user after delete -> 404
```

最终 `status.sh`（退出码 **0**）：

```console
------------------------------------------------------------------------
versions / paths
[ ok ] jq jq-1.7.1-apple
[ ok ] mailpit v1.32.0 … -> .sandbox/bin/mailpit
[ ok ] keycloak distribution Keycloak 26.8.1 -> .sandbox/keycloak
[ ok ] java openjdk version "21.0.12.1" 2026-08-18 (/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home)
------------------------------------------------------------------------
processes / listeners (loopback only)
[ ok ] keycloak listening on 127.0.0.1:8180 (pid 61689)
[ ok ] keycloak-mgmt listening on 127.0.0.1:9000 (pid 61689)
[ ok ] mailpit-web listening on 127.0.0.1:8025 (pid 61529)
[ ok ] mailpit-smtp listening on 127.0.0.1:1025 (pid 61529)
------------------------------------------------------------------------
HTTP health
[ ok ] mailpit API http://127.0.0.1:8025/api/v1 200 (messages stored: 2)
[ ok ] keycloak master realm http://127.0.0.1:8180/realms/master 200
[ ok ] keycloak /health/ready 200 on management interface http://127.0.0.1:9000
[ ok ] realm moonbyte-test OIDC discovery 200 (issuer http://127.0.0.1:8180/realms/moonbyte-test)
------------------------------------------------------------------------
admin API (test-only credentials from .sandbox/secrets/sandbox.env)
[ ok ] admin token acquired for user 'kcadmin'
[ ok ] realm moonbyte-test present (registrationAllowed=false, verifyEmail=true)
[ ok ] realm SMTP -> 127.0.0.1:1025 from=no-reply@moonbyte.test
[ ok ] realm users: 0
[ ok ] client onboarding-test present (public=true, pkce=S256, directGrant=true)
------------------------------------------------------------------------
[ ok ] LOCAL SANDBOX healthy: keycloak http://127.0.0.1:8180 (realm moonbyte-test) + mailpit http://127.0.0.1:8025
```

监听确认（`lsof -nP -iTCP -sTCP:LISTEN` 摘录）：所有 4 个监听都在 `127.0.0.1`，
无 `0.0.0.0`/`[::]` 绑定；`8080`（其它项目）未被本任务触碰。

### 7.1 service-account 客户端（client_credentials，供自动化 Admin API）

`up.sh` 每次启动都会幂等执行：确认 `onboarding-admin` 存在 → 把 `.sandbox/secrets/sandbox.env`
里的 `SANDBOX_ADMIN_CLIENT_SECRET` 固定写入客户端 → 给 service-account 用户挂
`realm-management` 的 `manage-users / view-users / query-users / view-realm` → 用 client_credentials
换 token 并真实调用一次 `GET /admin/realms/moonbyte-test/users` 验证。

`reset.sh`（重新导入 realm）后的实测输出（`.sandbox/evidence/J-reset-with-admin-client.log`）：

```console
[   ok  ] added service-account credentials to .sandbox/secrets/sandbox.env (chmod 600)
[   ok  ] admin service account 'onboarding-admin' ready (client_credentials -> GET /admin/realms/moonbyte-test/users 200)
[   ok  ] admin client 'onboarding-admin': confidential, service account roles -> manage-users,query-users,view-realm,view-users
[   ok  ] client_credentials token for 'onboarding-admin' works (Admin API access verified)
```

独立证据脚本（`.sandbox/evidence/K-service-account-evidence.log`）——用 client_credentials token
（**不是** master 管理员 password grant）完成 list/create/delete：

```console
=== K1. client_credentials token (service-account client 'onboarding-admin') ===
token length: 1405 chars
{"azp":"onboarding-admin","typ":"Bearer","iss":"http://127.0.0.1:8180/realms/moonbyte-test","exp_delta_s":599}
=== K2. GET users with service-account token ===
GET users -> 200
users currently: 0
=== K3. POST create throwaway user ===
POST users -> 201
created id=0cffca24-32c8-48f4-82ef-5ba6709c167d
GET user: {"username":"svc-probe","email":"svc-probe.1792848040@moonbyte.test","emailVerified":false,"enabled":true}
=== K4. DELETE throwaway user ===
DELETE users/{id} -> 204
GET users/{id} after delete -> 404
```

`status.sh` 中的对应检查（`.sandbox/evidence/L-status-with-service-account.log`，退出码 0）：

```console
[ ok ] admin client onboarding-admin present (confidential=true, serviceAccount=true)
[ ok ] client_credentials token works: GET /admin/realms/moonbyte-test/users 200 (no 60s TTL churn)
```

> token 有效期实测 599 s（客户端 `access.token.lifespan=600`），`azp=onboarding-admin`；
> 5 身份 E2E 用它可完全避免 master 管理员 60 s TTL 的反复刷新。
> secret 在 `.sandbox/secrets/sandbox.env`（600），`tests/sandbox/**` 与文档中均无明文 secret。

### 7.2 五身份门禁（`verify.sh --count 5`）

命令：`bash tests/sandbox/verify.sh --count 5`，退出码 **0**，耗时 **10 s**，
transcript `.sandbox/logs/verify-20261024-212045.log`，摘要 `.sandbox/evidence/M-verify-count5.log`：

```console
MULTI-IDENTITY E2E — 5 identities (LOCAL SANDBOX, realm moonbyte-test)
#   email                                     create  mail   verified  token  userinfo  delete  result
1   e2e-m1.1792848045@moonbyte.test           201     ok     true      200    ok        204     PASS
2   e2e-m2.1792848052@moonbyte.test           201     ok     true      200    ok        204     PASS
3   e2e-m3.1792848053@moonbyte.test           201     ok     true      200    ok        204     PASS
4   e2e-m4.1792848053@moonbyte.test           201     ok     true      200    ok        204     PASS
5   e2e-m5.1792848054@moonbyte.test           201     ok     true      200    ok        204     PASS
------------------------------------------------------------------------
mailpit messages stored: 5   transcript: .sandbox/logs/verify-20261024-212045.log
MULTI-IDENTITY RESULT: PASS — 5/5 identities created, email-verified, OIDC+PKCE token obtained, userinfo matched, deleted
```

每个身份都完整执行了与 §6 相同的链路：Admin API 建用户（201）→ `execute-actions-email`（204）→
Mailpit 收信并按 `To` 精确匹配 → 两步 action-token 点击 → Admin API `emailVerified=true` →
OIDC Authorization Code + PKCE S256 → token 200 → userinfo `sub` 与用户 id 一致 → DELETE 204。
任一步失败该行记为 FAIL、打印 `↳ detail` 并以非 0 退出。

复跑（最终状态，21:24）：`bash tests/sandbox/verify.sh --count 5` 同样 **5/5 PASS（9 s）**，
transcript `.sandbox/logs/verify-20261024-212417.log`、摘要 `.sandbox/evidence/Q-verify-count5-final.log`；
跑完后用 Admin API 查 realm 用户为 `[]`（无残留）。

## 8. 已知限制

1. **Keycloak dev 模式**：`start-dev` + dev-file H2 + 本地缓存，无 HTTPS、无集群、无生产加固；
   仅用于本地测试，不能作为生产部署方案参考。
2. **额外占用 `127.0.0.1:9000`**：Keycloak 26 的 Quarkus 管理接口（`/health/ready`、`/health/live`）默认启用。
   也是 loopback。若与其他本地服务冲突，可改为其它 loopback 端口（`--http-management-port`，脚本内一处配置）或关闭健康探针。
3. **首次需下载 175 MB**（Keycloak 166 MB + Mailpit 9.5 MB）。本网络直连 GitHub Release 会卡在 142 KB，
   已内置 gh-proxy / Maven 阿里云镜像回退；下载失败的完整现象见 §3。
4. **Keycloak 首次启动约 21 s**（JVM + H2 + realm 导入）；`reset.sh` 整体约 28–33 s。
5. **admin token 两种 TTL**：master/admin-cli password grant 只有 **60 s**；service-account
   `onboarding-admin` 的 client_credentials 是 **600 s**（该客户端 `access.token.lifespan=600`）。
   自动化/CI 建议优先用 service-account，避免 60 s 刷新。
6. **master 引导管理员为"temporary admin"**（Keycloak 26 启动日志如此描述）：Admin API 的 password grant
   正常可用；若用浏览器登录 admin console 可能被要求改密——本沙箱不依赖 console，故不影响。
7. **邮件验证链接两步**（见 §6 e）：若 E2E 只 GET 一次，`emailVerified` 仍为 `false`，容易误判为 bug。
8. **没有 HTTPS / TLS**：SMTP 明文、HTTP 明文，仅限 loopback；不要改成 `0.0.0.0`。
9. **realm 配置变更需要 `reset.sh`**：`--import-realm` 对已存在的 realm 会跳过（日志
   `Realm 'moonbyte-test' already exists. Import skipped`）；唯二例外是 `onboarding-admin` 的
   secret 与角色授权，这两项由 `up.sh` 通过 Admin API 幂等 reconcile（所以旧 realm 也能补上代码后新增的客户端）。
10. **service-account client secret 是本地生成的随机值**：保存在 `.sandbox/secrets/sandbox.env`（600），
    `up.sh` 每次启动把它固定写回 Keycloak；`reset.sh --full` 会重新生成（旧 secret 立即失效）。
    不要把 secret 写进任何提交文件。
11. **Mailpit 单实例、默认上限 500 封**（`--max` 未调）；`verify.sh` 每轮结束不清空邮箱（便于取证），
    5 身份测试建议每轮先 `DELETE /api/v1/messages`（`--count` 模式按唯一 email 过滤，不依赖清空）。
12. **未提供 Windows/Linux 启动脚本**：`up.sh` 的 Mailpit 制品名写死 `darwin-arm64`（可用 `MP_PLATFORM` 覆盖，
    但未在其它平台验证）。
13. **`--count` 模式串行执行**：5 个身份依次创建/验证（实测 10 s），没有并发压测；如需并发需自行控制
    邮件匹配与 token 刷新节奏。

## 9. 未完成项与后续建议

| 项 | 状态 | 说明 |
|---|---|---|
| 5 身份自动化 E2E（R2 正式门禁的 Go 实现） | 由 Lead / R2 E2E agent 实施 | 沙箱侧已提供 `verify.sh --count 5`（**5/5 PASS**）作为 wire-level 参考；Go 测试按 `tests/sandbox/README.md` §6 表格实现，每个身份独立 email，Mailpit 按 `To` 过滤 |
| `verify.sh --count N` | **已完成** | 本报告 §7.2 为 `--count 5` 证据；任一身份失败即退出非 0 |
| service-account / client_credentials 客户端 | **已完成** | `onboarding-admin`，见 §4/§7.1 |
| 把 `up.sh/status.sh/verify.sh` 接入 `make test-e2e` 或 CI | 未实现 | 未修改 `Makefile`（不在写入范围）；命令可直接被 CI 调用 |
| R1 的 Mailpit 适配器对真实 Mailpit 的 live 测试 | 本任务未执行 | 沙箱已提供真实 Mailpit（`http://127.0.0.1:8025`），R1 可据此补 live 用例 |
| Admin API 增量 reconcile（改 realm JSON 免 reset） | 未实现（service-account 例外） | 其余配置变更仍用 `reset.sh` 全量重建 |

## 10. 证据文件索引（本机，gitignored）

| 文件 | 内容 |
|---|---|
| `.sandbox/evidence/A-up-idempotent.log` | 服务已运行时重复 `up.sh`（幂等） |
| `.sandbox/evidence/B-down.log` / `C-down-again.log` | 正常停止 / 重复停止（幂等） |
| `.sandbox/evidence/D-status-down.log` | 停止状态下的 `status.sh`（6 项 FAIL，退出码 1） |
| `.sandbox/evidence/E-up.log` / `E-keycloak-import-skipped.log` | 数据级重启 + `Import skipped` 日志 |
| `.sandbox/evidence/F-reset.log` | `reset.sh` 全流程（清库 + 重新导入 + 自检） |
| `.sandbox/evidence/G-verify.log` | reset 后 16 项断言全 PASS 的完整输出 |
| `.sandbox/evidence/H-durability-before.log` / `H-durability-after.log` | 重启前后用户/realm/client 持久化 |
| `.sandbox/evidence/I-status.log` | 最终 `status.sh` 输出（首轮） |
| `.sandbox/evidence/J-reset-with-admin-client.log` | 新增 `onboarding-admin` 后的 `reset.sh`（含 service-account 授权） |
| `.sandbox/evidence/K-service-account-evidence.log` | client_credentials token + GET/POST/DELETE 用户 |
| `.sandbox/evidence/L-status-with-service-account.log` | `status.sh`（含 service-account 两条新检查） |
| `.sandbox/evidence/M-verify-count5.log` | `verify.sh --count 5` 五身份 PASS 表格 |
| `.sandbox/evidence/N-verify-detailed.log` | 多身份改动后的单身份 16 项断言回归 |
| `.sandbox/evidence/O-up-idempotent-with-sa.log` | 已运行时 `up.sh`（含 service-account reconcile，1 s） |
| `.sandbox/evidence/P-status-final.log` | 最终 `status.sh`（service-account 检查通过，退出码 0） |
| `.sandbox/evidence/Q-verify-count5-final.log` | 五身份门禁复跑 5/5 PASS（9 s，realm 无残留） |
| `.sandbox/logs/verify-*.log` | `verify.sh` 自身写的 transcript（含命令与输出） |
| `.sandbox/logs/keycloak.log` / `mailpit.log` | 服务端日志（含启动、realm 导入、SMTP 投递） |
| `.sandbox/secrets/sandbox.env` | 生成的测试凭据（600，勿外传） |

## 11. 一键复现

```bash
cd /Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding

bash tests/sandbox/up.sh              # 安装（首次）+ 启动 + service-account 授权 + 自检
bash tests/sandbox/status.sh          # 只读健康检查，退出码 0
bash tests/sandbox/verify.sh          # 单身份 16 项断言全链路验证，退出码 0
bash tests/sandbox/verify.sh --count 5 # 五身份门禁，打印 PASS 表格，退出码 0
bash tests/sandbox/down.sh            # 停止（数据保留）
bash tests/sandbox/reset.sh           # 清数据 + 重新导入 realm + 启动
```

`source tests/sandbox/env.sh` 后即可获得全部端点与测试凭据（含 `SANDBOX_ADMIN_CLIENT_ID/SECRET`
与 `client_credentials_token` 助手），详见 `tests/sandbox/README.md`。
