# 独立审查报告 — R6.1 容器运行时收口尝试（REVIEW-R6.1）

- 审查时间：`2026-10-25T10:42–10:58`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；本轮只新增本文件，未改代码/配置/他人文档）
- 审查对象：HEAD `d8438f6`（R6.1 文档轮：`20d0cb0` + `d8438f6`；基线 `6258e57`）
- 环境：macOS arm64；**Docker daemon/VM 仍未运行**（本轮未安装/未启动 Docker，未改宿主机设置）
- **结论：R6.1 = PASS（阻断轮次按预期执行，`DOCKER_RUNTIME_UNAVAILABLE` 判定正确）；P0 = 0，P1 = 0，P2 = 0，P3 = 3。**
  - `DEPLOYMENT_BLOCKED` **成立且必须保持**：容器 build/run/安全/生命周期/故障恢复/单写锁/备份恢复全部 **NOT_RUN**；
  - **无镜像、无 Image ID、无 digest**（未构建、未 push）——报告写法（N/A）正确；
  - 报告**未**声明 `CONTAINER_RC_READY`/`LIVE_READY`，反而显式否定；无过度宣称；
  - 环境探测的每个退出码/事实均可独立复现（§2）。
- 冻结：HEAD `d8438f60fa8b58fcebd32c47499f608e2904f644`；
  `git ls-tree -r HEAD | sha256sum` = `0a3b0d6e4d9ff64f5588a5c31b32633fbaf1ccc37a495e7605e4a702cdafa65e`（详见 §6）。
  审查开始时工作树干净；本文件是唯一新增未跟踪文件。

> 边界：未 push/merge/deploy、未安装/启动 Docker Desktop、未改宿主机服务、未调用真实平台。
> 按本轮"提前停止、不重复离线测试"的要求，我只做了**一次**全仓库 `go test -v` 计数 sanity（见 §3），未重跑 `make verify`/race/E2E/TLS/Cloudflare live。

---

## 1. 事实核验（独立复现）

| 报告/证据声明 | 我的独立检查 | 结果 |
|---|---|---|
| HEAD `6258e57`（探测时）/ 最终 `d8438f6`；工作树干净 | `git rev-parse HEAD`；`git status --short` | ✓ `d8438f6`，干净 |
| Docker client 29.7.2，`docker version`/`docker info` 无 Server，exit 1 | 实际执行 `docker version`、`docker info`、`docker info --format '{{.ServerVersion}}'` | ✓ **1 / 1 / 1**，无 Server 段 |
| daemon socket 陈旧；`_ping` HTTP **000** | `curl --unix-socket ~/.docker/run/docker.sock …/_ping` | ✓ **000**；socket 为 Oct 21 23:33 的 0 字节 socket |
| `docker desktop status` exit 1 | 实际执行 | ✓ 1：`Could not retrieve status. Is Docker Desktop running?` |
| 仅本地 context；`DOCKER_HOST` 未设置 | `docker context ls` 的 context 仅 default/desktop-linux；`env` 检查 | ✓ 仅本地；`DOCKER_HOST=<unset>` |
| 无替代运行时（podman/nerdctl/colima/limactl/finch/rancher） | `command -v` 逐一检查 | ✓ 全部 absent |
| 未构建镜像、未创建容器/网络/Volume | 探测日志中无 build/run 命令；daemon 不可用无法执行；报告 §5 声明 | ✓ 与证据一致（daemon 下无法枚举镜像，但无任何 build/run 尝试） |
| `docker compose version` = v5.5.0（仅 client，不能 up） | 证据日志 | ✓ 与 client-only 状态一致 |
| 本轮无代码改动（仅文档） | `git diff --stat 7bd33a6..HEAD`（R6.1 之前的 R6 P4 代码修复不计入本轮）；`git show 20d0cb0 --stat` / `d8438f6 --stat` | ✓ R6.1 两提交仅 README/FINAL/index/R6.1.md/probe log；**R6.1 本身无代码变更** |

---

## 2. 是否有过度宣称 / 错误退出码

- `docs/acceptance/R6.1.md:9` 明确写"**没有宣告 `CONTAINER_RC_READY`**；`LIVE_READY` 不属于本轮范围"；§7 再次写 `DEPLOYMENT_BLOCKED`、`CONTAINER_RC_READY` 未满足、非 `LIVE_READY`。
- §2 表将容器 build/run/健康/非 root/迁移/WAL/持久化/撤销/SIGTERM/SIGKILL/单写锁/备份恢复/失败关闭全部标为 **NOT_RUN**；Image ID/digest 标 **N/A（未构建）**——与"无 Docker runtime"一致，**未把静态检查写成容器 PASS**。
- §6 的"下一步"只给操作员步骤：启动 Docker Desktop/daemon → `docker info` exit 0 → `make verify`/`compose config`/`dockerfile_audit`/`compose_matrix` → `docker_acceptance.sh`（0=PASS,1=FAIL,3=NOT_RUN）→ 记录 Engine/Image ID/平台；**未包含任何自动启动/安装/改宿主机配置的指令**，并明确"未获授权不得访问远程服务器"。操作安全。
- README `R6.1` 行、FINAL-REPORT §15、index 行均与报告一致：`DEPLOYMENT_BLOCKED`、container gates **NOT_RUN**、无 image/digest、无 `CONTAINER_RC_READY` 声明。
- 我抽查了报告引用的 R6 P4 修复仍有效：CI workflow 已含 `set -euo pipefail`（`.github/workflows/container-acceptance.yml:23`）；Cloudflare dry-run 已改为 `onboardd configcheck` + 负向用例（`tests/cloudflare/live_preflight.sh`）；`redact_scan` 对 `R6-P2-cloudflare-dryrun.log` 现在 **exit 0**（误报已修）；`build-info` 证据在干净树 `38b17df` 生成、`vcs.modified=false`。
- **未发现夸大或错误退出码。**

---

## 3. 我执行的命令与退出码（快速核验，未重复离线套件）

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `git rev-parse HEAD`；`git status --short` | 0 | `d8438f6…`；干净 |
| `docker info` / `docker version` / `docker info --format '{{.ServerVersion}}'` | **1 / 1 / 1** | 无 Server；socket 不可连 |
| `curl --unix-socket …/_ping` | 0（curl） | HTTP **000** |
| `docker desktop status` | **1** | `Is Docker Desktop running?` |
| `docker desktop start --help`（只读，未启动） | 0 | 命令存在；未执行 start |
| `docker context ls`；`echo $DOCKER_HOST` | 0 | 仅本地；unset |
| `command -v podman/nerdctl/colima/limactl/finch/rancher` | 全部 1（absent） | 无替代运行时 |
| `go test ./... -list '.*'`（计数 sanity） | 0 | **325 顶层** |
| `go test ./... -v -count=1`（**单次** sanity，非 race） | 0 | **320 PASS + 5 SKIP = 325**；**634 PASS 含子测试**；0 FAIL —— 与 R6.1 §7 一致 |
| `scripts/redact_scan.sh --file R6-P2-cloudflare-dryrun.log` | **0** | R6 P4 误报修复有效 |

> 按你的要求，未重跑 `make verify`/`go test -race`/E2E/TLS/Cloudflare live；R6/R6.1 报告中的历史结果未被我重新执行，本报告仅核验其证据自洽性与环境事实。

---

## 4. P3（不阻塞，建议顺手修）

| 编号 | 位置 | 内容 |
|---|---|---|
| **P3-1** | [`docs/acceptance/evidence/R6.1-docker-probe.log:10`](../../docs/acceptance/evidence/R6.1-docker-probe.log) vs [`R6.1.md`](../../docs/acceptance/R6.1.md) §1 表 | 探测日志记录 `status_lines=1`，而报告 §1 写"工作树 **干净（0 changes）**"。差异来自**当时正在生成的 R6.1 未跟踪文档/探测日志之一**（`REVIEW-R6.md` 已在 `6258e57` 入库，故不是代码脏变更；探测脚本会把自身输出计入 status）。不是运行时能力夸大，但属证据/报告不一致——建议 probe 排除自身输出文件后统计，或报告写"除本探测日志外干净" |
| **P3-2** | [`R6.1.md`](../../docs/acceptance/R6.1.md) §6 第 2 步 | 写"代码 HEAD 仍应为 `6258e57`"；但操作员执行 `git rev-parse HEAD` 时实际会看到 `d8438f6`（其后只有文档提交）。建议改为"代码等价于 `6258e57`；当前 HEAD 含 R6.1 文档提交" |
| **P3-3** | [`R6.1.md:6`](../../docs/acceptance/R6.1.md) | "本轮代码/文档提交：见 `git log --oneline -1`（本文件所在提交）"仍是占位式写法；实际 `R6.1.md` 由 `20d0cb0` 加入、最终 HEAD 为 `d8438f6`。建议写死提交哈希，便于审计/冻结 |

---

## 5. 判定与残余风险

| 判定 | 是否成立 | 依据 |
|---|---|---|
| **`DOCKER_RUNTIME_UNAVAILABLE`** | **成立** | client 存在，daemon/VM 不响应：`docker version/info/ServerVersion` exit 1、`_ping` 000、`desktop status` exit 1、无替代运行时、`DOCKER_HOST` 未设置、无授权远程环境 |
| **`DEPLOYMENT_BLOCKED`** | **成立且必须保持** | 无镜像、无 Image ID/digest、无任何容器 build/run/health/迁移/故障恢复/单写锁/备份恢复证据；TLS 生产 ingress 与 live 平台仍未验收 |
| **不是 `CONTAINER_RC_READY`** | **成立**（报告亦未声明） | 容器路径从未真实执行；静态/离线审计不能替代运行时证据 |
| **不是 `LIVE_READY`** | **成立**（报告亦未声明） | 无真实 Cloudflare/GitHub/WorkBuddy；本轮明确 out of scope |
| **R6.1 = PASS（阻断轮次）** | **成立** | 环境探测真实、退出码一致；提前停止符合任务要求；未假造容器证据；操作员解除阻断步骤安全、命令/退出码正确（除 P3-2 的 HEAD 措辞） |

**残余风险**：无运行时证据（容器内 UID/批量 WAL 权限/单写锁跨容器/容器重建/健康检查行为）；无镜像与 digest（不可审查供应链/签名/SBOM）；
生产 TLS ingress、Cloudflare/GitHub/WorkBuddy live 仍 NOT_RUN；`docker_acceptance.sh` 仍从未在真实 daemon 上执行；解除阻断依赖操作员启动 Docker Desktop（或提供已授权隔离 daemon）。

---

## 6. 冻结

| 项 | 值 |
|---|---|
| HEAD | `d8438f60fa8b58fcebd32c47499f608e2904f644`（含 `20d0cb0` R6.1 记录 + `d8438f6` FINAL 章节排序） |
| 基线 | `6258e57`（R6 最终基线） |
| HEAD 树清单 | `git ls-tree -r HEAD \| sha256sum` = `0a3b0d6e4d9ff64f5588a5c31b32633fbaf1ccc37a495e7605e4a702cdafa65e` |
| 工作树 | 审查开始时干净；本文件为唯一新增未跟踪文件 |
| `docs/acceptance/R6.1.md` | `57846164…` |
| `docs/acceptance/evidence/R6.1-docker-probe.log` | `bfe22d2b…` |
| `docs/acceptance/FINAL-REPORT.md` | `eb0e8998…` |
| `README.md` / `docs/acceptance/index.md` | `09d280db…` / `46c6287c…` |
| 我的证据 | 仅终端输出；未落盘额外证据（本轮只读核验，无容器/镜像操作） |
