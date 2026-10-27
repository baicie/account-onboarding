# 独立对抗性审查报告 — R15.1 Outbox / SSRF Security Closure（REVIEW-R15.1，final）

- 复审时间：`2026-10-26T17:50–18:25`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `8d25aa7`**（PR #10，Draft/OPEN，base R15 `270aa1e`）；R15.1 无 `docs/acceptance/R15.1.md`/独立 evidence；沿用 R15 design/evidence；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry/arm64 证据来自 GitHub Actions；本地 R15.1 代码在临时 worktree 执行。
- **最终判定：R15_1_OFFLINE_CLOSED（P0=0，P1=0，P2=0，P3=8）。**
  - 上一轮 P2 SSRF 已关闭：`validateEgressIP` 新增 0.0.0.0/8、240/4、192.0.0.0/24、TEST-NET-1/2/3、198.18/15、NAT64 64:ff9b::/96、6to4 2002::/16、2001:db8::/32；IP table 覆盖 0.0.0.1/0.255.255.255/240.0.0.1/255.255.255.255/192.0.0.1/198.18.0.1/198.51.100.1/203.0.113.1/224.0.0.1/64:ff9b::1/2002::1/2001:db8::1；本地 webhook tests 全 PASS。
  - Outbox >500 批量 SQL、lease authority、per-delivery 分类/identity、schema5→6 restore、queue/materializer 保持；未发现新的 P0/P1/P2。
  - 8d25aa7 最终 CI 在 review 冻结时仍在进行；cde6d9b 七项全绿可作前序参考，但不能替代最终 head。Authorized Live、R16 sandbox/browser 等仍 NOT_RUN；PR Draft/OPEN；`LIVE_READY` 不宣告。

---

## 1. P2 关闭复核（P2=0）

### P2-1（closed）— SSRF egress 保留地址

- `internal/alerts/webhook.go` 的 `validateEgressIP` 现对 `0.0.0.0/8`、`240.0.0.0/4`、broadcast、`192.0.0.0/24`、TEST-NET-1/2/3、`198.18.0.0/15`、NAT64 `64:ff9b::/96`、6to4 `2002::/16`、`2001:db8::/32` 返回 blocked；loopback/private/link-local/metadata/CGNAT/mapped IPv6 原规则保留。
- 测试 table 覆盖上述地址；`go test ./internal/alerts -run TestWebhook -count=1` **PASS**（signature/HMAC、redirect、circuit、permanent 4xx、IP policy、mixed records、DNS/cert、proxy、delivery identity 全通过）。
- **closed。**

### 其余 R15.1 主体复核

- Outbox 批量 reconcile：单事务 temp current-rule set；100 active behind 600 resolved、1 active+1000 delivered、1000 active/rule preservation、in-flight、Resolve/Claim race、reopen 700 active 测试通过。
- Lease：Complete 拒绝 expired lease（未 reclaim 也拒绝），校验 owner；post-HTTP actual time；expired-lease/reclaim 测试通过。
- 分类/identity：per-delivery；403 permanent → DEAD_LETTER(maxAttempts=1)；503/429/network/timeout → RETRY_PENDING；2xx delivered；DeliveryID/AlertID/idempotency/HMAC 测试通过。
- Schema5 restore：真实 schema5 fixture backup → restore → Open migration6；health/schema/external ready；empty outbox；测试通过。
- 本地 full `go test ./...` 0；targeted storage/alerts/api/ops 0。

---

## 2. P0/P1 — 核心（clean）

- P0=0，P1=0：未发现 secret 泄漏、remote 二次写、CSRF/authz 绕过、lease 穿透、queue double-send 或 SSRF 新绕过。
- R15.1 的 outbox SQL/atomicity、SSRF、分类/identity、lease 主体均独立验证通过。

---

## 3. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R15.1 docs/PR | 无 `docs/acceptance/R15.1.md`/独立 evidence；PR #10 Draft/OPEN；仅有 R15 design/evidence，最终 docs-only commit 后需核对。 |
| P3-2 | 8d25aa7 CI | 冻结时所有 workflow still in_progress；cde6d9b 七项全绿可参考，但不能替代最终 8d25aa7 全量。 |
| P3-3 | Authorized Live/R16 | Authorized Live、真实 Keycloak/Mailpit 5-identity、Playwright browser E2E、Docker/K8s materializer、full ARM64 staging、BuildKit/SLSA、publish/GHCR 均 NOT_RUN；R16 sandbox/browser 仍未启动。 |
| P3-4 | Outbox delivered rearm | `ResolveAlertOutboxMissing` 只处理 active；DELIVERED/DEAD_LETTER 需显式 resolve。repro `resolve_missing=0 reenqueue_after_recovery=0 delivered=1`；in-memory queue 自动 re-arm 与 durable outbox 不一致，需 runbook/UI 明示。 |
| P3-5 | SSRF advanced | `192.88.99.0/24`（6to4 relay anycast）、`64:ff9b:1::/48`（local NAT64）、`2001:10::/28`（ORCHID）等未列入；loopback TLS only，无真实 target。 |
| P3-6 | Outbox idempotency/restart | webhook idempotency stable AlertID；fallback content hash 仅非 DeliveryNotifier；无 outbox 全 restart/reopen + delivery history 组合测试；dead-letter re-arm 依赖显式 resolve。 |
| P3-7 | Schema5/backup | schema5 fixture 通过；但 fixture 是“drop outbox tables”重建，未覆盖真实旧备份/WAL；retention 仍无。 |
| P3-8 | Materializer/fixtures | R15 materializer 9/9 + CI step 保持；Docker/K8s real projection、ACL、非 root ownership/emptyDir 仍未执行。 |

---

## 4. 真实 CI

### 8d25aa7（review 冻结时）

| 工作流 | run id | 状态 |
|---|---|---|
| ci | **40303253070** | in_progress |
| r9-release | **40303245085** | in_progress |
| r10-release-dry-run | **40303245116** | in_progress |
| release-image | **40303245109** | in_progress |
| supply-chain | **40303245182** | in_progress |
| staging-acceptance | **40303245266** | in_progress |
| r12-integration | **40303245207** | in_progress |

### 前序 head

- cde6d9b 七项全 success（ci 40301795059、r10 40301787119、supply 40301787104、r9 40301787120、staging 40301787149、release 40301787125、r12 40301787101）。
- 9e524cf 的 r12 40300946596 = cancelled（历史 head，不作全绿证据）。

---

## 5. 本地命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（cde6d9b worktree） | **0** | 全量绿 |
| `go test ./internal/alerts -run TestWebhook`（8d25aa7） | **0** | SSRF/HMAC/circuit/identity 全 PASS |
| `go test ./internal/storage -run 'TestResolveMissing|TestAlertOutbox'` | **0** | bulk/lease/outbox |
| `go test ./internal/api -run 'TestDispatch|TestAdminResolve'` | **0** | classification/identity/resolve |
| `go test ./internal/ops -run TestSchema5BackupRestoresAndMigratesTo6` | **0** | schema5→6 restore |
| outbox delivered rearm repro | **0** | `resolve_missing=0 reenqueue_after_recovery=0 delivered=1`（P3-4） |
| `bash tests/r15/secret_materialize.sh` | **0** | 9/9 |
| `bash tests/r11/deploy_recovery.sh`（无 Docker） | **3** | NOT_RUN: Docker unavailable |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署、未启动 Docker；未修改仓库代码/文档/证据（临时 worktree/repro 仅审查用）。

---

## 6. 结论

**最终：R15_1_OFFLINE_CLOSED（P0=0/P1=0/P2=0/P3=8）。** SSRF 保留地址缺口已关闭；outbox 批量 SQL、lease、分类/identity、schema5→6 restore 独立验证通过。Authorized Live、R16 sandbox/browser（Keycloak/Mailpit/Playwright）、Docker/K8s materializer、full ARM64 staging、BuildKit/SLSA、publish/GHCR 仍 NOT_RUN；PR Draft/OPEN；R15.1 docs 未提交。`LIVE_READY` 不得宣告。

### docs-only commit

- 可作为最终 PR head，前提仅 `docs/**`/evidence、不改代码/workflow/scripts/Makefile/配置；报告/evidence 准确反映 8d25aa7（实际 run IDs、SSRF 修复、P3 限制、NOT_RUN 清单）；docs-only head 最终 CI 全 success。
- 提交后核对 diff 仅 docs/evidence；若夹带代码/workflow 则需重新 review。

### 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `8d25aa7`；树 `6d955054…`；`git ls-tree -r 8d25aa7 \| sha256sum` = `082ae936…` |
| 文档/证据树 | `1280dd1e…`；沿用 R15 design/evidence，**无 R15.1 report/evidence** |
| 关键文件 | `webhook.go` `79f038c4…`；`webhook_test.go` `e5f3e01a…`；outbox/API 沿用 cde6d9b |
| 工作树 | 本审查文件新增；R15.1 代码未 checkout；R15.1 docs/evidence 未提交 |
