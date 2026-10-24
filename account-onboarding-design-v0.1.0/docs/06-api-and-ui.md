# 06 控制面 API 和 UI

## API（详情见 OpenAPI）

| HTTP | Endpoint | 作用 |
|---|---|---|
| `GET` | `/healthz` | 无身份敏感信息的 liveness |
| `GET` | `/readyz` | 私有 readiness（DB/migration/SecretStore） |
| `POST` | `/v1/runs` | 创建运行，必须有幂等键 |
| `GET` | `/v1/runs` | 分页列出运行，可过滤状态 |
| `GET` | `/v1/runs/{id}` | 查询状态和进度摘要 |
| `GET` | `/v1/runs/{id}/events` | 分页读取脱敏事件 |
| `POST` | `/v1/runs/{id}/resume` | 人工确认前置条件后恢复 |
| `POST` | `/v1/runs/{id}/cancel` | 请求取消，不能假定远端回滚 |
| `GET` | `/v1/providers` | 列出可用 Provider及权限状态 |

接口认证：Bearer admin API Token（使用恒定时间比较）或与本机代理集成的认证；浏览器 UI 使用 `HttpOnly + Secure + SameSite=Strict` session cookie，并校验 Origin + CSRF token。部署若只有 token 模式，静态 WebUI 不得通过 localStorage 持久化高权限 Token。

## POST /v1/runs

```json
{
  "template":"sandbox-email-verify-oidc",
  "mode":"sandbox-e2e",
  "mailbox_provider":"mailpit",
  "identity_provider":"keycloak-test",
  "oauth_provider":"oidc-test",
  "idempotency_key":"test-suite-20261024-001"
}
```

响应：`202 Accepted`，`{ "id":"...", "status":"CREATED", "links":{"self":"/v1/runs/..."} }`。

`real-account` 只有已有合法身份 `existing_identity_ref`，严禁支持自动 `account_count` 或生成第三方新账号。

## UI 信息架构

1. Dashboard：近 24h 成功/失败/等待人工操作数；Provider 可用状态。
2. Runs：状态列表，按模板/状态筛选，创建任务、取消。
3. Run Detail：事件时间线、当前步骤、失败分类、重试时刻、操作入口。
4. Settings：只显示 secret 的配置状态和最近验证时间；绝不回显密钥。
5. Provider Health：Cloudflare / Keycloak / GitHub 官方 API / WorkBuddy adapter（disabled by default）。

## 防误操作 UX

- 用户点击 `resume` 必须明确显示「即将验证哪个已有身份」；操作不可用时说明原因。
- 无权限或需书面许可时按钮 `disabled`，不自动降级为无授权的接口抓包。
- UI 上不存在“批量注册 5 个 GitHub/WorkBuddy 免费账号”的按钮。五身份用于 Sandbox 测试。
- 在人类登录之前，不从系统生成的邮箱里自动提取 GitHub 新注册验证码提交到 GitHub。
