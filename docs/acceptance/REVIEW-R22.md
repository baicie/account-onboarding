# 独立对抗性审查报告 — R22 Product-first Final Acceptance（REVIEW-R22，final head）

- 复审时间：`2026-10-27T11:25–12:00`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只新增/更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `abd185d`**（R22 PR #16，Draft/OPEN，base R21 `98645e8`）；R22 acceptance docs 已提交但仍停在前一 head；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker/Keycloak/Mailpit/Playwright 未启动；真实证据来自 GitHub Actions artifacts/logs 与仓库代码；本地 Go 测试执行。
- **Gate verdicts：**
  - **R16：PASS (scoped)**；**R17：PARTIAL**；**R18：PARTIAL**；**R19：PARTIAL**；**R20：PARTIAL**；**R21：PARTIAL**。
  - **R22：PARTIAL**（14-test matrix 与 validation 增强成立；full matrix/infra/live 未完成）。
  - **Combined release gate：NOT READY / BLOCKED**。

**P0=0，P1=0，P2=0，P3=8。**

---

## 1. P2 复核（P2=0）

### P2-1（closed）— Resolve/Re-arm 语义

- `ResolveAlertOutbox` 只更新 `PENDING/IN_FLIGHT/RETRY_PENDING`；DELIVERED/DEAD_LETTER 不再被 Resolve 改写。
- 新增显式 `RearmAlertOutbox`：仅 DELIVERED/DEAD_LETTER→RESOLVED，delivery history 不变，后续 Reconcile 可重新入队。
- UI 独立 Re-arm 按钮/确认；Dispatch 无 transport 时 disabled；API/browser tests 覆盖。
- **P2 closed。**

### P2-2（closed）— publish revalidation

- `verify_trusted_artifact.py` 要求 10 gates/7 workflow records/generation run identity/candidate commit/provenance/重算 image ID；tests reject empty runs/missing gate/wrong image-id/run-id。
- **P2 closed。**

---

## 2. R22 final head 实际增量（abd185d）

- **M1 filter/cursor validation**：`/ui/` invalid status/template/cursor → 400；storage `ErrConflict` → 400；external-ops invalid status → 400；alerts invalid status → 400；alert detail `bad` → 400、`al-does-not-exist` → 404。
- **Run-ID redirect test**：`TestRunIDLookupRedirectsToRunDetail` 真实 `/ui/?run=<id>` → 303 `/ui/runs/<id>`。
- **>50-run cursor pagination test**：`TestRunsCursorPaginationNoDuplicates` 55 runs、no duplicates。
- **External ops UX**：error/notice 渲染、conflict message；detail/return context。
- **Deterministic seeder + 14-test matrix**：`tests/browser/seed/main.go`；7 operator + 7 matrix; `workers:1`, `retries:0`; CI `ONBOARD_BROWSER_MATRIX=1`。
- **Matrix coverage**：run lookup/filter/pagination/invalid 400；external filters/detail/audit；two independent contexts resolve once/conflict; alerts delivered/deadletter/rearm/delivery-history/transport fail-closed; pending resolve; create/reload/logout replay.
- **CI PR r17 40444650921 success**，artifact `r17-browser-evidence`：**expected=14, unexpected=0, tests=14**；artifact commit=`b35df260…`（PR merge SHA，不是 head abd185d；P3）。
- **本地**：`go test ./...` 0；targeted 6/6 PASS；browser 14/14 由 Lead 报告，reviewer 未启动 fixture。

---

## 3. P0/P1

- P0=0，P1=0：无 credential/CSRF 直接绕过；`uiAuth` + CSRF 覆盖 POST；alert actions audit；无可复现 secret。
- R22 产品功能成立：真实 SQLite alerts/outbox、run filters/cursor/lookup、external-ops run_id filter；14-test browser matrix evidence expected14/unexpected0。

---

## 4. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R22 docs/evidence stale | `docs/acceptance/R22.md`/`R22-EXECUTION-STATE.md` 仍写 Code HEAD `4916687`、Browser 7/7；`r22-gate-matrix.json` 仍把 `two-browser-context-resolve` 标 NOT_RUN；final abc/abd 的 14-test matrix 未反映。 |
| P3-2 | Browser evidence identity | PR r17 artifact commit=`b35df260…`（PR merge SHA），不是 head `abd185d`；PR 事件的 GITHUB_SHA 是 merge commit，evidence 未绑定 head；push r17 仍在 in_progress。 |
| P3-3 | Full browser matrix | 14 tests 仍缺 session expiry/revocation/readonly、real-sandbox browser、in-flight/retry alerts、external-ops full filters/409；没有 `BROWSER_FULL_MATRIX_PASS`。 |
| P3-4 | Fixture honesty | `tests/browser/seed/main.go` 用 raw SQL 把 55 runs 置 `FAILED_TERMINAL`，并直接 claim/complete alert fixtures；测试确定性好，但不等于真实 workflow 状态转换/transport 行为。 |
| P3-5 | M1 edges | alert offset invalid silently 0；`renderDashboardError` 使用空 `http.Request` 取 auth，错误页无 CSRF/session 上下文；external provider 未枚举校验；cursor 只校验长度/base64。 |
| P3-6 | Infra/ARM64/SLSA | Docker/Kind/ACL、full ARM64/native、trusted BuildKit/SLSA、historical backup 未执行。 |
| P3-7 | Merge/Live | PR stack merge dry-run、Live/GHCR/Release/Production 未执行；release_authorized=false。 |
| P3-8 | R21 trust/CI | R21 workflow-definition trust P2 仍 BLOCKED；R22 push CI 尚未全绿、无 R22 candidate/trusted artifact；PR untrusted code；no publish。 |

---

## 5. 实际命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（abd185d worktree） | **0** | 全量绿 |
| `go test ./internal/api -run 'TestInvalidRunFilterAndCursorAre400|TestRunsCursorPaginationNoDuplicates|TestInvalidExternalAndAlertFiltersAre400|TestAlertDetailDistinguishesNotFoundAndInvalid|TestRunIDLookupRedirectsToRunDetail|TestResolveDoesNotRewrite' -v` | **0** | 6/6 PASS |
| `gh run download 40444650921 -n r17-browser-evidence` | **0** | expected=14 unexpected=0 tests=14；commit b35df260（merge SHA） |
| `tests/browser/run.sh` | **NOT_RUN** | reviewer 未启动 Playwright/onboardd；Lead 报告 14/14 twice |
| R22 CI | **in_progress** | PR ci 40444651010/r17 40444650921 success、r16 40444650885 in_progress；push r10 40444645078 success；release-image 40444645115、r9/r12/supply/staging/ci/r16/r17 still in_progress；无 R22 candidate artifact |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署；未修改仓库代码/文档/证据。

---

## 6. CI 证据

- R22 PR：r17-browser 40444650921 success（artifact expected14/unexpected0/tests14，commit b35df260 merge SHA）；ci 40444651010 success；r16 40444650885 in_progress。
- R22 push：r10 40444645078 success；release-image 40444645115、r9 40444645154、r12 40444645112、supply 40444645105、staging 40444645126、ci 40444645167、r16 40444645095、r17 40444645157 in_progress。
- R22 candidate/trusted artifact 尚未产出；不能引用 R21/R20 的 trusted success 代替。

---

## 7. 结论

**R16 PASS (scoped)、R17 PARTIAL、R18 PARTIAL、R19 PARTIAL、R20 PARTIAL、R21 PARTIAL、R22 PARTIAL；combined release gate NOT READY/BLOCKED。** R22 的 M1 validation、run-ID/cursor tests、external-ops UX、deterministic 14-test matrix 成立；CI PR evidence expected14/unexpected0/tests14。P2=0；但 R22 docs/evidence 仍停在前一 head、PR merge SHA vs head、full browser matrix/session/readonly/real-sandbox、Docker/K8s/ACL/ARM64/BuildKit/SLSA/historical backup/merge dry-run/Live/GHCR 未完成，R21 workflow-definition trust P2 仍 BLOCKED。R22 不是 Final Qualification；`LIVE_READY` 不得宣告。

### 冻结

| 项 | 值 |
|---|---|
| 代码 HEAD | `abd185d`；树 `dc53afcf…`；`git ls-tree -r abd185d \| sha256sum` = `9bb0d5ca…` |
| 文档/证据 | R22.md/EXECUTION/gate-matrix 仍写 4916687/7 tests；docs tree `c0c77e88…`；R22 artifact pending |
| 关键文件 | `internal/api/ui_alerts.go` `5b0541bd…`；`internal/api/ui.go` `69063703…`；`tests/browser/matrix.spec.js` `9b539f75…`；`tests/browser/seed/main.go` `bcd0d3c3…` |
