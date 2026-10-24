# 13 R0–R4 路线图与任务清单

每轮必须 code + tests + docs + evidence 同步提交。不自行创建对外公开项目或推送生产配置；只有用户明确要求才进行 Git push/PR、线上部署、真实 API 写入。

## R0 — 基线与契约

- [ ] 创建 Go module，确定 `go 1.25+` 的实际版本并保证 CI / 服务器兼容，禁止把宿主不支持的 Go 版本硬编码。
- [ ] 建立 `cmd/onboardd` + `internal/api` + `internal/workflow` + `internal/provider` + `internal/storage` + `internal/security`。
- [ ] 实现 config loader：严格校验、secret redaction、disable-by-default Live Provider。
- [ ] 迁移 `0001_initial.sql`，单库写锁、仓库层事务测试。
- [ ] 定义 Provider 接口、错误类型、OpenAPI、统一事件脱敏。
- [ ] 建立 `Makefile`、Dockerfile、CI workflow，执行 `go test ./... -race`。

**Exit**：CI 全绿；空任务 CRUD/lease/fencing 基础测试通过；不得声称完整 E2E 已实现。

## R1 — Mailbox 和控制台 MVP

- [ ] 实现 Fake/Mailpit/Cloudflare Mailbox Provider 的接口与合同测试。
- [ ] Cloudflare admin/new_address 地址创建、Parsed Mail 读取、site password 双认证。
- [ ] `runs/create/list/detail/resume/cancel/events/providers` API。
- [ ] 最小 UI：列表、步骤、日志、需人工确认、状态筛选。
- [ ] 邮件去重、收件时间窗、Sender allowlist、终态日志脱敏。
- [ ] 可选 Cloudflare live smoke（需要显式允许、合法 SMTP 发件服务）。

**Exit**：本地 Mailbox Fake/Mailpit 全通过，真实 Cloudflare 验证有独立 LIVE 证据或明确 NOT_RUN。

## R2 — 全自动 Sandbox E2E

- [ ] 测试专用 Keycloak Realm 与 SMTP Mailpit 的 Compose fixture。
- [ ] Playwright 在 Keycloak 受控注册页/本地测试网页自动提交自己生成的测试身份。
- [ ] 自动从 Mailpit 读取验证码/验证链接，完成邮件验证与 OIDC 授权码 + PKCE。
- [ ] 5 个测试用户并发（受限）完成端到端；失败自动清理/可重试。
- [ ] crash injection，DB commit 前/后崩溃，HTTP 429/410/5xx、OAuth cancel 断点验证。

**Exit**：5/5 LOCAL Sandbox E2E 成功；不能以它推断真实平台成功率。

## R3 — 已有真实账户适配

- [ ] GitHub Official Email API contract + 已有账号授权检查。
- [ ] 由本人确认验证邮件，检测 `verified=true` 状态。
- [ ] WorkBuddy 仅实现 Fake contract，明确禁用真实适配；若许可证明存在，经人工评审才允许启用真实适配。
- [ ] WorkBuddy 410（原网关重启）要求重新授权；完成以后凭据仍保存在原网关。
- [ ] UI 展示真实授权依赖人工操作，区分 Sandbox E2E 与 Live Integration。

**Exit**：GitHub Existing Identity 实测或 NOT_RUN；WorkBuddy 默认禁用且 Fake 合同全通过。

## R4 — 运行可靠性/安全闭环

- [ ] 备份/恢复/密钥轮换，模拟密钥丢失的 fail-closed 行为。
- [ ] RBAC/CSRF/CSP、TLS ingress、请求速率限制、Origin 检查、SSRF 防护。
- [ ] 日志/事件敏感字段黑名单扫描，恶意输入回归测试。
- [ ] Docker GHCR 发布流程（推送前得到明确授权）、非 root、只读 FS、资源限额。
- [ ] 30 次串行 + 5 次并发 Sandbox 可靠性回归（次数为测试计划，不是成功率承诺）。
- [ ] 最终设计偏差表，逐项关闭问题，冻结验收报告。

**Exit**：所有安全门禁真实执行，未通过项清晰列出，不用 PASS 包装 PARTIAL。
