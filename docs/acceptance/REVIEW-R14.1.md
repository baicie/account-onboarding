# 独立对抗性审查报告 — R14.1 Final Safety Closure（REVIEW-R14.1）

- 审查时间：`2026-10-26T12:38–13:20`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只新增本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI HEAD `5380063`**（PR #8，Draft/OPEN，base R14 `080429b`）；**R14.1 文档/证据不在 branch**（无 `docs/acceptance/R14.1.md`/evidence）；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry/arm64 证据来自 GitHub Actions；本地 R14.1 代码在临时 worktree 执行。
- **最终判定：R14_1_OFFLINE_CLOSED（P0=0，P1=0，P2=0，P3=8）。**
  - 离线安全闭环主体成立：queue ID/batch/IN_FLIGHT/RETRY_PENDING 生命周期、GET purity、显式 POST reconcile/resolve/drain、secret exact 0400/0600、candidate Dockerfile base refs/digests 精确比较、UI two-admin race、CONFIRMED recovery 均验证通过。
  - 尚无 P0/P1/P2 阻断项；剩余均为 P3/acceptance 限制：Authorized Live **NOT_RUN**、publish skipped、PR #8 Draft/OPEN、R14.1 docs/evidence 未提交、notifier 仍为本地 fake、部分测试覆盖/断言边界。
  - 因此按 **离线 closure** 接受为 `R14_1_OFFLINE_CLOSED`，不是 `LIVE_READY`；不得宣告 Live/GHCR/merge/production 完成。

---

## 1. 重点验证结果

### Queue 生命周期/race/panic/ordering

- `queue.go`：stable `nextID`；`PENDING/IN_FLIGHT/RETRY_PENDING`；`deliveryBatch.id`；Notify 期间 **不持锁**；成功仅删除仍存在的 batch ID；失败仅对仍存在的 entry 置 RETRY_PENDING；attempts cap 10；context cancel 按失败重试；concurrent Drain 由 `q.batch != nil` 返回 `ErrDrainInProgress`。
- Resolve/Reconcile 在 Drain 阻塞时删除 entry/active，Drain 完成时按缺失 ID skip，不会 resurrect；`Reconcile` 清 recovered、re-enqueue re-armed；drop 只针对非 in-flight，且删除 active。
- 测试：`Resolve/Reconcile/EnqueueAndDrop/FailureResolve/ContextCancel` while drain blocked、`ReconcileRearmsAfterDelivered`、`RepeatedResolve`、`ControlledConcurrentChurn` 均通过；本地 `go test ./internal/alerts -race -run TestQueue -count=20` **0**；API alert race `-count=10` **0**。
- 复核未发现 queue 数据竞争、deadlock、double-send 或 Resolve/Reconcile 后 stale acknowledge；`Status` 显示 Ephemeral/Active/Pending/InFlight/Delivered/Resolved/Deduped/Dropped/Sent。
- **P3 残余**：`Drain` 没有 recover Notifier panic；若未来外部 Notifier panic，`q.batch` 会停留在 in-flight，后续 Drain 返回 ErrDrainInProgress。当前生产 notifier 是本地 RecordingNotifier（不 panic），故不构成当前 P2。

### GET purity / control API

- `GET /v1/ops/alerts` 不再 Reconcile，只 Evaluate + queue status；显式 `POST /v1/ops/alerts/reconcile`、`/resolve`、`/drain` 均 `apiAuth(true,true)`（cookie CSRF/Origin）；missing resolve 404。
- `TestAlertReconcileEndpointIsExplicitAdminControl` 通过；GET 不再改变 active/pending。
- **P3**：POST queue 的 cookie CSRF/Origin 无专门负向测试；生产 drain 仍 RecordingNotifier。

### Secret exact modes / all providers

- `readSecureSecretFile` 改为 exact `perm == 0400 || perm == 0600`；TestSecureSecretFileModes 覆盖 0000/0400/0450/0500/0600/0644/0640/0660/0666/0700；0000/0500/0700/0450/0644/0660/0666 全部拒绝；path/dir/missing/empty/oversize/symlink/replacement 保持。
- `TestAllProviderSecretFilesUseSecureReader` 覆盖 Admin/ReadOnly/Master/Cloudflare/Keycloak/GitHub/WorkBuddy/OIDC 各 `*_FILE`，均走 secure reader。
- **P3**：exact mode 测试是 direct reader；all-provider 用例是 0644，未逐 provider 跑 0500/0700/0450；同一 reader 下风险低，但测试完整性有限。

### Candidate base digest provenance

- `candidate_identity.sh` 从 Dockerfile 正则读取 `GOLANG_IMAGE`/`RUNTIME_IMAGE`，要求恰好两个；`base_image_refs` 必须精确等于 `golang_image=... runtime_image=...`；`base_image_digests` 排序后必须精确等于 Dockerfile refs（不再只含 `@sha256`）。
- 负向 probe：image-id/provenance、image.tar、base digest mismatch；CI same-artifact `SUMMARY pass=14 fail=0`，digest/image ID 验证通过。
- **P3**：这验证 metadata 与 Dockerfile refs，不是镜像 layer/build provenance；candidate 与 general dry-run 仍是两套 job；publish skipped，未验证 GHCR。

### CONFIRMED / revoked session recovery

- `recovery_state_test.go` 新增 `ExternalConfirmed` seed/assert；restore 后 CONFIRMED/PREPARED/IN_FLIGHT/OUTCOME_UNKNOWN/RECON/RESOLVED/audit/schema5 断言。
- **P3**：`session-expired-r14` 是在 **restore 之后**在 restored store 上创建再检查过期，不是 backup 内已有 expired revocation 的 restore 断言；R14.1 “expired revocation in restore” claim 目前只部分证明。建议先 seed expired revocation，再 backup/restore/assert。

### UI two admins / CSRF

- `TestTwoAdminsUIResolutionRace` 通过 cookie/CSRF + Origin 并发两个 resolution，两个请求都在 UI 路径，最终 one resolution/one audit，状态 resolved。
- **P3**：测试复用同一 `loginUISession` cookie/CSRF，并未创建两个独立 admin session；并发 CAS 成立，但 “two admins” 隔离/不同 session 未证明；detail reason 仍原样显示。

### Runbook

- 状态新增 `AUTHORIZED_READONLY_LIVE_NOT_RUN`，明确是 R14.1 唯一可接受 terminal state；Stage B 明确单独授权。
- **P3**：这是文档/流程，无 live 执行；Authorized Live NOT_RUN。

---

## 2. P0/P1/P2

- **P0=0**：未发现 secret 泄漏/绕过、queue 双发/丢失、candidate identity 假绿、publish 未受 same-artifact 门禁、UI 双写。
- **P1=0**：未发现 TOCTOU/symlink 绕过、remote 二次写、authz/CSRF 绕过。
- **P2=0**：R14 的 0450、publish dependency、queue reconcile/resolve 均已关闭；R14.1 queue lifecycle 与 GET purity 未发现阻断项。

---

## 3. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R14.1 docs | 无 `docs/acceptance/R14.1.md`/evidence；PR #8 Draft/OPEN；docs/evidence 未提交；最终 docs-only commit 后需核对仅 docs。 |
| P3-2 | Authorized Live | Authorized Live **NOT_RUN**；runbook 状态/Stage B 是流程，不是执行；publish skipped、无 GHCR/merge/production。 |
| P3-3 | Queue/notifier | `AlertNotifier` 仍 RecordingNotifier；drain 只本地记录不外部发送；queue ephemeral，restart 丢失未持久化。 |
| P3-4 | Queue panic/cap | Notifier panic 无 recover，会遗留 `q.batch` 使后续 Drain 永 ErrDrainInProgress；attempts=10 cap 与 batch ID 未暴露/未单测；当前无外部 notifier，P3。 |
| P3-5 | Recovery | expired revocation 在 restore 之后创建，未真实验证 backup 内 expired revocation 恢复；CONFIRMED 已覆盖。 |
| P3-6 | UI | two-admin race 复用单 session/cookie，未验证独立 admin/session/filter；detail reason 原样显示（HTML 转义但非 redact）。 |
| P3-7 | Secret | exact mode 仅 direct reader；all-provider 用例用 0644；ACL/Docker 0444/K8s projected 0444 适配仍依赖 copy/文档，未实机测试。 |
| P3-8 | Candidate/release | base refs/digests 是 Dockerfile metadata 对比，不是镜像 layer provenance；candidate 与 general dry-run 两套 job；publish 未执行；R14.1 docs 未提交。 |

---

## 4. 最终 CI（head `5380063`，全部 success）

| Run/工作流 | 实际 run id | 关键证据 |
|---|---|---|
| ci | **40269198576** | gofmt/build/vet/race/compose |
| r12-integration | **40269180665** | deploy **16/16**、Cloudflare mock、ARM64 artifact **`RUNTIME_SERVICE_PASS linux/arm64 schema=5 readyz=200 ... restart=persisted`** |
| staging | **40269180703** | staging success |
| supply | **40269180704** | supply PASS |
| r9-release | **40269180694** | caddy/rollback |
| dry-run | **40269180650** | OCI/sign/attest |
| release-image | **40269180686** | candidate + same-artifact success；same-artifact **pass=14 fail=0**；publish **skipped**；无 GHCR |

所有 run head_sha=`5380063`；publish skipped 不按 PASS 计。

---

## 5. 本地命令/退出码（临时 worktree `5380063`）

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试 |
| `go test ./... -race -count=1` | **0** | 全绿 |
| `go test ./...` 计数 | **0** | **416 顶层（411 PASS + 5 SKIP）/799 含子测试/0 FAIL** |
| `go test ./internal/alerts -run TestQueue -race -count=20` | **0** | queue lifecycle/barrier/race |
| `go test ./internal/api -run 'TestAlert|TestTwoAdmins' -race -count=10` | **0** | queue API/UI race |
| `go test ./internal/config -run 'TestSecureSecretFile|TestAllProvider'` | **0** | exact modes/all providers |
| `go test ./internal/ops -run 'TestEncryptedBackup|TestWrongBackup|TestCorrupt'` | **0** | backup/restore |
| `bash tests/r11/deploy_recovery.sh`（无 Docker） | **3** | NOT_RUN: Docker unavailable |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署、未启动 Docker；未修改仓库代码/文档/证据（临时 worktree 仅审查用）。

---

## 6. 结论 / docs-only commit

**最终：R14_1_OFFLINE_CLOSED（P0=0/P1=0/P2=0/P3=8）。** 离线安全 closure 可接受；Authorized Live 明确 NOT_RUN，publish skipped，PR Draft/OPEN，R14.1 docs/evidence 未提交，这些使本结论不是 `LIVE_READY`。

### docs-only commit

- 可以作为最终 PR head，前提仅 `docs/**`/evidence（不改代码/workflow/scripts/Makefile/配置）；R14.1 报告/evidence 准确反映 `5380063`（实际 run IDs、deploy 16/16、ARM64 RUNTIME_SERVICE_PASS、same-artifact pass=14、publish skipped、Authorized Live NOT_RUN、P3 限制）；docs-only head 最终 CI 全 success。
- 由于 R14.1 docs/evidence 尚未提交，提交后核对 diff 仅 docs/evidence；若夹带代码/workflow 则必须重新 review。即使 docs-only CI 绿，也不能宣告 `LIVE_READY`。

### 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `5380063`；树 `7f5744b9…`；`git ls-tree -r 5380063 \| sha256sum` = `ac99db9d…` |
| 文档/证据树 | `5cf44118…`；docs manifest `cd78cdb6…`；**无 `docs/acceptance/R14.1.md`/evidence** |
| 关键文件 | `queue.go` `b17c7d5a…`；`queue_test.go` `74a568df…`；`config.go` `6beccbfa…`；`candidate_identity.sh` `09fb9c32…`；`ops.go` `cbd31bfa…`；`server.go` `fa0a7be5…`；runbook `35274cad…` |
| 工作树 | 本审查文件新增；R14.1 代码未 checkout；R14.1 docs/evidence 未提交 |
