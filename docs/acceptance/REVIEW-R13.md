# 独立对抗性审查报告 — R13 Integration Reliability（REVIEW-R13，post-fix）

- 复审时间：`2026-10-26T01:35–02:05`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI HEAD `c49a630`**（PR #6，Draft/OPEN，base R12 `d9e3ee9`）；R13 文档/证据仍**未提交**；审查使用临时 worktree，未修改仓库。
- 环境：macOS arm64；本地 Docker **不可用**（`docker info` exit 1，未启动）；Docker/TLS/registry/arm64 证据来自 GitHub Actions；本地 candidate negative probe 使用 PATH 上的 fake docker shim，仅验证脚本逻辑，不替代真实 CI。
- **最终判定：R13_PARTIAL（P0=0，P1=0，P2=0，P3=8）。**
  - 首轮两个 P2 已独立复核关闭：candidate identity 现在 `docker load` image.tar、比较实际 loaded image ID，并运行镜像内 `version` 校验 commit；tampered image-id + provenance image_id 被拒。`supply_chain.sh` 恢复 100755，直接执行不再 Permission denied。
  - R13 核心 UI/secret/deploy/contract/notifier 主体成立；Authorized Read-Only Live **NOT_RUN**，PR Draft/OPEN，R13 docs/evidence 待提交，full arm64/publish 等 P3 未完成，故不能 R13_PASS。`LIVE_READY` 不得宣告。

---

## 1. P2 关闭复核

### P2-1 — candidate identity 绑定 image ID 到实际镜像字节：已修复

- `tests/r13/candidate_identity.sh` 现在：
  - `test -x scripts/supply_chain.sh`；
  - `docker load -i image.tar`；
  - `docker image inspect --format {{.Id}} account-onboarding:candidate` 得到实际 content-addressed ID；
  - 与 `image-id.txt` 比较；不一致直接失败；
  - `docker run --rm account-onboarding:candidate version`，要求输出包含 `commit <source SHA>`；
  - 仍校验 image tar sha256/SBOM/provenance/base refs。
- 独立 fake-docker probe（/tmp，不改仓库）：
  - 合法 metadata（actual ID）→ **exit 0**，`PASS candidate identity ... loaded image id, binary commit ...`；
  - 同时篡改 `image-id.txt` 与 provenance `image_id`（tar 不变）→ **exit 1**，`FAIL loaded image ID ... != recorded ...`。
- CI release-image **40216029372** 的 `Verify candidate artifact identity` step success；candidate job 输出 `PASS candidate identity: ... loaded image id, binary commit ...` + tampered provenance rejection。
- **P2-1 CLOSED。**

### P2-2 — supply_chain.sh 恢复可执行：已修复

- `c49a630` tree：`scripts/supply_chain.sh` mode **100755**；`candidate_identity.sh` 内 `test -x` 断言。
- 独立直接执行：`scripts/supply_chain.sh` 无参数 → exit 1 usage（不再是 126 Permission denied）；带合法参数可直接生成 evidence。CI 仍用 `bash`，但本地/脚本 Usage 路径可用。
- **P2-2 CLOSED；P2 总数 = 0。**

### 附带

- deploy cleanup 现在对 root-owned `toonew` 先 `rm -rf`，失败回退 `sudo -n rm -rf`；新 r12-integration artifact 的 deploy log 不再出现 `rm: cannot remove ... Permission denied`，SUMMARY **16/16**。
- machine report 仍是 `cleanup_status: attempted`（P3，不是 P2）。

---

## 2. P0/P1 — 核心（clean）

- **UI/recovery**：R13 UI 测试覆盖 PREPARED/IN_FLIGHT 无 form、直接 POST 不改状态/version、CONFIRMED restricted form（无 NO_RESOURCE_CREATED）、MANUAL_COMPENSATION_COMPLETED 可关闭、audit 未知 404；existing lifecycle 不自动 retry。
- **Secret permissions**：`secureSecretFile` 拒绝 group/other-bit Admin/ReadOnly/Master/Cloudflare/OIDC secret；deploy bad-secret 644 负例；staging compose 成功。
- **Deploy matrix**：docker restart、bad secret perms、schema-too-new、machine report；`assert_expected_failure` 要求匹配日志，不再只凭非 200/非零。
- **Contract fixture**：Go normalizer 与 shell preflight 共用 `contracts/cloudflare/parsed_mails_envelopes.json` 的 10 个 valid envelope；明确不是 live Worker schema。
- **Notifier**：RecordingNotifier fake，无网络；测试 copy/error propagation。
- **结论：P0=0，P1=0。** 未发现 remote write 绕过、active UI resolve、secret 权限绕过或 unknown 二次写。

---

## 3. P3（8 项残余）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | R13 docs/evidence | R13 无 `docs/acceptance/R13.md`/evidence in branch；PR #6 Draft/OPEN；docs/evidence 尚未提交。最终 docs-only commit 后需确认仅 docs/evidence 且 CI 成功。 |
| P3-2 | Authorized Live | Authorized Read-Only Live **NOT_RUN**；contract fixture 仅 Go/shell mock parity，不是 live Worker schema；mode C/authz window 未真实执行。 |
| P3-3 | ARM64 | 只有 build + `version` smoke；未跑完整 arm64 staging/backup/UI/API 回归。 |
| P3-4 | Candidate/publish | candidate 与 dry-run 仍是独立构建；base digests 仅检查 `@sha256:` presence，未与 Dockerfile/registry 比对；Publish 未执行、无 GHCR/tag/release。 |
| P3-5 | Deploy | 新增 16/16 覆盖 restart/bad-secret/schema-new 等，但无 backup、session revocation、external-op recovery 完整矩阵；report `cleanup_status=attempted` 不等于 cleanup 验证；环境依赖 Docker 本地不可用。 |
| P3-6 | UI | CONFIRMED form/direct POST 已测，但无并发 admin、session、filter 自动化；detail 当前 reason 仍原样显示（HTML 转义但非 redact）。 |
| P3-7 | Notifier | 只有 fake/RecordingNotifier，无生产 notification wiring/channel；实际部署未发送告警。 |
| P3-8 | Contract/secret 兼容 | shared fixture 只证明 10 个 valid envelope 被 Go normalizer 和 shell preflight 接受，不证明 normalization 字段/error/status/live parity；secret 权限未覆盖 Docker secret 默认 0444/0440、ACL、symlink 等部署形态。 |

---

## 4. 真实 CI（head `c49a630`）

| Run/工作流 | 结果 | 关键证据 |
|---|---|---|
| ci **40216034597** | success | gofmt/build/vet/race/compose |
| r12-integration **40216029446** | success | deploy/cloudflare/arm64 jobs success；deploy artifact **16/16**；cleanup log 无 rm permission error |
| staging **40216029527** | success | staging + container |
| supply **40216029388** | success | supply PASS |
| r9-release **40216029432** | success | caddy + old-image-rollback |
| dry-run **40216029383** | success | OCI/sign/attest |
| release-image **40216029372** | success | candidate success；`Verify candidate artifact identity=success`；publish **skipped**；无 GHCR push |

所有 run head_sha=`c49a630`；publish skipped 不按 PASS 计。

---

## 5. 本地命令/退出码（临时 worktree `c49a630`）

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试 |
| `go test ./... -race -count=1` | **0** | 全绿 |
| `go test ./...` 计数 | **0** | **385 顶层（380 PASS + 5 SKIP）/735 含子测试/0 FAIL** |
| `go test ./internal/api -run 'TestReconciliationActionStates|TestAudit'` | **0** | UI states/audit 404 |
| `go test ./internal/provider/mailbox/cloudflare -run 'TestParsedMailsContractFixtures'` | **0** | 10 fixture cases |
| `go test ./internal/config -run 'TestLoadRejectsWorldReadableSecretFile'` | **0** | secret permissions |
| `go test ./internal/alerts -run 'TestRecordingNotifier'` | **0** | fake notifier |
| `go test ./tests/r11 -run 'TestCloudflareSharedContractFixtures'` | **0** | shell shared fixture |
| fake-docker candidate valid ID | **0** | `PASS candidate identity ... loaded image id` |
| fake-docker candidate tampered image-id+provenance | **1** | `FAIL loaded image ID ... != recorded ...` |
| 直接 `scripts/supply_chain.sh` | **1 usage** | 不再 Permission denied（100755） |
| `bash tests/r11/deploy_recovery.sh`（无 Docker） | **3** | NOT_RUN: Docker unavailable |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |

> 未触发/重跑 Actions、未 push/部署、未启动 Docker；未修改仓库代码/文档/证据（临时 worktree/fake shim 仅审查用）。

---

## 6. 最终判定与 docs-only commit

| 关闭标准 | 结果 | 说明 |
|---|---|---|
| P0 核心 UI/recovery/deploy | **0** | active form/direct POST、CONFIRMED restricted、audit 404、deploy matrix |
| P1 安全边界 | **0** | secret perms、token/CSRF、no automatic second write |
| P2 candidate/supply | **0** | docker load image ID/commit binding；supply 100755 + test |
| CI | **PASS** | 7 runs success/head c49a630；publish skipped；无 GHCR |
| Authorized Live | **NOT_RUN** | 不能当作 live 通过 |
| Docs/evidence | **待提交** | 无 R13.md/evidence；PR Draft/OPEN |

**最终：R13_PARTIAL（P0=0/P1=0/P2=0/P3=8）。** 不是 R13_PASS：Authorized Live NOT_RUN、full arm64/publish 未完成、R13 docs/evidence 未提交、PR 仍 Draft/OPEN；也不是 R13_BLOCKED：两个 P2 已关闭，核心运行时/安全加固成立。

### docs-only commit

- **可以作为最终 PR head**，前提：该 commit **仅 docs/**、evidence**（不改代码/workflow/scripts/Makefile/配置）；R13 报告/evidence 准确反映 `c49a630`（含 candidate `Verify candidate artifact identity`、CI run IDs、deploy 16/16、Authorized Live NOT_RUN）；该 docs-only head 触发的最终 CI 全部 success。
- 提交后需核对 diff 仅 docs/evidence；满足则无需再 review 代码。若夹带任何代码/workflow 变更，必须重新 review。即使 docs-only CI 绿，也不能宣告 `LIVE_READY`。

### 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `c49a630`；树 `561baf6c…`；`git ls-tree -r c49a630 \| sha256sum` = `1f41564a…` |
| 文档/证据树 | `520f973d…`；docs manifest `a76d9866…`；无 `docs/acceptance/R13.md` |
| 关键文件 | `candidate_identity.sh` `f2a544a1…`（100755）；`supply_chain.sh` `22146b31…`（100755）；`deploy_recovery.sh` `ea80071a…`（100755）；`config.go` `031b8654…`；`ops.go` `3acf6684…`；detail template `7a20e885…`；contract fixture `77abe5bd…`；notifier `b1a08d61…`；fixtures `9fa1bfd4…`；release workflow `c32af09c…` |
| 工作树 | 本审查文件 + 既有 REVIEW-R12 未跟踪；R13 代码未 checkout；R13 docs/evidence 未提交 |
