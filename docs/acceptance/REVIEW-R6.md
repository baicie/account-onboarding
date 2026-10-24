# 独立对抗性审查报告 — R6 Autonomous Sprint（REVIEW-R6）

- 审查时间：`2026-10-25T01:35–02:05`（Asia/Shanghai）
- 审查者：独立 reviewer（**未参与实现**；本轮只新增本文件，未改代码/配置/他人文档）
- 审查对象：`cf13d50`（基线）→ `43f7f1a`（P0/P1）→ `55e6555`（P2）→ **`7bd33a6`（最终 HEAD，P3）**
- 环境：macOS arm64、Go 1.25.3（cgo `mattn/go-sqlite3`）；**Docker daemon 不可用**（`docker info`/`build`/`images`/`ps` 全部 exit 1）；
  沙箱 Keycloak+Mailpit 由我临时拉起跑完 E2E 后已停回。
- **结论：R6 = PASS（范围限定：离线审计 + 代码修复 + 测试 + 诚实 NOT_RUN）；P0 = 0，P1 = 0，P2 = 4，P3 = 11。**
  - **`DEPLOYMENT_BLOCKED` 仍然正确**：容器 build/run、镜像 digest、容器级故障注入、生产 TLS ingress、live 平台全部 **NOT_RUN**。
  - **没有任何 P1 阻断 `LOCAL_RC_READY`**：DB/WAL/SHM 0600 硬化、busy-wait、goroutine、并发、备份恢复撤销保留均有真实可复现证据；未发现默认配置可触发的安全缺陷或数据丢失路径。
  - **`CONTAINER_RC_READY` 未被声明（正确）**；**`LIVE_READY` 未被声明（正确）**。
  - 4 项 P2 都是"门禁/证据/审计轨迹"层：2 项会产生**假 PASS**（CI pipeline、Cloudflare dry-run 的假配置校验）、2 项是**证据过期**（构建 provenance、进度日志）。不改变代码正确性，但修好前不能把这些门禁/证据当作可信依据。
- 冻结：HEAD `7bd33a61e2c9120bf358173c33605e7326b8b338`；
  `git ls-tree -r HEAD | sha256sum` = `6cb42d1af818ffef15baedcb340d78a747ab3f2895c377266ec7344970e88e8c`（详见 §11）。
  ⚠️ 审查开始时工作树干净；审查期间 **Lead 的 P4 并行编辑**使工作树变脏（`M README.md`、`M docs/acceptance/FINAL-REPORT.md`、
  `M docs/acceptance/AUTONOMOUS-SPRINT-20261025.md`、`M docs/acceptance/index.md`、`M deploy/README.md`、
  `?? docs/acceptance/evidence/R6-P1-e2e.log`）——**不是我的改动**；
  本报告以 **HEAD 树**为准，§10 会区分"已提交版 / 工作树（P4 进行中）"。

> 边界：未 push/merge/release/deploy、未安装/启动 Docker、未修改宿主机服务、未调用真实 Cloudflare/GitHub/WorkBuddy；
> 无镜像/容器/卷创建。审查结束无遗留监听进程。

---

## 1. P0 / P1

**无 P0，无 P1。**

- P1 声称的 4 个新测试全部真实存在、可重复（各跑 3 次全 PASS）、不是 trivial 断言：
  `TestDatabaseFilesAreOwnerOnly`、`TestSQLiteBusyTimeoutWaitsForLock`、`TestEngineStartStopDoesNotLeakGoroutines`、`TestConcurrentHTTPReadsAndWrites`。
- 我另外用独立探针验证了 DB 硬化生命周期（§7.1）：fresh → 0600；重开 → 0600；**既有 0644 DB → 0600**；
  WAL/SHM 缺失时跳过、存在时 0600；并用裸 `sqlite3` 驱动确认 **SQLite 以 DB 文件模式创建 WAL（0600 会被继承）**，
  因此"仅在 Open 时 chmod"不留下 WAL 后建为 0644 的缺口。
- 版本注入（`-ldflags -X`）、busy 等待、goroutine 生命周期、HTTP 并发均实测工作；`make verify`/全量 race 绿。

---

## 2. P2（非阻塞，但修复前不要依赖对应门禁/证据）

| 编号 | 级别 | 文件:行 | 复现 | 影响 | 建议修复 | 回归 |
|---|---|---|---|---|---|---|
| **P2-1** | P2（CI 假 PASS） | [`.github/workflows/container-acceptance.yml:25`](../../.github/workflows/container-acceptance.yml) | 步骤用 `bash tests/r51/docker_acceptance.sh \| tee container-acceptance.log`；GitHub `run:` 默认 shell 是 `bash -e`（**无 pipefail**），管道退出码取 `tee`。实测等价：`bash -e -c 'bash -c "exit 1" \| tee /dev/null'` → **exit 0** | 未来在 runner 上执行时，**容器门禁 FAIL 也会显示绿色**（假 PASS）；workflow 本轮未执行，故当前无 release 影响 | 给该 step 加 `shell: bash`（GitHub 显式 bash 使用 `-eo pipefail`）或在脚本里 `set -o pipefail`；再加 `grep -q 'SUMMARY .*fail=0'` 断言 | 本地用 `bash -eo pipefail -c '… \| tee'` 断言失败会传播 |
| **P2-2** | P2（假配置校验） | [`tests/cloudflare/live_preflight.sh:50-57`](../../tests/cloudflare/live_preflight.sh)、[`cmd/onboardd/main.go:39`](../../cmd/onboardd/main.go) | dry-run 的"config validation with fake in-memory files"实际执行 `./bin/onboardd version`；`version` 在 `main()` 里**直接 return，从不调用 `config.Load()`**。实测：`ONBOARD_MODE=bogus-mode ONBOARD_CLOUDFLARE_ENABLED=true ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE=/nonexistent ./bin/onboardd version` → **exit 0**，dry-run 仍打印 `PASS config accepts documented Cloudflare file-based settings` | dry-run 的"配置契约"结论是**空断言**；可能掩盖 Cloudflare file-based secret 配置错误（真实 live 前无法发现） | 用真正加载配置的入口做校验（如 `onboardd migrate --dry-run`/新增 `config-check` 子命令；或启动一次 sandbox 服务并断言 `/healthz` 200 + 配置里 cloudflare enabled=false/凭据路径可读） | 负向测试：坏 `ONBOARD_MODE`/缺失凭据文件 → dry-run 必须 FAIL |
| **P2-3** | P2（供应证据过期） | [`docs/acceptance/evidence/R6-P3-build-info.json:3`](../../docs/acceptance/evidence/R6-P3-build-info.json)、`R6-P3-deployment-audit.log` | 已提交证据写 `"commit": "cf13d50"`，`build_info` 含 `vcs.modified=true`、`mod …+dirty`；而最终 HEAD 是 `7bd33a6`。我在 HEAD 复跑 `scripts/release_info.sh`：**reproducible=true、commit=7bd33a6、sha256=6ce3a516…**（连续两次运行一致） | "可复现构建 / provenance" 的证据指向**基线提交 + 脏树**，与最终交付 HEAD 不一致；FINAL/报告引用时不可复核 | 在最终 HEAD（干净树）重跑 `scripts/release_info.sh` 并提交证据；JSON 增加 `dirty:false`/`git_status` 字段；`R6-P3-deployment-audit.log` 同步重生成 | 证据中 `commit == git rev-parse --short HEAD` 且 `dirty=false` |
| **P2-4** | P2（审计轨迹过期） | [`docs/acceptance/AUTONOMOUS-SPRINT-20261025.md:6,27-29,40-42`](../../docs/acceptance/AUTONOMOUS-SPRINT-20261025.md)（HEAD 版） | 已提交的进度日志仍写"当前 HEAD `cf13d50`"、P2/P3 **PENDING**、命令表止于 01:30（无 Cloudflare/release_info/version/E2E 行）；而 `55e6555`/`7bd33a6` 已提交 P2/P3 代码与证据。已提交的 `FINAL-REPORT.md`/`README.md` **没有 R6 行**（`grep -c R6` = 0） | 冲刺的"进度/交付"审计轨迹与仓库实际状态矛盾；P4 未提交的编辑虽已修正状态，但仍有一条命令写 `commit cf13d50`（应为 `7bd33a6`） | 提交 P4 时一并更新：日志写 `当前 HEAD 7bd33a6` + P2/P3 命令与证据行；FINAL-REPORT/README 写 R6 行；命令行改为实际输出 `onboardd 0.1.0 commit 7bd33a6` | 日志/报告中的 HEAD/命令/退出码与 `git log -1` 及复跑一致 |

---

## 3. P3（记录，不阻塞）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | [`internal/storage/store_test.go:513`](../../internal/storage/store_test.go) | `TestDatabaseFilesAreOwnerOnly` 对 WAL/SHM 缺失是 `continue`（不是失败）；且断言 `perm&077==0` 允许 0400（注释却写"want 0600"）。建议要求 **WAL 必存在** + 精确 0600 |
| P3-2 | [`internal/api/api_test.go:855`](../../internal/api/api_test.go)（helper [`api_test.go:122-148`](../../internal/api/api_test.go)） | 并发测试在 goroutine 里调用 `e.request(t, …)`，其内部用 `t.Fatal/t.Fatalf`；Go 规定 `FailNow` 只能在测试 goroutine 调用。失败可能不被正确上报/触发 panic。建议返回 `(status, err)` 并在 `wg.Wait()` 后断言 |
| P3-3 | [`internal/workflow/engine_test.go:511`](../../internal/workflow/engine_test.go) | goroutine 泄漏测试容忍 `baseline+4`、25ms sleep；5 轮各泄漏 1 个会被抓到，但 ≤4 个的小泄漏会漏网；`newHarness` 的 `t.Cleanup` 与手动 `store.Close()` 重复关闭（无害但噪声）。建议更严格 settle/容差 |
| P3-4 | [`scripts/release_info.sh:9,12,50`](../../scripts/release_info.sh) | 默认把证据写进**已跟踪**的 `docs/acceptance/evidence/`（运行即脏树/覆盖历史证据，可能正是 P2-3 的成因）；`COMMIT` 不标记 dirty；`cgo`/`trimpath` 硬编码 true。建议默认写临时/CI artifact + `git status --porcelain` 记录 |
| P3-5 | `docs/acceptance/evidence/R6-P3-go-modules.json`、`R6-P3-build-info.json` | 已提交证据含本机绝对路径（`/Users/liuzhiwei/go/pkg/mod/...`、`/var/folders/...`）。非秘密，但属可移植性/隐私噪声，建议脱敏 |
| P3-6 | [`.dockerignore:23`](../../.dockerignore) | `*.md` 排除后 `!docs/**` 又把 **docs 全目录（含 acceptance evidence）** 拉回 build context；仅进 builder stage、不进最终镜像，但会放大上下文并携带本地路径。建议排除 `docs/`，并补 `*.pem`/`*.key`/`*.crt`/`*.enc` 防御 |
| P3-7 | `Dockerfile:14-15`、`deploy/compose.yaml`（无 `build.args`） | `ARG COMMIT=unknown` 且 compose 构建未传 build args → `docker compose build` 产物的 `onboardd version` 会显示 `commit unknown`。建议 compose 增加 `build.args: {VERSION, COMMIT}` 或在 supply-chain 文档给出 `docker build --build-arg …` |
| P3-8 | R6 P2 dry-run 证据（见 §2 P2-2 同文件） | 该证据被仓库自己的 `scripts/redact_scan.sh` 判为 `long_token`（**误报**：go test 包路径），`--file` **exit 2**；而 `security-gate.md` 要求证据扫描 exit 0。R5.1 的 docker probe 日志也有同类历史误报。建议扫描器对 go test 的 ok 行/包路径加豁免，或证据生成时过滤 |
| P3-9 | [`tests/cloudflare/live_preflight.sh:74-100`](../../tests/cloudflare/live_preflight.sh) | live 模式把 admin/site 密码作为 `curl -H` argv（同用户 `ps` 可见）；dry-run 依赖预构建 `bin/onboardd` 但无 `-x`/build 检查；未知额外参数被忽略（`--live --foo` 仍 live）。建议 `curl -K`/临时 config 传 header、启动前检查二进制、拒绝未知参数 |
| P3-10 | [`tests/r51/dockerfile_audit.sh`](../../tests/r51/dockerfile_audit.sh) | 审计覆盖 22 项但未检查新增的 `.dockerignore` `.env`/`deploy/.env*` 行，也未检查 `docs/`/证书类文件是否进上下文；"Dockerfile does not copy secrets/data" 仅 grep Dockerfile 文本（依赖 .dockerignore 的部分未被该断言覆盖） |
| P3-11 | `docs/deployment/*.md`（设计文档） | Caddy/nginx 示例语法未验证（工具未安装，audit 已标 NOT_RUN）；`security-gate.md` 的"SBOM 输入"与 syft/trivy NOT_RUN 并存，发布说明需明确"SBOM NOT_RUN"，避免读者误以为已生成 SBOM |

---

## 4. 独立执行的命令与真实退出码（HEAD `7bd33a6`）

| # | 命令 | 退出码 | 结果 |
|---|---|---:|---|
| 1 | `git rev-parse HEAD`；`git status --porcelain` | 0 | `7bd33a6…`；开始时干净，审查期间出现 Lead P4 未提交编辑（见 §10） |
| 2 | `make verify` | **0** | fmt + vet + 全量 `-race` + build |
| 3 | `go test ./... -race -count=1` | **0** | 29 包全绿 |
| 4 | 计数 `go test ./... -list` / `-v` | 0 | **324 顶层（319 PASS + 5 SKIP）/ 633 PASS 含子测试（638 RUN）/ 0 FAIL** |
| 5 | `go test ./internal/storage -run 'TestDatabaseFilesAreOwnerOnly\|TestSQLiteBusyTimeoutWaitsForLock'`（-count=3） | **0** | 3 次均 PASS（~0.84s） |
| 6 | `go test ./internal/workflow -run TestEngineStartStopDoesNotLeakGoroutines`（-count=3） | **0** | 3 次均 PASS（20.8s） |
| 7 | `go test ./internal/api -run TestConcurrentHTTPReadsAndWrites`（-count=3） | **0** | 3 次均 PASS |
| 8 | `bash tests/r6/restore_revocation.sh` | **0** | **11/11**（备份→恢复→重启，run + 已撤销会话保留） |
| 9 | `bash tests/r51/dockerfile_audit.sh` | **0** | **22 PASS / 0 FAIL / 2 WARN**（digest pin、OCI labels 已记录为 P2） |
| 10 | `bash tests/r51/compose_matrix.sh` | **0** | 6/6（含 default/override 各 15 项断言、缺 env 必败、preflight 正/负例） |
| 11 | `bash tests/r51/isolation_audit.sh` | **0** | 无全局破坏命令；override config exit 0；daemon not available（informational） |
| 12 | `bash tests/cloudflare/live_preflight.sh`（dry-run） | **0** | 离线契约测试 + refusal 说明；**但配置断言为空（P2-2）** |
| 13 | `bash tests/cloudflare/live_preflight.sh --live`（无授权） | **2** | `REFUSED: live mode requires ONBOARD_CLOUDFLARE_LIVE_AUTHORIZED=…` |
| 14 | `scripts/release_info.sh .sandbox/review/r6-release`（HEAD） | **0** | reproducible=true，commit=7bd33a6，sha256=6ce3a516…（第二次运行同 sha） |
| 15 | `make build && ./bin/onboardd version` | **0** | `onboardd 0.1.0 commit 7bd33a6`；`make build VERSION=9.9.9 COMMIT=deadbeef` → `onboardd 9.9.9 commit deadbeef` |
| 16 | `docker info` / `build` / `images` / `ps` | **1/1/1/1** | daemon 不可用；无镜像/digest 可产生 |
| 17 | `docker compose -f deploy/compose.yaml config`（及 override） | **0/0** | 静态清单通过 |
| 18 | 沙箱 E2E：`ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1` | **0** | **4/4 PASS**（12.165s；Keycloak+Mailpit 真夹具） |
| 19 | 我的 DB 文件生命周期探针（fresh/重开/legacy 0644/WAL 缺失） | 0 | DB/WAL/SHM 均 **0600**；legacy 0644 → 0600；裸驱动确认 SQLite 以 DB 模式创建 WAL（0600） |
| 20 | 我的 CI pipefail / Cloudflare 假校验复现 | — | pipeline 失败被 `tee` 掩盖（exit 0）；invalid config 下 `version` exit 0（P2-1/P2-2） |
| 21 | 证据秘密扫描（逐文件 `scripts/redact_scan.sh --file`） | 多数 0；2 个 2 | `R6-P2-cloudflare-dryrun.log` 与历史 `R5.1-docker-probe.log` 为 `long_token` **误报**（P3-8）；其余 R6 证据 0 findings |

---

## 5. Docker 诚实性核验

- `docker info`/`version`/`images`/`ps`/`build` 全部 **exit 1**（我复跑）；daemon socket 不响应；无 image ID/digest/container 可产生。
- 提交的报告/日志**没有**声明 `CONTAINER_RC_READY` 或"容器已运行"：`R6` 章节、README/FINAL 的计划更新都写 `NOT_RUN`；`tests/r51/docker_acceptance.sh` 在本轮从未被执行（仅静态/`bash -n` 审计）。
- 未安装/启动 Docker、未改宿主机服务；`R6-P0B-script-audit.log` 也记录 `daemon: not available`。
- `.github/workflows/container-acceptance.yml` 是 `workflow_dispatch` 手动 workflow，**未执行、未 push**（工作区没有 Actions 运行证据）。

---

## 6. P0-B 离线容器审计核验

| 项 | 我的复核 |
|---|---|
| `tests/r51/dockerfile_audit.sh` | 22 PASS/2 WARN；逐项对照 Dockerfile 均成立（多阶段、非 root 10001、`--start-period=10s`、`onboardd healthcheck`、ENTRYPOINT/EXPOSE/VOLUME、`-trimpath`、CGO、无 privileged）；2 WARN（base tag 未 digest pin、无 OCI labels）已在供应文档中标注为 P2-6，**诚实** |
| `.dockerignore` 修复 | 已含 `/deploy/data/`、`/deploy/secrets/`、`/deploy/.env`、`/deploy/.env.*`；审计的 7 个 exclusion 均 PASS；最终镜像只 `COPY --from=build /out/onboardd` → secret/data 不进最终镜像（P3-6 只是 build context 里的 docs 问题） |
| `tests/r51/compose_matrix.sh` | 6/6；逐项断言 read_only、user、cap_drop、no-new-privileges、restart、healthcheck、secret 文件、回环端口、无通配发布；缺 `R51_*` 必败；preflight 正/负例；**未发现误报** |
| `tests/r51/isolation_audit.sh` | 无 `docker system prune`/全局 rm/volume/network 清理；仅本 project/image/容器；daemon 缺失 exit 0（informational，设计正确） |
| 脚本审计 | 24 个 `bash -n` 全 0；shellcheck NOT_RUN（未安装，诚实）；2 个脚本缺 strict mode 已 WARN |
| CI workflow | YAML 可解析（`workflow_dispatch`，permissions contents:read）；**但 pipeline 无 pipefail（P2-1）**，且未执行 |

---

## 7. P1 代码修复核验

### 7.1 DB/WAL/SHM 0600（`store.go:154-175`）

- 实现在 `Open` 末尾（`Migrate` 之后）对所有三方文件 `os.Chmod(0600)`；不存在则跳过；其他错误包装返回（fail closed）。
- 我的探针（非测试代码）结果：
  - fresh DB + 写：`db=0600 wal=0600 shm=0600`；
  - `Close` 后删除 WAL/SHM 再 `Open`：重新出现即 **0600**；
  - 既有 **0644** DB：`Open` 后 **0600**；
  - 用裸 `mattn/go-sqlite3` 验证：SQLite 创建 WAL 时**复制 DB 文件模式**（DB 0600 → WAL 0600），所以"只在 Open chmod"不会留下 WAL 后建为 0644 的窗口。
- 错误路径：`sql.Open`/`Ping`/`Migrate` 失败均 `releaseLock()` + `Close`（我的代码走查 + 既有测试覆盖）；`Close` 释放锁后再次关闭无害。
- 残余（P3）：`os.Chmod` 跟随符号链接；NFS/只读 FS 上可能使 `Open` 失败（安全 fail-closed，但需文档说明）。

### 7.2 其余 P1 证据

- `TestSQLiteBusyTimeoutWaitsForLock`：外部连接 `BEGIN IMMEDIATE` 持锁 150ms，store 写入等待后成功（实测 0.17s）；3 次重复不 flaky。
- `TestEngineStartStopDoesNotLeakGoroutines`：5 轮 Start/Stop 回到基线（+4 容差）；3 次重复 PASS。
- `TestConcurrentHTTPReadsAndWrites`：24 并发读全 200、10 并发创建全 202、持久化 ≥10；3 次重复 PASS（但失败上报方式有 P3-2）。
- `tests/r6/restore_revocation.sh`：**11/11**，备份→verify→restore→在恢复库上重启→run COMPLETED + `/readyz` true + 已撤销 Cookie 仍 303；有源库 revocation 行数前置断言，不是"重启导致 303"的假阳性。
- 沙箱 E2E：**4/4 PASS**（12.165s，真实 Keycloak/Mailpit），说明 DB 硬化/锁/健康检查未回归崩溃重启。

---

## 8. P2 Cloudflare 准备核验

- dry-run：**无网络**（仅 `go test` 离线契约 + 本地 `version`）；打印 precondition 时只输出路径/unset，不输出秘密；live 计划文本清晰。
- `--live` 无 `ONBOARD_CLOUDFLARE_LIVE_AUTHORIZED=YES_I_HAVE_WRITTEN_AUTHORIZATION` → **exit 2 REFUSED**（实测）；base URL 仅允许 `https://api.moonbyte.top`（精确匹配）；domain 固定 `moonbyte.top`；test name 正则 `^[a-z0-9-]{8,40}$`；admin 文件必须可读；JWT 写 0600 临时文件且不打印；**脚本中无任何 DELETE**（grep 仅注释/提示中的 "never deletes"），创建单个显式测试地址。
- 缺陷：dry-run 的"配置校验"是空断言（P2-2）；live 模式凭据走 curl argv（P3-9）；未知额外参数被忽略（P3-9）。
- **未向 `api.moonbyte.top` 发送任何请求**（本轮无 live 运行；dry-run 无网络调用）——与证据一致。

---

## 9. P3 版本/部署/供应链核验

- 版本注入：`Makefile:36-41` 用 `-X …Version=… -X …Commit=…`；`make build` → `onboardd 0.1.0 commit 7bd33a6`；`VERSION`/`COMMIT` 覆盖生效；Dockerfile `ARG VERSION/COMMIT` + 同样 `-X`；`cmd/onboardd/main.go` 的 `version` 输出 commit；`internal/api/server.go` 由 `const` 改 `var`（供 `-X`），`/v1/meta` 仍只暴露 version（commit 仅 CLI，文档措辞可更精确）。
- `scripts/release_info.sh`：HEAD 上可复现（两次独立运行同 sha 6ce3a516…）；但默认写已跟踪证据、无 dirty 标记（P3-4），且已提交证据是基线+脏树（P2-3）。
- `docs/deployment/**`：均为 design/runbook，明确写"no production deployment performed"；rollback 文档正确记录 R5 前回滚丢失持久撤销、旧二进制不加锁、JSON keyring 版本兼容；first-deploy 清单正确要求 secret 对 UID 10001 可读；TLS 文档明确测试 ingress 非生产、HSTS 需确认后再开。**未发现设计文档冒充已执行证据**。
- `deploy/examples/{Caddyfile,nginx.conf}`：占位域名/证书路径、Reference ONLY；语法未验证（caddy/nginx NOT_INSTALLED，audit 已记录 NOT_RUN）。

---

## 10. 文档/证据准确性

| 项 | 结果 |
|---|---|
| 计数 | 实测 **324 顶层（319 PASS + 5 SKIP）/ 633 含子测试 / 0 FAIL**；已提交 `FINAL-REPORT.md` 仍是 R5.1-B 的 320/315/629（**P4 未提交**）；Lead 的未提交 P4 编辑尚未包含新计数 → 提交时应更新 |
| 冲刺日志 | HEAD 版写 `当前 HEAD cf13d50`、P2/P3 PENDING（**P2-4**）；未提交 P4 编辑修正了状态但命令行仍写 `commit cf13d50`（应为 7bd33a6） |
| FINAL/README | HEAD 版无 R6 行（`grep -c R6` = 0，P4 未提交）；未提交 P4 编辑新增了 R6 `PARTIAL / DEPLOYMENT_BLOCKED` 行与 §14——**若提交，需一并修正计数和命令行** |
| 证据秘密 | 9 份已跟踪 R6 证据中 8 份 `redact_scan` 0 findings；`R6-P2-cloudflare-dryrun.log` 被误报 `long_token`（P3-8）；历史 `R5.1-docker-probe.log` 同类误报 |
| 绝对路径 | `R6-P3-build-info.json`（临时目录）与 `R6-P3-go-modules.json`（`/Users/...`）含本机路径（P3-5） |
| 诚实性 | 报告/日志未把静态审计写成容器 PASS；未声明 `CONTAINER_RC_READY`/`LIVE_READY`；Cloudflare live REFUSED exit 2；部署文档 design-only；`govulncheck/syft/trivy/shellcheck` 全部如实标注 NOT_INSTALLED/NOT_RUN |

---

## 11. 冻结（HEAD 树）

| 项 | 值 |
|---|---|
| HEAD | `7bd33a61e2c9120bf358173c33605e7326b8b338`（base `cf13d50`；含 `43f7f1a`、`55e6555`、`7bd33a6`） |
| HEAD 树清单 | `git ls-tree -r HEAD \| sha256sum` = `6cb42d1af818ffef15baedcb340d78a747ab3f2895c377266ec7344970e88e8c` |
| 工作树 | 审查开始时干净；审查期间出现 Lead P4 未提交编辑（README、FINAL-REPORT、AUTONOMOUS-SPRINT、index、deploy/README）+ 未跟踪的 R6-P1-e2e.log + 本报告。**我的冻结以 HEAD 树为准** |
| 关键 blob（HEAD） | `store.go 14328503`、`store_test.go 5abf89e6`、`busy_test.go d0f11f3c`、`engine_test.go b2a6ad55`、`api_test.go 7087b113`、`server.go 011ab54c`、`main.go f0516fb4`、`Makefile af8fc261`、`Dockerfile c10001c1`、`.dockerignore deb1ee92`、`dockerfile_audit.sh 571eaf0b`、`compose_matrix.sh 367e2d43`、`isolation_audit.sh a58875f8`、`restore_revocation.sh c3ee69bc`、`live_preflight.sh abfe4a00`、`release_info.sh 58c0cf81`、`container-acceptance.yml a400e53a`、`AUTONOMOUS-SPRINT dce1a75b`、`R6-P3-build-info.json 37782f3f`、`R6-P3-go-modules.json f5672417` |
| 我的证据 | `.sandbox/review/logs/r6-gates.log`、`r6-e2e.log`、`r6-sandbox-up.log`、`.sandbox/review/r6-wal-probe.log`、`.sandbox/review/r6-release*/`、`.sandbox/review/r6-walmode/` |

---

## 12. 最终判定

| 判定 | 是否成立 | 依据 |
|---|---|---|
| **R6 sprint = PASS（范围限定）** | **成立** | P0-B 静态审计真实；P1 代码修复 + 4 个新测试 + restore/revocation + E2E 全部可复现；P2 Cloudflare live 正确拒绝；P3 版本注入/可复现构建/部署 runbook 真实；无 P0/P1；Docker 全部诚实 NOT_RUN。**但**：4 项 P2（CI 假 PASS、Cloudflare 假配置校验、provenance 证据过期、进度日志过期）修复前，不能把这些门禁/证据当作发布依据 |
| **`DEPLOYMENT_BLOCKED`** | **成立，且必须保持** | 无镜像、无 digest、无任何容器 build/run/故障注入；生产 TLS ingress 与 live 平台未验收；`docker compose config` 只是静态。R6 没有改变这一结论 |
| **`CONTAINER_RC_READY`** | **不可宣告（未宣告，正确）** | 容器路径从未真实执行；CI workflow 未运行；离线审计不能替代运行时证据 |
| **`LIVE_READY`** | **不可宣告（未宣告，正确）** | 无真实 Cloudflare/GitHub/WorkBuddy 证据；Cloudflare live 仅 REFUSED 门禁 |
| **是否有 P1 阻断 `LOCAL_RC_READY`** | **没有** | DB 硬化、busy/goroutine/并发、备份恢复撤销、版本注入均实测正确；P2/P3 不改变本地 RC 能力 |

**结论：R6 可按 PASS（离线范围）交付；`DEPLOYMENT_BLOCKED` 判定正确且不可放宽。提交 P4 前请先修 P2-1/P2-2（假 PASS 门禁）并在干净 HEAD 重生成 P2-3/P2-4 的证据与日志；这些不阻塞 `LOCAL_RC_READY`，但阻塞"把这些门禁/证据当作发布凭据"。**
