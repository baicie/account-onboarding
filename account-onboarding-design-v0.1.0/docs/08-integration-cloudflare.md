# 08 Cloudflare Temp Email 集成设计

## 当前环境

- 自有邮件域：`moonbyte.top`。
- Worker API 域：`https://api.moonbyte.top`。
- 数据：Cloudflare D1。
- Stalwart 已删除；不再依赖旧 `mail.moonbyte.top` MX，也不需重启 Stalwart。
- Email Routing 根域启用并把 Catch-all 绑定正确 Worker，是正常收信的前置条件。

## 管理 API（仓库文档已核对）

```http
POST https://api.moonbyte.top/admin/new_address
x-admin-auth: <ADMIN_PASSWORD>
x-custom-auth: <SITE_PASSWORD_IF_CONFIGURED>
Content-Type: application/json

{"enablePrefix":false,"name":"sandbox001","domain":"moonbyte.top"}
```

成功响应含 `address`,`address_id`,`jwt`；实际返回 HTTP schema 应使用本机 live contract test 冻结。

```http
GET https://api.moonbyte.top/api/parsed_mails?limit=20&offset=0
Authorization: Bearer <ADDRESS_JWT>
x-custom-auth: <SITE_PASSWORD_IF_CONFIGURED>
```

解析邮件时优先使用发件人域白名单 + `received_at` 新时间窗 + 收件人完全匹配，而不是在全站邮箱内搜索所有验证码；同一 `message-id` 只消费一次。不得假设发件人 From 一定不会被伪造，沙箱测试通过自有 IdP 的签发上下文关联。

## 访问模型

- 创建邮箱需要 Admin 密码；使用地址 JWT 只能读取对应邮箱；这两个凭据必须分开存储。
- Worker 私有站点密码 `PASSWORDS` 可能要求 `x-custom-auth`，请将其配置到 Adapter 而不是关闭防护。
- Cloudflare 的 D1 中邮箱/邮件的生命周期由项目自动清理配置控制；重要长期地址应禁用地址自动清理并做好独立备份。
- 项目中的 `ADMIN_PASSWORDS` 如果设为 Secret 无法直接读出，应在 Cloudflare Worker 设置里安全轮换；不能把密码粘到工单或日志。

## 联通验收步骤

1. 检查 DNS MX 不再指向旧 Stalwart，Cloudflare Email Routing 显示 Active。
2. 检查 Catch-all 目标为实际 `api.moonbyte.top` 绑定的 Worker（Worker 名称与网页域名可能不同）。
3. `POST /admin/new_address` 创建**一个专用测试邮箱**。
4. 用拥有合法权限的外部 SMTP 发信账户向它发送随机 nonce 测试邮件。
5. 轮询 `/api/parsed_mails`，验证邮件收件人、主题和 nonce，最长 180s；超时应输出 `MAIL_DELIVERY_TIMEOUT`。
6. 进程重启再读相同邮件，确保不是只在内存里缓存。
7. `GET /api/parsed_mail/:id` 读取详情（以当前仓库/实例确认具体 path 和 schema）。
8. 根据可配置 cleanup 策略决定测试邮箱是否删除，真实长期地址绝不自动清理。

**重要区别：** Cloudflare Email Routing 是入站，不能作为 Keycloak 的 SMTP 发信服务器。默认 CI 应走 Mailpit；Cloudflare Live Smoke 需要单独授权的 SMTP relay（例如自有 SMTP 或被允许的邮件发送服务），并且先核对域名 SPF/DKIM。

## 429 / 阻断 / 延迟

- 429 尊重 Retry-After，不通过增加无关 IP 或邮箱规避限制。
- 邮件未收到不等于账号注册失败；表述为独立投递失败、DNS 配置异常、发件服务失败或远端没有发信，需要可观察事件区分。
- 邮件 API 500/超时的轮询策略指数退避，禁止每 100ms 无限扫 D1。

参考：`https://github.com/dreamhunter2333/cloudflare_temp_email/blob/main/vitepress-docs/docs/zh/guide/feature/new-address-api.md`。
