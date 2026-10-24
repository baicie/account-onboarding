# 分阶段独立提示词

## R0 Prompt

以 `ai/MASTER_PROMPT.md` 为根约束。当前只实现 R0：只读检查当前 repo；创建 Go 项目骨架、config、OpenAPI、SQLite migration、fencing lease、Fake Provider、身份验证中间件、Dockerfile/CI/Makefile。增加幂等、并发 lease、secret redaction 单测，执行 `go test ./... -race -count=1` 与 `go vet ./...`，写 `docs/acceptance/R0.md`。不要调用任何生产 Cloudflare/GitHub/WorkBuddy API，不要开始 R1/R2，不擅自推送。

## R1 Prompt

先读取已完成 R0 的验收报告并核验基线。实现 Mailpit Fake 和 CloudflareTempEmail provider：`POST /admin/new_address` + `x-admin-auth`（以及条件性的 `x-custom-auth`）、`GET /api/parsed_mails` + address JWT，处理 schema 变化、邮件去重、超时、Sender allowlist 和重试。加基础 CRUD+events API 和 UI；默认只跑 Fake。若真实 Secret 不在环境，不强求用户提供，Live Smoke 标 NOT_RUN。运行全部测试、输出 R1 证据。不得将 Admin 密码/地址 JWT 打到日志。

## R2 Prompt

从已 PASS 的 R1 出发，只对**自控环境**完成 5 账号无人值守 E2E：Keycloak Realm + Mailpit SMTP + Playwright + OIDC PKCE。禁止连接真实 GitHub/WorkBuddy，也不能拿假环境成功冒充线上成功。实现 lease fence、崩溃恢复、retry/cancel 与分布式副作用 reconcile，跑 5/5 LOCAL TEST 身份，写故障注入证据与 R2 报告。未达标标 PARTIAL，继续定位修复。

## R3 Prompt

实现 GitHub EXISTING ACCOUNT 邮箱官方 API 适配（官方 scoped 授权、本人确认 verified），做 Fake/contract 测试；在有操作者明示 Live 许可后才测试 GitHub Real。WorkBuddy 仅实现与已有网关 `POST /panel/api/login/start`、`GET /panel/api/login/poll` 的 Fake adapter，默认禁用真实调用，410 映射 REAUTHORIZE_REQUIRED。不要开发自动注册真实平台、反 CAPTCHA 或多免费账户配额规避。写 R3 报告。

## R4 Prompt

进行授权控制、CSRF/CSP、SSRF 限制、静态密文与密钥轮换、SQLite backup/restore、部署镜像与私网 Compose，审查 Docker 端口绑定、网关 Secret 是否泄露。模拟重启、DB 锁竞争、重复 API 请求、HTTP 429/5xx/410、密钥丢失、安全审计与成功后失败回滚。完整运行测试并请独立 reviewer 用 `ai/REVIEW_PROMPT.md` 审核。更新 R4 证据和最终技术债。
