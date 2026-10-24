# 01 产品目标、用户故事与范围

## 1. 背景

现有环境：`moonbyte.top` 使用 Cloudflare DNS；Cloudflare Temp Email 部署在 Worker，并通过 `https://api.moonbyte.top` 提供收件管理；`baicie/workbuddy2api-panel` 是已有 Go 网关，已实现 WorkBuddy OAuth 开始、状态轮询及凭据落盘。此前已删除 Stalwart，不应恢复第二套邮件服务器。

当前缺口不是 SMTP 服务或独立 OAuth 客户端，而是**跨系统编排、可观察性、可恢复的任务状态、密钥边界和真实验收**。

## 2. 最终目标

提供一个可插拔、自托管、可复用的身份/邮箱/OAuth Onboarding Engine。它应能在自有测试身份源中全自动执行 E2E，并通过受控适配器辅助已有真实账户完成邮箱及 OAuth 管理。

## 3. 用户故事

- US-01 我可创建一个名为 `sandbox-e2e` 的任务，在自有 Keycloak 中完成 5 个测试用户注册与邮箱验证，全过程可追踪。
- US-02 我可连接自己 Cloudflare Temp Email 的单一邮件地址，安全地读取并识别最近一封验证邮件。
- US-03 我可使用已有 GitHub 账号添加自己的邮箱、看到 GitHub 发送的验证信，并查询 `verified` 结果。
- US-04 我可在确有平台许可时，从控制台开始一个现有 WorkBuddy 账号的授权，自己在浏览器同意；系统跟踪状态，不读取第三方密码。
- US-05 任何一步失败、进程重启或网络断开，我都能辨别 `WAITING_USER_ACTION`、`RETRYABLE`、`FAILED_TERMINAL`、`COMPLETED`，继续或停止操作。
- US-06 我可从事件日志判断发生了什么，且日志不含密码、Token、OTP、OAuth Code。
- US-07 我可备份、迁移 SQLite 数据与相关加密密钥，实测恢复成功。

## 4. 功能需求（MUST）

| ID | 要求 |
|---|---|
| F01 | 支持创建、查询、取消、恢复工作流；API 幂等 |
| F02 | 任务状态与事件持久化，重启后不会误判成功 |
| F03 | Provider 清晰分层，可替换并执行契约测试 |
| F04 | Mailbox 支持自控地址创建、去重读取、验收删除 |
| F05 | 测试 IdP 支持真实邮件验证、授权码 + PKCE OIDC 流程 |
| F06 | GitHub 适配只操作已授权账号与自有邮箱，不创建新 GitHub 账号 |
| F07 | OAuth 可暂停等待本人授权，失败可恢复，Consent 不得隐式同意 |
| F08 | API/Worker 管理凭据只通过 Secret 注入，不出现在审计和 HTTP 响应 |
| F09 | 简洁 UI：任务、时间线、需人工处理、重试、取消、结果摘要 |
| F10 | 5 个 Sandbox 用户无人工完成 E2E、幂等和故障恢复门禁 |

## 5. 非功能需求

- NFR-01 本地单进程、SQLite WAL；受控并发（默认 2，可配最高 5）；不依赖 Kafka/Redis/Postgres。
- NFR-02 API 默认监听 127.0.0.1；未配置管理员认证时拒绝非健康检查请求。
- NFR-03 客户端 HTTP timeout、错误分类、指数退避 + 抖动，禁止对 401/403/验证码过期盲重试。
- NFR-04 时钟统一 UTC 存储；用户界面本地化显示。
- NFR-05 凭据或邮箱 JWT 加密静态保存；密钥轮换/遗失有清晰恢复说明。
- NFR-06 绝不将真实邮件全文与附件完整写入日志；保留必要元数据最小化。
- NFR-07 生产编排和测试编排不得混用 Provider 或自动把测试凭据导入真实网关。
- NFR-08 部署资源上限目标：单 Go 服务 256 MiB 内存、0.5 CPU；指标为设计预算，须上线后测量，不是实测。

## 6. 非目标与限制

- 不提供任意第三方平台的机器人批量注册、不绕过 CAPTCHA、DataDome、速率限制、账号数/免费额度限制。
- 不提供未经许可的 WorkBuddy 接口兼容/账号共享/额度池套利。
- 不要求 Cloudflare 能发 SMTP：它在本架构中是**收件和存储**。测试 Keycloak 用 Mailpit 或有权限的 SMTP Relay 发信。
- 不在 R0/R1 引入 AI Agent 自主决定要不要执行认证动作；所有真实外部写操作必须明示目标身份与授权。

## 7. release 范围

- v0.1.0：R0–R2，**可用的测试自动化产品**。
- v0.2.0：R3，**已有真实身份适配**，平台授权仍受限制。
- v0.3.0：R4，私有部署可靠性与可操作性闭环。
