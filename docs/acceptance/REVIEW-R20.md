# 独立对抗性审查报告 — R20 Final Product / Release Closure（REVIEW-R20）

- 审查时间：`2026-10-26T23:35–00:10`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只新增本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `14ed833`**（PR #14，Draft/OPEN，base R19 `ac6762f`）；R20 无 `docs/acceptance/R20.md`/release manifests；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker/Keycloak/Mailpit/Playwright 未启动；真实证据来自 GitHub Actions artifacts/logs 与仓库代码；本地 `go test ./...` 执行。
- **Gate verdicts：**
  - **R16：PASS (scoped)**；**R17：PARTIAL**；**R18：PARTIAL**；**R19：PARTIAL**。
  - **R20：PARTIAL**（pagination/event fix 成立；untrusted workflow definition 仍是 P2；product/full gates NOT_RUN）。
  - **Combined release gate：NOT READY / BLOCKED**。

**P0=0，P1=0，P2=1，P3=8。**

---

## 1. P2

### P2-1 — trusted gate 仍信任 commit 内的 workflow definition（path/event 已验，但 workflow 内容可自改）

- **位置**：`scripts/trusted_ci_gates.py`：只校验 `name`、exact `path`、`event=push`、full SHA、repository、status/conclusion；没有校验 run 所用 workflow 文件内容/blob 是否等于受保护 base/default 的工作流定义，也没有 required status checks/ruleset 绑定。
- **触发**：R20 分支是 feature branch push；贡献者可在同一 commit 修改 `.github/workflows/ci.yml`（或任一 required workflow）为 no-op/弱测试，push 后 API 返回 exact path + push + success；trusted preflight 会 PASS。exact path/event 不能阻止 workflow definition 被自改。
- **影响**：trusted gate 的“no caller PASS”只消除了 caller 自报，未消除 commit 内 workflow 自报；release gate 仍可能被 self-declared workflow 绕过。
- **修复/测试**：
  1. 校验 workflow 文件 blob/digest（对受保护 base/default ref）与 run 的 workflow 版本一致；
  2. 使用 GitHub branch protection/ruleset required checks，或验证 run 由受保护 ref/required check 产生；
  3. 记录并比较 workflow_id；拒绝与受信 baseline 不一致的 definition；
  4. 加测试：same path + modified workflow definition + success API 必须 FAIL。

---

## 2. P0/P1

- P0=0，P1=0：无 credential artifact；trusted artifact 无 secret；release_authorized=false；no publish/merge/production。
- R20 pagination/event 修复成立：`TRUSTED_EVENTS={"push"}`，R20 branch wired，tests PASS；但未改变 P2-1 workflow-definition trust。
- 历史 R16/R17/R18/R19 证据保留：R16 PASS scoped、R17/R18/R19 PARTIAL。

---

## 3. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | Pagination test realism | `tests/r20/trusted_pagination_test.py` 的 required runs 全在 page1，page2 为空；未证明 required gate 仅在 page2 时能找到，也无跨页 dedupe/重叠测试。 |
| P3-2 | Page cap/pagination scope | `per_page=100` + `max_pages=20` 上限 2000 runs；wait loops（release-image / r19-trusted-preflight / manual）仍只用单页 100，未用 pagination；可能 false reject。 |
| P3-3 | Link trust | `next_link` 跟随 API `Link` header 的绝对 URL，并把 `Authorization` token 发到该 URL；应校验 scheme/host 为 GitHub API，避免 token 发往异常 host。 |
| P3-4 | Publish revalidation | publish `needs: trusted-preflight`，但不下载/重验 trusted-preflight JSON；`if-no-files-found: ignore`；job dependency 是唯一 gate。 |
| P3-5 | R20 candidate/preflight | R20 candidate/preflight/manifests 未生成；PR #14 Draft/OPEN；不能把 R19 2f9c1d1 的 trusted success 当 R20 evidence。 |
| P3-6 | Full product/UI gates | full operator UI/alerts pages、full browser matrix（external ops/alerts/two-context/session/filter/pagination/readonly/409）NOT_RUN；仅既有 admin core。 |
| P3-7 | Infra/ARM64/SLSA | Docker/Kind/ACL、full ARM64/native、trusted BuildKit/SLSA、historical old-binary backup 未执行。 |
| P3-8 | Live/release | Live/GHCR/Release/Production、PR stack merge dry-run 未执行；release_authorized=false；no publish. |

---

## 4. 实际命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（14ed833 worktree） | **0** | 全量绿 |
| `python3 tests/r20/trusted_pagination_test.py` | **0** | TRUSTED_PAGINATION_TEST_PASS（page2 empty/failure/loop；required gate 全 page1） |
| `python3 tests/r19/trusted_gates_test.py` | **0** | TRUSTED_GATES_TEST_PASS |
| `python3 tests/r19/preflight_trusted_test.py` | **0** | PREFLIGHT_TRUSTED_TEST_PASS |
| R20 CI | **in_progress** | PR ci/r16/r17 success；push ci/r16/r17/supply/dry-run/r9/staging success；release-image/r12 still in_progress；无 R20 trusted artifact |
| local real sandbox/browser | **NOT_RUN** | 未启动真实 fixture |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署；未修改仓库代码/文档/证据。未输出 secret 值。

---

## 5. CI 证据

- R20 PR：ci 40352164098 success、r16 40352164224 success、r17 40352164272 success。
- R20 push：ci 40352127913、r16 40352127909、r17 40352127812、supply 40352128002、dry-run 40352127813、r9 40352128683、staging 40352127970 success；release-image 40352127862、r12 40352127980 in_progress。
- R20 trusted-preflight artifact/candidate 尚未产出；不能引用 R19 2f9c1d1 trusted success 作为 R20 证据。

---

## 6. 结论

**R16 PASS (scoped)、R17 PARTIAL、R18 PARTIAL、R19 PARTIAL、R20 PARTIAL；combined release gate NOT READY/BLOCKED。** pagination/event/push 修复成立，trusted pagination tests PASS；但 trusted gate 仍信任 commit 内可自改的 workflow definition（P2-1），且 R20 candidate/preflight、full product/UI、Docker/K8s/ARM64/BuildKit-SLSA/Live 未执行。R20 不是 PRE_RELEASE_READY；`LIVE_READY` 不得宣告。

### 冻结

| 项 | 值 |
|---|---|
| 代码 HEAD | `14ed833`；树 `9b6c5400…`；`git ls-tree -r 14ed833 \| sha256sum` = `9431c95a…` |
| 文档/证据 | 无 R20.md/manifests；docs tree `09392373…`；R20 artifact 尚未产出 |
| 关键文件 | `scripts/trusted_ci_gates.py` `a3880e8f…`；`tests/r20/trusted_pagination_test.py` `37599ea7…` |
