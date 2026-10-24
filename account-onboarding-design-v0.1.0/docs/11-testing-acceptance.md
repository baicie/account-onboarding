# 11 测试体系与验收门禁

## 测试分层

1. **单元测试**：状态转移矩阵、OTP 邮件时间窗解析、幂等键、加解密、退避与 429/410 映射。
2. **Provider Contract**：使用 `httptest.Server` 注入 200/400/401/403/404/409/410/429/5xx、非 JSON、慢响应、跳转泄密、重放。
3. **集成测试**：SQLite migrations + WAL + lease fencing；Keycloak + Mailpit fixture；Mock Worker。
4. **Sandbox E2E**：5 个完全可控测试身份，从注册到邮箱验证、授权码+PKCE、Token 验证和回收。
5. **Cloudflare Live Smoke（可选）**：真实 D1 Worker 自己的邮箱地址和合法发件服务；所有 live 操作需人工显式 flag；不要以缺乏 SMTP relay 为由默默跳过却宣称成功。
6. **真实 GitHub 已有账号（可选）**：官方 API 权限与本人验证；不能在 CI 使用个人 Token。
7. **WorkBuddy**：仅 Fake 合同验证；有明确许可后再进行经授权的实际测试。

## R2 必须跑通的典型用例

| Case | 场景 | 期望 |
|---|---|---|
| E2E-001 | 5 个 Keycloak 测试用户全自动邮箱验证和 OIDC | 5/5 完成，凭据正确归属 |
| E2E-002 | 重复提交相同 idempotency key | 只创建 1 个 run |
| E2E-003 | 接收验证码前重启 Go 进程 | 继续等待，不能重复生成身份 |
| E2E-004 | Keycloak 邮件 120s 延迟 | 等待、不无限重试 |
| E2E-005 | 使用错误/过期验证链接 | 失败或要求重新触发，不能虚报 |
| E2E-006 | OAuth 人工取消或拒绝 | 状态为取消/失败，不保存凭据 |
| E2E-007 | DB busy 并发抢租约 | 只有一个 worker 成功提交 |
| E2E-008 | Worker 在外部成功后、DB 提交前崩溃 | reconcile，不重复外部副作用 |
| E2E-009 | 全链路日志扫描 | 没有邮箱 JWT、PAT、OTP、Token、密码 |
| E2E-010 | 数据备份和恢复 | 原任务/事件/加密数据可正常校验 |
| E2E-011 | 网关 OAuth state 返回 410 | REAUTHORIZE_REQUIRED |
| E2E-012 | 真实 provider 未授权 | 明确 NOT_AUTHORIZED，Fake 流仍可用 |

## 测试命令（实现阶段约定）

```bash
go test ./... -race -count=1
go vet ./...
# 仅包含测试服务，不操作外部账号
docker compose -f tests/sandbox/compose.yaml up -d --wait
make test-contract
make test-e2e-sandbox
make test-security
```

这些命令是**目标契约**，设计阶段尚未创建对应 Makefile/代码，Agent 需在 R0–R2 实现。

## 门禁和证据

每轮输出 `docs/acceptance/Rx.md`：命令、时间、运行环境、退出码、关键日志（脱敏）、有无真实 Cloudflare/Keycloak/GitHub/WorkBuddy 调用、失败原因、未完成项。模拟测试必须标 `MOCK`，线上标 `LIVE`，未执行标 `NOT_RUN`；不能把 Fake 的 5/5 称为 WorkBuddy 5/5。

## 性能与容量

- 并发 5 个沙箱用户测试应无重复身份，端到端耗时记录 P50/P95，但不预设必须“10s 全部完成”。
- 负载目标是 self-hosted 轻量服务；数据库性能需真实评估，消息投递延迟与 API 延迟分开统计。
