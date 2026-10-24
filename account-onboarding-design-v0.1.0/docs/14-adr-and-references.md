# 14 ADR、来源与代码勘查证据

## ADR-001 — New Go project, not Fork VentourF

**Decision:** 自建 Go 小编排服务。**Why:** `VentourF/WorkBuddy2api` 的 `workbuddy_register.py` 约 2,796 行 Python，包含实验性浏览器与特定环境依赖；README 声明完整 GitHub 新注册 + OIDC 未稳定完成，且仓库检查时无 `LICENSE` 文件、API `license=null`。不能假设可以复制其源码。

**Tradeoff:** 初始工程成本高一些，但减少未知许可证、交叉依赖、真实服务风控耦合。

## ADR-002 — Keep workbuddy2api-panel as credential authority

**Decision:** 已有 Go 网关保留所有 Token 的持久化与续期责任；Onboarding 只保存 `credential_ref` 和脱敏状态。

**Evidence:** `baicie/workbuddy2api-panel/internal/panel/login.go` 内有 `loginStart/loginPoll` 与 `a.SaveAtomic()`，`panel.go` 注册带 `p.withAuth()` 的 API，并定义 `loginTTL=15*time.Minute`。

## ADR-003 — SQLite over Postgres/Temporal

**Decision:** 仅单实例，使用 SQLite WAL + 显式 lease/fencing。**Why:** 用户规模小、私有单服务器、低运维负担。**Revisit:** 多主高可用或每分钟数千 runs 时再重评。

## ADR-004 — Keycloak+Mailpit baseline, Cloudflare live test separately

**Decision:** 离线/CI E2E 走 Keycloak SMTP -> Mailpit；另有 Cloudflare live smoke。**Why:** Cloudflare Email Routing 是入站，不提供供 Keycloak 使用的 SMTP 发件服务器，不能假装完整邮件验证已打通。

## ADR-005 — Non-abusive identity integration

**Decision:** 真实 GitHub API 只为已有账号管理邮箱；WorkBuddy 真实 Adapter 默认禁用、需要必要授权；Sandbox 5 用户全自动可作为技术可行性证明。**Why:** 平台条款与后续可维护性。

## 代码事实（2026-10-24，GitHub connector 核对）

| Repo/file | 可确认的事实 |
|---|---|
| `dreamhunter2333/cloudflare_temp_email/vitepress-docs/docs/zh/guide/feature/new-address-api.md` | `POST /admin/new_address` + `x-admin-auth`，获取 JWT；`GET /api/parsed_mails` Bearer JWT |
| `dreamhunter2333/cloudflare_temp_email/worker/src/worker.ts` | `x-custom-auth` 对站点私有访问部分路由生效；Admin/User 认证和 route 不同 |
| `baicie/workbuddy2api-panel/internal/panel/panel.go` | login start/poll 由 `p.withAuth` 包装；OAuth state 内存 TTL 15 分钟 |
| `baicie/workbuddy2api-panel/internal/panel/login.go` | global OAuth start/poll 以及原子落盘 `SaveAtomic` |
| `VentourF/WorkBuddy2api/README.md` | README 明确承认完全无人值守 GitHub/Keycloak 仍未稳定实现 |
| `VentourF/WorkBuddy2api/workbuddy_register.py` | FastAPI 自动注册与收信原型，依赖外部 `oc/cf_mail.py`/`main.py` 组件 |
| `VentourF/WorkBuddy2api` | 公开目录中无根 LICENSE，GitHub API `license=null` |

## 参考链接

- Cloudflare Temp Email: https://github.com/dreamhunter2333/cloudflare_temp_email
- Cloudflare 配置: https://github.com/dreamhunter2333/cloudflare_temp_email/blob/main/vitepress-docs/docs/zh/guide/worker-vars.md
- GitHub Email API: https://docs.github.com/en/rest/users/emails
- GitHub ToS: https://docs.github.com/en/site-policy/github-terms/github-terms-of-service
- Keycloak: https://www.keycloak.org/documentation
- Mailpit: https://github.com/axllent/mailpit
- Existing WorkBuddy gateway: https://github.com/baicie/workbuddy2api-panel
- VentourF reference: https://github.com/VentourF/WorkBuddy2api
- any-auto-register reference (AGPL-3.0): https://github.com/lxf746/any-auto-register

## 不确定项

1. 用户 Cloudflare Worker 当前线上版本、D1 Schema、管理员密码是否存在、Worker Catch-all 是否绑定：**未检查线上实例**。
2. GitHub 已有账号 API 的具体 OAuth scope 与即时认证结果：**未实际授权调用**。
3. WorkBuddy 是否会为该用途提供必要互操作许可：**未知**，故默认禁用。
4. `VentourF` 代码是否存在其他分支的许可证或作者个别授权：**当前未确认**。
5. 文档是工程规划，不代表已运行测试或部署。
