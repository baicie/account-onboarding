# 07 安全、隐私与平台边界

## 威胁模型

| 资产 | 威胁 | 控制 |
|---|---|---|
| Cloudflare Admin 密码 | 管理 API 被恶意调用，可创建/操作邮箱 | SecretStore，IP 限制，独立 adapter credential，速率限制 |
| Address JWT | 任意读取特定邮箱邮件 | 密文存储，最短保留期限，禁止日志输出 |
| GitHub 授权令牌 | 修改/读取本人 GitHub 账号信息 | 最小 scopes、令牌轮换、现有账号のみ |
| OAuth 授权 state | CSRF/replay/state 泄露 | 随机 state，TTL，single-use，secret ref，禁止明文日志 |
| WorkBuddy 会话 | 账号泄露/越权使用 | 凭据只由原网关持有、不在编排层复制或展示 |
| 任务数据库 | 数据丢失/任务重放 | SQLite Backup、WAL、fencing、完整性测试 |
| 控制台 | 非授权操作 | localhost 私网、登录、CSRF、RBAC、审计 |
| E2E 浏览器 | 泄露真实 Cookie | sandbox 独立 profile、临时目录、退出销毁 |

## 必须实施

- 仅受控域名 allowlist，例如 `api.moonbyte.top`、sandbox 内 localhost。禁用任意 URL redirect，HTTP client 禁止把凭据发送到第三方 hostname（含 3xx 重定向）。
- `TLS >= 1.2` 用于所有生产对外服务。Keycloak/Mailpit 本地测试可以受隔离私网环境保护。
- 禁止把 `ADMIN_PASSWORDS`、`PASSWORDS`、JWT_SECRET、GitHub PAT、SMTP password、WorkBuddy token 输出到日志、导出、Run JSON。
- 选用内建 SQLite SecretStore 时使用 AES-256-GCM + 独立 96-bit nonce、key version、AAD(tenant+secret-type+id)，加密主密钥通过只读 Docker Secret 文件注入，不放在同卷、源码或 .env 里。
- 密钥轮换的旧版本必须可解密已有数据，直到 re-encrypt jobs 验收；密钥遗失必须 fail closed。
- User actions：真实 GitHub/WorkBuddy 登录和授权仍由持有人完成。OAuth provider 不得自动点击真实平台的 Consent。
- Worker 密码和 `x-admin-auth` 只交给 Mailbox Adapter，UI 不能直接持有。工作流只持有 opaque reference。
- 仅自用服务，初期不支持开放用户注册；不将朋友账号的 Token 汇总到单个未经授权的共享池。

## 平台规则与工程门禁

- GitHub 官方禁止机器人注册账号，且对多免费账号有约束；该系统不实现机器人注册。参见 `https://docs.github.com/en/site-policy/github-terms/github-terms-of-service`。
- WorkBuddy 的非官方接口与跨平台互操作需审查现行服务条款、获得必要授权；本项目真实 WorkBuddy adapter **默认关闭**，实现的 Fake Adapter 可用于 CI。
- 多个免费账户绕过额度、反检测/指纹伪装、代理轮换规避注册限制、验证码/验证挑战规避均不在工程范围。
- 仓库即使是 Public 且有代码，也不意味着有再许可权；`VentourF/WorkBuddy2api` 检查时无 `LICENSE` 文件与 recognized license，不能复制其源码并直接当 MIT 授权。

## 测试边界

单测/CI 永远不触达生产 GitHub/WorkBuddy，也不产生付费模型调用。live 测试必须通过显式 `RUN_LIVE_INTEGRATION=1` 与 allowlist 双重门禁，默认关闭。

## 上线审批检查

- [ ] 是否存在非自有外部主体的数据处理？
- [ ] SecretStore 加解密、轮换、恢复测试通过？
- [ ] Console 是否真正限制在私有网络并有强认证？
- [ ] OAuth 取消/拒绝/过期是否映射正确？
- [ ] 真实 WorkBuddy adapter 是否有授权文件/许可审查记录？没有则保持 disabled。
- [ ] 数据保留、备份、日志敏感字段检查是否通过？
