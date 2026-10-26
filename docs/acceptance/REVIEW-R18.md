# 独立对抗性审查报告 — R18 Final Release Candidate Closure（REVIEW-R18，post-P2-fix）

- 复审时间：`2026-10-26T21:25–22:00`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码 HEAD `4eb8223`**（PR #12，Draft/OPEN，base R16/R17 `cc60937`）；R18 文档/证据已提交（`docs/acceptance/R18.md`、`release/r18/*`、`docs/acceptance/evidence/r18-release/preflight-actual.json`）；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker/Keycloak/Mailpit/Playwright 未启动；真实证据来自 GitHub Actions artifacts/logs 与仓库代码；本地完整 `go test ./...` 执行。
- **Gate verdicts：**
  - **R16：PASS (scoped)** — 真实 sandbox/app/browser core；artifact policy 已关闭。
  - **R17：PARTIAL** — 真实 Chromium admin core 4/4；fake providers、admin core only、无 external ops/alerts/two-context、pre-release 未完成。
  - **R18：PARTIAL** — P2-2 closed；P2-1 preflight 绑定加强但 **仍未接入 release-image/CI**；full browser matrix/Docker/K8s/ARM64-full/BuildKit-SLSA/Live 未执行。
  - **Combined release gate：NOT READY / BLOCKED**。

**P0=0，P1=0，P2=1（preflight CI 集成残余），P3=8。**

---

## 1. P2 复核

### P2-2（closed）— R17 只上传 sanitized browser evidence

- `.github/workflows/r17-browser-e2e.yml` 现在：
  - 运行 `tests/browser/artifact_allowlist_test.sh`；
  - 设置 `BROWSER_EVIDENCE_PATH`，运行 `tests/browser/run.sh` + sanitizer；
  - `Enforce browser artifact allowlist`（`id: allowlist`）；
  - upload 只含 `browser-evidence.json`，条件 `always() && steps.allowlist.outcome == 'success'`，`if-no-files-found: error`。
- 不再上传 `test-results`/`playwright-report`；trace off；sanitizer 只保留 test/status/duration/commit/runner。
- 本地 `BROWSER_ARTIFACT_ALLOWLIST_TEST_PASS`；**closed。**

### P2-1（partially closed，residual P2）— preflight 绑定加强但未接入 release CI

- **已加强**：
  - 从 `image.tar` config blob 计算 actual image ID，与 image-id/provenance 比较；
  - 解析 same-artifact `SUMMARY` digest/image_id 与 `ATTESTATION_VALID` subject_digest/commit，要求 exact match；
  - gate report 要求 `commit` 匹配 candidate；
  - test 新增 wrong evidence image ID、wrong gate commit、tamper tar 负例。
- **残余**：
  - `git grep release_preflight` 仍只在 `.github/workflows/ci.yml` 的 test step；**release-image/staging/r9 没有调用 preflight**，没有 cross-workflow gate query/CI artifact binding。实际 R18 preflight PASS 是本地/committed evidence，不是 release workflow 的 fail-closed gate；
  - R18.md 仍写 Code/CI HEAD `73fc6aa`，而最终代码 HEAD 是 `4eb8223`；release candidate/preflight/CI runs 绑定 73fc6，final head 未重新构建 candidate；
  - gate report 的 `commit` 只证明一份 JSON 自述，没有绑定 CI run/artifact digest。
- **结论**：P2-1 的算法/绑定大部分关闭，但 **P2-1 residual 仍是 release gate 的 CI 集成缺口**；R18 只能 PARTIAL，不能称为 CI-wired release candidate gate。

---

## 2. P0/P1

- P0=0。
- P1=0：Phase A 旧 credential artifacts 在 5 个 run 独立验证为空；r16 artifact（40326133679/73fc6）只有 4 个 evidence JSON + summary，无 admin-token/master-key/client-secret/state.sqlite/log；new r16 upload fail-closed。
- R16 真实 sandbox/5-identity、crash/restart、idempotency/cancel、Chromium admin core 成立。

---

## 3. R18 candidate/preflight honesty

- `release/r18/release-preflight.json`：status PASS、commit 73fc6、gates all required PASS、authorized_live/ghcr/release/prod NOT_RUN、`release_authorized=false`、blockers 列出 Live/GHCR/Release/Production；**honest**，不宣称可发布。
- `release/r18/release-candidate-manifest.json`、`release-notes.md`、`rollback-manifest.json`、`release-blockers.json` 已提交。
- R18.md 明确 Phase B/C/D/F 未执行、R18_PARTIAL；**没有伪造 full browser/Docker/K8s/ARM64/SLSA/Live PASS**。
- 但 R18.md Code/CI HEAD 73fc6aa 与 final 4eb8223 不一致；candidate 73fc6 未在 final head 重建；preflight 未 CI 调用（P3/P2）。

---

## 4. P3（8）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R18 docs/HEAD | R18.md 仍写 Code/CI HEAD 73fc6aa；final 4eb8223；release manifest/CI runs 绑定 73fc6，candidate 未按 final head 重建。 |
| P3-2 | 4eb8223 CI | PR/push 全部 in_progress/queued；最终九项+新 gates all-green 尚未完成。 |
| P3-3 | Browser full matrix | external ops/alerts/two-context/session/filter/pagination/readonly/409 未执行；仅 admin core 4/4。 |
| P3-4 | Docker/K8s/ACL | Docker secret projection、K8s projected secret、ACL 正反向、non-root/read-only/rotation 未执行。 |
| P3-5 | ARM64/SLSA | full ARM64 build/migration/restore/rollback/Caddy/materializer 未执行；trusted BuildKit/SLSA layer provenance NOT_RUN。 |
| P3-6 | Live/publish | Authorized Live、GHCR、Tag/Release、Production 未执行；release_authorized=false；blockers 明确。 |
| P3-7 | Preflight CI binding | preflight 未在 release-image/staging/r9 调用；gate report 未绑定 CI run/artifact digest；actual pass 不可在 CI 复现。 |
| P3-8 | R16/R17 evidence/CI | R16/R17 real evidence retained；r17 failure artifact sanitized; cleanup checks 4 loopback ports/PIDs; PR workflows contents:read, setup SHAs pinned. |

---

## 5. 实际命令/退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `go test ./... -count=1`（73fc6/4eb changes scripts/docs only） | **0** | 全量绿 |
| `bash tests/r18/release_preflight_test.sh`（4eb8223） | **0** | PASS；wrong evidence image-id/wrong gate commit/tamper tar 负例 |
| `bash tests/browser/artifact_allowlist_test.sh` | **0** | PASS |
| `bash tests/sandbox/artifact_allowlist_test.sh` | **0** | PASS |
| r16 artifact 40326133679 download | **0** | 4 evidence JSON + summary；无敏感文件 |
| old r16 artifacts API（5 runs） | **0** | 全部空 |
| R18 release artifacts | **0** | candidate/same-artifact；preflight/manifests committed |
| 本地 real sandbox/browser | **NOT_RUN** | 未启动 Keycloak/Mailpit/Playwright |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署；未修改仓库代码/文档/证据。

---

## 6. CI 证据

- 73fc6aab 九项全 success（ci 40326133768、r16 40326133679、r17 40326133664、release 40326125182、staging 40326125152、supply 40326125278、r9 40326125465、dry-run 40326125137、r12 40326125127）。
- 4eb8223：PR ci 40328359628、r16 40328361136、r17 40328359638 in_progress；push 全部 gates in_progress。最终 all-green 未完成。
- R18 preflight/evidence 绑定 73fc6，不是 4eb8223 final head。

---

## 7. 结论

**R16：PASS (scoped)；R17：PARTIAL；R18：PARTIAL；combined release gate：NOT READY/BLOCKED。** P2-2 已关闭；P2-1 绑定/负例已加强但 release CI 集成仍缺失，R18 只能保持 PARTIAL。full browser matrix/Docker/K8s/ACL/ARM64-full/BuildKit-SLSA/Live/GHCR 未执行；`LIVE_READY` 不得宣告。修复 preflight CI wiring + final-head candidate rebuild 后可重评，但仍受 missing gates 限制。

### 冻结

| 项 | 值 |
|---|---|
| 代码 HEAD | `4eb8223`；树 `880d18e2…`；`git ls-tree -r 4eb8223 \| sha256sum` = `5c6b2f35…` |
| 文档/证据 | R18.md、release/r18/*、preflight-actual；docs tree `d988c677…`；R18.md 仍写 73fc6 |
| 关键文件 | `release_preflight.py` `977df73…`；`r17-browser-e2e.yml` `b1f49a25…` |
