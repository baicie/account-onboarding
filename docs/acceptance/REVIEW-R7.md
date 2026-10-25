# 独立对抗性审查报告 — R7 容器运行时收口（REVIEW-R7）

- 审查时间：`2026-10-25T12:14–12:35`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；本轮只新增本文件，未改代码/配置/他人文档）
- 审查对象：HEAD `0ba9e00`（基线 `61af5a7`；容器证据对应代码 `79e9926`）
- 环境：macOS arm64；**本地 Docker daemon 仍不可用**（`docker info` exit 1；未安装/未启动 Docker）
- **结论：R7 = PASS（容器运行时收口）；P0 = 0，P1 = 0，P2 = 1，P3 = 10。**
  - 真实 Docker 验收证据来自 GitHub Actions run `40160487981`（commit `79e9926`）。我通过 GitHub API **独立拉取并核对了该 run**：`conclusion=success`、`head_sha=79e9926…`、`event=workflow_dispatch`、job/step 全部 success —— **不是仅凭提交的 JSON 自证**。
  - **`CONTAINER_RC_READY` 在本轮关闭标准下成立**（P0=0/P1=0；build/runtime/health/SQLite 持久化+迁移/SIGTERM/SIGKILL/单写锁/secrets/备份恢复 全部真实 PASS）；**`LIVE_READY` 仍不宣告**（生产 TLS、GHCR、live 平台均 NOT_RUN）。
  - 唯一 P2 是**证据记录缺陷**（`R7-image-inspect.json` 是 "No such image" 错误，见 §2），不影响运行时 PASS 的成立（镜像 ID/arch/user/size 已由验收脚本在清理前记录于验收日志），但**在对外宣告 `CONTAINER_RC_READY` 前应修正引用/工作流**。
- 冻结：HEAD `0ba9e0099999ff11ed0bb4f8dfcc5f5b3e0f44d4`；
  `git ls-tree -r HEAD | sha256sum` = `e999c08d2302b453aedef2a18d40cfa4d2f7a850d549612b5ae9858d6dc760bb`（详见 §7）。
  ⚠️ 审查开始时工作树干净；`docs/acceptance/R7.md`、`README.md`、`FINAL-REPORT.md`、`index.md` 的 R7 内容是 **Lead 尚未提交的 P5 草稿**（`M/??`），不是我的改动；本报告以 **HEAD 树**为代码/证据冻结，R7 报告引用的是审查时草稿（hash `1e7de130…`）。

> 边界：未 push/merge/deploy/release、未安装/启动 Docker、未调用真实平台；未触发/重跑 GitHub Actions。
> 唯一外部读取是 GitHub Actions API 的只读 run/job 元数据核对。

---

## 1. P0 / P1

**无 P0，无 P1。**

- P0-2 sweep 竞争修复语义正确（事务内重读 + 条件 UPDATE + `RowsAffected==1` + 单次终态事件；`CANCEL_REQUESTED` 不被覆盖；租约清理/fencing 保留）；5 个确定性测试**无 sleep**，`-race -count=10` 通过（§5.1）。
- P0-1 Docker 验收脚本的 Idempotency-Key 契约与 `--self-test`（11/11）真实、可离线复现（§5.2）。
- 容器运行时矩阵 63/63 真实 PASS（§4），未发现会令 `CONTAINER_RC_READY` 不成立的代码/运行时缺陷。

---

## 2. P2（1 项，必须在正式宣告前修正）

| 编号 | 文件:行 | 复现/事实 | 影响 | 建议修复 | 回归 |
|---|---|---|---|---|---|
| **P2-1** | `.github/workflows/container-acceptance.yml:51-58`；[`tests/r51/docker_acceptance.sh:56-71`](../../tests/r51/docker_acceptance.sh)（`:70` `docker image rm "$IMAGE"`）；证据 [`R7-image-inspect.json`](evidence/r7-container-ci/R7-image-inspect.json)；[`R7.md:28,53,64`](R7.md) | 验收脚本的 cleanup trap 在退出时**删除镜像**；workflow 的 "Record image identity" 步骤在其后执行 `docker image inspect "$IMG" > image-inspect.json 2>&1 \|\| true`，因此 inspect 必然失败。证据文件实际内容为 `[]` + `Error response from daemon: No such image: sha256:084ae09a…`。R7.md 却把该文件列为 "Image arch / user / size PASS / Exit 0" 的证据，§5 写 "manifest/Image inspect 已归档" | 被引用的 inspect 证据是错误输出，而非 inspect 结果；"image inspect PASS（exit 0）" 与事实不符。**不影响**运行时 PASS：镜像元数据（Image ID / linux/amd64 / user 10001:10001 / size 97417603 / commit 79e9926）已在清理前由脚本记录于 [`R7-container-acceptance.log:239`](evidence/r7-container-ci/R7-container-acceptance.log)，`R7-image.txt` 也记录了 Image ID。属**证据链/工作流**缺陷（会误导后续审计），非运行能力夸大 | 让验收脚本在 cleanup 前把 `docker image inspect` 写入证据目录（例如传入 `R51_EVIDENCE_DIR`）或提供 `--keep-image`；workflow 去掉 `\|\| true` 并断言 inspect JSON 的 `.Id/.Os/.Architecture/.Config.User` 非空；R7.md 改为引用验收日志第 239 行 + `R7-image.txt`，明确 Image ID 不是 registry manifest digest | CI step 应在 inspect 无效时失败；R7.md 的镜像元数据引用与日志逐字段一致 |

---

## 3. P3（记录，不阻塞）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | `R7.md:49` | "Cleanup/isolation PASS — Evidence: log"，但验收日志中并无 cleanup/删除资源的输出（cleanup trap 静默）。静态脚本确实只清理自身容器/project/volume/image，但该行应引用脚本（`tests/r51/docker_acceptance.sh:56-71`）或让脚本打印清理结果 |
| P3-2 | [`R7-compose-config.yaml`](evidence/r7-container-ci/R7-compose-config.yaml) | 与本地 `docker compose config` 的差异是 **runner 绝对路径 + compose v2.38.2 的 `bind.create_host_path`**（其余端口/secrets/read_only 一致）；快照非逐字节可复现（环境/版本相关），报告中应注明"runner 环境快照" |
| P3-3 | [`internal/ops/scan.go:86-90`](../../internal/ops/scan.go)、[`scan_test.go`](../../internal/ops/scan_test.go) | 新 `scanKebabIdent` 将所有 4+ 段 lowercase kebab 串从 `long_token` 启发式中屏蔽。我的独立探针：生成的容器名被正确忽略，但**裸 kebab 串** `abcdefgh-ijkl-mnop-qrst-uvwx-yz012345-6789abcd` 现在 **0 findings**（赋值/`Bearer`/PEM 仍被捕获）。建议把掩码收窄到生成标识符形状（如 `onboarding-r51-…`），并加"非生成器 kebab 样 secret 仍应被启发式覆盖"的负向测试 |
| P3-4 | [`internal/storage/runs.go`](../../internal/storage/runs.go) sweep 事件 | `appendEventTx(..., now)` 使用事务前的 `now`，而 `finished_at/updated_at` 用 `txNow`；事件时间可能略早于 finished_at。建议统一用 `txNow`（纯一致性，不影响状态机） |
| P3-5 | [`internal/storage/runs.go:390`](../../internal/storage/runs.go) | `sweepAfterSelectHook` 是生产文件中的测试专用全局变量（无 build tag、无同步）；当前测试串行使用、`-race` 通过，但未来并行测试可能竞争。可改为 build-tag 或注入接口 |
| P3-6 | [`internal/provider/mailbox/cloudflare/cloudflare.go:21`](../../internal/provider/mailbox/cloudflare/cloudflare.go) | 实现已 fail-closed（409 → CONFLICT，不重试），但错误映射注释仍写 "409 … (bounded recovery below)"，与 R7 "修正过时注释" 不完全一致（行为正确，文案残留） |
| P3-7 | [`tests/r51/docker_acceptance.sh:29-50`](../../tests/r51/docker_acceptance.sh) | `free_port` 只保证选出的 6 个端口互不相同，未在绑定前预留；与外部进程存在极小端口竞争窗口（CI 已通过） |
| P3-8 | [`tests/r51/docker_acceptance.sh:560-601`](../../tests/r51/docker_acceptance.sh) | Compose secret 严格断言（0400/10001 + UID 10002 拒绝）仅在 `id -u == 0` 或 passwordless sudo 时执行；否则 WARN + "non-authorized denial NOT_RUN"。CI runner 有 sudo（日志中 strict PASS），本地 Docker Desktop 可能只做"可读"检查 |
| P3-9 | [`R7.md:70`](R7.md) | "Sandbox E2E … 由 CI `make verify` 覆盖离线部分" 措辞混淆：sandbox E2E 本轮未本地重跑；离线 `make verify` 覆盖的是离线测试，容器内 sandbox run 是另一项（已 COMPLETED）。建议明确区分 |
| P3-10 | HEAD/工作树 | 容器镜像构建于 `79e9926`；HEAD `0ba9e00` 相对它仅新增 **evidence + `internal/ops` scanner**（`go list -deps ./cmd/onboardd` 不含 `internal/ops`，server 二进制不受影响）。R7.md 已注明证据对应 `79e9926`，但若要对 "HEAD `0ba9e00`" 做严格 provenance，建议在最终 HEAD 重跑一次 CI 或在报告中写明"server code 等价、image commit 为 79e9926" |

---

## 4. CI 证据真实性核验（独立）

我用 GitHub API 只读核对了 run（不是只读提交的 JSON）：

| 项 | 值 | 来源 |
|---|---|---|
| run id | `40160487981` | `docs/acceptance/evidence/r7-container-ci/R7-ci-run.json` + GitHub API |
| head_sha | `79e9926534239e6fc06e57ce8f8c97e24b28d1d9` | 同上（一致） |
| conclusion | **success**；`event=workflow_dispatch`；`run_attempt=1` | GitHub API `actions/runs/40160487981` |
| job | `docker`，conclusion **success**；steps 含 Offline gates、self-test、Docker environment、Docker runtime acceptance、Record image identity、Upload evidence | GitHub API `.../jobs`（与提交 JSON 一致） |
| 验收日志 | **63 PASS / 0 FAIL**（`grep -c '^PASS '` = 63），覆盖 build/health/hardening/v2 升级/幂等/撤销/SIGTERM/SIGKILL/单写锁/备份恢复/降级/fail-closed/compose | [`R7-container-acceptance.log`](evidence/r7-container-ci/R7-container-acceptance.log) |
| 镜像元数据 | `id=sha256:084ae09a…c44f platform=linux/amd64 user=10001:10001 size=97417603 commit=79e9926…`（清理前记录）、二进制内 commit 校验 PASS | 日志第 239-240 行 |
| Docker 环境 | Engine **28.0.4**、linux/amd64、Ubuntu 24.04、compose v2.38.2 | [`R7-docker-environment.txt`](evidence/r7-container-ci/R7-docker-environment.txt) |
| 自检 | **11/11 PASS**（本地复跑一致） | [`R7-self-test.log`](evidence/r7-container-ci/R7-self-test.log)、`bash tests/r51/docker_acceptance.sh --self-test` |
| 未 push | 仅本地 Image ID `sha256:084ae09a…`，**无 manifest digest / GHCR push** | R7.md 第 8 行；无 registry 步骤 |
| 过度宣称 | **无 `CONTAINER_RC_READY` 既定宣告**（R7.md §5 写"待独立审查确认后生效"）；无 `LIVE_READY` 声明 | R7.md:76-78 |

结论：**CI 运行是真实的且与提交证据一致**；唯一证据缺陷是 P2-1 的 image-inspect 记录（运行本身不影响）。

---

## 5. 代码与脚本审查

### 5.1 `SweepDeadlines` 竞争修复（`internal/storage/runs.go`）

- 事务内重新读取 `id`，若状态为 `COMPLETED/FAILED_TERMINAL/CANCELLED/**CANCEL_REQUESTED**` 或 `DeadlineMS > txNow` 则跳过 → 不会把取消覆盖为 `FAILED_TERMINAL`。
- `UPDATE ... WHERE id=? AND status NOT IN (...) AND deadline_ms <= ?` + `RowsAffected==1` 才 append `RUN_FAILED` → 并发 worker/sweep 不会重复终态事件；租约清理（`lease_owner/lease_until` 置空 + version+1）保留 fencing 语义。
- 5 个测试：cancel-after-select、worker terminal-after-select、二次 sweep 幂等、重启后 sweep、已取消跳过；**无 sleep**；`go test ./internal/storage -run TestSweep -race -count=10` → 0。
- 残余：事件时间用 `now` 而非 `txNow`（P3-4）、测试 hook 全局变量（P3-5）。

### 5.2 Docker 验收脚本（`tests/r51/docker_acceptance.sh`，610 行）

- `--self-test` 在 daemon 检查之前进入 mock 模式（本地无 Docker 可跑，11/11），验证真实 helper 的 400/202/409/200 契约；cleanup 在 self-test 模式只 kill mock。
- 动态端口（6 个）+ 每容器命名 `$PROJECT-*`；secret 用专用 volume + root init 容器 chown 10001/0400，断言 10001 可读、10002 被拒；v2 DB 用 root 容器 chown 10001/0600；备份/恢复、restored 实例、wrong-key fail-closed、degraded、fail-closed 配置、compose 严格 secrets 均有对应 PASS。
- cleanup trap 只删自己的容器/project/volume/image，无 `docker system prune`/全局 rm；与 R5.1 审计一致。
- 唯一实质缺陷：cleanup 先删镜像，导致 workflow 后续 inspect 失败（P2-1）。

### 5.3 其他代码

- **scanner masking**：见 P3-3（行为可复现，风险低；赋值/Bearer/PEM 仍检测）。
- **Cloudflare**：`CreateAddress` 对 duplicate 返回 `ClassConflict`，不重试/不创建第二个地址（fail-closed）；注释残留 P3-6。
- **workflow**：`workflow_dispatch` only、`contents: read`、`concurrency`、`shell: bash` + `set -euo pipefail`、self-test gate、artifact 上传；"Record image identity" 缺陷见 P2-1。
- 无新增依赖；`go.mod` 未变；镜像未 push。

---

## 6. 本地独立执行的命令与退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `git rev-parse HEAD`；`git status --short` | 0 | `0ba9e00…`；开始时干净（期间出现 Lead 的 P5 草稿：`M README/FINAL/index` + `?? R7.md`） |
| `make verify` | **0** | fmt + vet + 全量 race + build |
| `go test ./... -race -count=1` | **0** | 29 包全绿 |
| `go test ./internal/storage -run TestSweep -race -count=10` | **0** | 5 个 sweep 测试 ×10 无 flake |
| `bash tests/r51/docker_acceptance.sh --self-test` | **0** | **11/11**（无 Docker） |
| `docker info` | **1** | 本地 daemon 仍不可用（未启动） |
| `scripts/redact_scan.sh --file` × 7 份 R7 证据 | **0/0/0/0/0/0/0** | 0 findings；另做 kebab 掩码对抗探针（P3-3） |
| `docker compose -f deploy/compose.yaml config` | **0** | 静态校验；与 runner 快照仅路径/版本差异（P3-2） |
| GitHub API `actions/runs/40160487981` + `/jobs`（只读） | 200 | `success`、`head_sha=79e9926…`、steps 全 success |

> 未触发/重跑 GitHub Actions，未安装/启动 Docker，未 push/部署。

---

## 7. `CONTAINER_RC_READY` 判定与冻结

### 7.1 关闭标准逐项核对

| 标准 | 证据 | 结论 |
|---|---|---|
| 真实镜像构建 | CI log build #22；Image ID `sha256:084ae09a…`；注入 commit `79e9926` 且镜像内 binary 校验 PASS | **PASS** |
| 真实容器运行 + health/readiness | `/healthz 200`、`/readyz 200`、Docker HEALTHCHECK healthy、in-container healthcheck 0 | **PASS** |
| SQLite 持久化 + 迁移 | fresh schema 3；v2→v3 upgrade；WAL/SHM 0600、owner 10001、bind mount 持久化 | **PASS** |
| 生命周期/故障恢复 | SIGTERM exit 0 + restart healthy；SIGKILL + restart healthy；run 数据保留 | **PASS** |
| 单写锁 | 同卷第二容器 exited 且日志含 `single-writer` | **PASS** |
| Secrecy | secret volume 0400/10001 可读、UID 10002 拒绝；compose strict secrets PASS | **PASS** |
| 备份/恢复 | encrypted snapshot（0600）、verify、restore、restored 实例 `/readyz` + 原 run；wrong-key fail-closed 且 target untouched | **PASS** |
| 失败关闭 | 缺/坏 master key、不可写 data dir → 容器退出；degraded DB→`/readyz 503`+healthcheck 非 0 | **PASS** |
| 容器级独立审查 | 本报告：P0=0，P1=0；唯一 P2 是证据/workflow 记录缺陷（运行时 PASS 不依赖它） | **PASS（有条件）** |

**结论：`CONTAINER_RC_READY` 在本轮关闭标准下成立。** 建议 Lead 在最终文档中把 `R7.md`/`FINAL-REPORT`/`README`/`index` 的 "pending review" 更新为 claimed，同时修掉 P2-1 的证据引用/工作流步骤（不必重跑容器门禁；但若要做"HEAD `0ba9e00` 严格 provenance"，建议重跑一次 CI，见 P3-10）。`LIVE_READY` 仍不成立；生产 TLS/GHCR/live 平台仍 NOT_RUN，因此"可部署到公网/production"仍不成立（这不等同于 `CONTAINER_RC_READY`）。

### 7.2 冻结

| 项 | 值 |
|---|---|
| HEAD | `0ba9e0099999ff11ed0bb4f8dfcc5f5b3e0f44d4` |
| 基线 | `61af5a7` |
| HEAD 树清单 | `git ls-tree -r HEAD \| sha256sum` = `e999c08d2302b453aedef2a18d40cfa4d2f7a850d549612b5ae9858d6dc760bb` |
| 工作树 | 审查开始时干净；期间出现 Lead 的 P5 草稿（`M README.md`、`M FINAL-REPORT.md`、`M index.md`、`?? R7.md`；R7.md hash `1e7de130…`），非我的改动；本报告新增未跟踪 |
| `internal/storage/runs.go` | `92f4c86e…` |
| `internal/storage/sweep_race_test.go` | `98df5992…` |
| `internal/ops/scan.go` / `scan_test.go` | `494a89dd…` / `07d90ab1…` |
| `internal/provider/mailbox/cloudflare/cloudflare.go` | `0356f1d0…` |
| `tests/r51/docker_acceptance.sh` / `compose.override.yaml` | `3ae0e17e…` / `7f527dea…` |
| `.github/workflows/container-acceptance.yml` | `484f7437…` |
| 证据 | `acceptance.log 1c1aefd9…`、`image-inspect.json f2d741ba…`（错误输出，P2-1）、`image.txt e74b3cfb…`、`self-test 5b7cab8f…`、`docker-environment bb5f2373…`、`ci-run 23fec4e3…`、`compose-config e25de7b5…` |
| 报告草稿 | `R7.md 1e7de130…`；HEAD 的 `FINAL/README/index` 尚未包含 R7 行（P5 进行中） |
