# 独立对抗性审查报告 — R11 Integration Readiness / Controlled Live Test（REVIEW-R11，post-fix）

- 复审时间：`2026-10-25T20:45–21:05`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI HEAD `0c4de93`**；**文档/证据 HEAD `887db42`**（分支 `feat/r11-integration-readiness`，base R10 code `d9bcc6f`/docs `4dcb091`，main `9726fc1`）；工作树仅本审查文件与既有 `tests/r10/__pycache__` 未跟踪，其余干净。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry/arm64 证据来自 GitHub Actions；用只读 `gh api` 与下载的 run/artifact 证据核验。
- **最终结论：R11 = PASS；P0 = 0，P1 = 0，P2 = 0，P3 = 8。`CONTROLLED_LIVE_TEST_READY` 在文档边界内成立：工具/dry-run/fail-closed/授权前准备就绪，不代表 Authorized Read-Only Live 已执行。`LIVE_READY` 仍不宣告。**
  - 首轮唯一 P2（strict attestation 未强制恰好一个 subject）已修复并独立复核：validator 现拒绝 `matching-sha + extra-subject-without-digest`；新增负向测试 9/9；dry-run 在 `0c4de93` 15/15 且 `ATTESTATION_VALID ... commit=0c4de93…`。
  - Cloudflare preflight 现明确 `redaction_result=NOT_RUN`，mock 目标强制 loopback；R11.md 已更新相应声明。
  - Authorized Read-Only Live 仍 **NOT_RUN**；真实 Cloudflare/GitHub/WorkBuddy、生产 TLS/ACME/HSTS、GHCR push/sign/attest、正式 Release/Tag 均未执行，`LIVE_READY` 不成立。

---

## 1. P2 关闭复核（首轮唯一阻断项）

### P2-1 — strict attestation 必须恰好一个 subject/一个 sha256：已修复

- `tests/r10/validate_attestation.py` 现在：
  - `len(subjects) == 1`；
  - `subject.digest` 必须是 dict 且恰好一个算法；
  - 该算法必须是 `sha256` 且值非空、完整匹配镜像 digest；
  - 仍要求 `predicate.Data`（dict 或 JSON string）中完整 commit/registry_digest 和 cosign predicate type。
- 新增 `extra-subject-no-digest` 负向用例；`tests/r11/attestation_validator_test.sh` **9/9 PASS**。
- 独立探针（/tmp，不改仓库）：matching sha + 一个无 digest 的额外 subject → validator **exit 1**、`ATTESTATION_INVALID: expected exactly one subject (got 2)`。
- post-fix dry-run run **40192593098**：15/15 PASS，日志 `ATTESTATION_VALID subject_digest=sha256:e81e72… commit=0c4de93…`。
- **P2-1 CLOSED；P2 总数 = 0。**

### 附带修复：Cloudflare 报告/模式边界

- `scripts/cloudflare_preflight.sh` 报告现在显式 `redaction_result=NOT_RUN`；mock-live 目标 host 强制 `127.0.0.1|localhost|::1`。
- R11.md:48 已改为“恰好一个 subject、其 digest 恰好一个 sha256”；dry-run 证据更新为 9 个测试/15 项。R11.md:60 已注明 redaction NOT_RUN 与 mode B loopback。

---

## 2. P0 — UNRESOLVED 决议语义（保持 PASS）

| 检查 | 结果 |
|---|---|
| UNRESOLVED 不关闭 | 只 `version+1` + 原子 `external_operation.review` audit，保持 OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED/CONFIRMED，不进入 RESOLVED |
| 队列/全局计数 | `TestUnresolvedReviewKeepsPendingQueueAndAudit`：仍在列表、`TotalNeedsReconciliation=1` |
| 不能重试 | UNRESOLVED 后 `BeginExternalOperation` 仍被 blocked；不会触发 provider 调用 |
| 重复审核 | 新 version、独立 audit；queue/count 保持 |
| 只有三种决议关闭 | RESOURCE_CONFIRMED / NO_RESOURCE_CREATED / MANUAL_COMPENSATION_COMPLETED 才走 RESOLVED；CONFIRMED 禁止 NO_RESOURCE_CREATED；IN_FLIGHT 拒绝 |
| 原子性/并发 | 状态与 review audit 同事务；version CAS；audit 失败回滚 |
| 审计/历史 | API `/v1/ops/external/{id}/audit` admin-only；UI current status 与 history 分离；HTML 转义/CSRF |

**P0：0 findings。** UNRESOLVED 不会消失/关闭/变成重试许可；RESOLVED 只由三种确认动作产生。

---

## 3. P1 — Engine fail-closed / API/UI 安全（保持 PASS）

- `Engine.Start` 先执行 `RecoverExternalOperations`；失败返回 error 且零 Worker（`started=false`），main 作为致命启动错误；HTTP listener 尚未 Serve。`TestEngineStartFailsClosedWhenRecoveryFails` 覆盖。
- `Stop` 排空 Worker 后返回 recovery error；Start 失败后的 Stop 安全 no-op；重复 Start 返回 `ErrEngineAlreadyStarted`（atomic CAS + 测试）；recovery audit 失败回滚（`TestRecoveryFailsClosedWhenAuditWriteFails`）。
- API/UI：resolve/audit admin-only；viewer 403、匿名 UI 重定向、无 CSRF 403；detail `<script>` 转义；reason/evidence 校验与 Redactor 保留。
- UI list 仍对 PREPARED/IN_FLIGHT 显示 resolve 表单（API 会拒绝），detail 仅 pending 显示表单、CONFIRMED recovery 未暴露（P3）。

**P1：0 findings。** 未发现 fail-closed 绕过、恢复前 Worker/provider 调用、viewer/CSRF/XSS 绕过。

---

## 4. P3（8 项，不阻断本轮边界）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | `docs/acceptance/R11.md:9` | 第 1 节标题仍写 `HEAD dad3e33`，而顶部/表格/证据已是 `0c4de93`；文档一致性需修正。另 R11.md:43 的 “filter 保持”无自动化测试，R11.md:28 的“版本竞态”无显式并发测试 |
| P3-2 | `scripts/cloudflare_preflight.sh`；R11.md:60 | Authorized Read-Only Live 仍 NOT_RUN；mock 目标 loopback 限制无单独负向测试；mode B 仅 CI 使用；2xx body 语义/CSRF 无关；redaction_result=NOT_RUN 诚实但未真实测量 |
| P3-3 | `.github/workflows/r11-integration.yml:82-87` | ARM64 runtime 失败时写 `RUNTIME_NOT_RUN` 但 job 仍 success；本次证据确为 `RUNTIME_PASS`，但未来 runtime 回归不会失败。smoke 只跑 `version`，未跑 API/首次安装 |
| P3-4 | `tests/r11/deploy_recovery.sh:54-70` | missing-secret/read-only/corrupt 用例忽略 `start` 失败，只要 readyz !=200 即 PASS；可能因端口/镜像/无关启动错误假 PASS。目录 777 非生产权限；恢复后 external_operation 状态未断言 |
| P3-5 | `internal/api/templates/ops_external.html:29`；`ops_external_detail.html:15`；`ui_ops.go` | list 对 PREPARED/IN_FLIGHT 显示 resolve 表单但 API 拒绝；detail 仅 pending 有表单，CONFIRMED recovery 无 UI；audit 未知 ID 返回 200 空列表；详情当前 reason 仍原样显示（HTML 转义但非 redact）；filter 保持无测试 |
| P3-6 | `internal/workflow/engine.go:196-220` | Stop 在 recovery 前 started=false，第二次 Stop 返回 nil、首次 stop recovery 错误丢失；Stop 后 Start 仍可启动（stopCh 已关闭，Worker 立即退出）；并发 Start/Stop 未测；main errCh 路径只记录 Stop 错误 |
| P3-7 | `tests/r10/validate_attestation.py`；storage/audit | 除已修 subject 外，UNRESOLVED 无显式并发 version-race 测试；`ListExternalOperationAudit` 用 `json_extract` 全表扫描、未知 ID 无 404；candidate 与 dry-run 独立构建，未强制同一 artifact |
| P3-8 | 发布/平台范围 | Publish 未执行；arm64 未跑完整 staging/backup；真实 Cloudflare/GitHub/WorkBuddy、生产 TLS/ACME/HSTS、GHCR push/sign/attest、正式 Release/Tag 均 NOT_RUN（R11.md 已列） |

---

## 5. post-fix CI 与证据（`0c4de93`）

| Run/工作流 | 结果 | 证据 |
|---|---|---|
| r11-integration **40192593181** | success/head `0c4de93` | deploy **11/11**、Cloudflare mock-live PASS、ARM64 `RUNTIME_PASS linux/arm64`；jobs/artifacts 见 run |
| staging **40192593104** | success/head `0c4de93` | **27/27** + container **63/63** |
| supply **40192593089** | success/head `0c4de93` | PASS；base digests 单一 canonical |
| r9-release **40192593256** | success/head `0c4de93` | rollback **20/20**、Caddy **18/18** |
| dry-run **40192593098** | success/head `0c4de93` | **15/15**；`ATTESTATION_VALID ... commit=0c4de93…`；registry immutable valid-different manifest → 400 |
| release-image **40192593177** | success/head `0c4de93` | candidate PASS、publish **skipped**；无 GHCR push |

- 证据目录 `r11-deploy/` 11/11；`r11-arm64/version.txt` RUNTIME_PASS + commit `0c4de93`；`r11-cloudflare/mock.log` PASS（TLS/Bearer/GET/redirect/403/429）、dry-run/self-test；`r11-candidate/R11-candidate-summary.txt` commit `0c4de93`、真实 Trivy target `account-onboarding:candidate`、实际 scanned_at/DB 时间。
- 6 个 run 的 `r11-ci/*.json` 均为 success/head `0c4de93`；无 GHCR/live 平台执行。

---

## 6. 本地命令与退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试 |
| `go test ./... -race -count=1` | **0** | 29 包全绿 |
| `go test ./...` 计数 | **0** | **370 顶层（365 PASS + 5 SKIP）/688 含子测试/0 FAIL**（与 R11.md 一致） |
| `go test ./internal/storage -run 'TestUnresolvedReview|TestRecoveryFailsClosed|TestResolve|TestAdminResolve' -race -count=3` | **0** | UNRESOLVED/recovery/audit fencing |
| `go test ./internal/workflow -run 'TestEngineStart|TestEngineStartStop|TestExternalOperation' -race -count=3` | **0** | fail-closed lifecycle |
| `go test ./internal/api -run 'TestReconciliationUI|TestOpsAlerts|TestExternalOperation|TestOpsMetrics' -race` | **0** | UI/auth/RBAC/XSS/audit/alerts |
| `go test ./tests/r11 -run 'TestCloudflare' -race -count=3` | **0** | mock TLS/GET/redirect/403/429 |
| `go test ./internal/alerts -race` | **0** | alert rules |
| `bash tests/r11/attestation_validator_test.sh` | **0** | **9/9 PASS**（含 extra-subject-without-digest） |
| 独立 extra-subject 探针 | **1（预期）** | `ATTESTATION_INVALID: expected exactly one subject (got 2)` |
| `bash tests/r11/deploy_recovery.sh`（无 Docker） | **3** | NOT_RUN: Docker unavailable |
| `bash scripts/cloudflare_preflight.sh --self-test` | **0** | fail-closed PASS |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |
| R11 证据/文档 `redact_scan` | **0** | 无 findings |
| `gh api` / artifact 证据 | 只读核验 | run maps/artifacts/日志 |

> 未触发/重跑 Actions、未 push/部署、未启动 Docker、未改代码/文档/证据。

---

## 7. `CONTROLLED_LIVE_TEST_READY` 最终判定

| 关闭标准 | 结果 | 说明 |
|---|---|---|
| P0 UNRESOLVED | **0** | pending/queue/count/version/audit；三种确认决议关闭；不重试 |
| P1 Engine fail-closed/API/UI | **0** | recovery 失败零 Worker + main fatal；Stop error；重复 Start；auth/CSRF/XSS |
| P2 strict attestation | **0** | 恰好一个 subject/一个 sha256；extra subject 拒绝；9/9 + dry-run 15/15 |
| 首次部署/恢复 | **PASS（P3-4 语义）** | 11/11；失败用例可再强化 |
| ARM64 build/runtime | **PASS（P3-3 语义）** | 本次 `RUNTIME_PASS`；runtime 失败不会使 job 失败 |
| Cloudflare dry/mock/fail-closed | **PASS（边界内）** | mock/self-test/dry-run；Authorized Read-Only Live **NOT_RUN**；redaction NOT_RUN |
| Candidate/publish | **PASS（Publish skipped）** | candidate 真实扫描；publish 静态 gate，无 GHCR |
| 文档诚实性 | **PASS（P3-1 小问题）** | LIVE_READY 不宣告、authorized live NOT_RUN 明确；第 1 节标题 stale |

**最终：R11 = PASS；P0/P1/P2=0。`CONTROLLED_LIVE_TEST_READY` 在“工具/dry-run/fail-closed/授权前准备就绪”的文档边界内成立；它不是 Authorized Read-Only Live 已执行的证明。`LIVE_READY` 仍不宣告**（真实 Cloudflare/GitHub/WorkBuddy、生产 TLS/ACME/HSTS、GHCR push/sign/attest、正式 Release/Tag 均 NOT_RUN）。

P3-1/P3-2 等剩余项不影响本轮 PASS；进入真实平台验收前应优先完成授权只读 live（mode C）、arm64 全回归/失败门禁强化、deploy 假 PASS 修正、UI/Engine 边界与 candidate/publish 同一 artifact 一致性。

---

## 8. 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `0c4de93`；树 `aeddcf22…`；`git ls-tree -r 0c4de93 \| sha256sum` = `584d1be6…` |
| 文档/证据 HEAD | `887db42`；树 `4d16ff15…`；`git ls-tree -r 887db42 \| sha256sum` = `89c7ff70…` |
| 工作树 | 本审查文件 + 既有 `tests/r10/__pycache__` 未跟踪；其余干净 |
| `docs/acceptance/R11.md` | `d61ee1a2…` |
| 证据 | dryrun `a6624e71…`；cloudflare mock `abb4ea29…`；candidate summary `929f6276…`；arm64 version `1f62a56c…`；deploy `86d5dd74…`；r11-ci `b9aef6c2…` |
| 代码 | validator `9ed787cb…`；preflight `39c2acd2…`；attestation test `3c01f891…`；external_operations `dd85e6df…`；engine `c8e5492d…` |
