# 独立对抗性审查报告 — R16/R17 Real Sandbox & Pre-release（REVIEW-R16-R17，final）

- 复审时间：`2026-10-26T20:35–21:05`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `48b9ca3`**（PR #11，Draft/OPEN，base R15.1 `c4f7927`）；R16/R17 无 `docs/acceptance/R16.md`/`R17.md` 或独立 evidence；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker/Keycloak/Mailpit/Playwright 未启动；真实证据来自 GitHub Actions artifacts/logs 与仓库代码；本地完整 `go test ./...` 执行。
- **Gate verdicts：**
  - **R16：PASS (scoped)** — 真实 Keycloak/Mailpit/onboardd sandbox、5-identity、crash/restart、idempotency/cancel、real Chromium admin core 均通过；artifact P1/P2 已关闭。
  - **R17：PARTIAL** — 真实 Chromium admin core 4/4；fake providers、admin core only、无 external ops/alerts/two-context、pre-release gates 未完成。
  - **Combined release gate：NOT READY / BLOCKED** — Docker/K8s materializer、full ARM64 staging、BuildKit/SLSA、release manifest、GHCR、Authorized Live 未执行。

**P0=0，P1=0，P2=0，P3=8。**

---

## 1. P1/P2 关闭复核

### P1（closed）— credential-bearing r16 artifact

- 独立验证旧 `r16-sandbox-evidence` artifacts 在以下 run 均为空：
  40317782501、40317774810、40316566208、40316559127、40315254616。
- 新 workflow 不再复制 `.sandbox/e2e`/logs；只选 `evidence-*.json` + 生成 `summary.txt`。
- **P1 closed。**

### P2-1（closed）— upload 必须 allowlist 成功

- workflow 已拆为 `Enforce artifact allowlist`（`id: allowlist`）与 upload：
  `if: always() && steps.allowlist.outcome == 'success'`。
- collect/allowlist 失败时 upload 不执行；独立 synthetic safe tree 0、bad tree 1。
- **closed。**

### P2-2（closed）— allowlist 不打印 secret 内容

- `artifact_allowlist.sh` 改用 `grep -RIlE`，只打印文件名；删除 `/tmp` hits 文件。
- 独立合成：`admin-token=supersecret` evidence → exit1，输出只含 `evidence-1.json`，**日志不含 secret 值**。
- 新 `tests/sandbox/artifact_allowlist_test.sh` 覆盖 safe tree PASS、admin-token filename rejected 且 value absent、master-key content rejected 且 value absent；本地 **ARTIFACT_ALLOWLIST_TEST_PASS**，CI 在 sandbox bring-up 前运行。
- **closed。**

---

## 2. R16 功能复核（PASS scoped）

- **Sandbox infra**：`tests/sandbox/verify.sh --count 5` 代码驱动 loopback Keycloak Admin API + Mailpit + OIDC/PKCE/userinfo/delete；CI step success。
- **Real app E2E**：`ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e` 使用真实 Keycloak/Mailpit/onboardd；断言 5/5 COMPLETED、distinct subjects/users/emails、one email per address、userinfo sub/email/verified、crash/restart no duplicate identity、idempotency one run/one email、cancel no OAuth/credential。CI step success。
- **Real browser**：Chromium 4 tests（invalid/valid login、UI create/reload、CSRF-less 403、logout）通过；app/Caddy/nginx `Referrer-Policy: same-origin` 修复 Origin null。
- **Honesty**：browser 使用 fake providers、仅 admin core；不把 R17 当完整 provider E2E。
- 本地 off-fixture `go test ./...` 0，`go test ./tests/e2e` 0（SKIP）。

---

## 3. R17 状态（PARTIAL）

- 真实 Chromium admin core 4/4 通过。
- 未覆盖：external ops、alerts、two independent contexts、provider live flows、two-admin concurrency via browser、pre-release manifest。
- R17 **PARTIAL**，不 over-credit。

---

## 4. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R16/R17 docs/PR | 无 R16/R17 acceptance docs/evidence；PR #11 Draft/OPEN；最终 docs-only commit 后核对。 |
| P3-2 | 48b9ca3 CI | review 冻结时 PR ci/r17 success、PR r16 in_progress；push r16/r10/supply success，release/r17/r9/staging/r12 still in_progress；最终七项+两项 all-green 未完成。 |
| P3-3 | R17 scope | fake providers、admin core only；no external ops/alerts/two-context/provider live/two-admin browser。 |
| P3-4 | R17 artifact redaction | trace off 且删 trace.zip；失败 screenshot/playwright-report 仍需上传前 scrub/allowlist。 |
| P3-5 | Referrer policy | no-referrer → same-origin 改变既有安全语义；同源 URL 可能出现在 referrer，需在安全基线记录。 |
| P3-6 | Sandbox cleanup | `.sandbox/secrets` 0600、`.sandbox` gitignored、purge 存在；workflow `down.sh --purge || true` 吞 cleanup 失败；redaction 不能依赖 purge。 |
| P3-7 | CI supply chain/PR | r16/r17 均 pull_request + contents:read、无 repo secrets；setup-java/setup-node 未 pin SHA；r16 下载 Keycloak/Mailpit；PR 代码可运行 workflow。 |
| P3-8 | Missing gates | Docker/K8s materializer real projection、full ARM64 staging、BuildKit/SLSA、release manifest、publish/GHCR、Authorized Live 均 NOT_RUN；combined release 仍 NOT READY。 |

---

## 5. 实际命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（6db17f3 worktree） | **0** | 全量绿（sandbox SKIP） |
| `go test ./tests/e2e -count=1` | **0** | 无 fixture SKIP |
| `go test ./internal/api -run TestSecurityHeaders` | **0** | Referrer-Policy |
| `go test ./internal/ops -run TestHTTPHardening` | **0** | headers/CSP |
| `bash tests/sandbox/artifact_allowlist_test.sh`（48b9ca3） | **0** | ARTIFACT_ALLOWLIST_TEST_PASS |
| safe allowlist tree | **0** | PASS |
| admin-token evidence / master-key content | **1 each** | reject，日志仅文件名，无 secret 值 |
| old r16 artifacts API query（5 runs） | **0** | 全部空 |
| 48b9ca3 CI | **in_progress** | 尚未产出新 artifact/最终结果 |
| local `tests/sandbox/verify.sh --count 5` | **NOT_RUN** | 未启动 Keycloak/Mailpit |
| local `tests/browser/run.sh` | **NOT_RUN** | 未启动 Playwright fixture |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署；未修改仓库代码/文档/证据。只检查 artifact 文件名/大小/allowlist，不输出 secret 值。未执行真实 sandbox/browser 本地回归，依赖 CI artifact/step 证据。

---

## 6. CI

- 前序 head 92d612b：PR ci 40319962551、r16 40319962474、r17 40319962510 success；旧 artifacts 已删除。
- 48b9ca3：PR ci 40320761116 success、r17 40320760147 success、r16 40320760117 in_progress；push r16 40320753869、r10 40320753679、supply 40320753681 success，release/r17/r9/staging/r12 still in_progress。最终 all-green 未完成，不能声称。
- 新 r16 artifact 尚未产出；需等 CI 完成后验证 artifact 只含 evidence JSON/summary。

---

## 7. 结论

**R16：PASS (scoped)。** 真实 sandbox/app/browser core 可接受；P1 artifact 与 P2 upload/logging 均关闭。限制：docs/CI 最终状态、真实 Docker/K8s/ARM64-full/provenance 未执行。
**R17：PARTIAL。** 真实 Chromium admin core 4/4；scope 明确不含 external ops/alerts/two-context/pre-release。
**Combined release gate：NOT READY / BLOCKED。** 缺 Docker/K8s materializer real projection、full ARM64 staging、BuildKit/SLSA、release manifest、GHCR、Authorized Live。`LIVE_READY` 不得宣告。

### 冻结

| 项 | 值 |
|---|---|
| 代码 HEAD | `48b9ca3`；树 `953e01b6…`；`git ls-tree -r 48b9ca3 \| sha256sum` = `1e23e4aa…` |
| 文档/证据 | 无 R16/R17 acceptance report；docs tree `d57577c3…`；旧 r16 artifacts 空；新 artifact 待 CI |
| 关键文件 | `r16-sandbox-e2e.yml` `be471c2d…`；`artifact_allowlist.sh` `a7fa4c8a…`；`artifact_allowlist_test.sh` `d6a98694…` |
