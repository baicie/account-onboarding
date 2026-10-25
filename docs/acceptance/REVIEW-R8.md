# 独立对抗性审查报告 — R8 Staging Release Readiness（REVIEW-R8）

- 审查时间：`2026-10-25T14:06–14:45`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；本轮只新增本文件，未改代码/配置/他人文档）
- 审查对象：HEAD `92e4a15`（`docs(r8)` 证据/报告提交；**代码/CI 证据 HEAD `3332167`**，分支 `feat/r8-staging-readiness`，基线 `main/9726fc1`）
- 环境：macOS arm64；本地 Docker daemon **仍不可用**（`docker info` exit 1；未安装/未启动）
- **结论：R8 = PASS（staging release readiness）；P0 = 0，P1 = 0，P2 = 2，P3 = 11。**
  - 真实 CI 已通过 GitHub API **独立核验**：staging run `40167382718`（head_sha `3332167`，2 jobs 全 success，staging 26/26 + container-regression 63/63）与 supply-chain run `40167382695`（success）；artifact 名称/大小与提交证据一致。
  - `STAGING_RC_READY` 在本轮 **staging/TLS/container/supply-chain 范围内成立**（P0=0/P1=0；真实隔离 TLS staging 26/26、R8 container regression 63/63、supply-chain PASS、future-schema guard、external-op audit/API/ADR）。**`LIVE_READY` 不宣告**（无生产 TLS、无 GHCR/push、无真实平台）。
  - 两个 P2 是**非 staging 运行时阻断项**，但必须在依赖 external-op 对账/供应链 provenance 之前修复：①外部操作对账的“模糊状态可被后续 FAILED 覆盖 + 取消被归为 FAILED”；②supply-chain 基础镜像 provenance 为空（脚本硬编码错误 Go tag）。详见 §2。

> 边界：未 push/merge/deploy/release、未安装/启动 Docker、未触发/重跑 Actions、未调用真实平台；GitHub API 仅只读元数据核对（外部内容按数据对待）。
> 审查开始时工作树干净；审查期间 Lead 提交了 `92e4a15`（R8.md + 证据 + FINAL/README/index），本报告以 `92e4a15` 冻结；我的文件是唯一新增审查文件。

---

## 1. P0 / P1

**无 P0，无 P1。**

- `sweepAfterSelectHook` 已改为 per-Store `Store.sweepHook`（生产 nil），不再有包级全局测试状态；5 个 sweep 竞争测试迁移后 `-race -count=10` 通过（§5.1）。
- future-schema guard 正确：`Migrate` 在应用迁移前检查 `applied > maxKnown` 并返回 `ErrSchemaTooNew`；`TestMigrateRejectsFutureSchema` 通过（§5.2）。
- 未发现会导致 staging/TLS/container/supply-chain 结论不成立的 P0/P1；external-op 的问题见 P2-1（staging 使用 fake provider，未在 staging 运行中触发；不阻断 staging 结论，但生产对账前必须修）。

---

## 2. P2（2 项）

### P2-1 — 外部操作对账：模糊状态可被后续 `FAILED` 覆盖，且取消被归为 `FAILED`

- **位置**：[`internal/storage/external_ops.go:38-68`](../../internal/storage/external_ops.go)（`LIMIT` 后按 operation_key 只保留最新事件）
  [`internal/workflow/external_ops.go:40-54`](../../internal/workflow/external_ops.go)（`ExternalFailureStatus` 默认 `FAILED`）
  [`internal/provider/mailbox/cloudflare/cloudflare.go:632-633`](../../internal/provider/mailbox/cloudflare/cloudflare.go)（`context.Canceled` → `ClassCancelled`）
  [`docs/adr/0001-external-operation-reconciliation.md:60-66`](../../docs/adr/0001-external-operation-reconciliation.md)
  [`docs/acceptance/R8.md:65`](R8.md)（PREPARED/CONFIRMED/OUTCOME_UNKNOWN/... 语义）
- **复现（独立探针，未改仓库代码）**：
  1. 对同一 `operation_key=run-1:prepare:v1` 依次 append `PREPARED` → `OUTCOME_UNKNOWN` → `FAILED`（模拟超时后重试，重试确定性失败；`TIMEOUT_UNCERTAIN` 是 `errs.IsRetryable` 的类，见 `internal/errs/errs.go:131-134`，因此该序列在真实重试路径可达）；
  2. `Store.ListExternalOperations()` 返回 `status=FAILED needs_review=false` —— 先前“远端可能已创建”的含糊状态被静默覆盖，`needs_reconciliation` 计数为 0；
  3. `workflow.ExternalFailureStatus(ClassCancelled)` 返回 **FAILED**；而取消可能发生在 POST 已发出、远端已创建之后（`transportError` 对 `context.Canceled` 的归类），此时远端副作用存在但不会进入对账队列。
- **影响**：远端 mailbox 可能已创建，而 admin 诊断显示“无需对账”，操作员不会对账 → 孤儿/重复地址；这正是本 feature 要避免的类别。**不违反 exactly-once 声明**（ADR 明确不承诺），但审计状态机应保持“一旦含糊，永久需复核”，而不是可降级。
- **修复**：
  - `NeedsReview` 采用粘性聚合：只要该 operation_key 任一事件为 `OUTCOME_UNKNOWN`/`RECONCILIATION_REQUIRED`，就保持 need-review（例如 SQL 对 key 做 `MAX` 严重度，或返回最新事件时同时返回“历史是否含糊”）；
  - 创建调用一旦开始，`ClassCancelled` 归为 `OUTCOME_UNKNOWN`（或在 provider 调用前写显式 `call_started`/`attempt` 标记，取消后按模糊处理）；pre-call 校验/鉴权的取消仍可为 FAILED；
  - 补测试：`sticky OUTCOME_UNKNOWN then FAILED`、`cancel after write-start`；更新 ADR/R8.md 的状态转换规则。
- **回归**：`go test ./internal/workflow ./internal/storage -race`；新增 stuck/cancel 用例；现有 timeout/duplicate/admin 用例保持。
- **残余**：audit-write 失败时失败事件可能丢失，但 PREPARED 仍是最新事件 → need-review（偏保守，可接受）；修复后可能增加误报（取消发生在写入前），需在 UI/ops 中标注为“需人工确认”而非自动处置。

### P2-2 — supply-chain 基础镜像 provenance 为空（脚本硬编码错误 Go tag）

- **位置**：[`scripts/supply_chain.sh:24`](../../scripts/supply_chain.sh)（`docker image inspect … golang:1.25-bookworm debian:bookworm-slim`；Dockerfile 默认已是 `golang:1.26.10-bookworm`）
  [`docs/acceptance/evidence/r8-supply/metadata.json`](evidence/r8-supply/metadata.json)（`"base_images": ""`）
  [`docs/acceptance/R8.md:40`](R8.md)（称 CycloneDX 含 `base` 元数据）
  [`deploy/BASE_IMAGES.md:21`](../../deploy/BASE_IMAGES.md)（称脚本记录 local base refs/RepoDigests）
- **复现**：CI 的 buildx 使用 `docker/dockerfile:1`，拉取的基础层不会留下 `golang:1.26.10-bookworm`/`debian:bookworm-slim` 的本地 tag；脚本查询的又是错误的 `golang:1.25-bookworm`，`|| true` 吞掉失败 → `base_images=""`，SBOM 的 `base_images` 属性为空（`sbom.cdx.json`）。
- **影响**：supply-chain 证据无法绑定/审计基础镜像版本或 digest；R8.md 的“base 元数据”与 `BASE_IMAGES.md` 的“脚本记录”表述不符。未发现 digest 过度宣称（R8.md 明确 digest NOT_RUN；`metadata.note` 也说明无 registry digest），因此是 provenance/证据缺陷而非假造。
- **修复**：从 Dockerfile ARG/构建参数解析实际 `GOLANG_IMAGE`/`RUNTIME_IMAGE`；用 `docker buildx imagetools inspect` 或 build metadata 解析 digest；无解析能力时写显式 `base_images: NOT_RUN`（并让 R8.md 引用该字段）；增加脚本 golden/负向测试。
- **回归**：CI supply-chain 重跑（或本地脚本 mock）；`metadata.base_images` 非空或显式 NOT_RUN；SBOM 属性一致。
- **残余**：即使修复，若仍不 pin digest，provenance 只到 tag 级别；这与 R8 的“未 push 无 manifest digest”边界一致，应在文档中注明 tag-level。

---

## 3. P3（11 项，记录）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | [`internal/storage/external_ops.go:38-41`](../../internal/storage/external_ops.go) | 先 `LIMIT 200` 再按 operation_key 去重：外部事件超过 200 条时，较早但仍在等待对账的 operation 可能被截掉；建议 SQL window/聚合（先按 key 取最新再 limit）或分页 |
| P3-2 | [`docs/adr/0001-external-operation-reconciliation.md:60-66`](../../docs/adr/0001-external-operation-reconciliation.md) | ADR 列出 4 个测试，但没有“pre-call audit 写失败 → 不调用 provider”的测试（该行为在 `beginExternalOperation` 中实现且为关键 fail-closed 承诺）；建议加 store failpoint 测试，并补 cancellation 用例 |
| P3-3 | [`scanner-policy.txt`](evidence/r8-supply/scanner-policy.txt) | 写 “trivy: action 0.28.0”，实际 workflow 下载并校验的是 **Trivy v0.76.1** binary（`supply-chain.yml:56-64`，`trivy-version.txt`）；证据文案与工具不符 |
| P3-4 | [`.github/workflows/supply-chain.yml:68,73`](../../.github/workflows/supply-chain.yml) | CRITICAL/HIGH 都带 `--ignore-unfixed`；R8.md:42 的 “CRITICAL 0/HIGH 0” 未说明“仅统计可修复漏洞”，审计者可能误解覆盖范围 |
| P3-5 | [`R8-sbom-summary.txt`](evidence/r8-supply/R8-sbom-summary.txt) | 原始 `sbom.spdx.json`/`sbom.cdx.json` 未提交仓库，仅摘要绑定 Actions artifact；我核对 `/tmp/r8-final` 中 artifact 的 hash 与摘要一致（cdx `9d11dffc…`、spdx `d30b4ab7…`），但离线审计仍需访问 GitHub artifact；建议在报告/证据索引中注明获取方式 |
| P3-6 | [`R8-sbom-summary.txt`](evidence/r8-supply/R8-sbom-summary.txt)、[`R8.md:40`](R8.md) | CycloneDX 只有 **2 components**（container + `go-sqlite3`；仓库确实只有该 Go 依赖，`go list -m all`=2）；SPDX 97 packages 来自 Syft（含 OS packages）。R8.md 的“SBOM”容易让读者以为 CycloneDX 覆盖全镜像；建议注明 cdx 仅 Go 模块级，Syft SPDX 才是镜像级 |
| P3-7 | [`deploy/BASE_IMAGES.md:15`](../../deploy/BASE_IMAGES.md) | 示例使用 `golang:1.25-bookworm@sha256:…`，与默认 `golang:1.26.10-bookworm` 不一致；会误导 pin 示例 |
| P3-8 | [`.github/workflows/release-image.yml:19`](../../.github/workflows/release-image.yml) | 默认 `publish=false` 的 build/scan 流程也授予 `packages: write`；publish guard 尚未实现（显式 exit 1），建议将 `packages: write` 只放到 publish job/环境（least privilege） |
| P3-9 | [`.github/workflows/supply-chain.yml:46`](../../.github/workflows/supply-chain.yml) | `anchore/sbom-action@v0` 与 checkout/setup-go/upload-artifact 使用浮动 tag，未 SHA pin；作为供应链门禁可进一步收紧（syft version 已 pin，trivy 已 checksum） |
| P3-10 | [`tests/r51/ingress/main.go:172`](../../tests/r51/ingress/main.go) | 代理固定设置 `X-Forwarded-Port: "443"`，staging 实际公开端口是动态 45917；app 又开启 `ONBOARD_TRUST_PROXY_HEADERS=1`。当前测试（cookie/redirect）通过，但依赖 absolute URL/port 的逻辑可能生成错误链接；建议传递实际 public port 或移除该头 |
| P3-11 | [`tests/r8/staging_acceptance.sh:53`](../../tests/r8/staging_acceptance.sh)、[`deploy/staging/Caddyfile`](../../deploy/staging/Caddyfile) | staging 镜像只把 `COMMIT` 作为 build-arg 记录，未像 R7 容器脚本那样在镜像内验证 `onboardd version` 的 commit；Caddyfile 仅剥离 `Forwarded`/`X-Real-IP`，未显式处理 `X-Forwarded-Host/Port`（未在 CI 执行）。建议 staging 加 commit 校验，Caddy 参考配置补齐 header 边界说明 |

---

## 4. CI 证据真实性（独立核验，不只读提交 JSON）

| 项 | 值 | 来源 |
|---|---|---|
| staging run | `40167382718`；head_sha `3332167…`；**success**；event `push`；2 jobs 全 success（staging + container-regression） | GitHub API `actions/runs/40167382718` + `/jobs`；提交的 `evidence/r8-staging/R8-ci-run.json` 与 API 的 databaseId/headSha/conclusion/job 数一致 |
| staging job | **26 PASS / 0 FAIL**；隔离 TLS、Host 421、TLS1.1 拒绝/1.3 接受、cookie flags、CSRF/Origin、revocation、internal network、loopback-only、UID 10001、read-only、cap_drop | [`R8-staging-acceptance.log`](evidence/r8-staging/R8-staging-acceptance.log)（`grep -c '^PASS '`=26，FAIL=0）；`app-inspect.json`/`ingress-inspect.json` 显示 app 无端口绑定、ingress 仅 `127.0.0.1`、UID 10001、ReadonlyRootfs=true、CapDrop ALL |
| container-regression job | **63 PASS / 0 FAIL**；含 v2→v3 upgrade、SIGTERM/SIGKILL、single-writer、encrypted backup/restore、restored instance | [`R8-container-regression.log`](evidence/r8-container/R8-container-regression.log)（63/0；grep 到 v2 upgrade/restored DB/SIGKILL/single-writer） |
| supply run | `40167382695`；head_sha `3332167…`；**success**；job supply-chain 全 steps success | GitHub API `actions/runs/40167382695` + `/jobs`；提交的 `evidence/r8-supply/R8-ci-run.json` 一致 |
| supply 工具结果 | govulncheck **No vulnerabilities**；ShellCheck `-S error` 空输出（0 errors）；Trivy v0.76.1 CRITICAL/HIGH 0；`go mod verify` all modules verified；SPDX-2.3 97 packages；cdx 2 components | [`evidence/r8-supply/`](evidence/r8-supply/)；`trivy-version.txt` = 0.76.1 |
| artifact | staging `staging-acceptance-evidence-40167382718`（13,204 B）、`r8-container-regression-40167382718`（5,341 B）；supply `supply-chain-evidence-40167382695`（367,409 B） | GitHub API `/artifacts`（名称/大小与 Lead 描述一致；均在有效期内） |
| SBOM 摘要绑定 | cdx sha256 `9d11dffc…`、spdx `d30b4ab7…`；我将其与 `/tmp/r8-final` 的 artifact 文件实际 hash 核对一致 | `R8-sbom-summary.txt` + `/tmp/r8-final` |
| 镜像/发布 | 四个本地 Image ID；**无 registry manifest digest、无 GHCR push、无 Release**；staging app/ingress、container、supply image 均可在对应日志/metadata 中找到 | `R8.md:11-16`、`metadata.json`（`note` 明确无 digest） |
| 过度宣称 | 无 `LIVE_READY` 声明；R8.md 明确“STAGING_RC_READY 待独立审查后生效”、旧镜像回滚 NOT_RUN、digest NOT_RUN、no exactly-once | `R8.md:9,37-45,79`；FINAL/README/index 均 pending review |

结论：**CI 运行真实、与提交证据一致，staging/supply 的 PASS 不是自证**。P2-2/P3 是 evidence/provenance 完善项，不改变运行时 PASS。

---

## 5. 代码与脚本审查

### 5.1 per-Store sweep hook（P0 修复）

- `internal/storage/store.go` 新增 `Store.sweepHook`（test-only，nil in production）；`runs.go` 从 `s.sweepHook` 调用；`sweep_race_test.go` 改为 `s.sweepHook` 并 `t.Cleanup` 置空。**无包级测试状态**；`go test ./internal/storage -run 'TestSweep' -race -count=10` 0（本地）。
- 逻辑与 R7 审查的 cancel/terminal 竞争修复一致（事务内重读 + conditional UPDATE + RowsAffected==1 + 单次终态事件）。

### 5.2 future-schema rollback guard（P3 要求）

- `Migrate` 读取 `MAX(version)` 后比较 `maxKnown`；`applied > maxKnown` 返回 `ErrSchemaTooNew` 并拒绝启动。测试 `TestMigrateRejectsFutureSchema`（插入 999）通过；旧镜像回滚本身 **NOT_RUN**（R8.md 如实说明），guard 是“拒绝旧二进制在更新 schema 上运行”的机制，不等同于已做旧镜像演练。

### 5.3 external operation audit / reconciliation（P4）

- **正面**：PREPARED 在 provider 调用前写（`beginExternalOperation` 失败即返回 err → 不调用 provider）；CONFIRMED 通过 `Outcome.Events` 与成功 Step 同事务；timeout/5xx/transport → OUTCOME_UNKNOWN；duplicate → RECONCILIATION_REQUIRED；event payload 走 allowlist + Redactor，仅 run/step 标识、provider、operation_type、截断 SHA-256 fingerprint、status、attempt、error_class，无 token/JWT/地址/完整响应；`GET /v1/ops/external` 为 admin-only（viewer 403，测试通过）；ADR 明确 **不承诺 exactly-once**、不自动删除/二次创建。
- **缺陷**：P2-1（模糊状态可被 FAILED 覆盖；Cancelled→FAILED）；P3-1/P3-2（LIMIT 先去重、缺 pre-call audit 失败测试/cancellation 测试）。

### 5.4 staging trust boundary

- app：`user 10001:10001`、`read_only: true`、`cap_drop ALL`、`no-new-privileges`、无 published ports、仅 internal network；ingress：同 UID/read-only/cap_drop，只发布 `127.0.0.1` 动态端口，加入 edge network 仅用于发布；`ONBOARD_TRUST_PROXY_HEADERS=1` 由 ingress 覆写 `X-Forwarded-*`/剥离 `Forwarded`（`newProxyHandler`），Host 白名单 421；HTTP 308 只允许 localhost/127.0.0.1；cookie Secure/HttpOnly/SameSite=Strict 在伪造 `X-Forwarded-Proto: http` 下未降级；CSRF/Origin 403、撤销重放 303→login。均与 CI 26/26 日志一致。
- 残余 P3-10（X-Forwarded-Port 443）、P3-11（Caddyfile header 边界、staging commit 校验）。

### 5.5 supply-chain 脚本/工作流

- trivy v0.76.1 从 release 下载并用 checksums 校验；govulncheck `@v1.1.4`；syft v1.20.0（action 内 pin）；shellcheck `-S error`；CRITICAL 阻断、HIGH report-only（`--exit-code 0`，显式）；`go mod verify`/module 列表。
- 缺陷：P2-2（base image provenance 空/错误 tag）、P3-3（scanner-policy 版本文案）、P3-4（`--ignore-unfixed` 未披露）、P3-5/P3-6（raw SBOM/组件范围）、P3-8/P3-9（release 权限/action tag）。

### 5.6 文档诚实性

- R8.md 明确 `LIVE_READY` 不宣告、STAGING_RC_READY 待 review、旧镜像回滚 NOT_RUN、无 registry digest、no exactly-once、base digest NOT_RUN；FINAL/README/index 的 R8 行均 pending review。与证据一致；P2-2/P3-4/P3-6 是“base 元数据/SBOM/Trivy 阈值”表述的精确性改进点，不构成 `LIVE_READY`/`STAGING_RC_READY` 的故意过度宣称。

---

## 6. 本地独立执行的命令与退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `git rev-parse HEAD` / `git status` | 0 | `92e4a15…`；开始时干净（期间 Lead 提交 92e4a15；我的文件为新增未跟踪） |
| `make verify` | **0** | fmt/vet/build/全量测试（本地） |
| `go test ./... -race -count=1` | **0** | 29 包全绿 |
| `go test ./... -v -count=1` 计数 | **0** | **338 顶层（333 PASS + 5 SKIP）/ 647 含子测试 / 0 FAIL** — 与 R8.md 一致 |
| `go test ./internal/storage -run 'TestSweep\|TestMigrateRejectsFutureSchema\|TestListEventsLargeHistory' -race -count=5` | **0** | 3 类测试稳定 |
| `go test ./internal/workflow -run 'TestExternalOperation' -race -count=3` | **0** | success/timeout/duplicate 用例 |
| `go test ./internal/api -run 'TestExternalOperationDiagnosticsAdminOnly' -race` | **0** | admin 200/viewer 403 |
| `go test ./tests/r51/ingress -race` | **0** | Host/loopback/staging flag 单测 |
| `bash tests/r8/staging_acceptance.sh`（本地无 Docker） | **3** | 明确 `NOT_RUN: Docker daemon unavailable`（符合预期） |
| `bash tests/r51/docker_acceptance.sh --self-test` | **0** | **11/11**（无 Docker） |
| `docker info` | **1** | 本地 daemon 仍不可用（未启动） |
| `redact_scan --file`（R8 证据 20 份 + R8.md） | 全部 **0** | 0 findings |
| GitHub API `actions/runs/40167382718`、`40167382695` + jobs/artifacts | 200 | success、head_sha `3332167`、artifact 名称/大小匹配 |

> 未触发/重跑 Actions、未 push/部署、未安装/启动 Docker、未改代码或宿主配置。

---

## 7. `STAGING_RC_READY` 判定与冻结

### 7.1 判定

| R8 关闭标准 | 证据 | 结论 |
|---|---|---|
| 真实隔离 staging + TLS | run 40167382718 staging job 26/26；TLS1.1 拒绝/1.3 接受、Host 421、308、cookie/CSRF/revocation、internal network、loopback-only、UID/read-only/cap_drop | **PASS** |
| 真实 container regression | 同 run container-regression 63/63：build/health/schema v2→v3/SIGTERM/SIGKILL/single-writer/encrypted backup/restore/restored instance | **PASS** |
| supply-chain 门禁 | run 40167382695 success；govulncheck 无漏洞、ShellCheck 0 errors、Trivy CRITICAL/HIGH 0（仅可修复项）、SPDX-97/cdx、go mod verify；trivy checksum/versions | **PASS**（P2-2/P3-4/P3-5/P3-6 为 provenance/阈值披露改进） |
| external-op reconciliation | PREPARED/CONFIRMED/OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED/FAILED；pre-call fail-closed；admin-only API；ADR；无 exactly-once 承诺 | **PASS（有 P2-1 / P3-1/P3-2 待修）**；staging 使用 fake provider，P2-1 未在真实 staging 触发；生产/真实 provider 对账前必须修复 |
| rollback/future schema | `ErrSchemaTooNew` + test；备份恢复回归；旧镜像回滚 NOT_RUN（如实） | **PASS（范围受限）** |
| 文档诚实性 | `LIVE_READY` 不宣告；STAGING pending review；无 digest/GHCR；no exactly-once；旧镜像回滚 NOT_RUN | **PASS** |
| 独立审查 P0=0/P1=0 | 本报告 | **PASS** |

**结论：`STAGING_RC_READY` justified（staging 范围）**；建议 Lead 先修 **P2-1**（external-op 对账准确性）与 **P2-2**（supply-chain base provenance），再在 FINAL/README/index 把 “pending review” 更新为 `STAGING_RC_READY`，并保留 `LIVE_READY` 不宣告。`STAGING_RC_READY` **不等于** `LIVE_READY`：生产 TLS ingress、GHCR publish、真实 Cloudflare/GitHub/WorkBuddy、旧镜像回滚演练仍 NOT_RUN。

### 7.2 冻结

| 项 | 值 |
|---|---|
| HEAD | `92e4a151b3be622327488e16851b78756826af74`（`docs(r8)` 证据/报告提交；代码/CI 证据 `3332167`；分支 `feat/r8-staging-readiness`，main `9726fc1`） |
| HEAD 树清单 | `git ls-tree -r HEAD \| sha256sum` = `f8568a6a25e7059f341fedf7114059df218ba3fa728083f3936e0d84efda7ae0` |
| 工作树 | 审查开始时干净；我的 `REVIEW-R8.md` 为唯一新增未跟踪文件 |
| `docs/acceptance/R8.md` | `7bfe963a…` |
| staging 证据 | `R8-staging-acceptance.log 7124e39c…`；`R8-ci-run.json`；app/ingress inspect；identity；login headers |
| container 证据 | `R8-container-regression.log e0995da5…` |
| supply 证据 | `metadata.json 80566ee4…`；`R8-sbom-summary.txt c5eb0251…`；govulncheck/shellcheck/trivy/go-mod-verify/image-inspect |
| ADR | `docs/adr/0001-external-operation-reconciliation.md b678cce3…` |
| 代码 | `internal/workflow/external_ops.go 9ff40e18…`；`internal/storage/external_ops.go 2d5dcfa3…`；`internal/storage/store.go f17c9b5e…`；`internal/api/ops.go 3aad9a67…` |
| 脚本/配置 | `tests/r8/staging_acceptance.sh 34bb16b6…`；`tests/r51/ingress/main.go 00dd59ab…`；`deploy/staging/compose.yaml 5bb842a4…`；`Dockerfile d31c4579…`；`scripts/supply_chain.sh b462ddeb…`；`.github/workflows/supply-chain.yml 67aba40a…` |
| 文档 | `FINAL-REPORT.md 9d334d61…`；`README.md c68498a8…`；`index.md 6a69540f…` |
