# 独立对抗性审查报告 — R14 Security/Release Closure（REVIEW-R14，final）

- 复审时间：`2026-10-26T11:20–11:50`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI HEAD `098a457`**（PR #7，Draft/OPEN，base R13 `8febb90`）；R14 文档/证据仍**未提交**（无 `docs/acceptance/R14.md`/evidence）；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry/arm64 证据来自 GitHub Actions；本地 R14 代码在临时 worktree 执行。
- **最终判定：R14_PARTIAL（P0=0，P1=0，P2=0，P3=8）。**
  - 上一轮 queue P2 已关闭：`handleOpsAlerts` 调用 `AlertQueue.Reconcile(currentEvaluatedRules)`；recovered rules 清出 active/pending，re-armed rules 重新入队；新增 admin `POST /v1/ops/alerts/resolve`（CSRF/Origin，missing 404）；queue reconcile/re-arm 与 API resolve missing 测试通过。
  - 0450 secret 策略、publish→same-artifact dependency 前一轮已关闭。
  - 最终 CI 全部 success/head `098a457`；ARM64 artifact `RUNTIME_SERVICE_PASS ... restart=persisted`；deploy 16/16；release candidate+same-artifact success、publish skipped。
  - 限制：Authorized Live **NOT_RUN**、publish skipped、PR Draft/OPEN、R14 docs/evidence 未提交；故为 R14_PARTIAL，不是 R14_PASS。`LIVE_READY` 不得宣告。

---

## 1. P2 关闭复核（P2=0）

### 0450 secret policy

- ADR 0005 现明确 0400/0600 accepted、0450 rejected；`secret_file_test.go` 增加 `{0o450,false}`；code 与 ADR/test 一致。
- **closed。**

### publish same-artifact dependency

- `release-image.yml` `publish.needs: [candidate, same-artifact-dry-run]`；publish 下载 same-artifact evidence 并强制 `SAME_CANDIDATE_ARTIFACT=`、zero-failure summary、`account-onboarding:candidate`；publish_gate_audit 同步。publish 仍 skipped。
- **closed。**

### alert queue production re-arm

- `internal/alerts/queue.go` 新增 `Reconcile`：以当前 evaluated rule set 重建 pending/active，清 recovered，重新 enqueue re-armed；`handleOpsAlerts` 生产调用 `Reconcile(alerts)`。
- `internal/api/ops.go` 新增 `POST /v1/ops/alerts/resolve`；`apiAuth(true,true)` 提供 CSRF/Origin 约束；missing rule 404；queue `Resolve` 清 active+pending。
- `TestQueueReconcileRearmsRecoveredRules` 覆盖 recovered 清空、re-fire re-enqueue、empty evaluation 清 active/pending；`TestAlertResolveAPIRequiresActiveRule` 覆盖 missing 404。
- **closed。**

---

## 2. P0/P1 — 核心（clean）

- **Secret**：single `readSecureSecretFile`；regular/non-symlink；0400/0600；1..64KiB；fd/inode `SameFile`；all provider fields；错误不泄值；TOCTOU/replacement test。
- **Backup/restore**：真实 SQLite + AES-GCM；completed/failed/retry、revoked session、PREPARED/IN_FLIGHT/OUTCOME/RECON/RESOLVED、audit/schema5；wrong key/corrupt target untouched。
- **API**：并发同/不同 resolution 单 winner；audit redaction/limit/404；queue status admin-only；resolve missing 404。
- **Cloudflare contract**：valid+invalid manifest 由 Go normalizer 与 shell preflight 消费；source 明确 compatibility, not live schema。
- **Same-artifact**：candidate artifact image.tar 真实 load/identity/commit；publish 依赖 same-artifact；无 rebuild。
- **结论：P0=0，P1=0。** 未发现 secret 泄露/TOCTOU 绕过、remote 二次写、audit redaction 绕过或 publish 无 same-artifact 门禁。

---

## 3. P3（8 项残余限制）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R14 branch/docs | 无 `docs/acceptance/R14.md`/evidence；PR #7 Draft/OPEN。docs/evidence 尚未提交；最终 docs-only commit 后仍需确认仅 docs/evidence 且 CI success。 |
| P3-2 | Authorized Live | Authorized Read-Only Live **NOT_RUN**；contract manifest 明确不是 live Worker schema；approval runbook 是流程不是执行；publish 未执行、无 GHCR。 |
| P3-3 | Notifier | `AlertNotifier` 仍是 `RecordingNotifier`；admin drain 只记录、不外部发送；无生产通知 transport；queue resolve/drain 是本地 admin 操作。 |
| P3-4 | ARM64 | 本次 artifact `RUNTIME_SERVICE_PASS linux/arm64 schema=5 readyz=200 ... restart=persisted`；但仅单 service smoke，未跑完整 arm64 staging/backup/UI 回归。 |
| P3-5 | Same-artifact/candidate | same-artifact job 已用 candidate bytes；candidate 与 general dry-run 仍是两套 job；base digest 仅 presence check；publish skipped，未验证 GHCR/tag/release。 |
| P3-6 | Backup | 测试覆盖核心 states，但未覆盖 CONFIRMED external op、运行中 restore、backup retention/rotation；仍依赖 ops 函数而非真实 CLI。 |
| P3-7 | UI/API | API 并发/audit/queue status/resolve 已测；UI 无 two-admin/session/filter 自动化，detail current reason 仍原样（HTML 转义但非 redact）。 |
| P3-8 | Contract/secret 兼容 | valid+invalid manifest 已共享，但 `provider_field_cases` 未全部消费，parity 不是字段/错误/live schema；ACL/Docker 0444/K8s projected 0444 适配未实际测试。 |

---

## 4. 最终 CI（head `098a457`，全部 success）

| Run/工作流 | 实际 run id | 关键证据 |
|---|---|---|
| ci | **40261168202** | gofmt/build/vet/race/compose |
| r12-integration | **40261164460** | deploy **16/16**、Cloudflare mock、ARM64 **RUNTIME_SERVICE_PASS** (`schema=5 readyz=200 ... restart=persisted`) |
| staging | **40261164439** | staging success |
| supply | **40261164456** | supply PASS |
| r9-release | **40261164446** | caddy/rollback |
| dry-run | **40261164452** | OCI/sign/attest |
| release-image | **40261164534** | candidate + same-artifact success；publish **skipped**；无 GHCR push |

**注意**：Lead 提供的 run id 中 dry-run/release-image 与实际 API 不一致（实际 dry-run 40261164452、release-image 40261164534；40261164456 是 supply）；review 以实际 `gh api`/branch run 列表为准。所有 run head_sha=`098a457`。

---

## 5. 本地命令/退出码（临时 worktree `098a457`）

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试 |
| `go test ./... -race -count=1` | **0** | 全绿 |
| `go test ./...` 计数 | **0** | **406 顶层（401 PASS + 5 SKIP）/786 含子测试/0 FAIL** |
| `go test ./internal/alerts -run 'TestQueue'` | **0** | reconcile/re-arm/drop/resolve/concurrent |
| `go test ./internal/api -run 'TestAlert|TestConcurrent|TestAudit'` | **0** | queue resolve/status/API concurrency |
| `go test ./internal/config -run 'TestSecureSecretFileModes|TestAllProvider'` | **0** | 含 0450 rejection |
| `go test ./internal/ops -run 'TestEncryptedBackup|TestWrongBackup|TestCorrupt'` | **0** | backup/restore |
| `go test ./internal/provider/mailbox/cloudflare -run 'TestParsedMails'` | **0** | valid/invalid contract |
| `bash tests/r11/deploy_recovery.sh`（无 Docker） | **3** | NOT_RUN: Docker unavailable |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署、未启动 Docker；未修改仓库代码/文档/证据（临时 worktree 仅审查用）。

---

## 6. 最终判定 / docs-only commit

| 关闭标准 | 结果 | 说明 |
|---|---|---|
| P0 core/secret/backup/API | **0** | TOCTOU/symlink/backup/API/contract/same-artifact |
| P1 安全边界 | **0** | secret 无泄露、无 TOCTOU、无 remote 二次写 |
| P2 | **0** | 0450、publish dependency、queue reconcile/resolve 均关闭 |
| CI | **PASS** | 7 runs success/head 098a457；publish skipped；无 GHCR |
| Authorized Live | **NOT_RUN** | 不能当作 live 通过 |
| Docs/evidence | **待提交** | 无 R14.md/evidence；PR Draft/OPEN |

**最终：R14_PARTIAL（P0=0/P1=0/P2=0/P3=8）。** 这不是 R14_PASS：Authorized Live NOT_RUN、publish 未执行、PR Draft/OPEN、R14 docs/evidence 未提交。允许在下列限制下接受 R14_PARTIAL；`LIVE_READY` 仍不得宣告。

### docs-only commit

- 可以作为最终 PR head，前提仅 `docs/**`/evidence（不改代码/workflow/scripts/Makefile/配置）；R14 报告/evidence 准确反映 `098a457`（**实际 run IDs**、deploy 16/16、ARM64 RUNTIME_SERVICE_PASS、queue/notifier 限制、Authorized Live NOT_RUN）；docs-only head 最终 CI 全 success。
- 由于 R14 docs/evidence 尚未提交，提交后需核对 diff 仅 docs/evidence；若夹带代码/workflow，必须重新 review。即使 docs-only CI 绿，也不能宣告 `LIVE_READY`。

### 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `098a457`；树 `fb061e27…`；`git ls-tree -r 098a457 \| sha256sum` = `0c34145e…` |
| 文档/证据树 | `fcebaa50…`；docs manifest `37b66317…`；**无 `docs/acceptance/R14.md`/evidence** |
| 关键文件 | `queue.go` `2243537b…`；`api/ops.go` `91bdecf…`；`api/server.go` `c17b911…`；`api/r14_test.go` `03e9ed94…`|
| 工作树 | 本审查文件新增；R14 代码未 checkout；R14 docs/evidence 未提交 |
