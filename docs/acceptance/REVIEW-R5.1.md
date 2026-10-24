# 独立对抗性审查报告 — R5.1 容器与 TLS 可靠性（REVIEW-R5.1）

- 审查时间：`2026-10-25T00:06–00:26`（Asia/Shanghai）
- 审查者：独立 reviewer（**未参与实现**；本轮只新增本文件，未改代码/配置/他人文档）
- 审查对象：`9eed291`（base `19dd248`），即"TLS ingress 验收 + 单写实例锁 + Docker 就绪脚本"提交
- 环境：macOS arm64、Go 1.25.3（cgo `mattn/go-sqlite3`）；真实 Keycloak 26.8.1 + Mailpit v1.32.0
  夹具（本轮由我 `tests/sandbox/up.sh` 拉起、审查结束 `down.sh` 停回）；**Docker daemon 不可用**（`docker info` → 1）
- **结论：R5.1 = PASS（本轮已执行范围）；P0 = 0，P1 = 0，P2 = 4，P3 = 9。**
  - TLS ingress 证据**真实**（无 `-k`/`--insecure`/skip-verify；全部 `--cacert`；错误主机名/弱协议/头注入均按失败断言）；
  - Docker 容器构建/运行/容器级故障注入 **确为 NOT_RUN**（daemon 不可用、无镜像/digest、脚本 exit 3），**未被写成 PASS**；
  - 单写锁实现正确（第二进程被拒、SIGKILL 后释放、权限 0600、`SkipInstanceLock` 不可由配置触达）；
  - 未发现发布阻断级问题；4 项 P2 均为"门禁脚本/测试 ingress/文档/部署顺序"级别，不改变 `DEPLOYMENT_BLOCKED` 判定。
- 冻结：HEAD `9eed29135b0be788d5fcfbb86757297bb446ab89`；
  `git ls-tree -r HEAD | sha256sum` = `88a84a3879a2e86b0904317bf370f36084c6dcbde2074c927b90d67837cef123`（详见 §8）。
  ⚠️ 审查期间工作树出现**非我产生的**未提交修改 `docs/acceptance/R5.1.md`（Lead 于 00:13 补了 §4.2 两行进程级证据、修正 §8 的 HEAD 表述）；
  我的冻结以 **HEAD 树**为准，并在 §7 区分"已提交版 / 工作树版"。

> 边界：未 push/merge/deploy、未 start Docker/安装软件/改宿主机配置、未调用真实 Cloudflare/GitHub/WorkBuddy，
> 未修改系统信任库；测试 CA/证书仅落在 gitignored 的 `.sandbox/r51/`。审查结束时无遗留监听进程。

---

## 1. 判定与问题表

### 1.1 P0 / P1

**无 P0，无 P1。** 未发现默认配置可触发的安全缺陷或发布阻断问题；R5.1 的两项主交付
（隔离 loopback TLS 验收、单写实例锁）经我独立复现成立。

### 1.2 P2（非阻塞，但建议在下一轮前处理）

| 编号 | 级别 | 文件:行 | 复现 | 影响 | 建议修复 | 回归测试 |
|---|---|---|---|---|---|---|
| **P2-1** | P2（门禁脚本正确性） | [`tests/r51/docker_acceptance.sh:103-105`](../../tests/r51/docker_acceptance.sh) | 脚本按**位置参数**调用备份：`bash scripts/backup.sh --encrypt "$WORK/data/state.sqlite" "$WORK/backup"`。实测同样调用 → **exit 2**（`backup: unknown argument: …`）；`backup.sh` 只接受 `--db/--out-dir/--encrypt/--keyring`。当前因无 daemon 脚本在 exit 3 处提前返回，故未暴露 | 一旦 daemon 可用，这个"准备好的容器验收门禁"会在"加密备份"步骤 **FAIL**（`bad "encrypted backup"`，最终 exit 1）。它不会假 PASS（会响亮失败），但会阻断 R5.2 容器收口，且说明该脚本从未真正被执行过 | 改为 `bash scripts/backup.sh --db "$WORK/data/state.sqlite" --out-dir "$WORK/backup" --encrypt --keyring "$WORK/secrets/master.key"`；顺带 `mkdir -p "$WORK/backup"`。可在无 daemon 环境先跑该命令做本地 dry-run 作为回归 | 新增/执行"无 daemon 时仍能验证备份步骤参数"的最小检查（或 CI 里跑一次本地 DB 的 backup+verify） |
| **P2-2** | P2（测试 ingress 硬化） | [`tests/r51/ingress/main.go:145-153`](../../tests/r51/ingress/main.go) | 明文端口 308 跳转直接使用客户端 `Host`：`curl -H "Host: evil.example" http://127.0.0.1:18080/healthz` → `308 https://evil.example:<tlsport>/healthz`（我的探针 `FAIL` 即此项；`R5.1-tls.log` 只测了 `localhost`） | 这是**测试用 ingress**（不进生产镜像/不是产品代码），影响有限：开放重定向目标等于攻击者自选的 Host，无法借此把受害者从"可信域"跳到攻击域；但它是 TLS 验收工件，模式被复制到生产反代会成为真实问题 | 在 `newRedirectHandler` 里对 Host 做 allowlist（`localhost`/`127.0.0.1`/`[::1]` + 可选 `-allowed-host` 参数），不匹配则 400；生产 ingress 也必须拒绝未知 Host | 单测加"未知 Host 不生成跳转（400）"；README/R5.1 §2.4 注明测试 ingress 的 Host 约束 |
| **P2-3** | P2（部署语义/文档） | [`internal/storage/store.go:102-108`](../../internal/storage/store.go)、[`internal/storage/lock_unix.go:19-42`](../../internal/storage/lock_unix.go)、[`docs/acceptance/R5.1.md`](../../docs/acceptance/R5.1.md) §4.1/§6 | ① 锁只由 **R5.1+ 二进制**获取：`git show 19dd248:internal/storage/store.go` 中 `instanceLock` 0 处，故"旧镜像 + 新镜像"滚动升级会短暂出现**双写**；② 服务运行时执行 `onboardd migrate` 现在会因锁被拒（我的探针 NOTE 已复现：`single-writer` 错误、exit 1）。R5.1 文档未写这两条运维语义 | 单写约束是**协作式**的：任何不调用 `storage.Open` 的写入者（旧版 onboardd、sqlite3 CLI、运维脚本）都不受锁约束；发布/升级顺序若写成"先起新再停旧"会破坏单写承诺。部署被阻断时风险有限，但属于必须写清的部署前置 | 在 R5.1 §4.1/§6 与部署文档写明：升级必须**先停旧实例再起新实例**；`migrate` 前必须停服务（或提供 `--offline` 说明）；明确锁仅对同版本+同一本地卷生效 | 文档核对；可选：启动时记录 schema/version，升级 runbook 加检查项 |
| **P2-4** | P2（文档/证据准确性） | [`docs/acceptance/R5.1.md:29`](../../docs/acceptance/R5.1.md) | §1 表格写 `docker compose -p r51-probe -f deploy/compose.yaml -f tests/r51/compose.override.yaml config` → **exit 0**；按字面执行（无 `R51_*` 环境变量）实测 **exit 1**（缺 `R51_ADMIN_TOKEN`/`R51_MASTER_KEY`/`R51_IMAGE`/`R51_DATA`）。设置这四个变量后 → **exit 0**（我复现：`127.0.0.1:28788`、`read_only`、`user 10001:10001`）。证据日志只记 `OVERRIDE_CONFIG_EXIT=0`，未记录命令与 env | 门禁表格里的 exit code 按其给出的命令不可复现；审计者无法从证据日志还原实际命令。属"发布报告命令不实"（轻微），会削弱可复现性 | 文档写出完整可复制命令（含 `R51_*` 导出/临时文件准备），或提供 `tests/r51/compose_probe.sh` 封装；证据日志记录命令 + env 来源 | 复跑文档命令得到 0；证据日志包含命令行 |

> P2-1/P2-4 都是"下一步要用"的东西：修 P2-1 再执行容器门禁，修 P2-4 再引用 exit code。

### 1.3 P3（杂项，不阻塞）

| 编号 | 文件:行 | 内容 |
|---|---|---|
| P3-1 | `docs/acceptance/R5.1.md:3,6`（HEAD 版） | 报告时间只写 `2026-10-24T`（无时分/时区）；"最终代码 HEAD：见文末 §8（提交后填写）"、§8 还写"代码提交见 `git log -1` 之前"——实际代码+文档是**同一提交** `9eed291`。工作树里 Lead 已修正 §8（未提交）；时间戳仍不完整 |
| P3-2 | `docs/acceptance/evidence/R5.1-tls.log` | 证据里混入 shell job-control 行 `Killed: 9 …`（脚本 SIGKILL 步骤的 stderr），非敏感但属噪声；建议脚本内 `disown`/重定向或证据生成时过滤 |
| P3-3 | `.gitignore`（无 `*.lock`） | DB 放在仓库内（如 `ONBOARD_DB_PATH=$PWD/x.sqlite`）会留下未跟踪 `<db>.lock`（实测 `?? r51-locktest.sqlite.lock`，0600）。建议补 `*.sqlite.lock` 或 `*.lock` |
| P3-4 | [`tests/r51/ingress/main_test.go:85`](../../tests/r51/ingress/main_test.go) | `if !strings.Contains("ok", "ok") { … }` 是无意义断言（只为保留 `strings` import）；删除并去掉 import 即可 |
| P3-5 | [`tests/r51/failure_modes.sh:21`](../../tests/r51/failure_modes.sh) | `env_for()` 定义后未使用（dead code） |
| P3-6 | [`tests/r51/docker_acceptance.sh:56`](../../tests/r51/docker_acceptance.sh) | `-e ONBOARD_DOMAIN=moonbyte.test`（单数）不是配置项（config 只读 `ONBOARD_DOMAINS`）；无害 typo，建议删除或改复数 |
| P3-7 | [`internal/storage/store.go:59-62`](../../internal/storage/store.go) | `SkipInstanceLock` 无任何调用者，注释却说"给只读工具/迁移测试使用"——目前是未使用 API + 误导性注释；若保留，建议补一个只读工具用例或改为测试内部选项 |
| P3-8 | [`internal/storage/lock_windows.go`](../../internal/storage/lock_windows.go) | Windows 上每次 `storage.Open` 都返回 "not supported" 错误（显式 fail-closed，方向正确），但 R5.1 残余风险表/README 未写"Windows 不受支持"；建议补一句 |
| P3-9 | 残余风险口径 | 单写锁是 advisory 锁：有 data 目录写权限的人删除 `<db>.lock` 后仍可启动第二实例（与"能写 DB 就能改状态"同一信任边界）。R5.1 已写 NFS 语义风险，建议把"锁文件可被有权限者删除/绕过、旧版本不参与加锁"也列进 §6 |

---

## 2. 独立执行的命令与真实退出码（HEAD `9eed291`）

| # | 命令 | 退出码 | 结果摘要 |
|---|---|---:|---|
| 1 | `git rev-parse HEAD`；`git status --porcelain` | 0 | `9eed291…`；开始时干净（审查中 Lead 改了 `R5.1.md`，见 §7） |
| 2 | `make verify`（fmt + vet + 全量 `-race` + build） | **0** | 29 包；`tests/r51/ingress` 也在其中（`ok`） |
| 3 | `go test ./... -race -count=1` | **0** | 29 包，14 个 `ok`、15 个 no-test |
| 4 | 计数：`go test ./... -list '.*'` / `-v` | 0 | **315 顶层（310 PASS + 5 SKIP）/ 624 PASS 含子测试（629 RUN）/ 0 FAIL** —— 与 FINAL-REPORT 完全一致 |
| 5 | `bash tests/r51/tls_acceptance.sh` | **0** | **25/25 PASS**（我自己复跑；非自证：含错误主机名、TLS1.1/1.0、伪造头、CSRF/Origin、SIGKILL 重启重放） |
| 6 | `bash tests/r51/failure_modes.sh` | **0** | **7/7 PASS**（第二实例拒绝、SIGKILL 释放锁、缺 master key fail-closed、缺撤销表 readyz 503 + cookie fail-closed） |
| 7 | `go test ./internal/storage/ -run TestSingleWriterInstanceLock -count=1` | **0** | PASS（第二 Store 被拒 + Close 后可再开） |
| 8 | `ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1 -timeout 900s`（真实 Keycloak+Mailpit） | **0** | **4/4 PASS**（我跑 18.217s；提交证据 `R5.1-e2e.log` 14.257s） |
| 9 | `docker info` / `docker build -t account-onboarding:r51-probe .` / `docker images` / `docker ps -a` | **1 / 1 / 1 / 1** | 全部 `Cannot connect to the Docker daemon` → 容器验收 NOT_RUN |
| 10 | `docker compose -f deploy/compose.yaml config` | **0** | 仅静态：回环 8788、非 root、read_only、cap_drop ALL |
| 11 | `docker compose -p r51-probe -f deploy/compose.yaml -f tests/r51/compose.override.yaml config` | **1**（无 env） / **0**（设 `R51_*` 后） | 见 P2-4：文档命令按字面不可复现 |
| 12 | `bash tests/r51/docker_acceptance.sh` | **3** | `NOT_RUN: Docker daemon is unavailable…`（未构建/未启动容器，未改宿主机配置） |
| 13 | 我的 TLS/lock 对抗探针 `.sandbox/review/r51-tls-probe.sh` | 脚本 1（15 PASS / 1 FAIL） | 唯一 FAIL = P2-2 开放重定向；其余：TLS1.1/1.0 按**协议协商**被拒、IP SAN 校验、ingress 覆盖客户端 XFP、直连 app 的 XFP 信任边界、ingress listen/upstream 非回环拒绝、锁文件 0600/pid、第二进程拒绝、SIGKILL 释放、migrate 被拒 |
| 14 | `bash scripts/backup.sh --encrypt <db> <outdir>`（复现 docker_acceptance 的调用） | **2** | `backup: unknown argument: …` → P2-1 |
| 15 | `bash scripts/redact_scan.sh --file` × 5 份 R5.1 evidence | **0/0/0/0/0** | 0 findings；另 grep 无 `BEGIN … PRIVATE KEY`/cookie/`set-cookie` |
| 16 | `git ls-files \| grep -iE '\.pem$|\.key$|\.crt$|\.p12$'` | 0（grep 无匹配） | 仓库**未提交**任何证书/私钥；测试 CA/key 只在 `.sandbox/r51/`（gitignored，key 0600 由 tlsgen 写入） |

---

## 3. TLS 证据真实性核验（核心）

### 3.1 "不是自证"的检查

- **没有关闭校验**：`grep -nE '\-k\b|--insecure|InsecureSkipVerify|skip.?verify' tests/r51/*.sh tests/r51/*/*.go` → **0 命中**；
  `tls_acceptance.sh` 中 `--cacert "$CA"` 出现 **14** 次，所有 HTTPS 请求都带测试 CA。
- **证书/主机名**：`openssl verify -CAfile`；错误 hostname 用 `--resolve wrong.invalid:18443:127.0.0.1` → curl **exit 35**；
  我另测 `https://127.0.0.1:18443`（SAN 含 IP）→ 200。
- **协议下限**：我用 `openssl s_client -tls1_1` 复跑，先区分"协议被拒"与"openssl 不支持该选项"——
  输出命中 `no protocols available/alert protocol version`（exit 1），且 `-tls1` 同样被拒；
  正常协商为 TLSv1.3。**不是**"命令跑错导致的假 PASS"。
- **断言确实读响应**：脚本直接 parse `set-cookie` 头断言 `Secure`/`HttpOnly`/`SameSite=Strict`；
  `curl -w '%{http_code}'` 断言 403/303；同源 Origin 用真实 `$BASE_HTTPS`；跨源用 `https://evil.example`；
  SIGKILL 后**显式 `Cookie:` 头**重放旧值并断言 `303 /ui/login`（阳性对照：登出前 dashboard 200）。
  我复跑输出与证据日志逐行一致（25/25）。
- **头注入防线**：`tests/r51/ingress/main_test.go` 的单测捕获 upstream 实际收到的头：
  `Forwarded` 为空、`X-Forwarded-For`/`X-Real-Ip` 被覆盖为真实远端、`X-Forwarded-Host` 被覆盖、upstream Host 保留为浏览器可见 Host、
  `X-Forwarded-Proto=https`；`ReverseProxy.Rewrite` 先 `Del` 全部客户端转发头再 `Set` 可信值。
  我通过真实 ingress 再发 `X-Forwarded-Proto: http` + `X-Forwarded-For: 6.6.6.6` 登录 → `Set-Cookie` 仍 `Secure`。
- **信任边界**（Lead 要求考虑的直连攻击）：`TRUST_PROXY_HEADERS=true` 时**直连 app 端口**伪造 `X-Forwarded-Proto: https` 会被信任
  （我实测：明文直连也能拿到 Secure Cookie）。这是**配置契约**而非缺陷：`ONBOARD_LISTEN` 只允许回环、compose 只发布
  `127.0.0.1`，R5.1 §2.4 明确"仅应部署在只有反代可达 app 端口的拓扑"。**边界描述属实**，但见 P2-2/P3-9（测试 ingress 自身可被伪造 Host）。
- **HTTP→HTTPS**：正常请求 `localhost:18080` → 308 `https://localhost:18443/healthz` ✓；**伪造 Host** → 308 到 `https://evil.example:…`（P2-2）。
- **生产 TLS 边界**：测试 ingress 无 ACME/证书轮换/HSTS/mTLS/WAF/DNS，`R5.1.md` §2.4 已明说"不是生产 ingress"——**未夸大**（我实测响应无 HSTS，与文档一致）。
- **不可信 XFP 关闭信任时**：`TRUST_PROXY_HEADERS=false` + 明文 HTTP + `X-Forwarded-Proto: https` → Cookie **无 Secure** ✓（真实进程测试，非 mock）。

### 3.2 结论

TLS 验收是**真实执行、真实验证**（测试 CA + 主机名 + 协议 + 头 + CSRF/Origin + 崩溃重启），
唯一发现的实质问题是测试 ingress 的 Host 跳转未做 allowlist（P2-2，非产品代码）。

---

## 4. 单写实例锁核验

| 检查 | 结果 |
|---|---|
| 实现 | `lock_unix.go`：`<db>.lock` 上 `flock(LOCK_EX|LOCK_NB)`；`O_CREATE|O_RDWR`、**0600**；写 `pid=` 便于诊断；`release()` 先 `LOCK_UN` 再 `Close` |
| 生命周期 | `store.Open` 在 `MkdirAll` 后、开 DB 前加锁；**所有** Open 错误路径调用 `releaseLock()`；迁移失败 `s.Close()`（`store.go:154-156`）→ 释放；`Store.Close()` 释放（`store.go:179`） |
| 真实第二进程 | 同一 DB 起第二个 `bin/onboardd` → **exit 1**，日志含 `single-writer`（我手工复现；`failure_modes.sh` 亦覆盖） |
| SIGKILL 释放 | `kill -9` 后在**同一 DB** 重启新实例 → 健康（我复现；E2E 崩溃重启 4/4 也经过该路径） |
| 锁文件 | 路径 `<db>.lock`、mode **0600**、内容 `pid=…`（实测） |
| 同进程二次 Open | 单测 `TestSingleWriterInstanceLock` 证明第二 Store 被拒，Close 后可再开（flock 是 open-file-description 语义，同进程不同 fd 也会冲突） |
| `SkipInstanceLock` 可配置性 | `grep -rn "SkipInstanceLock"` 仅 `store.go`；`internal/config` 无对应 env；**不可由配置/API 触达**；但它当前**无任何调用者**（P3-7） |
| CLI `migrate` | 服务运行时执行被拒（`single-writer`，exit 1）——**新行为**，R5.1 未写（P2-3） |
| 旧二进制 | `git show 19dd248:internal/storage/store.go` 中 `instanceLock` 0 处 → **旧版不参与加锁**；"旧+新"滚动升级存在双写窗口（P2-3） |
| 网络文件系统 | flock 在 NFS 语义不保证；R5.1 §6 已列为残余风险 ✓；容器跨 bind mount 的 flock 行为**未实测**（NOT_RUN，文档已注明） |
| Windows | `lock_windows.go` 直接返回错误（显式不支持）；R5.1 未列（P3-8） |
| advisory 锁绕过 | 有 data 目录写权限者可删除 `<db>.lock` 后启动第二实例（与 SQLite 信任边界同级）；建议写进 §6（P3-9） |

**结论：锁实现与测试正确，没有"假单写"；主要是使用边界/升级顺序需要在文档中收紧。**

---

## 5. Docker 诚实性核验

- `docker info` / `docker build` / `docker images` / `docker ps -a` 全部 **exit 1**（daemon 不可用）；
  `docker_acceptance.sh` 未进入任何构建/运行分支，输出 `NOT_RUN`（**exit 3**）。
  因此"无镜像、无 digest"与实测一致；daemon 不可用时我**无法枚举镜像存储**来独立排他证明"不存在旧镜像"，
  这一点在报告里如实标注为"不可独立枚举，但构建从未成功"。
- **唯一的容器证据是静态的**：`docker compose -f deploy/compose.yaml config` exit 0（我复现：回环 8788、`user 10001:10001`、
  `read_only`、`cap_drop ALL`、secrets 文件挂载）。R5.1 把它标注为"静态校验"，**未写成容器 PASS** ✓。
- **隔离脚本**：`docker_acceptance.sh` 使用唯一 `PROJECT`/`IMAGE` 名、临时 data/secrets、回环 28788、`cleanup` trap
  只删自己的容器/卷/镜像/项目，无 `system prune`、无全局 `down`、无宿主机配置修改 ✓。
- **一个真实缺陷**：脚本里的加密备份步骤调用方式错误（P2-1），daemon 可用时会在该步 FAIL。
- 镜像内容：Dockerfile 最终阶段只 `COPY --from=build /out/onboardd`（非 root 10001、`ENTRYPOINT`），
  `tests/r51/*` 只在 builder 阶段的 build context 里，**不会进入生产镜像** ✓（"不过度打包"这一点成立）。

---

## 6. 可靠性回归

- `make verify` **0**、`go test ./... -race` **0**；计数 **315 顶层（310 PASS + 5 SKIP）/ 624 含子测试 / 0 FAIL**，与
  `FINAL-REPORT.md` 的 R5.1 计数**逐字一致**。
- `tests/r51/failure_modes.sh` **7/7 PASS**：第二实例拒绝、SIGKILL 释放锁、缺 master key fail-closed、
  缺撤销表 `/readyz 503` + cookie fail-closed（与 R5 的 P2-2 修复联动，行为一致）。
- 真实沙箱 E2E **4/4 PASS**（我复跑 18.217s，提交证据 14.257s；`TestCrashRecoveryRestartsInWaitingEmail`、
  `TestFiveSandboxIdentities` 等均 PASS）——**单写锁没有回归崩溃重启**（SIGKILL 后内核释放锁，新进程正常接管）。
- TLS 下 SIGKILL→重启→旧 Cookie 仍被拒（脚本第 5 段）✓，说明锁/迁移/撤销三者在重启路径上兼容。

---

## 7. 文档准确性与秘密检查

| 项 | 核对 |
|---|---|
| 计数 315/624 | 实测**完全一致**（310 PASS + 5 SKIP；624 PASS incl. subtests） |
| TLS 25/25、failure 7/7、E2E 4/4（14.26s） | 与提交证据逐项一致（我复跑亦 25/25、7/7、4/4） |
| Docker exit 1 / NOT_RUN / 无 digest | 与实测一致；脚本 exit 3 一致；**未**把静态检查写成容器 PASS |
| "TLS ingress ≠ 生产 ingress" | 文档明确列出 ACME/HSTS/mTLS/WAF/DNS 未做（§2.4），我实测无 HSTS → **未夸大** |
| 命令可复现性 | P2-4：§1 的 override 命令按字面 exit 1（需 `R51_*`）；其余命令（make verify / tls / failure / compose config / docker probe）可复现 |
| 时间/HEAD | HEAD 版报告时间不完整、§8 有"提交后填写"占位；实际代码+文档同属 `9eed291`。**工作树（未提交）Lead 已修 §8**，但时间戳仍不完整（P3-1） |
| 秘密 | 5 份 evidence `redact_scan` 0 findings；无 `BEGIN … PRIVATE KEY`/`set-cookie: onboard_session`/admin token 字面量；仓库无 `.pem/.key/.crt/.p12`；测试 CA/key 仅 `.sandbox/r51/`（gitignored，key 0600）；`ONBOARD_MASTER_KEY_FILE`/token 全走临时文件 |
| 未提交变更 | 审查期间出现了 **Lead 对 `R5.1.md` 的未提交修改**（新增 §4.2 两行进程级证据 + 修正 §8）；不是我的改动，我已核对新增两行引用的测试存在且 E2E PASS（`TestCrashRecoveryRestartsInWaitingEmail`、`TestIdempotencyKeyCreatesOneRun` 均在 `tests/e2e` 通过包内） |

---

## 8. 冻结与哈希

| 项 | 值 |
|---|---|
| HEAD | `9eed29135b0be788d5fcfbb86757297bb446ab89` |
| base | `19dd248` |
| HEAD 树清单 | `git ls-tree -r HEAD \| sha256sum` = `88a84a3879a2e86b0904317bf370f36084c6dcbde2074c927b90d67837cef123` |
| 已提交 `docs/acceptance/R5.1.md` | `3a1fbce37be453d0b328ffa7bfbfbe2d040a2b0a` |
| 工作树 `docs/acceptance/R5.1.md`（Lead 未提交修改） | `5460728587dc640297509425fef7934599ad0a5f` |
| `internal/storage/lock_unix.go` | `6a497e2d77624a57af0b5079cdbab27f18351788` |
| `internal/storage/lock_windows.go` | `3fd743693bd22c7ed041517d920aa8bedc102abc` |
| `internal/storage/store.go` | `2ce2f6bb620b63511d1a36abddef406de29fa9f2` |
| `internal/storage/sessions_test.go` | `965ef3bc059b5bc504cf3a4a48c4b475184e2f12` |
| `tests/r51/tls_acceptance.sh` | `9d2f52a9565e35e60f78d1171d28c8364faaef42` |
| `tests/r51/failure_modes.sh` | `87de028320b06319be2b2f4105ecf2122414bb51` |
| `tests/r51/docker_acceptance.sh` | `299afcc71195147009602c8ca37ced8074ae06bd` |
| `tests/r51/compose.override.yaml` | `fa5a75e1aba0309fa10fac19b902dde572c41b64` |
| `tests/r51/ingress/main.go` | `ddd8e6b290659652ac8467b976feffe5aab3bb48` |
| `tests/r51/ingress/main_test.go` | `841a7d6cc95d6f4b6d6614f4ce90dc81756211f9` |
| `tests/r51/tlsgen/main.go` | `9f191ff3c2755fecc286e170310133f22ba0962a` |
| `docs/acceptance/FINAL-REPORT.md` | `6920ebe3273996469fa817e3610b2f71aea80015` |
| `README.md` / `docs/acceptance/index.md` | `740696e6…` / `25581248…` |
| evidence | `R5.1-tls.log 90db8696…`、`R5.1-docker-probe.log 10c96869…`、`R5.1-docker-acceptance.log 9c25af6c…`、`R5.1-failure-modes.log 483813de…`、`R5.1-e2e.log 6ac2f299…` |
| 证据目录 | `.sandbox/review/logs/r51-gates.log`、`r51-tls-rerun.log`、`r51-failure-rerun.log`、`r51-e2e-rerun.log`、`r51-tls-probe.log`、`r51-sandbox-up.log` |

> 注：工作树在冻结时**不是干净的**——`M docs/acceptance/R5.1.md`（Lead 的未提交修改）+ 本文件（新增未跟踪）。
> 上面的树清单是 HEAD 树的哈希；若 Lead 把该文档修改提交，最终 HEAD 会前进，需重新冻结。

---

## 9. 结论：`DEPLOYMENT_BLOCKED` 是否成立？

**成立，且必须保持。** 逐项判定：

| 判定 | 是否成立 | 依据 |
|---|---|---|
| **TLS ingress PASS** | **成立（限"隔离 loopback 测试 ingress"）** | 我独立复跑 25/25：真实测试 CA + 主机名校验（错误 host 被拒）+ TLS1.2 下限（1.1/1.0 被拒）+ HTTP 308 + Secure/HttpOnly/SameSite + CSRF/Origin 403 + SIGKILL 后重放拒绝 + 客户端转发头被剥离/覆盖 + 非回环 listen/upstream 拒绝。无 `-k`/skip-verify。 |
| **Docker/容器 NOT_RUN** | **成立** | `docker info/build/images/ps` 全 exit 1；无守护进程即无镜像/digest；`docker_acceptance.sh` exit **3**；唯一容器证据是 `compose config` 静态 exit 0。**没有**把静态检查/进程级测试写成容器 PASS。 |
| **不是 `CONTAINER_RC_READY`** | **成立** | 未构建镜像、未起容器、未做容器级健康检查/非 root/只读/卷持久化/SIGKILL/重建/故障注入；且容器门禁脚本自身有 P2-1，修复前不能执行。 |
| **不是 `LIVE_READY`** | **成立** | 无任何真实 Cloudflare/GitHub/WorkBuddy 成功证据；TLS 只到测试 ingress，生产反代/域名/HSTS/证书轮换均未做。 |
| **R5.1 = PASS（本轮已执行范围）** | **成立** | TLS + 单写锁 + 进程级可靠性均可复现；无 P0/P1。**但**：P2-1 必须先修再跑容器门禁；P2-2 修前不要把该 ingress 模式复制到生产；P2-3 的升级顺序必须写进部署文档。 |

**未发现任何声明被实质夸大**：R5.1 对 "测试 ingress 非生产"、"Docker 全部 NOT_RUN"、"无镜像/digest"、
"容器级故障注入 NOT_RUN（进程级仅支持性证据）"均有明确限定，与实测一致。

**残余/供应链风险（供下一阶段）**：基础镜像仅 tag 未 pin digest、无签名/SBOM/漏洞扫描；生产 ingress（ACME/HSTS/mTLS/WAF/DNS）未落地；
容器内非 root 写 bind-mount 的 UID 映射与跨容器 flock 语义未实测；单写锁为协作式 advisory 锁（旧版本不参与、锁文件可被有权限者删除）；
NFS/网络卷不支持；Windows 不支持。本轮无新增依赖（`go.mod`/`go.sum` 未变），供应链面未扩大。
