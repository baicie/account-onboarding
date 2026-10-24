# 独立对抗性审查报告 — R5.1-B Docker 运行时收口（REVIEW-R5.1-B）

- 审查时间：`2026-10-25T01:06–01:38`（Asia/Shanghai）
- 审查者：独立 reviewer（**未参与实现**；本轮只新增本文件，未改代码/配置/他人文档）
- 审查对象：`0d78e8d`（R5.1 交付树）→ `3f601d1`（R5.1-B 审计/修复/脚本）→ **`24042e7`（最终 HEAD：raw-key 优先级 + secret 可读性加固）**
  - 说明：Lead 消息写的是 "`66d0a2e`-series"，但 `git log --oneline -1` 实际为 **`24042e7`**；本报告以实际 HEAD 冻结。
- 环境：macOS arm64、Go 1.25.3（cgo `mattn/go-sqlite3`）；**Docker daemon 不可用**（`docker info`/`version`/`images`/`ps`/`build` 全部 exit 1）；
  Docker Desktop backend 进程（PID 5515）启动时间 **Wed Oct 21 23:33:21 2026**，早于本轮，佐证"未启动/未安装"。
- **结论：R5.1-B = PASS（范围限定：审计 + 两个代码修复 + 诚实性）；P0 = 0，P1 = 0，P2 = 2，P3 = 10。**
  - **`DEPLOYMENT_BLOCKED` 成立**：无镜像、无 digest、无任何容器 build/run，容器级门禁全部 NOT_RUN。
  - **不是 `CONTAINER_RC_READY`**：容器从未执行（本轮也未尝试）；且**容器验收脚本仍有 2 个执行缺陷（P2-1/P2-2）**，在真实 daemon 上会**响亮失败**（不会假 PASS），因此脚本在修复前不能作为门禁使用。
  - **不是 `LIVE_READY`**：真实 Cloudflare/GitHub/WorkBuddy、生产 TLS ingress、公网发布均未验收。
  - 两个代码修复（JSON keyring、认证 `/readyz` HEALTHCHECK）**真实、可复现、失败关闭**；未发现默认配置可触发的安全缺陷或发布阻断问题。
- 冻结：HEAD `24042e737d151fe114ce218943f7a052095f277a`；
  `git ls-tree -r HEAD | sha256sum` = `10b7860b9485f93c17079e42ea5a7d3b58f17d83d058f2c08a0b4e58139b3bae`（详见 §10）。

> 边界：未 push/merge/release/deploy、未安装/启动 Docker、未修改宿主机服务、未调用真实平台；
> 未创建容器/镜像/卷。审查结束时无遗留监听进程。

---

## 1. 判定与问题表

### 1.1 P0 / P1

**无 P0，无 P1。** 两个代码修复均正确且 fail-closed；JSON keyring 的 17 组对抗输入全部符合预期；
healthcheck 不泄漏 token/响应体；Docker 诚实性经独立复核；未发现会阻断发布的代码级问题。

### 1.2 P2（非阻塞，但会阻断"下一步真正执行容器门禁"）

| 编号 | 级别 | 文件:行 | 复现 | 影响 | 建议修复 | 回归 |
|---|---|---|---|---|---|---|
| **P2-1** | P2（脚本执行缺陷） | [`tests/r51/docker_acceptance.sh:55-58`](../../tests/r51/docker_acceptance.sh)（`ENVS` 无 `ONBOARD_ADMIN_TOKEN_FILE`）、[`Dockerfile:41-43`](../../Dockerfile)（ENV 只设 master key 文件）、[`internal/config/config.go:597`](../../internal/config/config.go) | 脚本手动容器用 `ENVS` 启动但**没有传 admin token 文件 env**（只 bind-mount 了 `/run/secrets/onboard_admin_token`）；而 `ENVS` 含 `ONBOARD_ALLOW_PUBLIC_LISTEN=1`，config 校验强制要求 token → **容器启动即 exit 1**：`fatal: public listen requires an admin token`。无需 daemon 即可复现：`onboardd` 带与脚本相同的 env（不含 token env、含 ALLOW_PUBLIC_LISTEN=1）→ **exit 1**；只带 Dockerfile ENV 启动则 `/readyz` **401**、`onboardd healthcheck` **exit 1**（`no admin/readonly token configured`） | daemon 可用时脚本在**第一条 `/healthz 200` 断言就失败**（容器根本没起）；后续 `badkey`/`ro`/第二容器等 fail-closed 检查也会因"先撞 token 规则"而**通过得没有意义**（归因错误）。属**假 FAIL + 弱断言**，不是假 PASS | 在 `ENVS` 增加 `-e ONBOARD_ADMIN_TOKEN_FILE=/run/secrets/onboard_admin_token`（并显式 `-e ONBOARD_MASTER_KEY_FILE=/run/secrets/onboarding_master_key`，不依赖 Dockerfile 隐式 ENV）；建议加一个无 daemon 的"env 契约"自检（用同一组 `ENVS` 跑本地 `bin/onboardd` 断言能启动/healthcheck 0） | 无 daemon：`ENVS` 组装的 env 起本地二进制 → `/healthz 200`、`/readyz 200`、`healthcheck 0`；容器内再跑一次 |
| **P2-2** | P2（脚本执行缺陷） | [`tests/r51/docker_acceptance.sh:41,50,136-147`](../../tests/r51/docker_acceptance.sh) | v2 模拟库由 host python 在 `umask 077` 下创建 → 文件 **`-rw-------`（0600，owner=host uid）**；脚本只 `chmod 777` 了目录（line 50），**没有 chmod DB 文件**。容器以 `--user 10001:10001` 运行，在标准容器 UID 语义下读不了该文件（0600、owner≠10001；脚本未设置 mode/uid-gid 映射）→ v2 升级容器启动失败 → `v2 database upgraded /readyz 200` 会 `bad`（Docker Desktop 若自行改写 owner/mode 则另说，但脚本并未依赖这一点）。对比：degraded DB 显式 `chmod 666`（说明作者知道该问题，v2 漏了） | daemon 可用时脚本在 v2→v3 迁移段 **FAIL**；不是假 PASS（会响亮失败），但会让首次真实执行中断在此处 | python 创建 v2 DB 后 `chmod 644 "$WORK/v2data/state.sqlite"`（或 666，与 degraded 一致）；更稳妥：在容器内用 UID 10001 生成/初始化 v2 库 | 本地：umask 077 创建 DB 后断言容器 UID 10001 可读（或静态断言 mode 含 o+r） |

> 两项都属于**测试脚本**缺陷，不影响产品二进制；但本轮交付物之一就是"可执行的容器门禁"，因此必须在 R5.2 前修复。

### 1.3 P3（杂项，不阻塞）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | `tests/r51/docker_acceptance.sh:273-276` | "unwritable data dir" 的 fallback 探测的是**主容器** `$PORT/readyz`（28788），不是 `$PROJECT-ro`；若 `-ro` 容器没退出，会读到主容器 200 → **假 FAIL**。建议 `docker exec $PROJECT-ro` 探测或给 `-ro` 单独端口 |
| P3-2 | `tests/r51/docker_acceptance.sh:113-121` | `MODES` 为空（`stat` 失败/镜像无 `stat`）时 while 不执行、`SECRET_OK` 保持 1 → 模式检查**空过**；建议要求读到 2 行 mode 且非空 |
| P3-3 | 同上（badkey/ro） | P2-1 未修前，"missing/invalid master key 容器退出"和"不可写 data dir"等检查全部先撞 `public listen requires an admin token` → 通过原因错误；修 P2-1 后需重新确认归因 |
| P3-4 | [`deploy/scripts/preflight.sh:120-126`](../../deploy/scripts/preflight.sh) | 新 warning 排除了 owner=0（root）：但容器 UID 10001 + `cap_drop ALL`（无 DAC_OVERRIDE）同样读不了 **root 0400**；建议 owner 不是 10001 都提示（或校验容器内可读性） |
| P3-5 | [`cmd/onboardd/main.go:221-249`](../../cmd/onboardd/main.go) | `onboardd healthcheck` 语义变严格：现在需要**完整配置 + admin/readonly token**；旧用法（仅 `ONBOARD_LISTEN` 当 liveness 探针）会 exit 1。R5.1-B §2.2/§2.4 已述新语义，但 README/R0/FINAL 仍把 healthcheck 描述为通用子命令——建议补一句"需要 token 配置，容器外 liveness 请改用 `/healthz`" |
| P3-6 | [`Dockerfile:48-49`](../../Dockerfile) | 镜像 HEALTHCHECK 没有 `--start-period`（compose 有 10s）；`docker run` 用户在大库迁移时可能被过早判 unhealthy。建议加 `--start-period=10s` |
| P3-7 | [`tests/r51/isolation_audit.sh:9-12`](../../tests/r51/isolation_audit.sh) | 危险命令正则未覆盖 `docker image prune`、`docker builder prune`、`docker container rm`、`docker rmi`；当前脚本本身干净，但审计门禁不完整 |
| P3-8 | `docs/acceptance/evidence/R5.1-B-isolation-audit.log:31` | 该行的 shell 变量赋值（`STATE` ← `docker inspect ...`）被仓库自己的 `scripts/redact_scan.sh` 判为 `credential_assignment`（**误报**，无秘密），导致 `redact_scan --file` **exit 非 0**（findings=1，实测 exit 1；其余 4 份 R5.1-B 证据均 exit 0）。建议对审计日志的赋值行做脱敏/换行输出，或在扫描器中把此类 shell 变量加入豁免 |
| P3-9 | `docs/acceptance/R5.1-B.md:84,216,220,222` | 文案不一致：§2.5 仍写"新增 4 个顶层测试"（计数行已写 5 个）；§9 同一份 `R5.1-B-isolation-audit.log` 证据列了**两次**；§1/§9 的提交仍是"见 `git log --oneline -1`"占位，未写死最终 HEAD |
| P3-10 | 本报告标题项 | Lead 消息称最终 HEAD 为 "`66d0a2e`-series"，实际 `git log -1` 为 **`24042e7`**；已按实际 HEAD 冻结（若后续还有提交需重算） |

---

## 2. 独立执行的命令与真实退出码（HEAD `24042e7`）

| # | 命令 | 退出码 | 结果 |
|---|---|---:|---|
| 1 | `git rev-parse HEAD`；`git status --porcelain` | 0 | `24042e7…`；干净 |
| 2 | `make verify`（fmt + vet + 全量 `-race` + build） | **0** | 29 包 |
| 3 | `go test ./... -race -count=1` | **0** | 29 包，14 个 `ok`、15 个 no-test |
| 4 | 计数：`go test ./... -list '.*'` / `-v` | 0 | **320 顶层（315 PASS + 5 SKIP）/ 629 PASS 含子测试（634 RUN）/ 0 FAIL** —— 与 R5.1-B/FINAL 的更新值一致 |
| 5 | `go test ./internal/config/ -run TestDecodeMasterKey -count=1` | **0** | 3/3（含 `TestDecodeMasterKeyJSONKeyring`、`TestDecodeMasterKeyRawKeyStartingWithBrace`） |
| 6 | `go test ./cmd/onboardd/ -count=1` | **0** | 3/3（认证/503/不可达、错误不泄漏 token） |
| 7 | `bash tests/r51/healthcheck_evidence.sh` | **0** | **6/6 PASS**（JSON keyring 启动、readyz 200、healthcheck 0、缺表→503、healthcheck 1、raw key 仍可用） |
| 8 | `bash tests/r51/isolation_audit.sh` | **0** | 无全局破坏命令；`compose override config: exit 0`；daemon not available（informational） |
| 9 | `bash -n tests/r51/*.sh`（5 个脚本） | **0/0/0/0/0** | 语法通过 |
| 10 | `docker info` / `docker version` / `docker images` / `docker ps` / `docker build -t account-onboarding:r51b-probe .` | **1/1/1/1/1** | 全部 `Cannot connect to the Docker daemon`；无镜像/digest 可产生 |
| 11 | `docker compose -f deploy/compose.yaml config`；带 `R51_*` 的 override config | **0 / 0** | 静态清单；证据快照 `R5.1-B-compose-config.yaml` 与我当前 `config` 输出**逐字节一致** |
| 12 | `bash tests/r51/docker_acceptance.sh` | **3** | `NOT_RUN: Docker daemon is unavailable`（未构建/未启动容器，未改宿主机） |
| 13 | 我的 JSON keyring 对抗探针（17 组） | 0（17/17 符合预期） | 见 §4 |
| 14 | 我的 healthcheck 行为探针 | 见表 | `ONBOARD_LISTEN=0.0.0.0` → 映射 127.0.0.1、exit **0**；错误 token → exit **1** 且不打印 token；503 stub → exit **1** 且不打印响应体 |
| 15 | `deploy/scripts/preflight.sh --secrets-dir … --data-dir …`（JSON keyring + 0600 secrets） | **0** | 输出 `PASS master key: JSON keyring with 1 version(s), current=1`；**WARN** secret owner 501（可操作提示），退出码 0（warning 不失败） |
| 16 | 手动容器 env 模拟（脚本 `ENVS`，无 token env + `ALLOW_PUBLIC_LISTEN=1`） | **1** | `fatal: public listen requires an admin token` → **P2-1** 直接证据 |
| 17 | v2 模拟库创建（umask 077） | — | 文件 mode **`-rw-------`**（0600）→ **P2-2** 直接证据 |
| 18 | `scripts/redact_scan.sh --file` 5 份 R5.1-B 证据 | 0/0/0/**1**/0 | 仅 `isolation-audit.log` exit 1（误报 shell 变量赋值，P3-8） |

---

## 3. Docker 诚实性核验

- **五种 docker 命令全部 exit 1**（本机复跑）；daemon socket 不响应；`docker compose config` exit 0 仅静态。
- `docker_acceptance.sh` 在 daemon 缺失时**只在开头的 `docker info` 守卫处返回 exit 3**，未进入任何 build/run/cleanup 分支；evidence `R5.1-B-docker-acceptance.log` 三行与实测一致。
- **无镜像/无 digest**：daemon 不可用时无法枚举 image store（这一点报告已如实写"未构建、无 Image ID、无远端 digest"）；`docker build` exit 1 与之一致。
- **未启动/未安装 Docker**：backend 进程 PID 5515 启动时间 `Wed Oct 21 23:33:21 2026`（早于本轮），无新进程/无 systemd（macOS）；报告明确写"未尝试启动 Docker Desktop（属修改宿主机服务）"。
- **未夸大**：R5.1-B 明确 `NOT CONTAINER_RC_READY`/`NOT LIVE_READY`；README/index/FINAL 均写 `DEPLOYMENT_BLOCKED`、container **NOT_RUN**、no image digest；未见把本地替代证据写成容器 PASS 的句子。
- 唯一"近似容器"的证据是 `R5.1-B-compose-config.yaml`（静态）与本地进程级 healthcheck 6/6——均已明确标注 **非容器**。

---

## 4. JSON keyring 修复核验（`internal/config`）

实现：`DecodeMasterKey` 先处理 **raw 32-byte key（含以 `{` 开头的二进制 key）**，再进入 `decodeKeyringEnvelope`
（`{"current":N,"keys":{...}}`）；envelope 解析失败/缺版本/尺寸不对一律**返回错误（fail closed）**，不回落 raw。
我用独立 Go 探针（17 组）验证：

| 输入 | 结果 |
|---|---|
| raw 32B 以 `{` 开头（含 NUL 字节） | **接受**（新优先级生效，`TestDecodeMasterKeyRawKeyStartingWithBrace` 覆盖） |
| raw 32B 随机二进制 | 接受 |
| JSON std / RawStd / URL / RawURL base64（32B key） | 全部接受，取 `current` 指定版本 |
| JSON 前有/后换行空格 | 接受（`TrimSpace`） |
| JSON 不合法 / `current=0` / 缺版本 / 版本 key 不是 32B / keys 非字符串 / current 为字符串 | **全部拒绝**（fail closed，无回落） |
| JSON 多余字段 | 接受（忽略未知字段） |
| hex 64 / base64 padded / 空串 | 接受/接受/拒绝（与旧行为一致） |

- 未发现 key 泄漏：错误文本只含版本号/长度语义，不含 key bytes；`preflight.sh` 也接受 JSON keyring 并只打印"1 version(s), current=1"。
- 无回退风险：base64/hex/raw 路径保持原语义，测试 `TestDecodeMasterKey` 仍 PASS。

---

## 5. HEALTHCHECK 修复核验（`cmd/onboardd/main.go`）

- `healthcheck()` 现在 `config.Load()` → 取 `AdminToken`（否则 `ReadOnlyToken`）→ 认证探测 `http://<Listen>/readyz`（3s 超时）；
  失败 exit 1，**不打印 token/响应体**。`/readyz` 路由为 `apiAuth(requireCSRF=false, adminOnly=false)`，所以 readonly token 也可用（fallback 有意义）。
- 实测：`ONBOARD_LISTEN=0.0.0.0:port` + `ALLOW_PUBLIC_LISTEN=1` → 映射 `127.0.0.1` → **exit 0**；错误 token → **exit 1**（`readiness status 401`，不含 token）；无 token 配置 → **exit 1**（`no admin/readonly token configured`）；503 stub → **exit 1**、不打印响应体；degraded DB（缺撤销表）→ `/readyz 503` + healthcheck **exit 1**（证据 6/6，我复跑一致）。
- 容器接线：Dockerfile `HEALTHCHECK … CMD ["/app/onboardd","healthcheck"]`；compose `healthcheck.test` 同命令 + `start_period 10s`，且 compose 显式设置 `ONBOARD_ADMIN_TOKEN_FILE`/`ONBOARD_MASTER_KEY_FILE` → **compose 路径的 token 供给正确**。
- **已知边界（P3-5）**：`onboardd healthcheck` 不再是无配置的 liveness 探针；外部监控若只设 `ONBOARD_LISTEN` 会得到 exit 1。Dockerfile 只设 master key ENV、未设 admin token ENV，这是 **P2-1** 在手动 `docker run` 路径失败的直接原因（见 §7）。
- 5s Docker HEALTHCHECK timeout vs 3s 内部超时 + config 读取：一般够用；大 keyring/慢文件系统下偏紧（P3-6 提 start-period/可考虑 4s）。

---

## 6. 容器 Secret 可读性修复核验（`24042e7` 新增）

- `deploy/scripts/preflight.sh`：secret 0400/0600 且 owner∉{10001,0,unknown} 时输出 **WARN**（不失败）。我用 0600/owner=501 的临时 secrets 实测：**exit 0**，两条 WARN 给出 `sudo chown 10001:10001` 提示；JSON keyring 同时被 PASS。
- `tests/r51/docker_acceptance.sh`：测试 secrets 显式 `chmod 644`（test-only，注释说明生产必须 10001/chown），
  secret 检查从"无 group/other **读**位"改为"无 group/other **写**位 + `test -r`（UID 10001）"；Compose 段也新增 `docker exec "$COMPOSE_CID" test -r /run/secrets/...` 断言，失败信息明确（`FAIL compose secrets not readable by UID 10001`），不会静默通过。
- 文档：R5.1-B §2.4 与 §7 风险表把"secret 对 UID 10001 可读性"标为**高（部署前置）**并注明需容器验证——诚实、与代码一致。
- **残余缺口（P3-4）**：preflight 对 owner=root 不告警（root 0400 对 10001 同样不可读，容器 `cap_drop ALL` 无 DAC override）；且生产 0400/0600 + 非 10001 owner 的常见组合仍只能靠 warning，最终必须在真实容器验证/`chown 10001` 或 compose uid/gid 映射解决（文档已列为高风险管理项）。

---

## 7. 容器验收脚本静态审计（`tests/r51/docker_acceptance.sh`）

**隔离性声明核验（静态 + 无 daemon 检查）**：唯一 `PROJECT`/`IMAGE`（PID+时间戳）、数据/密钥在 `.sandbox/r51/docker-$ID/`、
端口仅 `127.0.0.1:28788..28791`、默认 bridge、无 host 网络、compose 用独立 `-p` + `tests/r51/compose.override.yaml`、
`down -v` 只作用该 project、cleanup 只删本次命名的容器/本 project 资源/本镜像；`grep` 无 `docker system prune`/全局 rm/全局 volume/network 清理。
`bash -n` 通过；`isolation_audit.sh` exit 0。**未发现会破坏宿主/其他容器的操作**（P3-7 只是审计正则覆盖面不足）。

**但脚本在真实 daemon 上很可能在第一段就失败**（无需 daemon 即可证明的 2 个缺陷）：

1. **P2-1**：`ENVS` 缺 `ONBOARD_ADMIN_TOKEN_FILE`（Dockerfile 只提供 master key ENV）。`-e ONBOARD_ALLOW_PUBLIC_LISTEN=1` + 无 token →
   `config.Validate` 直接拒绝 → 容器退出。实测本地等价 env：`fatal: public listen requires an admin token`（exit 1）；
   只有 master key ENV 时 `/readyz` 401、`healthcheck` exit 1。故 `/healthz 200`、`/readyz`、容器 HEALTHCHECK、in-container healthcheck 全部会 FAIL。
2. **P2-2**：v2 模拟库 0600（umask 077）未 chmod → 容器 UID 10001 读不了 → v2 升级段 FAIL。

其余执行风险：P3-1（rodata fallback 探错端口）、P3-2（MODES 空过）、P3-3（fail-closed 检查归因被 P2-1 污染）。
**未发现任何 "会误报 PASS" 的容器级断言**；风险方向是"响亮 FAIL/弱归因"，不是"假装通过"。

另外，脚本对 restore 的调用（`restore.sh --snapshot … --target … --keyring … --force` + `CONFIRM_RESTORE=1`）、
v2→v3 迁移、SIGTERM/SIGKILL、同卷双容器、compose 端口回环等断言设计合理；`restore.sh` 的参数与用法匹配（`--snapshot/--target/--keyring/--force`，见其 usage 段）。

---

## 8. 证据与文档准确性

| 项 | 核对结果 |
|---|---|
| 计数 320 顶层（315 PASS + 5 SKIP）/ 629 含子测试 | **实测完全一致**（634 RUN、0 FAIL）；FINAL-REPORT 与 R5.1-B 计数行都已更新 ✅ |
| 新增测试数 | 实际新增 **5** 个顶层测试（config 2 + cmd/onboardd 3）；R5.1-B:25 已写 5，但 **:84 仍写 4**（P3-9） |
| healthcheck 6/6 | 我复跑逐行一致 ✅ |
| isolation audit | 我复跑 exit 0、override config 0 ✅；但提交的日志里 `STATE=` 行触发自身 `redact_scan`（P3-8） |
| compose 快照 | 与当前 `docker compose config` 输出**逐字节一致**；仅含文件路径，无秘密 ✅ |
| docker probe 日志 | 五个 exit 1 与实测一致；backend PID/启动时间与 `ps` 一致 ✅ |
| 镜像 digest | 明确 **NOT_RUN**；无任何伪造 digest ✅ |
| 秘密 | 5 份证据 `redact_scan` 中 4 份 exit 0；唯一 finding 是误报（P3-8）；无 PEM/Cookie/token 值 ✅ |
| 文档小问题 | R5.1-B:216 "见 git log -1" 占位、:220/:222 重复证据条目、:84 数字不一致（P3-9）；Lead 所述 HEAD 与实际 24042e7 不一致（P3-10） |

---

## 9. 残余风险与供应链

- **Secret 对 UID 10001 的可读性**是本轮暴露的**高（部署前置）**项：host 0400/0600 + owner≠10001 时容器读不了；
  现仅 preflight warning + 脚本断言，真正修复（chown 10001 / compose uid-gid 映射 / 其他注入方式）需在有 daemon 环境验证。
- Docker Desktop bind-mount UID 映射差异（macOS）仍可能让"644 test secret"在容器内表现不同；脚本首次真实执行需观察。
- 容器门禁脚本的两处执行缺陷（P2-1/P2-2）未修前不能用于 `CONTAINER_RC_READY` 评审。
- 镜像/供应链：无镜像、无 digest；基础镜像 tag 未 pin、未签名/SBOM/漏洞扫描；GHCR 未 push。
- `onboardd healthcheck` 现在需要完整配置 + token（P3-5）；若外部监控依赖旧 liveness 语义需改用 `/healthz`。

---

## 10. 冻结（最终 HEAD 树）

| 项 | 值 |
|---|---|
| HEAD | `24042e737d151fe114ce218943f7a052095f277a` |
| base | `0d78e8d`（两个提交：`3f601d1` + `24042e7`） |
| HEAD 树清单 | `git ls-tree -r HEAD \| sha256sum` = `10b7860b9485f93c17079e42ea5a7d3b58f17d83d058f2c08a0b4e58139b3bae` |
| 工作树 | 审查开始/冻结时 `git status --porcelain` 为空；本报告是唯一新增未跟踪文件 |
| `internal/config/config.go` | `92dff5c7…` |
| `internal/config/config_test.go` | `7375889e…` |
| `cmd/onboardd/main.go` | `788c6f77…` |
| `cmd/onboardd/healthcheck_test.go` | `87d7015d…` |
| `deploy/scripts/preflight.sh` | `ebd71cbf…` |
| `tests/r51/docker_acceptance.sh` | `b2821029…` |
| `tests/r51/healthcheck_evidence.sh` | `fbf0e6c4…` |
| `tests/r51/isolation_audit.sh` | `7288bf21…` |
| `docs/acceptance/R5.1-B.md` | `566eda52…` |
| `docs/acceptance/FINAL-REPORT.md` | `264c834e…` |
| `docs/acceptance/R5.1.md` | `f2b4a31b…` |
| `README.md` / `docs/acceptance/index.md` | `eca3e6d3…` / `c03cd93f…` |
| evidence | `R5.1-B-docker-probe.log f58ea6e1…`、`docker-acceptance 490daab4…`、`healthcheck b5bc44f9…`、`isolation-audit 9d32d567…`、`compose-config f626afa8…` |
| 我的证据 | `.sandbox/review/logs/r51b-final-gates.log`、`r51b-gates.log`、`.sandbox/review/keyprobe/`、`.sandbox/review/r51b-probe/`、`.sandbox/review/preflight/` |

---

## 11. 最终判定

| 判定 | 是否成立 | 依据 |
|---|---|---|
| **R5.1-B = PASS（范围限定）** | **成立** | 审计真实、两个代码修复正确且 fail-closed（keyring 17 组对抗输入、healthcheck 6/6 + 行为探针）、Docker 诚实性经独立复核（5 命令 exit 1、脚本 exit 3、无镜像/容器、无 CONTAINER_RC_READY 声明）；P0=0/P1=0。**但**：容器门禁脚本有 2 个 P2 执行缺陷（P2-1/P2-2），修好前不要把脚本描述为"可直接执行的门禁" |
| **`DEPLOYMENT_BLOCKED`** | **成立，且必须保持** | 无容器 build/run/healthcheck/持久化/故障注入证据；daemon 不可用；无镜像/digest；容器级门禁全部 NOT_RUN。TLS 仍为 R5.1 的 PASS（不改变本判定） |
| **不是 `CONTAINER_RC_READY`** | **成立** | 未构建镜像、未运行容器、未做容器内健康检查/非 root/只读/卷/WAL/迁移/崩溃/重建/备份恢复/双容器单写；且脚本自身 P2-1/P2-2 未修 |
| **不是 `LIVE_READY`** | **成立** | 无真实 Cloudflare/GitHub/WorkBuddy 证据；生产 TLS ingress/域名/公网发布均未做 |
| **是否夸大** | **未发现实质夸大** | 报告明确写 NOT_RUN/无镜像/无 digest，并在 §2.4/§7 标出高风险 secret 可读性；需软化的是"脚本已 hardened ready"这一措辞（实际有 2 个执行缺陷），以及 3 处 P3 文案 |

**结论：R5.1-B 可按 PASS（范围限定：审计 + 两个已修复代码缺陷 + 诚实 NOT_RUN）交付；`DEPLOYMENT_BLOCKED` 的判定正确且不可放宽。
修复 P2-1/P2-2 之前，`tests/r51/docker_acceptance.sh` 不应被当作可执行门禁，更不应据此宣告 `CONTAINER_RC_READY`。**
