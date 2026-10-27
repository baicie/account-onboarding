# 独立对抗性审查报告 — R10 Live-Preflight / Release Dry-Run Hardening（REVIEW-R10，post-fix）

- 复审时间：`2026-10-25T18:35–18:52`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；本轮只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI HEAD `d9bcc6f`**；**文档/证据 HEAD `2c31baf`**（分支 `feat/r10-live-preflight`，base R9 `e0b09b0`，main `9726fc1`）；工作树仅本审查文件未跟踪，其余干净。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry 证据来自 GitHub Actions；本轮用已认证 `gh api`（只读）核验 5 个 post-fix run/job/artifact 及关键日志，不只读提交 JSON。
- **结论：R10 = PASS；P0 = 0，P1 = 0，P2 = 0，P3 = 8。`
  - 首轮 P2-1（base provenance 重复 `@sha256:`）和 P2-2（digest immutability 假阳性）已独立复核修复；`LIVE_PREFLIGHT_READY` 在 R10.md 明确限定的“工具/dry-run/fail-closed 门禁”边界内成立。
  - `LIVE_READY` 仍不宣告：真实 Cloudflare/GitHub/WorkBuddy、生产 TLS/ACME/HSTS、GHCR push/sign/attest、正式 Release/Tag 均 NOT_RUN。

---

## 1. P2 关闭复核（首轮两个阻断项）

### P2-1 — base-image provenance 重复拼接：已修复

- `scripts/supply_chain.sh` 现在对 `ref` 做分支：已含 `@sha256:` 时直接作为 entry，否则才追加 `@${d}`（`d9bcc6f`）。
- 新证据 `r10-supply/metadata.json`、`r10-candidate/metadata.json`/`R10-candidate-summary.txt` 均为单一 canonical 值：`golang:1.26.10-bookworm@sha256:632dff…; debian:bookworm-slim@sha256:7ff6ba…`（无第二个 `@sha256:`）。
- post-fix supply run **40183814849** success；artifact `supply-chain-evidence-40183814849` 367,886 B；与源码/证据一致。
- **判定：P2-1 CLOSED。**

### P2-2 — OCI dry-run “digest immutable” 假阳性：已修复

- `tests/r10/registry_dry_run.sh` 现在从 registry GET 已服务的 valid manifest，Python 修改 `config.size` 形成 **合法但不同的 manifest**，再 PUT 到同一 digest；只有 registry 返回 `400/409` 才 PASS（`c35fa45f…`）。
- post-fix dry-run run **40183814840** success；日志第 244 行：`PASS digest immutable: registry rejected a valid-but-different manifest with HTTP 400`；其余 14 项仍 PASS，`SUMMARY pass=15 fail=0`（artifact 5,720 B）。
- 不再使用非法 `{}` 作为 immutable 证据；该 PASS 现在能证明 registry 拒绝把不同 manifest 映射到既有 digest。
- **判定：P2-2 CLOSED。**

**P2 总数：0。** R10.md 第 95 行原先“等待复审确认 P2=0”的条件已满足。

---

## 2. P0 — 生命周期/完成 fencing 真实性（保持 PASS）

| 检查 | 结果 |
|---|---|
| Complete 必须带 LeaseOwner/FencingGeneration/OperationVersion | 事务内复核 run lease owner/gen/lease_until + operation status/version；stale lease → `RECONCILIATION_REQUIRED`（`LEASE_LOST_AFTER_REMOTE_CALL_MAYBE_SENT`）+ `ErrLeaseLost`，不覆盖终态 |
| 迟到 Worker 不能覆盖 Resolve | status `RESOLVED` 先于版本检查返回 conflict；`TestAdminResolveWinsAgainstLateWorkerCompletion` 覆盖；独立探针确认 late completion conflict、状态保持 RESOLVED |
| Live IN_FLIGHT resolve | `Resolve` 完全拒绝 IN_FLIGHT（不再用 lease 过期推断）；恢复后的 RECONCILIATION_REQUIRED 可 resolve |
| 启动/停止恢复 | `Engine.Start` 在 worker 前 `STARTUP_PREVIOUS_PROCESS` 恢复；`Engine.Stop` 在 `wg.Wait` 后 `GRACEFUL_STOP`；PREPARED→FAILED_DEFINITE、IN_FLIGHT→RECONCILIATION_REQUIRED（`engine.go:146-202`） |
| 无二次 provider 调用 | timeout/cancel → OUTCOME_UNKNOWN；unknown/resolved/confirmed/IN_FLIGHT 均阻止 Begin/Mark；timeout 用例验证 CreateAddress 调用次数=1；取消用例验证 OUTCOME_UNKNOWN |
| Schema 5/readyz | migration 0005；`Migrate` maxKnown 5；`/readyz` schema>=5 + external_operations 表；空/3/4→5、迁移失败回滚测试；旧镜像拒绝 schema5（rollback 20/20） |
| post-fix 新增 | stale-lease completion 现在在同一事务内写 `external_operation.completion_fenced` audit（`runID/opID/from/to/version/actor/reason`）；audit 失败则 tx 回滚，状态保留 IN_FLIGHT 待下次恢复，不覆盖终态 |

**P0：0 findings。** 未发现“未知/在途/已确认/已解决后再调用 provider”的路径；租约过期不再被当作 worker 已停止。

---

## 3. P1 — 审计身份与原子性（保持 PASS）

- actor 来自已认证 bearer/cookie（`bearer:admin:sha256:<12hex>`/`bearer:viewer:…`/`cookie:operator`），不从 JSON 取。
- audit `run_id=op.RunID`；detail 含 operation_id/from/to/version/actor/requested/resolved time/resolution/reason/evidence；状态修改 + audit 同一事务；audit 失败回滚（测试）。
- 输入校验：reason ≤2000、evidence ≤512、拒绝 NUL/CR/LF；viewer 403、重复/过期 409、未知 ID 404、空/非法 reason 400；cookie 路径仍走 CSRF/Origin/session revocation。
- post-fix stale-lease completion audit 也按凭证/operation/run 绑定，失败不静默。
- **P1：0 findings。**

---

## 4. P3（8 项，不阻断本轮边界）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | `scripts/cloudflare_preflight.sh:40-48`；`R10.md:95,100` | `--live` 在通过 allowlist/HTTPS/token/预算后仍**直接 exit 3**，不执行真实只读/写入请求。R10.md 现已明确：`LIVE_PREFLIGHT_READY` 只是工具/dry-run/fail-closed 门禁就绪，**不代表 live preflight 已执行**。该边界诚实；残余是真实只读 preflight 仍 NOT_RUN |
| P3-2 | `tests/r9/caddy_acceptance.sh:115-128`；`R10.md` §8 | 超限 body 以连接级 `000` 计 PASS；R10 已明确“无 HTTP 413、客户端按连接错误处理”。行为 fail-closed，但仍是运维语义限制 |
| P3-3 | `internal/api/templates/ops_external.html:27`；UI | UI 直接回显 `ResolutionReason` 自由文本（操作员可填入敏感串）；无 UI CSRF/RBAC/secret 自动化测试；IN_FLIGHT/PREPARED 仍显示 resolve 表单但 API 拒绝；CONFIRMED recovery 未在 UI 暴露 |
| P3-4 | `internal/workflow/engine.go:146-202`；tests | `Start/Stop` 生命周期恢复只有 storage 层测试，没有 engine 级启动/停止集成测试；建议补 PREPARED/IN_FLIGHT/已 RESOLVED 的 engine 生命周期用例 |
| P3-5 | `internal/storage/external_operations.go`；`internal/storage/metrics.go` | post-fix 的 stale-lease completion audit 与 `NOT_AUTHORIZED` provider 分类没有新增回归测试；代码正确性依赖现有测试/审查。建议加 audit 原子性/失败回滚与 metrics 分类单测 |
| P3-6 | `.github/workflows/release-image.yml:198` | publish job 仍 `curl` 下载 cosign 后直接执行，无 checksum/签名校验；R10 P3 dry-run 校验了 cosign，production publish 路径未校验。建议复用 dry-run 的 checksum 逻辑或 pinned action |
| P3-7 | `tests/r10/registry_dry_run.sh:34`；dry-run vs candidate | 临时 registry 使用 `registry:2.8.3` tag 未 digest pin；dry-run 是重新构建的 image，candidate artifact 与 dry-run image 没有一致性断言；publish 未执行，candidate 仅 artifact |
| P3-8 | `R10.md:71,99-101` | 仅 linux/amd64 真实验证；arm64、真实 Cloudflare mock 扩展、首次部署/DB 只读/Secret 丢失专项、任意跨版本回滚仍 NOT_RUN/未证明；候选 image 仅 artifact、无 registry digest |

---

## 5. post-fix CI 独立核验（gh api，只读）

5 个 run 均 `conclusion=success`、`head_sha=d9bcc6f…`：

| Run | Job/结果 | 证据 |
|---|---|---|
| staging **40183814895** | staging + container 均 success | staging **27/27**、container **63/63**；artifacts 13,219 B / 5,321 B |
| supply **40183814849** | supply-chain success | govuln 0、Trivy CRITICAL+HIGH 0、ShellCheck 0；artifact 367,886 B；base_image_digests 为单一 canonical `tag@sha256:` |
| r9-release **40183814916** | caddy-proxy + old-image-rollback success | rollback **20/20**、Caddy **18/18**；artifacts 5,181 B / 4,990 B |
| dry-run **40183814840** | dry-run success | OCI/sign/attest **15/15**；日志第 244 行 valid-but-different manifest → HTTP 400；artifact 5,720 B；Cloudflare dry-run/self-test |
| release-image **40183814884** | candidate success；publish **skipped** | candidate 真实 govuln/Trivy/ShellCheck + publish gate static audit；artifact 38,096,444 B；publish skipped，无 GHCR push |

**候选证据绑定**：`R10-candidate-summary` commit `d9bcc6f…`、image ID `4133fba1…` 与 `image-id.txt`/`metadata.json` 一致；`trivy-critical.txt` target 为 `account-onboarding:candidate`；scanner-policy 现在有实际值 `scanned_at_utc=2026-10-25T10:16:22Z`、`scanner_db_updated_at=2026-10-25T07:10:41Z`（首轮 P3-1 已关闭）。supply metadata/base digest 同样复核。

---

## 6. 本地独立执行的命令与退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试 |
| `go test ./... -race -count=1` | **0** | 29 包全绿 |
| `go test ./... -v` 计数 | **0** | **360 顶层（355 PASS + 5 SKIP）/675 含子测试/0 FAIL** — 与 R10 声明一致 |
| `go test ./internal/storage -run 'TestCompleteRejectsStaleLease|TestAdminResolveWins|TestRecoverySeparates|TestResolveAuditFailure' -race -count=3` | **0** | 生命周期/fencing/审计稳定 |
| `go test ./internal/workflow -run 'TestExternalOperation' -race -count=3` | **0** | confirmed/timeout/duplicate/cancel |
| `go test ./internal/api -run 'TestExternalOperation|TestOpsMetrics' -race` | **0** | resolve/RBAC/metrics |
| `tests/r10/registry_dry_run.sh`（无 Docker） | **3** | `NOT_RUN: Docker daemon unavailable`（符合预期） |
| `tests/r10/publish_gate_audit.sh` | **0** | 静态 publish gate PASS |
| `tests/r51/docker_acceptance.sh --self-test` | **0** | 11/11 |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |
| R10 证据/文档 `redact_scan` | 全部 **0** | 0 findings |
| `gh api`（5 runs/jobs/artifacts，只读） | **200** | 见 §5 |

> 未触发/重跑 Actions、未 push/部署、未安装/启动 Docker、未改代码/证据/文档（仅本审查文件）。

---

## 7. `LIVE_PREFLIGHT_READY` 判定（最终）

| 关闭标准 | 结果 | 说明 |
|---|---|---|
| P0 生命周期/fencing | **0** | Complete 租约/版本/状态 fencing；late worker 不覆盖 resolve；启动/停止恢复；Resolve 拒绝 IN_FLIGHT |
| P1 审计身份/原子性 | **0** | credential actor、run binding、同事务 audit、输入校验/404/409 |
| P2 provenance/immutability | **0** | base digests 单 canonical；valid-different manifest PUT → HTTP 400 |
| Staging/container/rollback/Caddy | **PASS（P3-2 语义）** | 27/27、63/63、20/20、18/18（body connection-level 已明确记录） |
| OCI sign/attest dry-run | **PASS** | 15/15；cosign v2.4.1 checksum/verify/attest/commit binding |
| Candidate/publish gate | **PASS（Publish skipped）** | candidate 真实扫描；publish 静态 gate，未执行；无 GHCR |
| Cloudflare | **PASS（边界内）** | dry-run/self-test/fail-closed；`--live` 仍 exit 3，真实只读/写入 NOT_RUN |
| 文档诚实性 | **PASS（边界明确）** | R10.md:95/100 明确 LIVE_PREFLIGHT_READY 不表示 live preflight 已执行；LIVE_READY 不宣告；NOT_RUN 列表完整 |

**最终判定：R10 = PASS（P0/P1/P2=0，P3=8 残余）。`LIVE_PREFLIGHT_READY` 在 R10.md 明确界定的“工具/dry-run/fail-closed 门禁已就绪”语义下成立；它不是真实平台 live preflight 已执行的证明。`LIVE_READY` 仍不宣告**（真实 Cloudflare/GitHub/WorkBuddy、生产 TLS/ACME/HSTS、GHCR push/sign/attest、正式 Release/Tag 均 NOT_RUN）。

P3 残余不影响本轮 PASS；后续若要进入真实平台验收，优先补 P3-1 的真实只读 preflight、P3-6 的 publish cosign checksum、P3-7 的 registry pin/candidate 一致性，以及 P3-4/P3-5 的 engine/audit/metrics 回归测试。

---

## 8. 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `d9bcc6f`；树 `228624e0…`；`git ls-tree -r d9bcc6f \| sha256sum` = `32b3d74e…` |
| 文档/证据 HEAD | `2c31baf`；树 `8adf7a14…`；`git ls-tree -r 2c31baf \| sha256sum` = `949ea209…` |
| 工作树 | 仅 `docs/acceptance/REVIEW-R10.md` 未跟踪；其余干净 |
| `docs/acceptance/R10.md` | `155f924a…` |
| 证据 | staging `7a50d177…`；container `da3ae674…`；rollback `bed493b1…`；caddy `0e9a3357…`；dryrun `689dff3a…`；candidate summary `eb451969…`、scanner-policy `748c0e07…`；supply metadata `766d4d13…`；ci release-image `df19a291…` |
| 代码 | `external_operations.go d920d1ab…`；`metrics.go c0041343…`；`supply_chain.sh e466f4b5…`；`registry_dry_run.sh c35fa45f…`；`release-image.yml 8eaf873b…`；`0005…sql cd08158c…`；`engine.go 63350e55…`；`api/ops.go 82b5a005…`；`api/server.go e0c3002b…`；`api/ui_ops.go 7e4b1351…` |
| 其他脚本/工作流 | `cloudflare_preflight.sh a4528120…`；`publish_gate_audit.sh dcc5c0c2…`；`r10-dry-run.yml 14c122da…`；`caddy_acceptance.sh 8204724e…`；ADR 0002 `cd91de0d…` |
