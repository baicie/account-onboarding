# Moonbyte Account Onboarding — v0.1.0 Engineering Design Pack

> Status: DESIGN APPROVED FOR IMPLEMENTATION, not an implemented application. Date: 2026-10-24. Language: 中文. Owner: private self-hosted engineering workflow.

## 一句话决策

**新建独立 Go 工程 `account-onboarding`，复用 `api.moonbyte.top` 的 Cloudflare Temp Email，复用现有 `baicie/workbuddy2api-panel` 的 OAuth 状态管理，使用 SQLite 实现可恢复状态机。**

不 Fork `VentourF/WorkBuddy2api`：该仓库注册原型存在外部依赖和真实平台授权失败的实测记录，且检查时未发现仓库许可证；可参考其行为边界，**不复制其源代码**。

## 工程边界 / 合规边界

- `sandbox-e2e`：对 **自己控制** 的 Keycloak/OIDC 测试身份源、Mailpit 测试邮箱实施全无人值守注册、邮箱验证、OAuth 和凭据生命周期测试。5 个测试身份是 R2 的验收项。
- `cloudflare-integration`：对自己控制的真实 `@moonbyte.top` 收件地址测试 API/实际邮件投递。实际邮件投递必须有 SMTP relay，Cloudflare Email Routing **只负责入站**。
- `real-account`：对**已经合法持有**的 GitHub 账号使用 GitHub 官方邮箱 API；真实平台的身份验证、授权同意由账号持有人操作。WorkBuddy 适配器默认禁用，必须核实平台书面许可后才启用。
- 不开发 GitHub 机器人注册、多账号免费额度轮转、验证码/CAPTCHA/反机器人绕过、浏览器指纹伪装、登录态窃取、未经许可的令牌提取或转售链路。

## 文档导航

| 文件 | 内容 |
|---|---|
| `docs/01-product-and-scope.md` | 产品需求、场景、非目标、功能/非功能需求 |
| `docs/02-architecture.md` | 组件、交互、部署、可靠性 |
| `docs/03-workflows.md` | 工作流与状态机、恢复/超时/幂等 |
| `docs/04-providers.md` | Mailbox / Identity / OAuth / Credentials 契约 |
| `docs/05-data-model.md` | SQLite、事务和迁移策略 |
| `docs/06-api-and-ui.md` | API、鉴权、前端交互 |
| `docs/07-security-and-compliance.md` | 安全模型、Token、隐私、限制 |
| `docs/08-integration-cloudflare.md` | Cloudflare 临时邮箱适配及联通验收 |
| `docs/09-integration-github.md` | 合规 GitHub 已有账号集成 |
| `docs/10-integration-workbuddy.md` | 已有网关适配及真实授权限制 |
| `docs/11-testing-acceptance.md` | 单元/契约/集成/E2E/故障注入门禁 |
| `docs/12-deployment-operations.md` | Docker、私网部署、备份、运维 |
| `docs/13-roadmap-and-tasks.md` | R0–R4 可执行任务清单 |
| `docs/14-adr-and-references.md` | ADR、代码勘查证据与来源 |
| `contracts/openapi.yaml` | 控制面 REST API 草案 |
| `db/migrations/0001_initial.sql` | 数据库 Schema 草案 |
| `deploy/compose.yaml` / `deploy/.env.example` | 部署模板 |
| `tests/acceptance-matrix.md` | Gherkin 验收用例 |
| `ai/MASTER_PROMPT.md` | 主实施提示词（可直接复制） |
| `ai/PHASE_PROMPTS.md` | R0–R4 分阶段提示词 |
| `ai/REVIEW_PROMPT.md` | 独立审查提示词 |

## 建议的实施入口

1. 审阅 `docs/01-...` 到 `docs/07-...`，以 `docs/14-...` 的 ADR 固定边界。
2. 给 Coding Agent **完整仓库与本设计包**，先提交 R0；不允许直接跳到真实第三方自动注册。
3. 执行 `ai/MASTER_PROMPT.md`；每轮按 `ai/PHASE_PROMPTS.md` 逐阶段推进。
4. 必须真实运行测试；没有真实 API 权限时以 `NOT_RUN` 声明，绝不能以 mocked 结果冒充线上结果。
5. 本包所有 `.env` 和 JSON 示例仅是模板，不包含凭据；不要将生产 API Key、OAuth Token 或邮箱 JWT 提交到 Git。

## 核心验收标准

- R1: 本地 Mailbox Fake 契约 100% 通过；可选真实 Cloudflare 创建/读取/删除 **自己的**测试邮箱且无泄漏。
- R2: 5/5 测试身份完整通过 Keycloak + Mailpit + Playwright/OIDC；异常注入下可恢复。
- R3: GitHub **已有账号**官方 API 检测到 verified=true；真实 WorkBuddy 接入仅在获得必要许可后测试。
- R4: SQLite 持久化、重启恢复、幂等、取消、密钥保护、备份/恢复全部有测试证据。

## 不是什么

这不是已经完成的代码仓库、不是可立即运行的生产镜像，也不是针对第三方服务的大规模账号注册工具。`deploy/compose.yaml` 是供实现阶段补齐镜像的部署契约。
