# 独立对抗性审查报告 — R12 Acceptance Integrity（REVIEW-R12，post-fix）

- 复审时间：`2026-10-26T00:00–00:20`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI HEAD `e0c1944`**（PR #5，Draft/OPEN，base `feat/r11-integration-readiness`；main `9726fc1`）；R12 文档/证据仍**未提交**（Lead 说明将在 verdict 后提交）；当前审查工作树仅本文件，R12 代码在临时 worktree `e0c1944` 中检查。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry/arm64 证据来自 GitHub Actions；本地 R12 代码在临时 worktree 执行，未修改仓库。
- **最终判定：PASS_WITH_LIMITATIONS（P0=0，P1=0，P2=0，P3=8）。**
  - 首轮两个 P2 已独立复核关闭：脚本 executable mode 恢复 100755，直接执行 dry-run exit 0；Cloudflare negative tests 补齐 401/oversize/timeout/temp cleanup/invalid authz/executable。
  - 剩余限制：Authorized Read-Only Live **NOT_RUN**；PR 仍 **Draft/OPEN**；R12 docs/evidence 尚未提交，最终 PR head 的 docs-only commit 需在提交后确认仅 docs/evidence 且最终 CI 通过。`LIVE_READY` 不得宣告。

---

## 1. P2 关闭复核

### P2-1 — 脚本 mode 100644 → 100755：已修复

- `e0c1944` tree：`scripts/cloudflare_preflight.sh`、`tests/r11/deploy_recovery.sh` 均为 **100755**。
- 新增 `TestCloudflareScriptsAreExecutableForRunbook` 断言两个脚本有 execute bit。
- 独立复现（临时 worktree）：直接执行 `scripts/cloudflare_preflight.sh --dry-run ...` → **exit 0**，输出 DRY RUN；之前为 exit 126。
- **P2-1 CLOSED。**

### P2-2 — Cloudflare negative coverage：已修复

- `tests/r11/cloudflare_preflight_test.go` 新增：
  - 401 → `AUTH_DENIED`；
  - >1 MiB body → `RESPONSE_TOO_LARGE`；
  - 12s delayed response + `--max-seconds 10` → `TIMEOUT`；
  - TMPDIR 下无 `cf-preflight` 临时文件残留；
  - live 缺 authorization file / `AUTHORIZED=NO` → 在联网前拒绝；
  - executable mode assertion。
- 独立执行 `go test ./tests/r11 -run 'TestCloudflare' -count=1` → **PASS**（约 23s）；CI cloudflare-mock-live success。
- 现有成功用例仍断言 token 不出现在 stdout/report、report 0600、TLS/schema PASS；direct dry-run 也通过。
- **P2-2 CLOSED；P2 总数 = 0。**

---

## 2. P0/P1 主体（保持 clean）

- **Engine**：`lifecycleMu` + 状态机；Start 串行 recovery/worker 注册，失败零 Worker；并发 Start 单 winner（测试）；并发 Stop 共享 recovery error；Stop 幂等；Stop 后 Start 拒绝；Start 失败后 Stop 安全 no-op。
- **UI**：NeedsReview 仅 OUTCOME_UNKNOWN/RECONCILIATION_REQUIRED；PREPARED/IN_FLIGHT 不再显示 Resolve；detail 页仅 pending 显示表单；CSRF/expected_version/audit 保留。
- **Cloudflare**：token 通过 0600 header file（非 argv）、token 文件 regular/0600/有界/无换行；mock loopback；live 仅 HTTPS/443、无 redirects、1 MiB、JSON Content-Type/body/schema 检查；authorization file exact host/address/operation/UTC window/budget；temp dir 0700，EXIT/INT/TERM cleanup；report O_NOFOLLOW/0600。
- **Deploy**：read-only injection 先验证 UID10001 不可写，容器失败日志须匹配；`assert_expected_failure` 不再仅凭非 200/非零。
- **结论：P0=0，P1=0。** 未发现远程写入绕过、active operation UI resolve、token 泄漏或 lifecycle 竞态导致零 Worker 失败仍服务。

---

## 3. P3（8 项残余限制，不阻断 PASS_WITH_LIMITATIONS）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R12 docs/evidence | R12 无 `docs/acceptance/R12.md`/evidence in branch；PR #5 Draft/OPEN。最终 docs-only commit 尚待提交；提交后须确认仅 docs/evidence 且最终 CI 通过。 |
| P3-2 | R12 CI artifact | deploy-recovery artifact 实际 `SUMMARY pass=12 fail=0`（新增 read-only probe assertion），与早期/Lead 的 11/11 文案不一致；最终 R12 报告应写 12/12。 |
| P3-3 | Authorized Live | Authorized Read-Only Live **NOT_RUN**；新增 authz 测试只覆盖 missing/`AUTHORIZED=NO`，没有真实 valid authorization window/budget/host 执行；mock 成功不等于真实 Worker 健康。 |
| P3-4 | `tests/r11/deploy_recovery.sh` | read-only probe 若 `docker run` 因镜像/权限/无关错误失败仍走 `ok` 分支；随后 app failure 断言可能仍通过，存在假 PASS 风险。恢复后 external_operation 状态未断言。 |
| P3-5 | UI | 仅修复 active Resolve 表单；CONFIRMED recovery 仍无 UI，audit 未知 ID 返回 200 空列表，filter 保持无自动化测试，detail 当前 reason 原样显示（HTML 转义但非 redact）。 |
| P3-6 | Engine | 无并发 Start+Stop 交错测试；Start 失败后 Start 返回 ErrStopped（不允许 retry）需在运行手册明确；无 provider-call 计数证明 recovery 失败零远程调用。 |
| P3-7 | Candidate/arm64/publish | candidate 与 dry-run 仍是独立构建，未强制同一 artifact；Publish 未执行；ARM64 runtime smoke 仅 `version`，未做 API/首次安装/全 arm64 回归；真实平台/GHCR/生产 TLS 均 NOT_RUN。 |
| P3-8 | Preflight 覆盖边界 | 新增 401/oversize/timeout，但仍无显式 5xx/TLS 失败/valid authorization window 用例；token 不出现在 stdout/report，但未直接断言进程 argv；`redaction_result=NOT_RUN`，mode B 仍 CI-only。 |

---

## 4. 最终 CI（head `e0c1944`）

| Run/工作流 | 结果 | 关键证据 |
|---|---|---|
| ci **40208953777** | success | gofmt、build vet test-race、compose success |
| r12-integration **40208948196** | success | deploy-recovery、cloudflare-mock-live、arm64-build 均 success；artifacts 4,294/1,156/4,227 B |
| staging **40208948252** | success | staging + container success；artifacts 13,280/5,424 B |
| supply **40208948172** | success | PASS；artifact 367,850 B |
| r9-release **40208948176** | success | caddy-proxy、old-image-rollback success；artifacts 4,978/5,169 B |
| r10-dry-run **40208948268** | success | OCI/sign/attest success；artifact 5,660 B |
| release-image **40208948220** | success | candidate success，publish **skipped**；artifact 38,101,194 B；无 GHCR push |

**注意**：R12 本轮未提交 docs/evidence；上述 artifacts 是独立核验来源。`r12-integration` 的 deploy artifact 本次为 **12/12**。

---

## 5. 本地命令与退出码（临时 worktree `e0c1944`）

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试 |
| `go test ./... -race -count=1` | **0** | 29 包全绿 |
| `go test ./...` 计数 | **0** | **379 顶层（374 PASS + 5 SKIP）/704 含子测试/0 FAIL** |
| `go test ./tests/r11 -run 'TestCloudflare' -count=1` | **0** | 含 401/oversize/timeout/temp cleanup/invalid authz/executable |
| 直接执行 `scripts/cloudflare_preflight.sh --dry-run ...` | **0** | 不再 Permission denied |
| `bash scripts/cloudflare_preflight.sh --self-test` | **0** | fail-closed PASS |
| `bash -n scripts/cloudflare_preflight.sh` | **0** | 语法 OK |
| `bash -n tests/r11/deploy_recovery.sh` | **0** | 语法 OK |
| `bash tests/r11/deploy_recovery.sh`（无 Docker） | **3** | NOT_RUN: Docker unavailable |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署、未启动 Docker；未修改仓库代码/文档/证据。

---

## 6. 最终判定 / docs-only commit

| 关闭标准 | 结果 | 说明 |
|---|---|---|
| P0 生命周期/UI/preflight/deploy | **0** | state machine、active UI、token/TLS/JSON、deploy 证据 |
| P1 安全边界 | **0** | token 非 argv/log、header/temp、CSRF/RBAC、fail-closed |
| P2 acceptance integrity | **0** | mode 755 + executable test；401/oversize/timeout/temp/authz 测试 |
| CI | **PASS** | 7 runs success/head e0c1944；publish skipped；无 GHCR |
| Authorized Live | **NOT_RUN** | 不能当作 live 通过 |
| Docs/evidence | **待提交** | R12 报告/evidence 尚未提交；PR Draft/OPEN |

**最终：PASS_WITH_LIMITATIONS（P0=0/P1=0/P2=0/P3=8）。** R12 code/e0c1944 的 acceptance-integrity 修复成立；Authorized Read-Only Live 仍 NOT_RUN，PR Draft/OPEN，R12 docs/evidence 待提交，故不是无条件 PASS。

### docs-only commit 是否可作为最终 PR head

- **可以，前提是**：该 commit **只改 `docs/**` / evidence**（不得改代码、workflow、scripts、Makefile、配置）；R12 报告/evidence 准确反映 `e0c1944`（含 deploy 12/12、CI run IDs、Authorized Live NOT_RUN）；该 docs-only head 触发的最终 CI **全部成功**。
- 由于 docs/evidence 尚未提交，我无法先验证其内容；提交后应检查 diff 仅 docs/evidence，并核对报告与 artifacts。满足以上条件时**不需要再 review 代码**；若 docs commit 夹带任何代码/workflow/CI 变更，则必须重新 review 该代码 head。
- 即使 docs-only head CI 成功，`LIVE_READY` 仍不得宣告；Authorized Live 保持 NOT_RUN。

### 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `e0c1944`；树 `1b18de96…`；`git ls-tree -r e0c1944 \| sha256sum` = `5a7d10d0…` |
| 关键文件 | `cloudflare_preflight.sh` `7c867413…`（100755）；`cloudflare_preflight_test.go` `cc86dea9…`；`deploy_recovery.sh` `fe094d30…`（100755）；`engine.go` `df7414af…`；`ui_ops.go` `b95b5551…`；`r11-integration.yml` `4f252e52…` |
| 工作树 | 本审查文件唯一新增；R12 代码未 checkout；R12 docs/evidence 未提交 |
