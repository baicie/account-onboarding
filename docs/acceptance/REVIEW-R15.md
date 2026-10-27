# 独立对抗性审查报告 — R15 Final Engineering Readiness（REVIEW-R15，final）

- 复审时间：`2026-10-26T16:45–17:10`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `49cbbca`**（PR #9，Draft/OPEN，base R14.1 `3796047`）；R15 文档/证据仅 `docs/design/R15-FINAL-ENGINEERING-DESIGN.md` + `docs/acceptance/evidence/r15/acceptance-matrix.json`，**无 `docs/acceptance/R15.md`**；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry/arm64 证据来自 GitHub Actions；本地 R15 代码在临时 worktree 执行。
- **最终判定：R15_OFFLINE_ENGINEERING_CLOSED（P0=0，P1=0，P2=0，P3=8）。**
  - P2-3 残余已关闭：`handleOpsAlertResolve` 先清 durable outbox，再处理 in-memory queue，只在 `durableResolved==0 && !queueFound` 时 404；新增 outbox-only/restart 测试独立通过。
  - materializer writable-source、outbox delivered 重入队、durable resolve 均已独立复核关闭；未发现新的 P0/P1/P2。
  - Authorized Live **NOT_RUN**、publish skipped、无 GHCR/merge/production、PR #9 Draft/OPEN、R15.md/evidence 未提交；这些是 P3/离线限制，不等于 `LIVE_READY`。
  - 49cbbca 最终 CI 在 review 冻结时仍在进行；最终所有 run success 后才能把 docs-only head 作为最终 PR head。

---

## 1. P2 关闭复核（P2=0）

### P2-1（closed）— materializer writable source

- `scripts/materialize_secrets.sh:34` 使用 `(8#$mode & 8#222) != 0`；独立 probe：0400/0444 接受，0600/0660/0620/0460/0666 全部 exit1。
- `tests/r15/secret_materialize.sh` **9/9**；CI 已新增 `Secret materializer tests` step。

### P2-2（closed）— delivered outbox 重入队

- outbox 对任何非 RESOLVED 状态去重；delivered 保持 active-until-resolve。
- 独立 repro：`first=1 post_delivery=0 second_claim=0 pending=0 delivered=1`；resolve 后 re-arm 测试通过。

### P2-3（closed）— restart/outbox-only resolve

- `handleOpsAlertResolve`：先 `Store.ResolveAlertOutbox(rule)`，再 `AlertQueue.Resolve(rule)`，仅 `durableResolved==0 && !queueFound` 返回 404。
- 新测试 outbox-only/restart：空 in-memory queue + SQLite outbox，POST resolve → 200，Resolved=1/Pending=0。
- 独立运行 resolve 测试：**0**；normal queue+outbox test 仍通过。
- **closed。**

---

## 2. P0/P1 — 核心（clean）

- Schema6/outbox lease/fencing/retry/dead-letter、backup/restore（CONFIRMED、expired revocation、outbox row）、queue panic recovery、webhook HMAC/circuit、independent cookie jars、secret exact modes 主体成立。
- **P0=0，P1=0**：未发现 secret 泄漏、remote 二次写、CSRF/authz 绕过、lease 穿透或 queue double-send。

---

## 3. P3（8，离线/交付限制）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R15 docs/PR | 无 `docs/acceptance/R15.md`；PR #9 Draft/OPEN；docs/evidence 仅 design+matrix，最终 docs-only commit 后需核对。 |
| P3-2 | 49cbbca CI | 冻结时 ci/r10-dry-run success，release-image/r9/staging/supply/r12-integration still in_progress；历史上 success 不能替代最终 49cbbca 全量。 |
| P3-3 | Authorized Live | Authorized Live **NOT_RUN**；publish skipped、无 GHCR/merge/production；webhook 生产 transport disabled。 |
| P3-4 | Webhook SSRF | exact host allowlist 存在，但无 resolved-IP/private-range/link-local 阻断；allowlisted host 的 DNS rebinding 仍可指向内网；仅 loopback TLS 测试。 |
| P3-5 | Outbox idempotency/restart | webhook idempotency header 用 alert 内容 hash，不是 delivery.id；outbox restart/reopen 与 >500 行分页无专门测试；dead-letter 只靠显式 resolve re-arm。 |
| P3-6 | Schema6/backup | schema6 当前备份恢复成立；旧 schema5 backup→新 binary 的迁移/恢复路径无专门测试，schema5 测试仍从 3/4 起步。 |
| P3-7 | Materializer fixtures | Docker/K8s fixture 未执行；ACL 不检查；CI 只跑 local shell materializer test；D matrix 的 real fixture 仍 NOT_RUN。 |
| P3-8 | NOT_RUN 清单 | 真实 Keycloak/Mailpit 5-identity E2E、Playwright、BuildKit/SLSA、publish/GHCR、ARM64 full staging、Docker/K8s secret fixture execution 均 NOT_RUN；acceptance matrix 基本如实标注。 |

---

## 4. CI 证据

### 49cbbca（review 冻结时）

| 工作流 | run id | 状态 |
|---|---|---|
| ci | **40293023434** | success |
| release-image | **40293017682** | in_progress |
| r9-release | **40293017687** | in_progress |
| staging-acceptance | **40293017802** | in_progress |
| r10-release-dry-run | **40293017714** | success |
| supply-chain | **40293017743** | in_progress |
| r12-integration | **40293017709** | in_progress |

### 历史 head（仅供参考，不替代 49cbbca）

7a318c2 ci 40292196315 success；60591b2 7 runs 全 success；ARM64 `RUNTIME_SERVICE_PASS ... schema=6 ...`，deploy 16/16，publish skipped。

---

## 5. 本地命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify`（60591b2 worktree） | **0** | fmt/vet/build/全量测试 |
| `go test ./... -race -count=1`（60591b2） | **0** | 全绿 |
| `go test ./...` 计数（60591b2） | **0** | **426 顶层（421 PASS + 5 SKIP）/809/0 FAIL** |
| materializer test（7a318c2） | **0** | **9/9** |
| materializer probe 0400/0444 | **0** | 接受 |
| materializer probe 0600/0660/0620/0460/0666 | **1 each** | 拒绝 |
| `go test ./internal/storage -run TestAlertOutbox`（7a318c2） | **0** | delivered dedupe/resolve history |
| `go test ./internal/api -run 'TestAdminResolve...'`（49cbbca） | **0** | normal + outbox-only/restart resolve |
| outbox post-delivery repro（7a318c2） | **0** | `post_delivery=0 second_claim=0` |
| `bash tests/r11/deploy_recovery.sh`（无 Docker） | **3** | NOT_RUN: Docker unavailable |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署、未启动 Docker；未修改仓库代码/文档/证据（临时 worktree/repro 仅审查用）。

---

## 6. 结论

**最终：R15_OFFLINE_ENGINEERING_CLOSED（P0=0/P1=0/P2=0/P3=8）。** 三个 P2 均已独立复核关闭；R15 离线工程 scope 可关闭。Authorized Live **NOT_RUN**、publish skipped、无 GHCR/merge/production、PR Draft、R15.md/evidence 待提交，以及 49cbbca 最终 CI 未完成，是明确的 P3/交付限制；**不是** `LIVE_READY`。

### docs-only commit

- 可作为最终 PR head，前提仅 `docs/**`/evidence、不改代码/workflow/scripts/Makefile/配置；报告/evidence 准确反映 49cbbca（实际 run IDs、P2 全关闭、P3 限制、NOT_RUN 清单、Authorized Live NOT_RUN）；docs-only head 最终 CI 全 success。
- 提交后核对 diff 仅 docs/evidence；若夹带代码/workflow 则需重新 review。

### 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `49cbbca`；树 `69ea10be…`；`git ls-tree -r 49cbbca \| sha256sum` = `92844584…` |
| 文档/证据树 | `e761da4f…`；有 design/matrix，**无 `docs/acceptance/R15.md`** |
| 关键文件 | `api/ops.go` `d6a33b34…`；`api/r15_test.go` `09752610…`；materializer/outbox 沿用 7a318c2 修复 |
| 工作树 | 本审查文件新增；R15 代码未 checkout；R15 docs/evidence 未提交 |
