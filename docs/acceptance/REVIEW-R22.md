# 独立对抗性审查报告 — R22 Product-first Final Acceptance（REVIEW-R22，post-P2-fix）

- 复审时间：`2026-10-27T09:10–09:50`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `4916687`**（R22 PR #16，Draft/OPEN，base R21 `98645e8`）；R22 无 `docs/acceptance/R22.md`/release manifests；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker/Keycloak/Mailpit/Playwright 未启动；真实证据来自 GitHub Actions logs/artifacts 与仓库代码；本地 Go 测试执行。
- **Gate verdicts：**
  - **R16：PASS (scoped)**；**R17：PARTIAL**；**R18：PARTIAL**；**R19：PARTIAL**；**R20：PARTIAL**；**R21：PARTIAL**。
  - **R22：PARTIAL**（真实 UI/API 功能成立；full matrix/known NOT_RUN 未完成）。
  - **Combined release gate：NOT READY / BLOCKED**。

**P0=0，P1=0，P2=0，P3=8。**

---

## 1. P2 关闭复核（P2=0）

### P2-1（closed）— Resolve 不再改写 DELIVERED/DEAD_LETTER；新增显式 Re-arm

- `internal/storage/alert_outbox.go` `ResolveAlertOutbox` 已改为：
  ```sql
  WHERE rule=? AND status IN (PENDING,IN_FLIGHT,RETRY_PENDING)
  ```
  不再触碰 DELIVERED/DEAD_LETTER，delivery history 不受影响。
- 新增 `RearmAlertOutbox`：仅把 DELIVERED/DEAD_LETTER→RESOLVED，保留 `alert_deliveries` 历史；后续 Reconcile 可重新入队新 active row。
- UI 有独立 Re-arm 按钮/确认文案（list/detail），与 Resolve 分离；Dispatch 无 transport 时 disabled。
- 独立验证：
  - resolve/rearm API test PASS：Resolve 后 DELIVERED 不变；Re-arm 后 RESOLVED；delivery history 仍 DELIVERED。
  - `TestRunIDLookupRedirectsToRunDetail` PASS：真实 `/ui/?run=<id>` → 303 `/ui/runs/<id>`。
  - Browser alert test 现覆盖 Dispatch disabled、detail/delivery history；run-ID redirect 有 API test。
- **P2 closed。**

---

## 2. P0/P1

- P0=0，P1=0：无 credential/CSRF 直接绕过；`uiAuth` 要求 cookie session，POST 校验 CSRF；alert 表单含 `csrf`；无 secret 泄漏。
- R22 产品功能成立：`/ui/alerts/outbox` list/detail 读取真实 `alert_outbox`/`alert_deliveries`；Reconcile/Dispatch/Resolve/Re-arm 路由；run status/template filters + cursor + run-ID lookup；external-ops `run_id` filter。
- R22 仍不是 final qualification：full browser matrix/two-context、Docker/K8s/ACL、ARM64-full、BuildKit/SLSA、historical backup、merge dry-run、Live/GHCR 未执行。

---

## 3. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R22 docs/manifests | 无 `docs/acceptance/R22.md`/release manifests/candidate；PR #16 Draft/OPEN；CI 未完成。 |
| P3-2 | Full browser matrix | 7 tests 仍是 admin core/alerts list/filters/run detail；无 two-context Resolve、external ops/alerts 全状态、session/readonly/409/网络错误；`retries:1` 可掩盖 flake。 |
| P3-3 | Re-arm browser coverage | Re-arm 有 storage/API test 和 list/detail 按钮，但 Playwright 未实际点击 Re-arm/confirm；alert browser 只测 Resolve PENDING。 |
| P3-4 | Run filter edges | 非法 cursor 仍 500（非 400）；status/template 无服务端枚举校验；external-ops `run_id` 无浏览器/API 专测。 |
| P3-5 | Alert semantics edges | status 任意字符串只产生空页；alert detail 任意 store error 都 404；Resolve/Re-arm 只能按 rule，不能只选单个 outbox row。 |
| P3-6 | Infra/ARM64/SLSA | Docker/Kind/ACL、full ARM64/native、trusted BuildKit/SLSA、historical backup 未执行。 |
| P3-7 | Merge/Live | PR stack merge dry-run、Live/GHCR/Release/Production 未执行；release_authorized=false。 |
| P3-8 | CI/permissions | R22 CI 尚未全绿；R21 workflow-definition trust P2 仍 BLOCKED；PR workflow untrusted code；no publish. |

---

## 4. 实际命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（4916687 worktree） | **0** | 全量绿 |
| `go test ./internal/api -run 'TestResolve|TestRunIDLookup|TestAlertsPage|TestAlertDispatch' -v` | **0** | 4/4 PASS |
| `tests/browser/run.sh` | **NOT_RUN** | reviewer 未启动 Playwright/onboardd；Lead 报告 7/7 |
| R22 CI | **in_progress** | PR ci 40431418113/r16 40431418098 success、r17 40431418223 in_progress；push ci/r9/r10/r17/supply success；release-image 40431412342 pending；staging/r12/r16 in_progress；无 R22 artifact |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署；未修改仓库代码/文档/证据。

---

## 5. CI 证据

- R22 PR：ci 40431418113 success、r16 40431418098 success、r17 40431418223 in_progress。
- R22 push：ci 40431412191、r9 40431412167、r10 40431412312、r17 40431412203、supply 40431412246 success；release-image 40431412342 pending；r12 40431412200、staging 40431412238、r16 40431412234 in_progress。
- 无 R22 acceptance/candidate artifact；不能引用 R21 之前成功代替。

---

## 6. 结论

**R16 PASS (scoped)、R17 PARTIAL、R18 PARTIAL、R19 PARTIAL、R20 PARTIAL、R21 PARTIAL、R22 PARTIAL；combined release gate NOT READY/BLOCKED。** P2-1 已独立复核关闭：Resolve 只改 active rows，Re-arm 显式且 delivery history 保留；run-ID redirect/detail-history tests 补齐。R22 仍因 full browser matrix、Docker/K8s/ACL、ARM64-full、BuildKit/SLSA、historical backup、merge dry-run、Live/GHCR/Release/Production NOT_RUN 保持 PARTIAL；R21 workflow-definition trust P2 仍 BLOCKED。不是 PRE_RELEASE_READY；`LIVE_READY` 不得宣告。

### 冻结

| 项 | 值 |
|---|---|
| 代码 HEAD | `4916687`；树 `b9522421…`；`git ls-tree -r 4916687 \| sha256sum` = `50cbe80a…` |
| 文档/证据 | 无 R22.md/manifests；docs tree `6dc8991d…`；R22 artifact 未产出 |
| 关键文件 | `internal/api/ui_alerts.go` `ecde4615…`；`internal/storage/alert_outbox.go` `3688c996…`；`internal/api/r22_ui_test.go` `70511ee3…` |
