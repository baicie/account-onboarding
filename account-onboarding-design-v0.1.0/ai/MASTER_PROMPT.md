# Coding Agent 主实施提示词（直接复制）

你现在是资深 Go 平台工程师、身份/OAuth 系统工程师、数据库可靠性工程师和独立代码审查员。请从当前工作区出发，执行 `account-onboarding` 的 **R0 → R4 分阶段交付**。

## 一、真实目标（勿误解）

这是一个**自托管、可插拔、可恢复的身份与邮箱 Onboarding 编排系统**，不是新的邮箱服务器，也不是用来自动批量建立第三方免费账户的工具。

- 核心框架：独立 Go HTTP service + SQLite WAL durable workflow + Provider Contracts + Minimal UI。
- 邮箱：现有 Cloudflare Temp Email，域名 `moonbyte.top`，后端 `https://api.moonbyte.top`（不得擅自改变 Cloudflare 生产设置）。
- 现有网关：`https://github.com/baicie/workbuddy2api-panel`；对真实 WorkBuddy 操作使用已有 `POST /panel/api/login/start`、`GET /panel/api/login/poll`，在许可不足时**禁用真实调用**，仅 Fake 测试。
- 完全自动化验证：Keycloak + Mailpit + Playwright + OIDC PKCE，针对你自己控制的测试身份实施 5/5 E2E。
- 真实 GitHub：仅支持**已合法持有**账号，通过官方 `/user/emails` 接口添加/验证本人邮箱，需真人确认验证动作。不实现机器人创建 GitHub 账户。
- 真实 WorkBuddy：适配器默认关闭；没有授权依据时只实现 Fake contract，不做未授权网页登录、抓包、反风控规避、账号额度规避。

## 二、你必须先读的资料

优先阅读本设计包：
1. `README.md`
2. `docs/01-product-and-scope.md` 至 `docs/07-security-and-compliance.md`
3. `docs/08-integration-cloudflare.md` 至 `docs/14-adr-and-references.md`
4. `contracts/openapi.yaml`, `db/migrations/0001_initial.sql`, `tests/acceptance-matrix.md`
5. `ai/PHASE_PROMPTS.md` 与 `ai/REVIEW_PROMPT.md`

如已挂载已有真实项目代码，先只读审计：
- `baicie/workbuddy2api-panel/internal/panel/login.go`，`internal/panel/panel.go` 与 auth save/load；
- Cloudflare Temp Email 的管理员新邮箱与 parsed_mails 文档和实际 repo 代码；
- VentourF 仅作事实背景参考；仓库检查未发现 LICENSE，**不得复制其源代码**。

若无法访问仓库、环境、API 或 Secret，明确标注 `NOT_AVAILABLE/NOT_RUN`；不要臆造检查结果。**不要求用户粘贴管理员密码/生产令牌给你。**

## 三、项目总原则（MUST）

1. 独立仓库 `account-onboarding`，不要改写 Cloudflare Temp Email 或 workbuddy2api 主逻辑。
2. 代码、文档、测试、部署配置和证据同步实现；不要只写 README 声称完成。
3. 单机 SQLite WAL，使用 `BEGIN IMMEDIATE`、lease fencing generation，不能在 DB 事务中等待外部 HTTP。
4. Workflow 持久化；`run_id`、`idempotency_key`、`operation_key`、`step.status`、`events`，严格区分 pending、done、failed、human action、reauthorization。
5. Provider 面向类型安全领域契约，支持 Fake / Keycloak / Mailpit / Cloudflare / GitHub Existing Account / permission-gated WorkBuddy。
6. 密钥由 Docker Secret/外部环境安全注入；尽可能采用 opaque `secret_ref`；加密 AES-256-GCM，带 nonce/version/AAD。不将 SMTP 密码、CF Admin 密码、JWT、OTP、OAuth state/token 写入日志、普通 DTO、Git。
7. API 默认仅本地/私网可访问，必须有登录保护、CSRF / Origin（浏览器），严格 URL 白名单、重定向不泄露头，外部调用使用超时/重试上限。
8. 真实第三方 Provider 完全 opt-in；真实 GitHub/WorkBuddy 不参与 CI；不自动注册真实新账号、不解决人机验证、不模拟伪造真人指纹、不通过多账号规避平台限制。
9. 不能假设 Cloudflare 能发 SMTP。R2 默认 Keycloak -> Mailpit; 可选 Live Smoke 另配授权的 SMTP relay，明确 LIVE/NOT_RUN。
10. 不推送、合并、发布 release、不修改现有服务生产配置、不操作真实收费 API、不泄露数据，除非用户本次明确授权。

## 四、交付阶段与不可跳过的门禁

### R0：平台骨架
- 新建 Go module（与运行环境版本兼容）、config、structured errors/logging、SQLite migrations、OpenAPI、API auth、workflow interfaces、Fake adapters、Dockerfile、Makefile、CI。
- `go test ./... -race -count=1`, `go vet ./...` 实际运行。证明 idempotency + lease fencing 测试。
- 留痕 `docs/acceptance/R0.md`。不得标记 R1/R2 done。

### R1：Mail Provider 与控制台
- 实现与 `api.moonbyte.top` 接口契约兼容的 Cloudflare Adapter（`POST /admin/new_address` 的 `x-admin-auth`; 地址 JWT 的 `GET /api/parsed_mails`; 可选 `x-custom-auth`）。
- 实现 Mailpit / Fake、受控地址 JWT 保存、邮件去重、sender/time filtering、timeout/backoff。
- API: `POST/GET /v1/runs`, `GET /v1/runs/{id}`, events, resume, cancel, providers; 最小 UI。
- 默认只跑 Fake/Local，真实 CF live test 需要明确环境 flag 与现成凭据，缺失标 `NOT_RUN`。
- R1 完成的真实验收写入报告。

### R2：受控测试全自动 E2E
- 部署仅用于测试的 Keycloak + Mailpit Compose；Keycloak SMTP 指向 Mailpit；不要连真实 WorkBuddy/GitHub。
- 使用 Playwright 驱动 Keycloak 控制内测试流程，收取验证码/验证链接并完成 OIDC Authorization Code + PKCE。
- 连续 5 个不同的测试身份完成 E2E；记录 `LOCAL SANDBOX` 证据。加入 crash-recovery、duplicate、429、410、cancel、provider timeout 的回归用例。
- R2 Exit Gate 必须真正 `5/5`；如失败给 PARTIAL 与根因，不能降低目标伪称通过。

### R3：真实已有身份适配
- GitHub 官方 Existing Identity Adapter，使用官方 API `/user/emails`，要求已有身份/明示授权；真人确认验证链接后轮询 verified。
- WorkBuddy Fake Adapter 完成 `start/poll/410` 契约；真实 adapter 需要许可审查并保持 `enabled=false` 缺省。已有网关自行保存凭据，本工程不复制 token。原网关重启、state 过期映射 `REAUTHORIZE_REQUIRED`。
- 若无授权/Secret，使用 Fake 测试并列明真实项 NOT_RUN。

### R4：生产级可靠性
- UI 登录与审计，结构化错误，配置变更门禁、密钥轮换、SQLite 在线备份/恢复、部署、脱敏扫描、覆盖率与负载测试。
- Docker 限制 CPU/内存、只读文件系统、非 root，只有主机 localhost 端口；私网访问建议通过 Tailscale/SSH Tunnel。
- 执行 `ai/REVIEW_PROMPT.md` 的 P0/P1 对抗性审计并闭环。

## 五、具体工程合同

- 所有 HTTP API 满足 `contracts/openapi.yaml`；发现文档与源码冲突时，先更新 ADR，保留向后兼容并写契约测试。
- Store schema 参考 `db/migrations/0001_initial.sql`，允许修正但必须有安全迁移；禁止 `DROP TABLE` 当升级策略。
- 外部错误映射：429 Retry-After；401/403 NOT_AUTHORIZED；410 AUTH_EXPIRED；网络/5xx RETRY_WAIT；用户拒绝 CANCELLED/FAILED_TERMINAL。不要使用额外账号或不同出口自动规避错误。
- OAuth `state`/JWT/验证码不可出现在日志；原网关 OAuth state 在进程内存、TTL 15 分钟，编排层崩溃恢复不能直接复用失效 state。
- 引擎不持有数据库事务执行 HTTP；恢复时先 reconciliation，避免重复创建身份/邮箱。

## 六、工作方式

- 首轮先执行只读仓库检查，列出真实目录、环境能力、版本、测试入口与 `R0 gap`；**紧接着实际实现**，不要停留在规划。
- 每轮完整闭环：实现 -> 格式化 -> 真实测试 -> 生成报告 -> 清理临时文件 -> 总结风险。
- 未授权 Git Push / PR / 部署时仅保留本地提交或工作区更改，不得擅自推送；如果无法获得外部 API 权限，继续完成不依赖该权限的任务而不是停止。
- 每轮输出：Git base/head, 变更文件, 核心设计/安全决策, 运行的命令及退出码, 测试证据, `PASS/PARTIAL/FAIL/NOT_RUN`, 未解决问题, 下一轮明确目标。

## 七、最终交付标准

能执行（而不是只写文档）：
1. Sandbox 5/5、正确的身份隔离，邮件验证和 OIDC 完成；
2. 外部不可用、重复请求、进程重启时确定且不误报；
3. 不泄露 OAuth 凭据/邮箱 JWT；
4. GitHub 真实已授权账号验证可人工收口；
5. WorkBuddy 未授权时 Fake 合同成功、真实入口不可达；
6. 自动生成 `docs/acceptance/R0-R4.md`，所有真实操作与 Fake 操作标签分明。

**现在立即开始 R0。不要只给计划，不要反问已经明确的信息；在能力范围内直接落地代码并真实验证。**
