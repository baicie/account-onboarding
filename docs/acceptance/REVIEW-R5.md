# 独立对抗性审查报告 — R5 发布准备（REVIEW-R5）

- 审查时间：`2026-10-24T15:01Z–15:20Z`（本机 CST `23:01–23:20`，Asia/Shanghai）
- 审查者：独立 reviewer（**未参与实现**；本轮只写本文件，未改任何代码/他人文档/运行态）
- 审查对象（冻结快照）：
  - 基线 `e557387`（v0.1.0 交付树）
  - R5 代码 `3719026`（R5-A 持久化会话撤销 + R5 门禁脚本/证据）
  - 文档 `814df64`（R5.md / FINAL-REPORT / README / index）
  - 追加 `4ab5899`（fail-closed 撤销存储测试 + Dockerfile 静态审计）
  - 追加 `0ec7574`（升级时旧 Cookie 失效说明）
  - **最终 HEAD = `0ec7574906d9ea3118edd37551b9fce766f778b9`**；审查开始时工作树干净，结束时唯一新增未跟踪文件为本报告
- 环境：macOS Darwin arm64、Go 1.25.3（cgo `mattn/go-sqlite3`）；本机真实 Keycloak 26.8.1 + Mailpit v1.32.0
  （`127.0.0.1:8180/8025`）；Docker daemon **未运行**（`docker info` → 1）
- 结论：**R5 = PASS（发布准备轮次）**。**P0 = 0，P1 = 0，P2 = 8**（均为非阻塞的硬化/文档/工具稳健性项）。
  R5-A（持久化会话撤销）实现正确且可复现；R5-B Docker、R5-C Cloudflare live 的 **NOT_RUN 声明诚实**；
  `LOCAL_RC_READY` / `DEPLOYMENT_BLOCKED` / `not LIVE_READY` 三个判定**均成立**。
- 冻结指纹：
  - `git ls-tree -r HEAD | sha256sum` = `3a54838b634076362c44ade78c4903b1c0023a545b0c9fc055918dac0831861f`
  - R5 关键文件 blob hash（`git hash-object`）见 §7。

> 边界声明：未 push/merge/deploy，未启动容器，未调用真实 Cloudflare/GitHub/WorkBuddy，未改凭据/DNS/Email Routing。
> 所有探针均在 `.sandbox/review-r5/`（gitignored）内运行；结束时无遗留监听进程（18796–18802 均空闲）、
> Keycloak realm 用户为 0。

---

## 1. 判定与问题表

### 1.1 P0 / P1

**无 P0，无 P1。** 未发现任何"发布即坏"或可由默认配置触发的安全缺陷；R5 的核心安全目标
（登出后旧 Cookie 跨进程重启不可重放、读撤销状态失败时 fail closed、迁移可增量升级）经我独立复现成立。
因此**没有需要立即升级处理的代码级 P1**；下列 P2 建议在真正对外开放前处理（不阻塞本轮 LOCAL_RC_READY）。

### 1.2 P2（非阻塞）

| 编号 | 级别 | 文件:行 | 复现路径 | 影响 | 建议修复 | 回归测试 |
|---|---|---|---|---|---|---|
| **P2-1** | P2（安全边界/配置变更） | `internal/api/ui.go:257`（撤销 TTL 取 `now + s.Cfg.SessionTTL`）、`internal/security/crypto.go:318-338`（`VerifyFull` 只返回 csrf+sid，不返回 cookie 自己的 `exp`）、`internal/storage/sessions.go:60-72`（prune 删除 `expires_at_ms <= now` 的行）、`internal/config/config.go:280`（`ONBOARD_SESSION_TTL` 可在重启时修改） | 实跑 `.sandbox/review-r5/probe-ttl.sh`：① 用 `ONBOARD_SESSION_TTL=10s` 登录 → ② 重启进程改为 `TTL=1s` → ③ 用旧 Cookie+CSRF 正常登出（写入的撤销行 TTL 仅 **866ms**）→ ④ 1.6s 后一次成功登录触发机会式 prune（行被删除）→ ⑤ **重放已登出的旧 Cookie 得到 200**（Cookie 自身还有约 5s 有效期） | 撤销行的寿命取自"**当前配置**"而不是"**该 Cookie 自己的 exp**"：若运维在登录与登出之间**下调** `SessionTTL`（常见的安全加固动作），或系统时钟回拨，撤销行会早于 Cookie 过期；随后任何一次登录触发的 prune 会删除它，使"已登出的 Cookie"重新可用。默认路径（配置不变）安全：`logout_now + TTL >= login + TTL = cookie_exp` | 让 `VerifyFull` 一并返回签名负载里的 `exp`，登出时撤销到 `max(cookie_exp, now+TTL)`；prune 再加一个 ≥ 历史最大 TTL 的宽限；`ONBOARD_SESSION_TTL` 变更写运维提示 | 新增测试：登录(长 TTL) → 新 Server(短 TTL) 登出 → 模拟 prune → 断言旧 Cookie 仍被拒；等价地断言"撤销行 expires >= cookie exp" |
| **P2-2** | P2（可观测性/自愈） | `internal/api/server.go:495`（`readyz` 仍用 `SchemaVersion >= 2`）、`internal/storage/store.go:162-224`（迁移只执行 `version > MAX(applied)`）、`internal/storage/migrations/0003_session_revocations.sql` | 实跑：schema 3 库上 `DROP TABLE session_revocations`（保留 `schema_migrations=3`）→ 重启服务 → `/readyz` 返回 `{"ready":true,"schema_version":3,"wal":"wal"}`；此时 Cookie 认证全部被拒（fail closed，正确）、`onboardd migrate` 也返回 `schema_version=3` 且**不重建表**（表计数 0） | 在"版本行领先于实际表"的损坏/半迁移库上，就绪探针与 migrate 都报"正常"，但会话能力不可用；排障信息误导。无认证绕过（fail closed），故非阻塞 | `readyz` 改为 `SchemaVersion >= 3`（或等于当前代码常量），并 probe 关键表是否存在（至少 `session_revocations`）；`migrate` 结束时做 schema+tables 断言；另可选：迁移按"表存在性"自愈 | 新增测试：删除表但保留版本行 → `readyz` 返回 503/`ready=false`；`migrate` 非 0 或明确报错 |
| **P2-3** | P2（工具稳健性/证据可复现） | `tests/r5/session_revocation.sh:7-8,31-56`、`tests/r5/provider_boundaries.sh:9-10,20-32` | 实跑：我第一次运行脚本时 `make verify` 正在重建 `bin/onboardd`，脚本 `exec` 失败 → 输出 `server did not become healthy; log:` +**空日志**，`exit 1`；`provider_boundaries.sh` 直接 `exit 7`（curl 连接失败透传）。等构建完成后重跑：**11/11 PASS**、**4/4 PASS** | 脚本依赖外部预构建的 `bin/onboardd`，但没有构建/存在性/版本检查；端口也硬编码（18796/18798），冲突时同样表现为"服务器未就绪 + 空日志"。会给发布门禁制造假失败或误判 | 脚本开头 `go build -o "$WORK/onboardd" ./cmd/onboardd`（或用 `-x "$BIN"` 检查并打印 mtime/版本）；端口改为可覆盖（`R5_PORT=${R5_PORT:-...}`）；启动失败时打印完整日志尾部 | CI 中先 `make verify` 再跑脚本；新增"缺二进制/端口占用时脚本给出明确错误"的自检 |
| **P2-4** | P2（文档准确性） | `docs/acceptance/FINAL-REPORT.md:36` | 实测（HEAD `0ec7574`）：`go test ./... -list '.*' \| grep -c '^Test'` = **308** 个顶层测试；`go test ./... -v` = **622** `=== RUN`（**617** PASS 含子测试、**5** SKIP）。FINAL-REPORT 写"**294 top-level / 616 including subtests**（after R5-A）"；"294" 恰好等于**基线 `e557387` 的顶层 PASS 数**（299 顶层 − 5 SKIP），"616" 等于 R5 代码提交（`3719026`）时的含子测试 PASS 数 | 发布报告里的测试规模数字**过期/口径错误**（把基线数字标成 after R5-A）。不影响功能，但属于"发布报告数据不实" | 改为实测值并在括号注明口径与提交：例如 `308 top-level (303 PASS + 5 SKIP) / 617 PASS incl. subtests @ 0ec7574` | 文档审阅即可；建议 R5 文档里只写命令 + "以当次输出为准"，避免硬编码计数 |
| **P2-5** | P2（证据卫生） | `docs/acceptance/R5.md:223-225`（"回环监听门禁实测 … exit 1"）、`tests/r5/provider_boundaries.sh`（只有 A/B/C 三项） | 我从零复现该断言：`ONBOARD_LISTEN=0.0.0.0:18797 ./bin/onboardd` → **exit 1**，错误文本与 R5.md 引用的完全一致。但 `docs/acceptance/evidence/R5-provider-boundaries.log` 只有 `SUMMARY pass=4`（A/B/C），**没有**这条非回环检查的归档输出 | 结论本身真实，但"实测"没有随证据归档；下次审计无法从 evidence 目录复核 | 把 D 项（非回环监听拒绝）加入 `provider_boundaries.sh` 并更新 evidence 日志（脚本化后即为可复现证据） | `bash tests/r5/provider_boundaries.sh` 输出 5/5 并归档 |
| **P2-6** | P2（残余风险未披露） | `internal/security/crypto.go:281-287`（会话签名密钥由 **MasterKey** 派生）、`internal/api/ui.go:190-226`（登录只校验 admin token）、`internal/storage/sessions.go`（只有 logout 写撤销） | 代码级：轮换 `ONBOARD_ADMIN_TOKEN`/只读 token **不会**使既有 Cookie 会话失效（会话与 admin token 解绑，只与主密钥+撤销表相关）；R5.md §6 的"残余风险"表未列此条 | 事件响应中"轮换操作员 token"是常见动作，但轮换后旧会话仍有效直到 TTL 或显式登出；R5 声称的"发布就绪"未覆盖该边界 | 在 R5/FINAL 残余风险中明确写出；可选实现 session epoch（token 变更时递增，鉴权比对）或提供"全局吊销所有会话"的运维命令 | 新增测试：轮换 admin token 后旧 Cookie 的行为（当前为仍有效）；文档写明 |
| **P2-7** | P2（回滚/降级安全性，文档） | `docs/acceptance/R5.md:114,155,253`（"升级回滚 NOT_RUN"）、R5 前的二进制（`e557387` 的 `server.go` 用内存撤销 map） | 代码/历史对照（已核）：`git show e557387:internal/api/server.go` 只有 `revoked map[string]int64` + `revokeSession/sessionRevoked` 进程内实现，无 SQLite 读取；若把 R5 数据库回滚到 **R4 二进制**运行，R5 期间写入 SQLite 的撤销行不会被旧代码读取，**已被登出的 Cookie 在旧进程重启后会重新有效**（旧代码启动时内存表为空） | 部署被阻断时不构成现实风险，但"回滚到旧镜像"是发布流程的一部分；R5 文档只写升级回滚 NOT_RUN，未说明"降级会丢失持久撤销语义" | 在 R5.md §3.3/§6 增加回滚注意：回滚到 R4 及更早版本前必须轮换 MasterKey（使旧 Cookie 全部失效）或接受已登出会话可能复活 | 文档审阅；无需代码测试 |
| **P2-8** | P2（证据完整性/诚实性说明） | —（非代码缺陷） | R5-C 声明"本轮没有向 `api.moonbyte.top` 发出任何请求"。我核实的**间接**证据：环境无 Cloudflare 凭据（`env` 0 命中）、`tests/r5/*.sh` 仅含 loopback 目标、R5 代码提交未触碰 Cloudflare adapter、默认 `/v1/providers` 中 cloudflare 未注册。但"零网络请求"本身无法从仓库内密码学证明 | 该声明属于**不可完全自证**的负向断言；当前证据只能说明"没有可用凭据且代码默认禁用"，不能证明"进程从未发包" | 若需要强证据：下一阶段在隔离网络/防火墙抓包（或使用 `network=host` 之外 deny-all 的网络命名空间）录制一次 R5 门禁运行的 pcap；否则在 R5.md 标注"未抓包，依据为无凭据+默认禁用" | 抓包/eBPF 证据归档 |

> 说明：P2-8 不是"发现的缺陷"，而是对一条**无法完全自证**的声明的审查结论；R5.md 本身已明确写
> NOT_RUN、没有把 Cloudflare 写成 PASS，因此**不构成夸大**。

---

## 2. 独立执行的命令与退出码（全部实跑）

> 证据落盘：`.sandbox/review/logs/r5-gates.log`（门禁）、`.sandbox/review-r5/probe.log`（撤销/CSRF/迁移探针）、
> `.sandbox/review-r5/probe-ttl.log`（P2-1）、R5 脚本自身日志 `.sandbox/r5/**`。所有命令在仓库根目录执行。

| # | 命令 | 退出码 | 结果摘要 |
|---|---|---:|---|
| 1 | `git rev-parse HEAD`；`git status --porcelain` | 0 | `0ec7574…`；审查期间除本报告外无变更（本文件为唯一新增未跟踪项） |
| 2 | `git show --stat 3719026` | 0 | 16 文件 +766/−51，仅 R5-A 相关（storage/api/tests/scripts/docs evidence） |
| 3 | `make verify`（fmt-check + vet + `test -race` + build） | **0** | 29 包全 ok；构建 `bin/onboardd` |
| 4 | `go test ./... -race -count=1` | **0** | 29 包：14 个 `ok`、15 个 `[no test files]` |
| 5 | `go test ./internal/storage/ -run 'TestSessionRevocation\|TestMigration0003' -count=1` | **0** | 4/4 PASS（Reopen / 并发幂等 / 校验 / v2→v3 迁移） |
| 6 | `go test ./internal/api/ -run 'TestLogout\|TestConcurrentAndRepeated\|TestSessionRejectedWhenRevocationStoreUnavailable' -count=1` | **0** | 5/5 PASS（含 fail-closed 契约；该测试在追加提交 `4ab5899` 加入，最终 HEAD 已包含） |
| 7 | `bash tests/r5/session_revocation.sh` | **0** | **11/11 PASS**（真实 `onboardd` 子进程、同 data dir/master key、SIGTERM 重启；撤销行=1；重启后旧 Cookie → 303 `/ui/login`）。注：第一次运行因我并发执行 `make verify` 重建 `bin/onboardd` 而失败，属我的调度问题，非产品缺陷；见 P2-3 |
| 8 | `bash tests/r5/provider_boundaries.sh` | **0** | **4/4 PASS**（无 live provider 启用；fake off；WorkBuddy 无 ack 拒绝启动；GitHub 无 token 拒绝启动） |
| 9 | `docker info` | **1** | daemon 不可用（R5-B NOT_RUN 的直接证据） |
| 10 | `docker build -t account-onboarding:r5 .` | **1** | `Cannot connect to the Docker daemon`（R5.md §3.1 的声明可复现） |
| 11 | `docker compose -f deploy/compose.yaml config` | **0** | 静态清单：`127.0.0.1:8788`、`user 10001:10001`、`read_only`、`cap_drop ALL`、secrets 文件挂载 |
| 12 | `ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1 -timeout 900s`（source `tests/sandbox/env.sh`） | **0** | **4/4 PASS**（43.85s；真实 Keycloak+Mailpit；新证据目录 `.sandbox/e2e/run-20261024T1504*`） |
| 13 | `.sandbox/review-r5/probe.sh`（我自己的对抗性探针，见 §3） | **0** | 25 项断言全 PASS（其中 2 项 NOTE 为发现的 P2-2 现象） |
| 14 | `.sandbox/review-r5/probe-ttl.sh`（P2-1） | 0（脚本）/ 结论 CONFIRMED | 配置下调 TTL + prune 后，已登出 Cookie 重放 **200** |
| 15 | `ONBOARD_LISTEN=0.0.0.0:18797 ./bin/onboardd` | **1** | 非回环监听被拒：`listen host "0.0.0.0" is not loopback; set ONBOARD_ALLOW_PUBLIC_LISTEN=1…`（复现 R5.md §5.1） |
| 16 | `go test ./... -list '.*' \| grep -c '^Test'`；`go test ./... -v \| grep -c '=== RUN'` | 0 | **308** 顶层 / **622** RUN（617 PASS 含子测试、5 SKIP）→ 与 FINAL-REPORT 的 294/616 不符（P2-4） |
| 17 | `scripts/redact_scan.sh --file docs/acceptance/evidence/R5-*.log` ×3 | **0** | 0 findings（证据日志无 Cookie/Token/JWT/PEM） |
| 18 | `scripts/backup.sh` → `restore.sh` → `rotate -dry-run`（对 schema 3 的真实 sandbox 库） | **0/0/0** | manifest `schema_version=3`、restore `secrets_verified=1` + `integrity=ok` + 恢复库含 `session_revocations`、rotate 正常 |
| 19 | `onboardd migrate` 矩阵：fresh / 连续两次 / 删版本行 / 删表 / 模拟 v2（删表+删版本行） | 全 **0** | fresh→3；连续两次幂等；删版本行→重放 0003（IF NOT EXISTS/INSERT OR IGNORE）表在；**删表保留版本行→不重建表**（P2-2）；模拟 v2→自动重建表并升到 3 |
| 20 | `docker ps` | 1（daemon 不可用） | 无容器可运行；R5-B 未运行容器属实 |

---

## 3. 我执行的对抗性探针（细节与结果）

探针全部针对**真实 `bin/onboardd` 子进程**（本机 loopback、临时 data dir/主密钥/admin token），不触碰真实平台。

### 3.1 登出 CSRF / Origin 矩阵（拒绝时不得写撤销行）

| 场景 | 结果 |
|---|---|
| 有 Cookie、**无 CSRF** | **403**，撤销表 0 行 |
| 有 Cookie、**错误 CSRF** | **403** |
| 正确 CSRF + `Origin: https://evil.example` | **403** |
| 正确 CSRF + **无 Origin/Referer** | **403** |
| 全部拒绝后撤销表仍为 0 行 | ✅（不会因 CSRF 失败而误撤销，避免 DoS） |
| 正确 CSRF + 同源 Origin | **303**，撤销行=1 |

结论：登出与其他 cookie mutation 一样被 `uiAuth` 的 CSRF + 同源检查保护；失败路径不产生副作用。

### 3.2 真实进程重启后的重放（脚本非自我实现）

- `tests/r5/session_revocation.sh` 的登录用 cookie jar，重放用**显式 `Cookie:` 头**（不是 jar），且**重启前后都验证**：
  登出前 dashboard `200`（阳性对照）、登出后 `303 /ui/login`、SIGTERM 重启后仍 `303 /ui/login`、新登录 `303` 正常。
  我独立跑出 **11/11 PASS**，并额外用自有探针复验（`probe.sh` 第 3 段）。
- 我另做了"**未重启也立即拒绝**"的检查（PASS），说明不是仅靠重启清空内存态，而是 SQLite 行生效。

### 3.3 撤销状态的信任边界（删行/改库）

- 直接 `DELETE FROM session_revocations`（模拟拥有 DB 写权限的人）后，旧 Cookie **重新可用（200）**。
  这是**预期语义**：SQLite 数据库本身就是认证状态的信任根；能写库的人也能改 run 状态/密文。
  R5 无需修复，但文档应明确"备份/快照中包含撤销表，恢复旧备份会复活当时的会话状态"——**R5.md 未写**（可并入 P2-6/7 的残余风险）。

### 3.4 prune 语义（"不能删除未过期行"）

| 检查 | 结果 |
|---|---|
| 插入 `expires=now+1h` 的行 + `expires=now−1h` 的行，然后一次成功登录触发机会式 prune | 未过期行**保留**，过期行**删除** |
| 结论 | `PruneSessionRevocations` 的 SQL（`expires_at_ms <= now`）满足"不删未过期行"；但"未过期"取决于**写入时的过期时间**，见 P2-1 |

### 3.5 旧版（pre-R5）Cookie

- 用主密钥派生签名密钥**伪造** R5 之前格式的 Cookie（`sub/exp/csrf/nonce`，**无 `sid`**）：被拒（`303 /ui/login`）——
  `SessionRevoked("")` 返回 `true`（fail closed）。
  **阳性对照**：同一伪造脚本生成**带 `sid`** 的 Cookie → `200`，证明签名方案本身有效，无 `sid` 的拒绝确实来自
  "空 id fail closed"而不是签名错误。
- 用该 Cookie 请求登出：`303` 重定向、不写撤销行（到不了 handler）。
- 与追加提交 `0ec7574` 的说明一致：**升级会使旧浏览器会话失效并要求重新登录**（有意的安全行为）。

### 3.6 撤销存储不可读时 fail closed

- 官方测试 `TestSessionRejectedWhenRevocationStoreUnavailable`（`4ab5899` 新增）：关闭 store 后，签名有效的 Cookie
  只能得到 `303 /ui/login`。我实跑 PASS（`internal/api` 包 26.7s，含该测试）。
- 我在"删表保留版本行"的真实进程场景下复验：Cookie 被拒（fail closed），`/readyz` 却仍 `ready=true` → P2-2。

### 3.7 迁移矩阵（幂等与自愈）

| 场景 | 结果 |
|---|---|
| 全新 DB | schema 3、表存在 |
| 连续两次 `migrate` | 幂等，均 exit 0 |
| 删除 `schema_migrations(3)` 但保留表 | 重新执行 0003 幂等（`CREATE TABLE IF NOT EXISTS` + `INSERT OR IGNORE`）→ 表在、schema 3 |
| 删除表但保留版本行 3 | exit 0、报告 schema 3，但**表未重建**（P2-2：不自愈、就绪探针不觉察） |
| 模拟 v2（删表 + 删版本行） | 重新 Open/migrate 后自动建表并升到 3（与 `TestMigration0003AppliesToExistingV2Database` 一致） |

### 3.8 其它

- **schema 3 与备份/恢复/轮换**：真实 sandbox 库（schema 3、含 1 个加密 secret）备份 → manifest `schema_version=3`；
  恢复 → `secrets_verified=1`、`integrity=ok`、恢复库包含 `session_revocations`；旋转 dry-run 正常。
- **真实 E2E 的 schema**：我本轮 e2e 产出的 `state.sqlite` 中 `MAX(schema_migrations)=3` 且表存在 → R5.md 的
  "schema 3 下的 E2E" 有直接物证。
- **秘密泄漏**：3 份 R5 evidence 日志 `redact_scan` 0 findings；我探针产生的全部服务日志中
  admin token / session cookie / master key **均 0 命中**；脚本仅在 0600 的临时文件/jar 中持有 Cookie，未打印。

---

## 4. R5-A 实现评估（正向）

| 要点 | 独立核对结果 |
|---|---|
| 迁移 `0003` 增量、幂等 | SQL 用 `IF NOT EXISTS` + `INSERT OR IGNORE`；迁移在单事务中应用；v2→v3 自愈（删表+删版本行）实测通过 |
| `RevokeSession` 幂等 upsert | `ON CONFLICT … DO UPDATE SET expires=MAX(existing,excluded)`；16 并发 → 1 行（测试 PASS） |
| `SessionRevoked` 读失败 fail closed | `authenticate` 在 store 读错误时拒绝会话并告警（无 session id/cookie 落地）；测试 + 我的真实进程场景均 PASS |
| 登出先持久化、失败不清 Cookie | handler 在 `RevokeSession` 失败时 500 且不清 Cookie（代码核对；正常路径下 CSRF/认证失败会先被 `uiAuth` 拦为 303/403） |
| prune 有界 | 登录时机会式 prune（失败只告警，不影响登录）；删除的只是已过期行（§3.4） |
| 不影响既有机制 | 全仓 `-race` 0；备份/恢复/轮换/租约/邮件回执回归全绿；`internal/ops`、`internal/api`、`internal/storage` 的 schema 断言已同步为 3（`git show 3719026` 逐个核对，仅 2→3，无弱化） |
| 真实进程级证据 | 脚本就是真实 `onboardd` 子进程 + 真实 SIGTERM 重启；**不是**只换 Server 对象的模拟（单元测试另有该模拟） |

---

## 5. R5-B / R5-C 的 NOT_RUN 是否诚实

| 声明 | 我的核实 | 结论 |
|---|---|---|
| Docker build/run/容器健康检查/持久化/SIGKILL/回滚 = NOT_RUN | `docker info` → 1；`docker build` → 1（Cannot connect）；`docker ps` 不可用；`docker compose config` → 0（仅静态） | **诚实**，未用静态检查冒充运行验收 |
| compose 静态加固 | 实跑 `config`：`host_ip 127.0.0.1`、`user 10001:10001`、`read_only`、`cap_drop ALL`、secrets 为文件 | **属实** |
| Dockerfile 静态审计（追加提交） | 多阶段 `golang:1.25-bookworm`→`debian:bookworm-slim`、`USER 10001:10001`、`HEALTHCHECK`、tag 未 pin digest（R5.md 自己标为 P2） | **属实**，未把 "未构建" 写成 PASS |
| Cloudflare live = NOT_RUN（无凭据/授权，未发请求） | 环境无 CF 凭据（0 命中）；`tests/r5/*.sh` 无外部目标；代码默认禁用；R5 代码提交未触碰 CF adapter；R5.md 明确"不得读成已联调" | **诚实**（负向断言无法完全自证，见 P2-8） |
| Cloudflare 只读契约审计 | 我抽查：`DefaultBaseURL=https://api.moonbyte.top`、`defaultAllowedHosts` 仅该 host、`GET /api/parsed_mails`/`/api/parsed_mail/{id}`、cloudflare **不实现** `MailboxCleaner`、duplicate → CONFLICT | **属实** |
| GitHub/WorkBuddy live = NOT_RUN | 脚本实测：无 live provider 启用、WorkBuddy 无 permission ack 拒绝启动、GitHub 无 token 拒绝启动；默认禁用 | **诚实** |
| TLS ingress = NOT_RUN（部署前置） | 代码仍纯 `http.Server`；compose 仅回环发布；非回环监听被拒（我复现 exit 1） | **诚实** |

**没有发现把 NOT_RUN 写成 PASS、或用 mock/静态检查冒充真实运行的情况。**

---

## 6. R5.md 数字/命令/测试名核对（逐项）

| R5.md 声明 | 实测 | 结论 |
|---|---|---|
| `make verify` exit 0 | 我跑 exit 0 | ✅ |
| `go test ./... -race` exit 0（29 包） | exit 0；29 包（14 ok / 15 no-test） | ✅ |
| 真实 E2E 4/4 PASS、44.85s | 我跑 4/4 PASS 43.85s；`evidence/R5-e2e.log` 末尾 `44.852s` | ✅（时间随运行波动，证据一致） |
| 会话重启脚本 11/11 PASS | 我跑 11/11；evidence 日志逐行一致 | ✅ |
| 平台边界 4/4（providers 列表一致） | 我跑 4/4；输出逐字一致 | ✅ |
| 非回环监听拒绝 exit 1 | 我复现 exit 1 + 相同错误文本 | ✅（但证据未归档，P2-5） |
| 新增测试名 8 个 + fail-closed 1 个 | 全部存在且 PASS（逐个 `grep func Test` 核对） | ✅ |
| E2E "schema 3" | 我本轮 e2e 的 state.sqlite：`MAX(version)=3` 且 `session_revocations` 存在 | ✅ |
| Docker build exit 1 | 我跑 exit 1 | ✅ |
| FINAL-REPORT "294 top-level / 616 incl subtests (after R5-A)" | 实测 HEAD：**308** 顶层（303 PASS+5 SKIP）/ **617** PASS 含子测试；"294" 是基线 `e557387` 的 PASS 数 | ❌ 过期/口径错误（P2-4） |

---

## 7. 冻结指纹（首轮审查快照，HEAD `0ec7574`）

| 项 | 值 |
|---|---|
| HEAD | `0ec7574906d9ea3118edd37551b9fce766f778b9` |
| 工作树 | 审查开始/结束时除本文件外无变更；`git status --porcelain` 在本文件写入前为空，写入后唯一未跟踪项为 `?? docs/acceptance/REVIEW-R5.md`（本报告本身，未提交） |
| `git ls-tree -r HEAD \| sha256sum` | `3a54838b634076362c44ade78c4903b1c0023a545b0c9fc055918dac0831861f` |
| `0003_session_revocations.sql` | `9af8320397dcf0b9f20d118c0ef128ba0ecc82fa` |
| `internal/storage/sessions.go` | `82c5274d42102c327911028c06ae4fc3d8899973` |
| `internal/api/server.go` | `630dd89272278b45b1daa394cbc9925d71bc203b` |
| `internal/api/ui.go` | `e5d156bd6ab03fccd0c04c6fc3ff1d015eb101db` |
| `internal/api/revocation_test.go` | `0dc380527989ae9ff19749cec10fcf388200497d` |
| `internal/api/testserver_test.go` | `850d00a1b81955958643e568c620192e605821bd` |
| `internal/storage/sessions_test.go` | `cfb414892c06c035fb3918f16c9c1bfc6a14448c` |
| `tests/r5/session_revocation.sh` | `90ab93ab8ebfa13dafde952deb7c10d5ffae2e7d` |
| `tests/r5/provider_boundaries.sh` | `61ba84874512e33cdbc465e9ecfcaa539d2a2a0f` |
| `docs/acceptance/R5.md` | `b911cdf093437b616d9babf8cd32072fad064f6f` |
| `docs/acceptance/FINAL-REPORT.md` | `c8488abd69f84494949f37046fee013c48851b64` |
| `README.md` | `7e1acd5d366eb16b4684baf9c8d98b8ec149e0ba` |

说明：`4ab5899` 新增的 `TestSessionRejectedWhenRevocationStoreUnavailable` 与 `0ec7574` 的旧 Cookie 说明均为**追加**，
未改变 `3719026` 的运行时行为；最终 HEAD 已包含二者，上面的文件 hash 与 §2/§3 的实跑均以该 HEAD 为准。

---

## 8. 首轮最终判定（快照，HEAD `0ec7574`；最终结论见 §9）

```
R5: PASS（发布准备轮次）
P0 = 0；P1 = 0；P2 = 8（见 §1.2，均为非阻塞）
```

### 是否为 `LOCAL_RC_READY`？

**成立。** 判定为本地 release candidate 所需的条件齐备：代码可构建、全量 `-race` 绿、真实本机
Keycloak+Mailpit E2E 4/4（schema 3）、R5-A 会话撤销持久化通过并发/重启/过期/迁移回归并有真实进程级证据、
文档与证据文件齐备且未发现秘密泄漏。**但**：该结论只适用于"本机/回环自托管"的代码候选，P2-1/P2-2
应在真正对外发布前修掉（前者影响登出保证的边界，后者影响可观测性）。

### 是否为 `DEPLOYMENT_BLOCKED`？

**成立且必要。** Docker daemon 不可用（build/run/健康检查/持久化/SIGKILL/回滚全部未验收）、
TLS ingress 未落地、Cloudflare/GitHub/WorkBuddy live 无授权——任一项都足以阻断非本机部署。
R5.md 把这些全部列为 NOT_RUN 并给出可执行清单，**没有夸大**。

### 是否为 `not LIVE_READY`？

**成立。** 没有任何真实第三方平台成功证据；Local Sandbox PASS 被明确限定，未外推。

### 是否有结论被夸大？

- **没有实质夸大**：R5-B/R5-C 均诚实标注 NOT_RUN；R5-A 的 PASS 有真实进程级证据支撑且我独立复现。
- **两处需更正/补强（P2）**：FINAL-REPORT 的测试计数（294/616 过期）属于**不实数据**，应更新；
  R5.md 的"非回环拒绝实测"缺少归档证据（结论真实但证据卫生不足）。
- 另外建议在 R5.md §6 残余风险中补充：admin token 轮换不吊销既有会话（P2-6）、回滚到 R4 会丢失持久撤销
  语义（P2-7）、恢复旧数据库备份会复活当时的会话撤销状态（§3.3）。

**结论：R5 可按 PASS（发布准备）交付；`LOCAL_RC_READY / DEPLOYMENT_BLOCKED / not LIVE_READY` 三判定均成立。
本 PASS 不放宽任何 NOT_RUN 项，也不构成"可部署/可公网/已联调"的结论。**

---

## 9. Post-review P2 closure（2026-10-24T15:28Z–15:45Z，HEAD `a488740`）

> 本节是最终复核；§1–§8 保留首轮审查快照（HEAD `0ec7574`）。最终交付树 = `a488740`（含本报告本身）。
> 说明：首轮报告已有 `§8 最终判定`，因此本节按顺序编号为 §9（内容即委托要求的“Post-review P2 closure”）。

### 9.1 冻结与变更范围

| 项 | 值 |
|---|---|
| HEAD | `a488740d8f09530205b0ea1cdd8da0527256c136` |
| 与首轮快照 `0ec7574` 的差异 | **14 文件**：4 代码（`server.go`/`ui.go`/`crypto.go`/`store.go`）、3 测试（`revocation_test.go`/`crypto_test.go`/`sessions_test.go`）、2 脚本、3 文档、2 证据日志（`R5-e2e.log`/`R5-provider-boundaries.log`；`R5-session-revocation.log` 内容未变）；其中 `REVIEW-R5.md` 是把首轮报告收录进仓库 |
| `git ls-tree -r HEAD \| sha256sum` | `f4614dd803cf61157d6c70220f0ef3215c75a9572b7e5d3e4a793c0a371935c5` |
| 审查开始/结束时工作树 | 干净（本节追加前）；本节追加后唯一变更即本文件 |
| 关键 blob hash | `crypto.go db1cadb2` / `crypto_test.go c00ed574` / `server.go c800917e` / `ui.go 26118594` / `revocation_test.go 347f205e` / `sessions_test.go 04f7873d` / `store.go efe71e17` / `session_revocation.sh 1e29b727` / `provider_boundaries.sh 5785731a` / `R5.md 408ebd2e` / `FINAL-REPORT.md 39f4567b` / evidence `5dfd5503`、`d519e2be`、`a7c5a474` |

未修改任何代码/配置；本节的验证全部针对 `a488740` 实际工作树。

### 9.2 P2 逐项关闭核验

| 编号 | 修复（我读到的实现） | 我的独立验证 | 状态 |
|---|---|---|---|
| **P2-1** 撤销寿命绑定 Cookie exp | `security.SessionSigner.VerifySession` 返回签名载荷 `exp`（`VerifyFull` 保持旧契约）；`authInfo.SessionExpires` 传递；`ui.go` 登出到 `max(now+cfg TTL, cookie exp)` | 新增测试 `TestLogoutRevocationOutlivesShortenedSessionTTL`、`TestVerifySessionReturnsCookieExpiryAndSessionID` 均 PASS。**重跑我首轮的利用脚本**（TTL 10s 登录 → 重启改 1s → 登出 → 1.6s 后 prune → 重放）：撤销行 TTL 现在是 **9113ms**（绑定 cookie exp）、prune 后行仍在、**重放 303 `/ui/login`（首轮为 200）** | **CLOSED** |
| **P2-2** readyz 表探测 | `storage.Health` 新增 `SessionRevocationsReady`（`sqlite_master` 探测）；`/readyz` 要求 `schema>=3 && SessionRevocationsReady`；R5.md 明确不自动重建 | `TestHealthProbesSessionRevocationTable` PASS。**重跑我的删表场景**：`/readyz` 现在返回 `{"ready":false,"schema_version":3,"wal":"wal"}`（首轮 `ready=true`）；Cookie 仍被 fail-closed 拒绝；migrate 仍不自愈（与文档一致） | **CLOSED** |
| **P2-3** 脚本自构建/端口可覆盖 | 两个脚本开头 `go build -trimpath -o "$BIN" ./cmd/onboardd`（`R5_SKIP_BUILD=1` 可跳过）；`R5_BIN`/`R5_PORT`/`R5_PROVIDER_PORT` 可覆盖 | 我**不再预先构建**，直接 `bash tests/r5/session_revocation.sh` → **11/11，exit 0**；`provider_boundaries.sh` → **5/5，exit 0**（首轮先跑时因并发构建失败的现象不再出现） | **CLOSED** |
| **P2-4** FINAL-REPORT 计数 | 改为 311 顶层 / 620 含子测试 | 我实测 HEAD：顶层 **311**（**306 PASS + 5 SKIP**）/ **620 PASS 含子测试**（625 `=== RUN`）/ 0 FAIL —— **与声明完全一致** | **CLOSED** |
| **P2-5** 非回环拒绝证据 | `provider_boundaries.sh` 增加 D 项；`R5-provider-boundaries.log` 归档 5/5；R5.md §5.1 更新 | 我实跑 5/5（含 `PASS D: non-loopback listen refused (exit 1)`），证据日志与提交内容一致 | **CLOSED** |
| **P2-6** admin token 轮换 | R5.md §6 残余风险新增行（轮换 admin token 不影响既有会话；需轮换 MasterKey 强制下线） | 文档核对：条目存在且与代码行为一致（签名密钥来自 MasterKey） | **CLOSED（文档）** |
| **P2-7** 回滚到 R4 | R5.md §6 新增"回滚到 R4 丢失持久撤销语义 → 先轮换 MasterKey 或书面接受" | 文档核对；`git show e557387:internal/api/server.go` 仍是内存 map，前提属实 | **CLOSED（文档）** |
| **P2-8** "零网络"表述 | R5.md §4 改为"构造性事实，不是抓包证明；授权阶段再抓包" | 文档核对，措辞准确，不再有不可自证的强断言 | **CLOSED（文档）** |

### 9.3 本轮命令与退出码（HEAD `a488740`，全部实跑）

| # | 命令 | 退出码 | 结果 |
|---|---|---:|---|
| 1 | `make verify` | **0** | fmt + vet + 全量 `-race` + build |
| 2 | `go test ./... -race -count=1` | **0** | 29 包，14 个 `ok`、15 个 `[no test files]` |
| 3 | `go test ./internal/api/ -run 'TestLogout\|TestConcurrentAndRepeated\|TestSessionRejected' -count=1` | **0** | **6/6 PASS**（含新 `TestLogoutRevocationOutlivesShortenedSessionTTL` 1.22s） |
| 4 | `go test ./internal/storage/ -run 'TestSessionRevocation\|TestMigration0003\|TestHealthProbes' -count=1` | **0** | **5/5 PASS**（含 `TestHealthProbesSessionRevocationTable`） |
| 5 | `go test ./internal/security/ -run TestVerifySession -count=1` | **0** | 1/1 PASS |
| 6 | `bash tests/r5/session_revocation.sh`（脚本内自建二进制） | **0** | **11/11 PASS** |
| 7 | `bash tests/r5/provider_boundaries.sh`（脚本内自建二进制） | **0** | **5/5 PASS**（A×2 + B + C + D） |
| 8 | `ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1 -timeout 900s` | **0** | **4/4 PASS**（我跑 15.14s；提交的 `R5-e2e.log` 12.227s） |
| 9 | 我首轮的利用/探针脚本重跑：`probe-ttl.sh` / `probe.sh` | 0 / 0 | TTL 利用 → **SAFE**（9113ms 撤销、replay 303）；总探针 **26/26 PASS**，readyz 在缺表时 `ready:false` |
| 10 | `scripts/redact_scan.sh` × 3 份再生成的 R5 evidence | **0/0/0** | 0 findings |
| 11 | `docker info` / `docker build -t account-onboarding:r5 .` / `docker compose … config` | **1 / 1 / 0** | 与 R5-B 的 NOT_RUN 口径一致（无 daemon） |
| 12 | 非回环监听（由脚本 D 覆盖；我此前也手工复现） | **1** | `listen host "0.0.0.0" is not loopback…` |

### 9.4 残余小项（P3，不阻塞）

1. `/readyz` 的 503 响应体只回 `schema_version`/`wal`，没有回显具体失败的子项（如 `session_revocations_ready:false`）；`Health` 里已有该字段，建议顺手加进响应/日志便于排障。
2. 两个 R5 脚本默认都构建到 `bin/onboardd`；若与 `make verify` 或彼此**并发**运行，仍可能竞争同一输出文件（P2-3 的核心问题——陈旧/缺失二进制——已修复）。并发场景请用 `R5_BIN` 指向独立路径。
3. P2-8 仍是"构造性事实"；若需要强证据，按 R5.md 所述在授权阶段抓包。
4. FINAL-REPORT 的计数未写出具体命令（只说 HEAD measured），但数字可精确复现；建议补一行取数命令以便机器核对。

### 9.5 结论

**R5 = PASS（发布准备轮次）维持。P0 = 0，P1 = 0；首轮 8 项 P2 全部 CLOSED**
（P2-1/P2-2 代码修复、P2-3 工具修复、P2-4/P2-5 文档与证据、P2-6/P2-7/P2-8 残余风险文档化）；
另有 4 条 P3 级小建议/说明，**无新增 P2、无新增 P1**。 未发现任何新引入的发布阻断问题；`LOCAL_RC_READY`、`DEPLOYMENT_BLOCKED`、
`not LIVE_READY` 三个判定与首轮相同且仍成立。本节只修改 `REVIEW-R5.md`，未触碰代码/配置。
