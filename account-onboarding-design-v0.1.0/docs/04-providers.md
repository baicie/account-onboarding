# 04 Provider 契约及适配层

## 核心原则

- Provider 可以被 Fake 替换；每个 Adapter 同时具备 unit/contract/live-integration 三层测试。
- 请求必须使用 `context.Context`，统一超时和取消机制。
- Provider 返回**领域类型和错误类别**，HTTP 状态、Cookie、JSON 特殊字段留在 adapter 内。
- 凭据不能从 Adapter 的对外公开 DTO 返回。必要时使用 opaque secret reference，例如 `secret://mailbox/{id}`。
- 每个第三方 Provider 在初始化时执行 allowlist 检查，URL 必须固定，防 SSRF；严禁由用户任意输入 Worker/SMTP/OAuth 端点。

## Go 契约草案

```go
type Mailbox struct {
    ID       string
    Address  string
    SecretRef string // address JWT: reference only
}
type Message struct {
    ID        string
    Recipient string
    Sender    string
    Subject   string
    ReceivedAt time.Time
    Text      string // transient only, never log/persist by default
}
type MailboxProvider interface {
    CreateAddress(ctx context.Context, key, name, domain string) (Mailbox, error)
    ListMessages(ctx context.Context, mailbox Mailbox, since time.Time) ([]Message, error)
    GetMessage(ctx context.Context, mailbox Mailbox, id string) (Message, error)
}

type IdentityProvider interface {
    RequestVerification(ctx context.Context, subjectID, address, idempotencyKey string) error
    CheckVerification(ctx context.Context, subjectID, address string) (VerificationStatus, error)
}

type OAuthProvider interface {
    Start(ctx context.Context, subjectID string) (AuthorizationSession, error)
    Poll(ctx context.Context, sessionRef string) (AuthorizationStatus, error)
}

type CredentialVerifier interface {
    Verify(ctx context.Context, credentialRef string) (CredentialStatus, error)
}
```

## 错误类别与动作

| Type | 示例 | Engine 动作 |
|---|---|---|
| `VALIDATION` | 不允许的邮箱域名 | FAILED_TERMINAL |
| `NOT_AUTHORIZED` | 无写权限/平台未许可 | FAILED_TERMINAL |
| `AUTH_REQUIRED` | OAuth 需要本人同意 | WAITING_USER_ACTION |
| `AUTH_EXPIRED` | 远端授权 state 410 | REAUTHORIZE_REQUIRED |
| `RATE_LIMITED` | HTTP 429 | RETRY_WAIT（遵守 Retry-After） |
| `UPSTREAM_TRANSIENT` | 502/503 | RETRY_WAIT（有上限） |
| `TIMEOUT_UNCERTAIN` | 请求发出但结果未知 | RECONCILE，必要时人工处理 |
| `PROVIDER_BUG` | Schema 不匹配 | FAILED_TERMINAL + 诊断事件 |
| `CANCELLED` | 用户取消 | CANCELLED |

## Cloudflare Temp Email Provider

实际已核对的文档入口：
- `POST /admin/new_address`，Header `x-admin-auth`，Body `name`, `domain`, `enablePrefix`，返回 `address`, `address_id`, `jwt`。
- `GET /api/parsed_mails`，Header `Authorization: Bearer <address JWT>`，返回对象字段以部署版本契约测试为准。
- 如果 Worker 启用私有站点密码 `PASSWORDS`，后台 API 还可能需要 `x-custom-auth`；不能只配置其中一个后绕过全站认证。

Adapter 必须检查服务域名允许为 `https://api.moonbyte.top`；在本地测试下另配置 Fake endpoint。管理员 JWT / 密码与普通地址 JWT 分离，权限最小化。

## GitHub Provider（已有账号）

仅使用官方 REST：`POST /user/emails`（添加本人邮箱）、`GET /user/emails`（获取 `verified` 状态）。访问令牌由 GitHub 官方授权获得，需要合适的 Email addresses 读/写权限；新增邮件验证仍需账号持有人同意。

不得以注册新账户替换该契约；不实现验证码绕过、自动同意 WorkBuddy 三方授权或 CAPTCHA 服务接入。

## Sandbox Keycloak Provider

- 本地 Realm `moonbyte-test`，测试用户由 Admin API / 测试表单创建。
- 验证邮件用 Keycloak SMTP -> Mailpit 收件，浏览器在本地完成验证链接点击。
- OAuth 采用授权码 + PKCE；测试账号不与真实 GitHub/WorkBuddy 混用。
- 每轮测试用不同 Realm/test-run namespace，并在 teardown 中移除，避免五身份测试重复累积。

## WorkBuddy Provider（权限门）

参考真实现有 Go 网关端点：
- `POST /panel/api/login/start`，请求 `{"realm":"global"}`，返回 `url`,`state`。
- `GET /panel/api/login/poll?state=...`，有 `done=false` 等待状态，授权完成时原网关内部执行 `SaveAtomic()`，并热加载账号。
- 控制面要求原网关面板 API 鉴权；**绝不从授权回调抓取 / 提取网页登录 token**。
- 适配器默认 `enabled=false`；获得必要书面许可前，真实调用标记 `NOT_AUTHORIZED`。
- `state` 属于原网关短期内存会话（源码 `loginTTL=15min`），不能跨原网关重启持久化复用；410 映射 `AUTH_EXPIRED`。

## Provider 版本与兼容

每个 Adapter 返回 `name`, `contractVersion`, `capabilities`, `health`, `restrictionReason`。任务创建时锁定 provider contract version；版本变更后旧任务若不能兼容则进入明确迁移/人工状态，不能偷偷接管。
