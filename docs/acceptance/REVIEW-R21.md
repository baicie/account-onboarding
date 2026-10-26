# 独立对抗性审查报告 — R21 Final Qualification / Trusted Artifact Revalidation（REVIEW-R21，post-P2-fix）

- 复审时间：`2026-10-27T00:45–01:20`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `b739b9d`**（PR #15，Draft/OPEN，base R20 `d123497`）；R21 无 `docs/acceptance/R21.md`/release manifests；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker/Keycloak/Mailpit/Playwright 未启动；真实证据来自 GitHub Actions logs/artifacts 与仓库代码；本地 `go test ./...` 执行。
- **Gate verdicts：**
  - **R16：PASS (scoped)**；**R17：PARTIAL**；**R18：PARTIAL**；**R19：PARTIAL**；**R20：PARTIAL**。
  - **R21：PARTIAL**（P2-2 closed；P2-1 workflow-definition trust 仍 BLOCKED；known NOT_RUN）。
  - **Combined release gate：NOT READY / BLOCKED**。

**P0=0，P1=0，P2=1（P2-1 workflow-definition trust），P3=8。**

---

## 1. P2 复核

### P2-2（closed）— publish revalidation 绑定 required gates/run identity/image ID

- `scripts/verify_trusted_artifact.py` 现在要求：
  - status PASS、release_authorized=false、commit、gate_source=github-actions-api；
  - `generation_run_id`/`generation_run_attempt` 与 CLI 当前 run/attempt 一致；
  - **全部 10 个 required gates PASS**；
  - **全部 7 个 required workflow records**（ci、r12-integration、r16-sandbox-e2e、r17-browser-e2e、supply-chain、staging-acceptance、same-artifact）存在、verified；非 same-artifact 记录 event=push/commit match；
  - candidate `commit.txt`/provenance commit、SBOM/provenance hash；
  - 从 `image.tar` config blob **重算 image ID**，并同时校验 candidate image-id、provenance image_id、preflight candidate_image_id。
- `scripts/release_preflight.py` 增加 `generation_run_id`/`generation_run_attempt`（GITHUB_RUN_ID/GITHUB_RUN_ATTEMPT）。
- 测试覆盖 empty ci_runs、missing gate、wrong image-id、wrong run-id；负例均 reject。
- 本地 `PUBLISH_REVALIDATION_TEST_PASS`；之前 empty-gates/arbitrary-image-id 的 P2-2 复现已 closed。
- **P2-2 closed。**

### P2-1（remains BLOCKED）— workflow-definition trust

- trusted gate 仍只校验 exact path/event=push/SHA/name/repo/conclusion；不校验 required workflow 文件 blob/digest 与受保护 base/default 一致，也没有 ruleset required checks/workflow_id baseline。
- R21 feature branch push 仍可在同一 commit 修改 required workflow 为 no-op/弱测试；API 返回 exact path+push+success，trusted gate 可 self-declared。
- **这是 documented BLOCKED 项，不能标 PASS；需要 protected ruleset/baseline/admin（scope 外）。**

---

## 2. P0/P1

- P0=0，P1=0：无 direct publish/secret exposure；publish 未执行；trusted gate 无 secret；R21 CI 尚在运行。
- R20 的 pagination/event/push 基础成立；R21 publish revalidation 现在独立于 job dependency 检查 artifact/gates/image identity；P2-2 closed。
- R16/R17/R18/R19/R20 历史状态保留（R16 PASS scoped；其余 PARTIAL）。

---

## 3. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R21 docs/manifests | 无 `docs/acceptance/R21.md`/release manifests/candidate；PR #15 Draft/OPEN；CI 未完成。 |
| P3-2 | Pagination test | `trusted_pagination_test.py` 的 required runs 全在 page1，page2 为空；未证明 required gate 只在 page2；无 overlap/dedupe 测试。 |
| P3-3 | Pagination boundary | wait loops（release-image/r19-preflight/manual）仍单页 `per_page=100` 未用 pagination；page cap 20/per_page100 可 false reject；`next_link` 跟随 Link 绝对 URL 时把 token 发往该 URL，需校验 host/scheme。 |
| P3-4 | Required list drift | verify_trusted_artifact 的 REQUIRED_GATES/REQUIRED_RUNS 是硬编码副本；与 release_preflight/trusted_ci_gates 的 gate 列表需单一来源，否则后续 drift 可能 false reject/accept。 |
| P3-5 | Artifact generation run | preflight `generation_run_id` 由 GITHUB_RUN_ID 写入；release-image 的 trusted-preflight job 与 publish job 同一 run 时成立；separate r19-trusted-preflight workflow_run 产出的 artifact 不绑定此路径，需明确只使用 same-run artifact。 |
| P3-6 | Full product/UI | full operator UI/alerts pages、full browser matrix（external ops/two-context/session/filter/pagination/readonly/409）NOT_RUN。 |
| P3-7 | Infra/ARM64/SLSA | Docker/Kind/ACL、full ARM64/native、trusted BuildKit/SLSA、historical backup、merge dry-run NOT_RUN。 |
| P3-8 | Live/release honesty | Live/GHCR/Release/Production 未执行；release_authorized=false；R21 candidate/preflight 未产出；no publish. |

---

## 4. 实际命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（b739b9d worktree） | **0** | 全量绿 |
| `python3 tests/r21/publish_revalidation_test.py` | **0** | PUBLISH_REVALIDATION_TEST_PASS；empty ci_runs/missing gate/wrong image-id/wrong run-id 负例 |
| `python3 tests/r20/trusted_pagination_test.py` | **0** | TRUSTED_PAGINATION_TEST_PASS |
| `python3 tests/r19/trusted_gates_test.py` | **0** | TRUSTED_GATES_TEST_PASS |
| R21 CI | **in_progress** | PR ci/r16/r17 in_progress；push release-image pending；ci/r9/r12/supply/staging/r16/r17/r10 in_progress；无 R21 trusted artifact |
| local real sandbox/browser | **NOT_RUN** | 未启动真实 fixture |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署；未修改仓库代码/文档/证据。

---

## 5. CI 证据

- R21 PR：ci 40358844279、r16 40358844369、r17 40358844522 in_progress。
- R21 push：release-image 40358834043 pending；ci 40358834191、r9 40358834164、r12 40358834090、supply 40358834137、staging 40358834020、r16 40358834124、r17 40358834039、r10 40358834125 in_progress。
- **R21 candidate/trusted-preflight artifact 尚未产出**；不能引用 R20 的 trusted success 作为 R21 evidence。

---

## 6. 结论

**R16 PASS (scoped)、R17 PARTIAL、R18 PARTIAL、R19 PARTIAL、R20 PARTIAL、R21 PARTIAL；combined release gate NOT READY/BLOCKED。** P2-2 publish revalidation 已独立复核关闭：required gates/runs、generation run identity、candidate commit/provenance、image ID 重算均有负例且测试 PASS。P2-1 workflow-definition trust 仍是 documented BLOCKED，需 protected ruleset/baseline/admin；full UI/browser、Docker/K8s/ACL、ARM64-full、trusted BuildKit/SLSA、historical backup、merge dry-run、Live/GHCR 未执行。R21 不是 PRE_RELEASE_READY；`LIVE_READY` 不得宣告。

### 冻结

| 项 | 值 |
|---|---|
| 代码 HEAD | `b739b9d`；树 `79196100…`；`git ls-tree -r b739b9d \| sha256sum` = `111ca743…` |
| 文档/证据 | 无 R21.md/manifests；docs tree `6827cfb7…`；R21 artifact 未产出 |
| 关键文件 | `scripts/verify_trusted_artifact.py` `697abad2…`；`scripts/release_preflight.py` `42c1515c…`；`tests/r21/publish_revalidation_test.py` `ac7849a2…` |
