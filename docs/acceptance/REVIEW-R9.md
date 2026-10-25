# 独立对抗性审查报告 — R9 Release Hardening（REVIEW-R9）

- 审查时间：`2026-10-25T15:59–16:20`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；本轮只新增本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI 证据 HEAD `5f13d78`**，**文档/证据 HEAD `b199e2a`**（`feat/r9-release-hardening`，base R8 `de89878`，main `9726fc1`）
  - 审查期间工作树 HEAD 已前进到 `fd80b54`/`3b46b99`（仅 `.github/workflows/release-image.yml` 的 candidate scanner 步骤与证据路径，+24 行）；这些提交**不在冻结的 CI 证据内**，也无对应 CI run。本报告按 Lead 指定的 `5f13d78`/`b199e2a` 冻结；`fd80b54` 仅在 P1-1 修复建议中提及。
  - 工作树审查时干净；本文件是唯一新增未跟踪文件。
- 环境：macOS arm64；本地 Docker daemon **不可用**（`docker info` exit 1；未启动/安装）；Docker/TLS/Caddy/rollback 证据均为 GitHub Actions 结果。
- **结论：R9 = P1-blocked（`RELEASE_CANDIDATE_READY` 在冻结证据下不成立）；P0 = 0，P1 = 1，P2 = 3，P3 = 10。**
  - 外部写入门禁（migration 0004、CAS、粘性状态）与 staging/container/rollback/Caddy 运行时证据基本真实；但 **release candidate 的“Trivy/govulncheck/ShellCheck”声明在冻结 CI 中没有任何 candidate 扫描步骤/证据**（P1-1），且 **publish 路径也不重跑/断言这些扫描**。这是 release-hardening 门禁完整性问题，必须修复并在最终代码上重新跑 release-image CI 后才能宣告 RC。
  - `LIVE_READY` 仍不宣告：真实 Cloudflare/GitHub/WorkBuddy、生产 TLS/ACME、GHCR push/signing、正式 Release/Tag 均 NOT_RUN。

---

## 1. P1（1 项，阻断 RC 宣告）

### P1-1 — Release Candidate 未真正执行所声称的漏洞/lint 扫描；扫描证据来自另一镜像的另一 run

- **位置**：
  - [`docs/acceptance/R9.md:84`](R9.md)（§7：“Candidate … SBOM/govulncheck/Trivy/ShellCheck”）
  - [`docs/acceptance/R9.md:109`](R9.md)（§11：把 `r9-supply/` 称为 “Candidate govulncheck/Trivy/ShellCheck/SBOM 摘要”）
  - 冻结 `5f13d78` 的 [`.github/workflows/release-image.yml`](../../.github/workflows/release-image.yml) candidate job：只有 Verify source、Unit/race/targeted、Docker build、`scripts/supply_chain.sh`（SBOM/provenance）、Publish guard、Upload（`git show 5f13d78:.github/workflows/release-image.yml` 行 42–89；无 `govulncheck`/`trivy`/`shellcheck`）
  - 同文件 publish job 的 “Re-scan loaded image” 只调用 `scripts/supply_chain.sh`（行 133–136；该脚本只做 SBOM/metadata，不做漏洞扫描）
  - 证据：`docs/acceptance/evidence/r9-candidate/` 只有 commit/image/provenance/version/summary，**无 govulncheck/trivy/shellcheck 文件**；`r9-supply/metadata.json` 的 `image_id=ae358b…`，而 candidate `image-id.txt=sha256:843cc4…` —— **两个不同镜像**。
- **触发/复现（独立核验，不只读提交 JSON）**：
  1. GitHub API `actions/runs/40173893716/jobs`：candidate job steps = Set up、checkout、setup-go、Verify source、Unit/race/targeted、Build candidate、Supply-chain gates/SBOM/provenance、Publish guard、Upload；**无扫描步骤**；publish job `skipped`。
  2. `actions/runs/40173893822/jobs` 的 supply-chain job 才执行 govulncheck/Trivy/ShellCheck；其 artifact `supply-chain-evidence-40173893822` 对应 supply image，不是 candidate image。
  3. 冻结 run 40173893716 的 candidate artifact `release-candidate-5f13d78…` 可下载，但不含扫描日志；publish job（若 `publish=true`）只校验 commit/image hash + 调 `supply_chain.sh`，**不会对 candidate 镜像做 Trivy，也不校验扫描日志存在**。
- **影响**：在冻结代码/CI 下，“Candidate PASS（含 Trivy/govuln/ShellCheck）” 不成立；publish 路径可发布一个**未被 Trivy 扫描的候选镜像**（govuln/shellcheck 是 source-level，由 supply run 覆盖同一 commit；Trivy 是 image-level，supply image 与 candidate 镜像不同）。R9 的 release-hardening 门禁因此被高估；属发布供应链门禁不完整，而非纯文档瑕疵。**这是 P1，阻断 RC 宣告。**
- **根因**：candidate workflow 只实现 SBOM/provenance；R9.md 把独立 supply-chain run 的扫描结果错误归到 candidate；publish job 的 “Re-scan” 只重跑 SBOM。
- **修复**：
  - 在 candidate job 真正执行并归档 `govulncheck@v1.1.4`、`shellcheck -S error`、Trivy v0.76.1（CRITICAL block/HIGH report，`--ignore-unfixed`），写入 `candidate/supply/*`；
  - publish job 断言 `candidate/supply/govulncheck.txt`、`trivy-critical.txt`、`trivy-version.txt` 存在且 CRITICAL 为空（或重跑同一套扫描后再 push）；
  - 在 **最终代码**（如采纳 `fd80b54` 的 candidate scanner 步骤）重跑 release-image，归档新 run/artifact，更新 R9.md §7/§11 的 run/镜像绑定；不得再用 supply run 的日志冒充 candidate。
- **回归/测试**：candidate job 必须在任一 scanner 非 0 时失败；publish job 缺少扫描证据时 fail closed；CI 证据中 candidate 与 supply 的 image ID 必须分别绑定；发布前再次核对。
- **残余**：即使修复，candidate 仍只在本地 artifact、无 registry manifest digest；Publish/signing 未运行（本轮默认 skip）仍是 NOT_RUN。

---

## 2. P2（3 项）

### P2-1 — `resolve` 允许对 `IN_FLIGHT` 操作在手写结论时与仍在运行的 worker 竞争

- **位置**：[`internal/storage/external_operations.go:427-459`](../../internal/storage/external_operations.go)（`ResolveExternalOperation` 的 `WHERE … status IN (IN_FLIGHT, OUTCOME_UNKNOWN, RECONCILIATION_REQUIRED)`）；API [`internal/api/ops.go:60-95`](../../internal/api/ops.go)；runbook 只给示例，未要求先停引擎。
- **触发**：worker 已 `MarkExternalOperationInFlight`（持有 lease）但 provider POST 尚未返回；admin 对同一 op 执行 `resolve`（例如 `NO_RESOURCE_CREATED`）。
- **复现（独立探针）**：`Begin → Mark` 后直接 `ResolveExternalOperation(..., NO_RESOURCE_CREATED)` 返回 **RESOLVED**；随后 worker 的 `CompleteExternalOperation` 返回 `conflict: completion raced or operation is not IN_FLIGHT`。若 provider 调用已在途/随后完成，远端 mailbox 已创建，但操作显示“人工确认无资源”。
- **影响**：违反人工对账的语义；远端副作用可能发生在“无资源”决议之后，形成未跟踪资源；不产生本地数据损坏，但正是 reconciliation 要避免的类别。
- **修复**：`resolve` 对 `IN_FLIGHT` 增加 lease/活性检查（例如要求 `runs.lease_until_ms < now` / 无 owner，或要求显式 `force` + 停止引擎的 runbook 前置条件），或提供独立的“stale in-flight”命令；补 resolve-vs-live-worker 测试（当前测试只在 `OUTCOME_UNKNOWN` 后 resolve，见 `external_operations_test.go:126-159`）。
- **残余**：对崩溃/过期 lease 的 IN_FLIGHT 仍需可恢复；检测后的人工流程应写入 runbook。

### P2-2 — Step commit 失败发生在 `CONFIRMED` 之后：操作是 CONFIRMED（不是 IN_FLIGHT），run 无法通过 API 恢复

- **位置**：[`internal/workflow/steps.go:117`](../../internal/workflow/steps.go)（先 `completeExternalOperation(..., ExternalConfirmed)`，再返回 Outcome）；[`internal/workflow/engine.go:231`](../../internal/workflow/engine.go)（之后才 `CommitStepResult`）；[`ResolveExternalOperation`](../../internal/storage/external_operations.go) 只允许 IN_FLIGHT/OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED；[`R9.md:42`](R9.md) 写“完成失败/commit 失败保持 IN_FLIGHT（保守，不重试）；成功先 CONFIRMED 再返回步骤结果”。
- **触发**：provider 成功 → `CompleteExternalOperation(CONFIRMED)` 提交成功 → 随后 `CommitStepResult` 因 DB 错误/lease lost/crash 失败；或进程在两步之间崩溃。
- **影响**：外部操作保持 `CONFIRMED`（远端 mailbox 存在），run 未提交该 step；重试时 `BeginExternalOperation` 因 CONFIRMED 被 blocked，`classifyError` 默认终态 → run `FAILED_TERMINAL`；`resolve` 不接受 CONFIRMED，操作员无 API 路径标记/恢复。**没有第二次 provider 调用**（gate truth 成立），但存在 stranded run/remote resource，且 R9.md 的“commit 失败保持 IN_FLIGHT”说法与实现不符。
- **修复**：让 CONFIRMED 与 step commit 原子化/outbox，或为“CONFIRMED + 失败 run”提供明确的人工恢复/对账动作（含审计），并加故障注入测试；文档如实描述该窗口与人工流程。
- **残余**：即使不修，安全上不会重复创建；但可运维性/一致性声明必须修正。

### P2-3 — `GET /v1/ops/metrics` 相关声明可接受；Caddy 的 HTTP→HTTPS 308 目标端口是容器内 8443，而非发布端口

- **位置**：[`tests/r9/Caddyfile:38`](../../tests/r9/Caddyfile)（`:8080 { redir https://localhost:8443{uri} 308 }`）；[`tests/r9/caddy_acceptance.sh:76-77`](../../tests/r9/caddy_acceptance.sh) 断言 `308 https://localhost:8443/healthz`；同脚本 `:59` 把 HTTPS 发布到动态 `$HTTPS_PORT`（如 45917）。
- **影响**：外部客户端访问发布的 HTTP 端口会被重定向到未发布的 8443，实际不可用；Caddy 17/17 的“308”检查因此只验证了容器内端口，未验证 production-shaped 的公网重定向。R9.md §6 称覆盖 HTTP→HTTPS 308 但未提该限制；`X-Forwarded-Host/Port` 已用 `R9_PUBLIC_HOST/PORT` 正确注入，唯独 redirect 未用。
- **修复**：Caddyfile 用 `{$R9_PUBLIC_HOST}`（或至少 `{}` 环境端口）构造 redirect；测试断言动态公网端口；重跑 Caddy CI。
- **残余**：真实 ACME/DNS 仍 NOT_RUN。

---

## 3. P3（10 项）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | [`internal/storage/external_operations.go:463-465`](../../internal/storage/external_operations.go)；[`internal/security/redact.go`](../../internal/security/redact.go) allowlist | `resolve` 的 audit detail 用 `SanitizeEventPayload`，但 allowlist 不含 `resolution`/`evidence_reference`；独立探针显示 audit detail 只剩 `{reason:…}`，决议枚举/证据引用丢失。权威行在 `external_operations` 表，审计日志不完整；建议 allowlist 增加这两键或落显式列，测试 audit 字段 |
| P3-2 | [`internal/storage/external_operations.go:329-333`](../../internal/storage/external_operations.go)；[`0004_external_operations.sql:30-35`](../../internal/storage/migrations/0004_external_operations.sql)；[`external_operations_test.go:233`](../../internal/storage/external_operations_test.go) | 无 status/provider/run 过滤的 pagination `ORDER BY created_at_ms DESC, id DESC` 没有 `(created_at,id)` 索引；query-plan 测试只覆盖 `status=?`；10k 行测试通过但无索引证明。建议加 `idx_external_ops_created` 或把 query-plan 测试扩到无过滤路径 |
| P3-3 | [`internal/workflow/external_ops.go:31-40`](../../internal/workflow/external_ops.go) | `ExternalCompletionStatus` 的 `default` 是 `FAILED_DEFINITE`（可重试）；显式 fail-safe 应白名单“确定未写”的 class，未知/新 class 默认 `OUTCOME_UNKNOWN`。当前已知 class（Validation/Auth/RateLimit/NotFound）大体安全，但默认方向是 fail-open；建议白名单 + 未知 class 测试 |
| P3-4 | [`internal/storage/external_operations.go:247-285`](../../internal/storage/external_operations.go) | `CompleteExternalOperation` 只 CAS `status=IN_FLIGHT`，不校验 lease owner/generation；虽然是单一 permit，但建议在完成时也校验 fencing，或明确记录“完成不依赖 lease”的设计理由 |
| P3-5 | [`internal/storage/external_operations.go:451-459`](../../internal/storage/external_operations.go)；[`internal/api/ops.go:77-84`](../../internal/api/ops.go) | 不存在的 op id 与版本冲突都返回 409（无 ErrNotFound 分支）；建议 missing id → 404，非法 status/limit → 400，便于 ops 客户端区分 |
| P3-6 | [`release-image.yml`](../../.github/workflows/release-image.yml)（冻结行 151-159） | publish job 的 cosign 二进制用 `curl` 下载后直接执行，无 checksum/签名校验；P1-1 修复后，publish 的 “Re-scan” 也应断言 scanner 证据而非只 `supply_chain.sh` |
| P3-7 | [`tests/r9/caddy_acceptance.sh:59`](../../tests/r9/caddy_acceptance.sh) | Caddy 容器以 root + `NET_BIND_SERVICE` 运行（官方 binary file capability 限制），且 Caddyfile Host 固定 `localhost`；作为测试夹具可接受，但不应被误读为生产加固；建议注释/文档明确 |
| P3-8 | [`tests/r9/old_image_rollback.sh:114,158`](../../tests/r9/old_image_rollback.sh) | 旧镜像拒绝 schema4/future 的判断依赖固定 `sleep 5/4` + logs grep；容器退出很快，CI 通过，但可改为 `docker wait`/轮询 exit，减少时序依赖。20/20 内容本身有意义（旧镜像拒绝、备份恢复、run/revocation 保留、wrong-key 不覆盖），限制（仅相邻版本）R9.md 已如实说明 |
| P3-9 | [`scripts/ops_external.sh`](../../scripts/ops_external.sh) | Bearer token 通过 `curl -H "Authorization: Bearer $TOKEN"` 放在 argv（进程列表可见），虽然不打印；建议用 `--header @file` 或 stdin，并保持 token 文件 0600；`list` 的 query 参数也未 URL-encode |
| P3-10 | [`internal/storage/metrics.go`](../../internal/storage/metrics.go) | `provider_error_classes` 实际来自 `steps.error_class`（含本地/引擎 class），并非纯 provider；固定标签/无 PII 成立，但命名/文档应澄清。另 `SQLiteBusyRetryCount` 为 nil（未插桩）已在字段注释说明 |

---

## 4. GitHub Actions 独立核验（不只读提交 JSON）

| 项 | 核验结果 |
|---|---|
| staging run `40173893796` | API：success，`head_sha=5f13d78…`，2 jobs（staging、container-regression）均 success；日志 27/27、63/63；artifacts `staging-acceptance-evidence-40173893796`（13,289 B）、`r8-container-regression-40173893796`（5,388 B）存在 |
| supply run `40173893822` | API：success，`head_sha=5f13d78…`；job steps 含 govulncheck/ShellCheck/Syft/Trivy；artifact `supply-chain-evidence-40173893822`（367,965 B）存在；govulncheck “No vulnerabilities found”、ShellCheck 空（0 errors）、Trivy v0.76.1 CRITICAL/HIGH 0、SPDX-2.3 97 packages、cdx、base digests 已记录（R8 P2-2 修复） |
| r9-release run `40173893779` | API：success，2 jobs（caddy-proxy、old-image-rollback）success；日志 17/17 + 1 WARN、20/20；artifacts `r9-caddy-proxy-evidence`（4,897 B）、`r9-old-image-rollback-evidence`（5,267 B）存在；旧镜像 ref `74d2c4e` 确为 R8 schema-3 commit（migration 0001–0003），非当前 R9 |
| release-image run `40173893716` | API：success，candidate job success，publish job **skipped**（默认 false/受保护 Environment）；candidate artifact `release-candidate-5f13d78…`（38,080,349 B）存在；**candidate job 无扫描步骤（P1-1）** |
| 过度宣称 | 无 `LIVE_READY`；R9.md/FINAL/README/index 均 `RELEASE_CANDIDATE_READY (pending review)`；无 GHCR push/registry digest；Publish NOT_RUN；旧镜像回滚限制已注明 |
| 本地计数 | `350 顶层（345 PASS+5 SKIP）/662 含子测试/0 FAIL` — 与 R9.md 一致 |

---

## 5. P0 门禁真实性（逐路径）

| 路径 | 结论 |
|---|---|
| PREPARED crash（Begin 后、Mark 前） | 新 worker 可 `Begin` 复用 PREPARED，`Mark` CAS 唯一 winner；旧/新 worker 都不会在 loser 路径调用 provider（workflow 返回 conflict）——**无重复调用** |
| CAS loser（并发 Mark） | 存储层 `RowsAffected != 1` 返回 blocked；`beginExternalOperation` 包装为 conflict；提供方调用只在 Mark 成功后发生 —— **无重复调用**；测试 8 并发 1 winner |
| 完成失败（Complete 返回错误） | 行保持 `IN_FLIGHT`，后续 Begin blocked —— 保守；无自动重试 |
| Step commit 失败（CONFIRMED 之后） | 行保持 `CONFIRMED`，重试 blocked，**不会二次调用 provider**；但产生 stranded run/remote resource（P2-2） |
| 取消（provider 调用中） | `ClassCancelled` → `OUTCOME_UNKNOWN`；测试 `TestExternalOperationCancellationIsUnknown`；无重试 |
| Retry/restart | 仅 `FAILED_DEFINITE` 可再次 Begin；其余状态 blocked；`TestExternalOperationSurvivesRestartAndAudit`；**无重复调用**（前提是 error class 正确，见 P3-3） |
| Resolve 后 | Begin 仍 blocked；但 `resolve` 允许 live `IN_FLIGHT`（P2-1），不构成二次调用，却可 mislabel 在途远端写入 |
| `/readyz` | schema>=4 + `external_operations` 表 + session revocation + WAL + writable；`ExternalOperationsReady` 检查；旧二进制对 schema4 拒绝（rollback 20/20） |

结论：**没有发现“未知/在途/已确认/已解决后再发第二次 provider 调用”的路径**；P0 门禁主体成立。P1-1 是发布扫描门禁问题，不是外部写入重复调用。

---

## 6. 本地独立执行的命令与退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试（本地） |
| `go test ./... -race -count=1` | **0** | 29 包全绿 |
| `go test ./... -v` 计数 | **0** | **350 顶层（345 PASS+5 SKIP）/662 含子测试/0 FAIL** |
| `go test ./internal/storage -run 'TestExternalOperation' -race -count=3` | **0** | 门禁/分页/并发/Mark 测试稳定 |
| `go test ./internal/workflow -run 'TestExternalOperation' -race -count=3` | **0** | confirmed/timeout/duplicate/cancel 用例 |
| `go test ./internal/api -run 'TestExternalOperation|TestOpsMetrics' -race` | **0** | resolve/RBAC/metrics |
| `tests/r9/old_image_rollback.sh`（本地无 Docker） | **3** | `NOT_RUN: Docker daemon unavailable`（符合预期） |
| `tests/r9/caddy_acceptance.sh`（本地无 Docker） | **3** | 同上 |
| `tests/r8/staging_acceptance.sh`（本地无 Docker） | **3** | 同上 |
| `tests/r51/docker_acceptance.sh --self-test` | **0** | 11/11 |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |
| R9 证据/文档 `redact_scan`（34 文件） | 全部 **0** | 0 findings |
| GitHub API（4 runs + jobs + artifacts） | **200** | 见 §4 |

> 未触发/重跑 Actions、未 push/部署、未安装/启动 Docker、未改代码或宿主配置。
> 独立探针（未入库）确认：resolve live IN_FLIGHT 成功且随后 completion conflict；audit detail 只剩 reason；`ExternalCompletionStatus(CANCELLED)=OUTCOME_UNKNOWN`、`VALIDATION/NOT_FOUND=FAILED_DEFINITE`。

---

## 7. `RELEASE_CANDIDATE_READY` 判定与冻结

### 7.1 判定

| R9 关闭标准 | 结果 | 说明 |
|---|---|---|
| 外部写入 gate（P0） | **PASS** | 表权威 + CAS/lease + 粘性状态；无二次调用路径；P2-2 为 commit-window 可运维性限制，R9.md 该句需修正 |
| 分页/全局计数 | **PASS（性能 P3）** | 201/1000/10000、tie-break、global counts 测试通过；无过滤排序索引缺失为 P3 |
| Resolve CSRF/RBAC/version/audit | **PASS（P2-/P3-）** | admin-only、viewer 403、version CAS、重复 409、审计同事务回滚；live IN_FLIGHT 竞争 P2-1，audit 字段丢失 P3-1 |
| 旧镜像升级/回滚 | **PASS（范围限制已注明）** | 20/20；旧镜像 `74d2c4e` 确为 schema3；限制（相邻版本、非任意跨版本）已写明 |
| 真实 Caddy TLS | **PASS（WARN 已记录；P2-3）** | 17/17 + 1 WARN；TLS/HSTS/Host/cookie/CSRF/backend/证书轮换真实；HTTP redirect 端口问题 P2-3 |
| Release candidate/publish | **FAIL（P1-1）** | 冻结 CI 的 candidate 无扫描步骤/证据；publish 仅校验 image hash + SBOM；不可宣告 candidate 扫描通过 |
| 文档诚实性 | **PARTIAL** | R9.md 整体诚实（LIVE 排除、rollback 限制、WARN、no digest）；但 §7/§11 candidate 扫描声明不实（P1-1），§2 commit 失败句不准确（P2-2） |
| 独立审查 P0=0/P1=0 | **P1=1** | 本报告 |

**结论：在冻结的 `5f13d78`/`b199e2a` 下，`RELEASE_CANDIDATE_READY` 不成立/不应宣告。** 修复 P1-1（candidate 真正扫描 + publish 断言 + 最终代码重跑 release-image CI + 证据归档）并处理 P2-1/P2-2/P2-3 后，可重新评估。`LIVE_READY` 仍不宣告（生产 TLS/ACME、GHCR publish/signing、真实 Cloudflare/GitHub/WorkBuddy、正式 Release/Tag 均 NOT_RUN）。

### 7.2 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `5f13d78a0d3a2d34520eb1ad3a5ccd636b87bc42`；树 `b29f82a5fbc9403a98cd56f0992ddd28e2176f7b`；`git ls-tree -r 5f13d78 \| sha256sum` = `e4c5710f6faab3fdb0c16fdc0b17c5dc5afd4d935b450525771be7faf2af91b9` |
| 文档/证据 HEAD | `b199e2a`；树 `6af221fa6c0547edae2365947671b548df937484`；`git ls-tree -r b199e2a \| sha256sum` = `b2ac3424cd0edc3d266d5f05f4ae02ae590010d19a198b66d916a986745a2e7e` |
| 审查完成时工作树 HEAD | `3b46b99`（`fd80b54` + absolute evidence path；均仅 `release-image.yml` candidate scanner 步骤，+24 行；不在冻结 CI 证据内，无对应 run）；工作树干净（除本审查文件） |
| `docs/acceptance/R9.md` | `a2a6f2cb…` |
| 证据 | staging `3df6c0ff…`；container `626ba6de…`；rollback `12a722db…`；caddy `a9b4884f…`；candidate provenance `9980b1d5…`；`r9-ci/*.json` 四 run 均 success/headSha=5f13d78 |
| 代码 | `internal/storage/external_operations.go 1041d413…`；`external_operations_test.go e452ea09…`；`internal/workflow/external_ops.go 29d6fb06…`；`steps.go 1164d34b…`；`internal/api/ops.go a2b2ffcd…`；`server.go cb38869d…`；`store.go 81a7e1d1…`；`metrics.go 266f2234…`；`0004_external_operations.sql c3af1fef…` |
| 脚本/工作流 | `tests/r9/Caddyfile c05566a4…`；`caddy_acceptance.sh 114b2f36…`；`old_image_rollback.sh 42e58888…`；`scripts/ops_external.sh 916f0d8e…`；`release-image.yml`（冻结）`3a68c7d4…`（`fd80b54` 版本不同，未冻结） |
| 文档 | `R9.md a2a6f2cb…`；ADR `0acef3ad…`；runbook `f48736ca…` |
