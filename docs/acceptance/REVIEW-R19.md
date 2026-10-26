# 独立对抗性审查报告 — R19 Trusted Release Gate（REVIEW-R19，post-P2-fix）

- 复审时间：`2026-10-26T23:10–23:45`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `2f9c1d1`**（PR #13，Draft/OPEN，base R18 `0875a1f`）；R19 `docs/acceptance/R19.md`/release manifests 尚未生成；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker/Keycloak/Mailpit/Playwright 未启动；真实证据来自 GitHub Actions artifacts/logs 与仓库代码；本地 `go test ./...` 执行。
- **Gate verdicts：**
  - **R16：PASS (scoped)**；**R17：PARTIAL**；**R18：PARTIAL**。
  - **R19：PARTIAL**（trusted gate P2 关闭；仍受已知 NOT_RUN 限制）。
  - **Combined release gate：NOT READY / BLOCKED**。

**P0=0，P1=0，P2=0，P3=8。**

---

## 1. P2 关闭复核（P2=0）

### P2-1（closed）— workflow event/path trust

- `scripts/trusted_ci_gates.py` 现在：
  - `DEFAULT_WORKFLOWS` 映射 exact `path` + gate names；
  - 只接受 `event in {push, workflow_dispatch}`；
  - 拒绝 same-name workflow at untrusted path（`other_paths` fail closed）；
  - records 含 `path/workflowId/event`。
- `release-image.yml` / `r19-trusted-preflight.yml` / manual workflow 的 wait loop 均校验 `event=="push"` + exact path。
- `ci.yml` 增加 R19 branch push trigger，实际存在 push `ci` run（40343924318），不再依赖 PR-event run。
- **独立验证实际 trusted artifact（artifact 12662170036 / release run 40343924475）**：
  - status PASS、commit `2f9c1d1`、gate_source=`github-actions-api`、release_authorized=false；
  - 六条 gate records：`ci`、`r12-integration`、`r16-sandbox-e2e`、`r17-browser-e2e`、`supply-chain`、`staging-acceptance` **全部 event=push、path 精确、verified=true、attempt=1**；
  - gates 10 项 PASS；blockers 空。
- 测试 `python3 tests/r19/trusted_gates_test.py` / `preflight_trusted_test.py` PASS，含 PR-event/wrong-path negatives。
- **P2 closed。**

### 其他

- Trusted preflight candidate 2f9c1d1：candidate/same-artifact success；publish skipped；无 GHCR。
- release_authorized=false 正确；trusted gate PASS 不授权发布。

---

## 2. P0/P1

- P0=0，P1=0：无 credential artifact leak；trusted-preflight JSON 无 secret；actions:read/contents:read；publish 仍需 environment + packages write；没有新增 publish 权限给 trusted job。
- R16/R17/R18 既有 bound 证据保留（R16 PASS scoped、R17/R18 PARTIAL）。

---

## 3. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R19 docs/manifests | `docs/acceptance/R19.md`、R19 release-candidate-manifest/notes/rollback/blockers 尚未生成；PR #13 Draft/OPEN。 |
| P3-2 | Known NOT_RUN | full browser matrix、Docker/Kind/ACL、full ARM64、trusted BuildKit/SLSA、Live/GHCR/Release/Production 未执行；R19 仍 PARTIAL。 |
| P3-3 | API pagination | trusted_ci_gates 只请求 `per_page=100`；>100 runs 时可能误 MISSING（false reject）。 |
| P3-4 | Event policy consistency | trusted_ci_gates 接受 `workflow_dispatch`，wait loop 只接受 push；manual dispatch 不会让 wait loop 通过，但两处语义不一致，需统一为 push-only 或显式允许 manual。 |
| P3-5 | Publish evidence revalidation | publish job 依赖 trusted-preflight job，但不下载/重验 trusted-preflight JSON；upload `if-no-files-found: ignore`；job dependency 是唯一 gate。 |
| P3-6 | Trusted test realism | tests 是 fake Actions API；没有对 live API pagination/run attempt/duplicate workflow 的集成测试；实际 artifact 已验证 event/path。 |
| P3-7 | Duplicate workflow runner | `r19-trusted-preflight.yml`（workflow_run）与 release-image 内 trusted-preflight 都会运行；两者都不 publish，但重复 evidence/manifest 生命周期需明确。 |
| P3-8 | Release docs honesty | release_authorized=false、blockers 明确；R19 candidate manifests 尚未绑定 2f9c1d1；known NOT_RUN 不能替代。 |

---

## 4. 实际命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（2f9c1d1 worktree） | **0** | 全量绿 |
| `python3 tests/r19/trusted_gates_test.py` | **0** | TRUSTED_GATES_TEST_PASS（含 PR-event/wrong-path negatives） |
| `python3 tests/r19/preflight_trusted_test.py` | **0** | PREFLIGHT_TRUSTED_TEST_PASS |
| `bash tests/r18/release_preflight_test.sh` | **0** | RELEASE_PREFLIGHT_TEST_PASS |
| `gh run download 40343924475 -n r19-trusted-preflight-2f9c…` | **0** | artifact JSON PASS |
| artifact event/path inspect | **0** | 六项 event=push、path 精确、verified=true |
| local real sandbox/browser | **NOT_RUN** | 未启动 Keycloak/Mailpit/Playwright |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署；未修改仓库代码/文档/证据。

---

## 5. CI 证据

- 2f9c1d1 九项 CI 全 success（Lead 提供）：ci 40343935168（PR）、r16 40343935178（PR）、r17 40343935185（PR）、release 40343924475（push）、r12 40343924365、staging 40343924329、supply 40343924380、r9 40343924417、dry-run 40343924382。
- release-image 40343924475：candidate success、same-artifact success、trusted-preflight success、publish skipped。
- trusted artifact 12662170036：commit 2f9c1d1、PASS、六项 event=push/path verified、release_authorized=false。
- 注：用户给出的 `ci 40343935168` event 实际为 pull_request；同 SHA 另有 push ci run 40343924318，trusted artifact 使用的是 push run。

---

## 6. 结论

**R16 PASS (scoped)、R17 PARTIAL、R18 PARTIAL、R19 PARTIAL；combined release gate NOT READY/BLOCKED。** P2-1 已独立复核关闭：trusted gate 现在只接受 exact path + push/workflow_dispatch，实际 artifact 六项全为 push/path verified；P2=0。R19 仍因 full browser/Docker/K8s/ARM64-full/trusted SLSA/Live/GHCR/Release/Production NOT_RUN 保持 PARTIAL；不是 PRE_RELEASE_READY，`LIVE_READY` 不得宣告。

### 冻结

| 项 | 值 |
|---|---|
| 代码 HEAD | `2f9c1d1`；树 `8eb8e013…`；`git ls-tree -r 2f9c1d1 \| sha256sum` = `036dc480…` |
| 文档/证据 | docs tree `bcbd9061…`；R19.md/manifests 未生成；trusted artifact 12662170036 |
| 关键文件 | `scripts/trusted_ci_gates.py`、`release-image.yml`、`r19-trusted-preflight*.yml` |
