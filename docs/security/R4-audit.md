# R4 安全与可靠性审计报告 — account-onboarding

- 审计时间：2026-10-24T13:10Z–13:30Z（本地 21:10–21:30 CST / Asia/Shanghai）
- 审计者：r4-reliability（reliability & security engineer）
- 范围：R4 安全闭环（备份/恢复/密钥轮换、CSRF/CSP/Origin/401-403、日志与事件敏感字段、SSRF、Docker 部署清单）
- 本版为 **R4b 收口版 + R4c 后审收口**（task-6 2026-10-24T13:35Z–13:45Z；task-7 2026-10-24T14:10Z–14:25Z）：RBAC（最小双角色）、请求速率限制、快照静态加密（AES-256-GCM）已实现；R4c 修复敏感扫描长路径误报并新增部署 preflight。
- 结论：**核心控制 PASS；仍开放的 P1 为 TLS ingress（NOT_RUN）与轮换/恢复停机编排；Docker build/run、GHCR、真实平台 live 为 NOT_RUN。** 本报告中的 PASS 均对应真实执行的命令或测试，无 PASS 包装。

审计对象是仓库当前代码，而非设计文档自述。Lead 正在并行修改 `internal/api/**`、`internal/workflow/**`、`internal/provider/**`、`cmd/**`；
本报告只读取这些文件，未修改。R4 新代码全部位于 `internal/ops/**`、`scripts/**`、`deploy/**`、`docs/security/**`、`docs/acceptance/R4.md`。

---

## 1. 环境与证据口径

| 项 | 值 |
|---|---|
| 工作目录 | `/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding` |
| OS / Arch | Darwin arm64 (macOS) |
| Go | `go version go1.25.3 darwin/arm64`；`CGO_ENABLED=1`（mattn/go-sqlite3） |
| SQLite | 服务内嵌 mattn/go-sqlite3 v1.14.32；`/usr/bin/sqlite3` 仅用于人工核对 |
| Docker | CLI 存在（Docker 29.7.2 / Compose v5.5.0），**daemon 未运行**：`docker info` 退出码 1 |
| 网络 | 所有测试/脚本仅使用 loopback 或纯本地文件；无生产凭据、无 git push、无外部 provider 调用 |
| 证据目录 | `/tmp/r4-evidence/**`（本机临时目录，不属于仓库；文档中的路径即真实执行路径） |
| 新增代码 | `internal/ops/**`（含 `cmd/*`）、`scripts/*.sh`、`deploy/**`、`docs/security/R4-audit.md`、`docs/acceptance/R4.md` |

PASS/PARTIAL/FAIL/NOT_RUN 定义：

- **PASS**：控制存在，且本轮真实运行了对应测试/命令并得到预期结果。
- **PARTIAL**：控制存在但覆盖范围有限，或依赖尚未完成的外部环节。
- **FAIL**：R4 要求存在但代码中未实现，或真实运行结果不符合预期。
- **NOT_RUN**：环境/权限不允许真实执行（如 Docker daemon、TLS 终止代理、真实平台）。

---

## 2. 威胁模型 → 控制映射（docs/07）

| 资产（docs/07） | 威胁 | 实现控制 | 证据 | 状态 |
|---|---|---|---|---|
| Cloudflare Admin 密码 | 管理 API 被恶意调用 | 只从 secret 文件读取；adapter 只向 allowlist host 发送 `x-admin-auth`；跨源 redirect 拒绝 | `internal/config/config.go:572`、`internal/provider/mailbox/cloudflare/cloudflare.go:566,687`；SSRF 表 §6 | PASS（限流已闭环，§3.20） |
| Address JWT | 任意读取邮箱邮件 | AES-256-GCM 密文存储、AAD 绑定 kind+id+version、日志 redactor、event payload allowlist | `internal/security/crypto.go:154,176`、`internal/security/redact.go:95`；§5、§7 | PASS |
| GitHub 授权令牌 | 越权操作账号 | 仅官方 `api.github.com`、无 userinfo/query/path、Bearer 只发往 allowlist host、token 经 SecretRef 读取 | `internal/provider/github/config.go:129`、`internal/provider/github/github.go:76,408` | PASS |
| OAuth state | CSRF/replay/state 泄露 | 随机 state、single-use（网关侧）、URL 以 SecretRef 保存、不写日志；Authorization URL 不在日志/事件中 | `internal/provider/workbuddy/workbuddy.go:598`、`internal/workflow/steps.go` OAuth 分支；`TestScanCleanArtifacts` | PASS（URL host 未 pin，P2-1） |
| WorkBuddy 会话 | 账号泄露/越权 | 真实 adapter 默认 disabled；凭据留在原网关，本系统只存 opaque ref；base URL 仅 loopback/RFC1918 | `internal/provider/workbuddy/config.go:175`、`internal/provider/boot/live.go` | PASS |
| 任务数据库 | 数据丢失/重放 | WAL + VACUUM INTO 在线快照 + sha256/manifest + integrity_check；lease fencing | `internal/ops/backup.go`、`internal/ops/verify.go`、`internal/storage/store.go:304`；§4 | PASS |
| 控制台 | 非授权操作 | loopback 默认、单 operator token、cookie 会话 + CSRF + Origin、CSP/XFO/nosniff、审计日志 | `internal/api/server.go:177–370`、`internal/api/ratelimit.go`；§3 | PASS（最小双角色；TLS ingress 另见 NOT_RUN） |
| E2E 浏览器 | 真实 Cookie 泄露 | 独立 sandbox profile（R2 工作流范围） | 本报告未执行 | NOT_RUN |

---

## 3. HTTP 安全控制审计（httptest 针对真实 `api.Server`）

审计方式：`internal/ops/httpaudit_test.go` 用真实 `api.Server.Handler()` + 真实 storage/workflow/security（仅 provider 为内存 fake），
`httptest.NewServer` 绑定 127.0.0.1 随机端口，不跟随 redirect。**未修改 API 代码。**

| # | 控制 | 结果 | 证据（测试 / 代码） | 说明 |
|---|---|---|---|---|
| 3.1 | 安全响应头：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`COOP: same-origin` | PASS | `TestHTTPHardeningHeadersAndCSP`；`server.go:177–192` | 所有响应（含 JSON API）均带 |
| 3.2 | CSP 仅作用于浏览器渲染路由 | PASS（范围限定） | `TestHTTPHardeningHeadersAndCSP`；`server.go:185–190` | `/` 与 `/ui/*` 带严格 CSP（`default-src 'none'; frame-ancestors 'none'` 等）；JSON API 不带 CSP（对 JSON 无执行语义）；见 P2-4 |
| 3.3 | `Cache-Control: no-store` | PASS | 同上 + `writeJSON`；`server.go:378` | UI 与 API JSON 均 no-store |
| 3.4 | cookie 会话 CSRF：缺失/错误 token → 403 | PASS | `TestHTTPCookieCSRFAndOrigin`；`server.go:269` | 错误 class = `NOT_AUTHORIZED` |
| 3.5 | Origin 不匹配的 cookie POST → 403 | PASS | `TestHTTPCookieCSRFAndOrigin`；`server.go:283` | `Origin: https://evil.example` 被拒绝；同源 Origin + CSRF → 202 |
| 3.6 | 无 Origin 的 cookie POST（mutation） | PASS | `server.go:283–301`；`TestHTTPCookieCSRFAndOrigin` | cookie 认证的 mutation 必须带 Origin 或同 host Referer，两者缺失 → 403；读取请求不受影响 |
| 3.7 | bearer 认证路径 | PASS | `TestHTTPBearerAuthNearMisses`；`server.go:245–257` | 空/错误/前缀/大小写/截短 token 全部 401；`WWW-Authenticate: Bearer realm="onboard"` |
| 3.8 | 认证比较为常量时间 | PASS（静态 + 功能） | `security.ConstantTimeEqual`（SHA-256 后 `subtle.ConstantTimeCompare`）；`crypto.go:238`；`server.go:248–252`；`TestHTTPBearerAuthNearMisses` | 无长度/前缀短路；近失配 token 全部拒绝 |
| 3.9 | 401（未认证） vs 403（已认证但无授权/CSRF）语义 | PASS | `TestHTTPUnauthenticatedReturns401`、`TestHTTPAuthNVsAuthZ` | 未认证 = 401 `AUTH_REQUIRED`；disabled provider = 403 `NOT_AUTHORIZED`；CSRF 失败 = 403 |
| 3.10 | 请求体上限 64 KiB | PASS | `TestHTTPBodyLimitsAndJSONStrictness`；`server.go:431` | 70 KiB body → 400，未进入业务逻辑 |
| 3.11 | 未知 JSON 字段拒绝（结构化禁止批量/account_count 类扩展） | PASS | 同上；`server.go:433` `DisallowUnknownFields` | `account_count` → 400 |
| 3.12 | `Idempotency-Key` 强制 | PASS | 同上；`server.go` create 校验 | 缺失 → 400 |
| 3.13 | UI 未认证访问跳转登录 | PASS | `TestHTTPUIRedirects`；`server.go:359–372` | `/ui/`、`/ui/runs/{id}` → 303 `/ui/login` |
| 3.14 | `/` 根路径 redirect | PASS | `TestHTTPHardeningHeadersAndCSP`；`ui.go:174` | 303 → `/ui/`，且带 CSP/XFO |
| 3.15 | 未知路径 JSON 404 | PASS | `TestHTTPAuthNVsAuthZ`；`server.go:497` | 不泄露框架栈信息 |
| 3.16 | `/readyz` 鉴权 + 就绪语义 | PASS | `TestHTTPReadinessAndReadModel`；`server.go:468` | 未认证 401；认证后 200，`ready=true`、schema 2、WAL |
| 3.17 | 日志不含 token/query secret | PASS | `TestHTTPLogsAndResponsesDoNotLeakSecrets`；`server.go:148–175`、`security.SanitizeURL` | 访问日志记录 path 而非 query；bearer token 不进入日志与响应 |
| 3.18 | Cookie 属性 | PASS（限明文 loopback） | `internal/api/ui.go:215–221`、`server.go:314–321`；`TestHTTPSecureCookieFlags` | `HttpOnly`、`SameSite=Strict`、`Path=/ui`；TLS 下或 `TrustProxyHeaders=1` + `X-Forwarded-Proto=https`（可信反代）时 `Secure`；明文 loopback 不设 Secure 是预期行为 |
| 3.19 | RBAC（最小双角色） | PASS（范围受限） | `server.go:216–257`；`TestHTTPViewerRoleReadOnly` | 新增 `ONBOARD_READONLY_TOKEN`：admin 可读写，viewer 仅 GET（readyz/meta/providers/runs/events/audit），所有 mutation 403 `NOT_AUTHORIZED`；未知 token 401。**仍是单 operator 服务**：无 per-user 账号、2FA、角色管理；viewer 是共享只读令牌（限制见 P2-11） |
| 3.20 | 请求速率限制（general bucket） | PASS | `internal/api/ratelimit.go`；`TestHTTPRateLimitGeneral` | 按客户端 IP 的 60s 固定窗口，默认 300/min（0 禁用）；超限 429 + `Retry-After`（≥1s）+ JSON class `RATE_LIMITED`；覆盖整个控制面（含 `/healthz`） |
| 3.21 | TLS ingress | **NOT_RUN** | 服务与 compose 只绑定 loopback（`deploy/compose.yaml` ports `127.0.0.1:8788:8788`） | 仓库不包含 TLS 终止代理；生产 ingress 未部署/未测试（P1-2 仍开放） |
| 3.22 | 登录限流（login bucket） | PASS | `ratelimit.go`；`TestHTTPRateLimitLogin` | 仅 `POST /ui/login`，默认 10/min（0 禁用）；第 4 次（limit=3）返回 429 + Retry-After + `RATE_LIMITED`；登录失败仍写审计 |
| 3.23 | cookie mutation 的 Origin/Referer 强制 | PASS | `server.go:255–296`；`TestHTTPCookieCSRFAndOrigin` | cookie 认证的 mutation 必须带 Origin 或同 host Referer；两者都缺失 → 403；同 host Referer 可替代 Origin（浏览器总是发送其一） |
| 3.24 | 可信代理头部处理 | PASS（需显式开关） | `ratelimit.go:clientKey`、`server.go:secureCookie`；`.env.example` 警告 | 仅 `ONBOARD_TRUST_PROXY_HEADERS=1` 时信任 `X-Forwarded-For` / `X-Forwarded-Proto`；未开启时忽略（测试覆盖 untrusted 栈）；若在客户端可直连时开启会被伪造（P2-9） |
| 3.25 | viewer 授权矩阵 | PASS | `TestHTTPViewerRoleReadOnly` | GET `/readyz`、`/v1/meta`、`/v1/providers`、`/v1/runs`、`/v1/audit` = 200；POST create/resume/cancel = 403；admin 仍 202；无 token = 401 |

恶意输入回归：超长 body、未知字段、缺失幂等键、错误 Content-Type、非法/近失配 token 全部被拒；见 3.10–3.12、3.7。

---

## 4. 备份 / 恢复 / 密钥轮换审计

实现：`internal/ops/{backup,verify,restore,rotate}.go`，CLI 在 `internal/ops/cmd/*`，脚本在 `scripts/*.sh`。

| # | 控制 | 结果 | 证据 | 说明 |
|---|---|---|---|---|
| 4.1 | 在线一致性快照（WAL 活跃时不拷贝裸 `.db`） | PASS | `TestBackupSnapshotWhileWALActive`；`internal/ops/backup.go:81` | 只读连接 + `VACUUM INTO`；测试在并发写 run 的同时完成快照，`integrity_check=ok` |
| 4.2 | 快照原子发布（temp + rename） | PASS | `backup.go:150–200`；`writeFileAtomic` | 快照与 manifest 均同目录 temp→rename；失败不留半成品 |
| 4.3 | 快照权限 0600、目录 0700 | PASS | `TestBackupSnapshotWhileWALActive` | `os.Stat` 断言 mode 0600；目录 `MkdirAll(0700)` |
| 4.4 | sha256 + schema_version + created_at + 行数清单 | PASS | `Manifest` 结构；`VerifyBackup` | 清单字段：format/tool/created_at/source sha256/schema/source_journal_mode/counts |
| 4.5 | 快照自校验：只读打开 + `PRAGMA integrity_check` + `foreign_key_check` + schema_migrations 最大版本 | PASS | `inspectSnapshot`；`VerifyBackup` 检查链 | 清单/快照任一被篡改均失败（`TestVerifyBackupDetectsTamperingAndMissingManifest`） |
| 4.6 | 恢复前校验 checksum，拒绝缺失 manifest | PASS | `TestRestoreRejectsMissingManifestAndCorruptSnapshot` | 缺 manifest、快照字节被改均拒绝，目标不产生 |
| 4.7 | 拒绝覆盖已有 DB；覆盖活库需显式 force | PASS | `TestRestoreRefusesOverwriteAndLiveDatabaseWithoutForce`；`restore.go` | 无 `--force` → “already exists”；目标 == live DB → “live service database” |
| 4.8 | 恢复后 reopening：integrity_check、run/event 计数、schema 版本 | PASS | `TestRestoreRoundTripProofs`；真实脚本证据（§10.4） | 计数与 manifest 完全一致（35 runs / 525 events / 35 secrets 实测） |
| 4.9 | 恢复后密文可解密（提供 keyring） | PASS | `verifySecretsDecryptable`；`TestRestoreRoundTripProofs` | 逐个 `secrets` 行用 `security.Box.Open` + AAD 验证；计数必须等于 manifest |
| 4.10 | 错误 key 恢复 fail closed、无明文回退 | PASS | `TestRestoreWrongKeyFailsClosedAndLeavesTargetUntouched`；脚本负向证据（§10.5） | 失败时目标不存在、无 staging 残留、源快照与活库不变 |
| 4.11 | 密钥轮换：多版本 keyring、读取旧版本、用当前版本重新加密 | PASS | `TestRotateSecretsOldToNewVersionIdempotent`；`rotate.go` | 旧 v1 → 新 v2 全部成功，轮换后仍可解出原明文 |
| 4.12 | 轮换事务性 + 幂等 + dry-run | PASS | 同上 + `TestRotateSecretsDryRunChangesNothing` | `BEGIN IMMEDIATE` 单事务；二次运行 `rotated=0` 且密文/nonce 字节不变；dry-run 零写入 |
| 4.13 | 缺失 key version 时失败且不丢数据 | PASS | `TestRotateSecretsMissingKeyVersionFailsWithoutDataLoss` | 事务回滚；行版本/密文/nonce 不变；旧 keyring 仍能解密 |
| 4.14 | 轮换用于“当前版本但错误 key”时 fail closed | PASS | `TestRotateSecretsWrongCurrentKeyFailsClosed` | GCM 认证失败 → 中止；无写入 |
| 4.15 | 快照静态加密（AES-256-GCM） | **PASS** | `internal/ops/sealed.go`、`backup.go`、`verify.go`、`restore.go`；`TestEncryptedBackup*` ×4；脚本证据 §10.9 | 可选 `--encrypt --keyring`：自描述文件头（magic/format/cipher/key_version/nonce/plaintext_sha256/size）+ AES-256-GCM；AAD = `kind=ops_snapshot, id=plaintext_sha256, version`；manifest 记录 `encryption/key_version/plaintext_sha256/plaintext_size`；明文 staging 文件写入后 best-effort 覆写删除，只保留 `.sqlite.enc` |
| 4.16 | 密钥与 DB 快照分离 | PASS | `scripts/backup.sh` 需要单独的 keyring；`scripts/restore.sh`/`verify_backup.sh` 单独传 keyring；文档 SOP | 工具不会把 keyring 写进备份目录；恢复/校验必须单独提供 keyring；仍建议 keyring 独立介质（P1-3 备注） |
| 4.17 | 恢复演练可重复执行 | PASS | 本轮真实执行 6 次恢复（明文 3、加密 2 成功、错误 key 2、轮换后旧 key 1）；脚本幂等 | `scripts/restore.sh` + `docs/acceptance/R4.md` |
| 4.18 | 加密快照 verify：解密后校验 plaintext sha256/size + integrity/counts | PASS | `VerifyBackupWithKeyring`；`TestEncryptedBackupVerifyRestoreRoundTrip`；§10.9 | 无 keyring → 明确失败；错 key → GCM 认证失败；manifest/header 不一致 → 失败 |
| 4.19 | 加密快照 restore：先解密到 staged 明文并验证，再替换目标 | PASS | `restore.go`；`TestEncryptedBackupWrongKeyFailClosed` | 错误 key/篡改密文/缺失版本时目标不创建、无 staging 残留；源快照与活库不变 |
| 4.20 | 多版本 keyring 加密快照 | PASS | `TestEncryptedBackupMultiVersionKeyring` | keyring current=2 加密 → manifest key_version=2；只有 v1 的 ring 无法 verify/restore（fail closed） |
| 4.21 | 加密快照篡改检测 | PASS | `TestEncryptedBackupTamperDetection` | 翻转密文尾字节 → verify/restore 失败；manifest key_version 与文件头不一致 → 失败 |
| 4.22 | 加密缺少 keyring 的提前拒绝 | PASS | `TestBackupEncryptRequiresKeyring`；`TestRestoreRequiresKeyringForEncryptedSecrets` | `--encrypt` 无 keyring 在写入前报错；restore 对 .enc 无 keyring 明确拒绝 |

轮换安全边界：真实轮换要求服务停止（写事务 + post-check）。`scripts/rotate_key.sh` 在 `--apply` 前要求 `CONFIRM_ROTATE=1`
并检查 `<db>-wal` 非空则拒绝；轮换成功后自动保留 `*.prev-*` 旧 keyring 并原子替换当前 keyring。

---

## 5. 敏感数据扫描（日志 / 事件行）

实现：`internal/ops/scan.go` + `internal/ops/cmd/redactscan` + `scripts/redact_scan.sh`。扫描对象：

- 任意文本/日志文件（逐行）；
- SQLite 数据库中的 `events.public_payload_json`、`steps.error_summary/result_json`、`audit_log.detail_json`、
  `resources.display_alias/extra_json`、`runs.provider_snapshot_json`；
- 事件 payload **key allowlist**（复用 `security.IsEventPayloadKeyAllowed`），越界 key 即判定泄露。

检测规则：JWT 形状、`Bearer`/`Basic` 凭据、敏感赋值（`password=`/`state=`/`token=`/`access_token=`/`x-admin-auth=` 等，
带 enum/掩码降噪）、长 base64-like token（48+，排除 40+ 位十六进制摘要）、PEM 私钥块、调用方提供的已知 secret 值。
输出只包含规则、来源、行号与字节数，**不回显原值**。

| 对象 | 结果 | 证据 |
|---|---|---|
| 35 run 压测产生的真实服务日志（1095 行 / 982 源 / 124 KB） | PASS 0 findings | `scripts/redact_scan.sh --file …/service.log --event-db …/state.sqlite`；`REDACT_CLEAN_EXIT=0` |
| 同一压测数据库的事件/步骤/审计行 | PASS 0 findings | 同上（扫描 35 runs、525 events 的持久化 JSON） |
| 负向对照（伪造 dirty log：Bearer / password= / state= / token= / known secret） | PASS（成功检出 5 findings，值已脱敏） | 直接运行编译后的 CLI：退出码 **2**；经 `go run` 时 Go 工具链把非零码归一为 1（文档已说明） |
| 单元测试：clean 样本无误报、8 类 dirty 样本全检出、DB 越界 key 检出 | PASS | `TestScanCleanArtifacts`、`TestScanDirtySamples`（8 子测试）、`TestScanDatabaseFindingsAndAllowlist` |

说明：`secrets` 表本身刻意不扫描（AES-GCM 密文/nonce 属高熵数据，扫描必然误报）；其保密性由 §4.9/4.10 的解密证明覆盖。
已知边界：长 token 规则为启发式，可能对极长的无分隔 base64 日志字段产生误报；`credential_assignment` 对 `state=OAUTH_PENDING`
这类全大写 enum 不误报（见 `suspiciousAssignment`）。

---

## 6. SSRF 只读审计（不重写任何 adapter）

审计方法：只读审阅 provider adapter 的 base URL 解析、HTTP client `CheckRedirect`、凭据 header 注入位置，**本轮未修改
`internal/provider/**`**。未对真实平台发起任何请求。结论：**未发现请求时任意 URL、也未发现跨 host 重定向转发 Authorization 的路径**。

| Adapter | base URL 约束 | Redirect 策略 | 凭据转发 | 结论 | 证据 |
|---|---|---|---|---|---|
| Cloudflare Temp Email | 默认仅 `api.moonbyte.top`；`AllowedHosts` 显式覆盖；loopback 仅在 `AllowLoopback=true` 测试开关下；非 loopback 强制 https；拒绝 userinfo/query/fragment | 只允许与首跳同 scheme+host；≥3 跳拒绝 | `x-admin-auth`、`x-custom-auth`、地址 JWT `Bearer` 仅发往构造时的 base origin | PASS | `internal/provider/mailbox/cloudflare/cloudflare.go:566,687,778,821` |
| GitHub（existing account） | 仅 `https://api.github.com`；loopback 需显式 `AllowLoopbackHTTP`；无 path/query/userinfo | 只允许同 scheme+host；≥3 跳拒绝 | token 运行时从 SecretRef resolve，只在 allowlist host 上设置 Bearer | PASS | `internal/provider/github/config.go:129`；`github.go:76,408` |
| Keycloak（sandbox IdP） | loopback host pin（`parseBaseURL`），http 需显式 loopback 测试开关；浏览器表单表单 action 不得离开 loopback | 浏览器/backchannel 均有 redirect guard | admin 密码仅用于 loopback；浏览器会话 cookie jar 局部 | PASS（静态；上线接入见备注） | `internal/provider/keycloak/keycloak.go:551,314,184,307`；`oidc.go:239,530,594` |
| WorkBuddy（权限门控网关） | 仅 loopback / RFC1918（`validateBaseURL`）；http 需 `AllowTestHTTP`；无 userinfo/query/path | 只允许同 scheme+host；≥3 跳拒绝 | gateway token 通过 SecretRef + Bearer，仅发往私有网关 | PASS + P2 | `internal/provider/workbuddy/config.go:175`；`workbuddy.go:103,488` |
| Mailpit（本地 sandbox） | 仅 loopback；`http/https`；无 userinfo/query/fragment | 只允许同 origin | adapter 不持有任何凭据（无 Authorization） | PASS | `internal/provider/mailbox/mailpit/mailpit.go:450,683` |
| Fake providers | 无 HTTP 客户端 | 无网络 | 无凭据 | PASS（构造上离线） | `internal/provider/fake/fake.go` |
| Config 全局 | 启用 live provider 时逐 host allowlist 校验；默认 loopback listen；公网 listen 需显式开关 + admin token | — | secret 文件读取，不写日志 | PASS | `internal/config/config.go:477–570,572` |

备注：
1. **Keycloak adapter 在本审计时点尚未接入 `internal/provider/boot/live.go`**（R2 工作流范围；本报告只做静态审阅）。
   一旦接入，必须保留 loopback pin 与 `CheckRedirect` guard；本报告给出接入前基线。
2. WorkBuddy `validateAuthorizationURL` 只校验“https + 有 host + 无 userinfo/控制字符”，对 **由网关返回、仅展示给操作者** 的
   authorization URL 不 pin host。adapter 从不会 fetch 该 URL，因此不构成 SSRF；但若网关被攻陷，操作者可能被引导到攻击者页面（P2-1）。
3. 所有 adapter 的 http.Client Timeout 均有配置上限；未发现无限期阻塞请求。

---

## 7. Docker 部署清单审计（静态）

命令：`docker compose -f deploy/compose.yaml config`（**无需 daemon**）— 退出码 **0**，解析结果 `/tmp/r4-evidence/compose-config.yaml`。
`docker build`/`docker run`/容器内 healthcheck：**NOT_RUN**（`docker info` 退出码 1，daemon 未运行）。GHCR 推送：**NOT_RUN**（无授权、未要求）。

| # | 控制 | 结果 | 证据（解析后的 compose 字段） |
|---|---|---|---|
| 7.1 | 本地可构建契约（非未发布 GHCR tag） | PASS | `build.context=/…/account-onboarding`，`dockerfile=Dockerfile`，`image=account-onboarding:local` |
| 7.2 | 只读根文件系统 | PASS | `read_only: true` |
| 7.3 | tmpfs（noexec/nosuid、大小限制） | PASS | `/tmp:size=16m,mode=1777,noexec,nosuid` |
| 7.4 | 非 root 运行 | PASS | `user: 10001:10001`；Dockerfile 内 `USER 10001:10001` |
| 7.5 | 丢弃全部 capability + 禁止提权 | PASS | `cap_drop: [ALL]`、`security_opt: [no-new-privileges:true]` |
| 7.6 | 资源限额 | PASS | `pids_limit: 128`、`mem_limit: 268435456`、`cpus: 0.5` |
| 7.7 | 仅 loopback 发布 | PASS | `host_ip: 127.0.0.1`、`published: 8788`、容器内 `0.0.0.0:8788` + `ONBOARD_ALLOW_PUBLIC_LISTEN=1`（注释解释边界） |
| 7.8 | 密钥文件挂载 | PASS | secrets `onboarding_master_key`、`onboard_admin_token` → `/run/secrets/*`；环境变量只放路径 |
| 7.9 | 默认关闭 live provider | PASS | `ONBOARD_CLOUDFLARE_ENABLED/GITHUB_ENABLED/WORKBUDDY_ENABLED/KEYCLOAK_ENABLED/MAILPIT_ENABLED/OIDC_ENABLED=false` |
| 7.10 | Fake provider 默认关闭 | PASS | `ONBOARD_FAKE_PROVIDERS=false` |
| 7.11 | healthcheck 使用镜像内子命令 | PASS | `test: [/app/onboardd, healthcheck]`，interval/timeout/retries/start_period 齐全 |
| 7.12 | 无 privileged / host network / docker.sock | PASS | 解析结果中不存在 |
| 7.13 | SQLite 持久化路径唯一可写 | PASS | bind mount `./data:/app/data`；容器其余路径只读 |
| 7.14 | 镜像构建与运行 | **NOT_RUN** | daemon 不可用；`docker compose … build` 退出码 1（Cannot connect to the Docker daemon） |
| 7.15 | GHCR 发布流程 | **NOT_RUN** | 未获得推送授权，本轮未执行 |
| 7.16 | 密钥文件存在性与权限预检 | PARTIAL | `docker compose config` 不校验 secret 文件是否存在；首次 `up` 前需按 `deploy/secrets/*.example` 生成 0600 文件（P2-5） |
| 7.17 | 基础镜像 digest pin / OCI label | **FAIL（供应链加固项）** | Dockerfile 使用 `golang:1.25-bookworm`、`debian:bookworm-slim` tag，未 pin digest；无 `org.opencontainers.image.*` labels（P2-6） |
| 7.18 | `deploy/.gitignore` 允许示例入库、真实 secret 忽略 | PASS | `secrets/*` + `!secrets/*.example`；`git check-ignore -q`：两个 `.example` 退出 1（未被忽略），真实 key 路径退出 0（被忽略）；`git status --ignored` 中真实 secret 为 `!!` |
| 7.19 | 新 env 文档（RBAC/限流/可信代理） | PASS | `deploy/.env.example` 记录 `ONBOARD_READONLY_TOKEN(_FILE)`、`ONBOARD_RATE_LIMIT_PER_MINUTE`、`ONBOARD_LOGIN_RATE_LIMIT_PER_MINUTE`、`ONBOARD_TRUST_PROXY_HEADERS`（含“仅可信反代开启”警告）与备份加密变量 |

Dockerfile 需求清单（镜像由 Lead 维护，实际文件已存在）：

- [x] 多阶段构建，cgo 构建器（gcc/libc6-dev），运行镜像只含二进制 + CA + tzdata；
- [x] 非 root UID/GID 10001，`USER 10001:10001`；
- [x] `/app/data` 由 10001 拥有（SQLite + WAL 唯一写路径）；
- [x] `ENTRYPOINT ["/app/onboardd"]`，`EXPOSE 8788`，`HEALTHCHECK` 调用 `onboardd healthcheck`；
- [x] `.dockerignore` 排除 `/secrets/`、`/data/`、`/.sandbox/`、`*.sqlite*`，不会把本地密钥/数据库烘进镜像；
- [ ] 建议补充：OCI labels（source/version/revision）、基础镜像 digest pin、镜像内不包含 shell 工具（可进一步缩减攻击面）、`-ldflags` 注入构建版本/commit；
- [ ] 运行约束：镜像 ENV 设置了 `ONBOARD_LISTEN=0.0.0.0:8788`，若脱离 compose 独立运行必须同时设置
      `ONBOARD_ALLOW_PUBLIC_LISTEN=1` 或改回 loopback，否则 config 校验会拒绝启动（已在本 compose 中处理）。

---

## 8. 本轮真实执行的命令与退出码（摘要）

完整逐条记录见 `docs/acceptance/R4.md` §2。关键门禁：

| 命令 | 退出码 | 结果 |
|---|---|---|
| `go build ./internal/ops/...` | 0 | 编译通过（含全部 cmd） |
| `go vet ./internal/ops/...` | 0 | 无输出 |
| `gofmt -l internal/ops` | 0 | 无输出（clean） |
| `go test ./internal/ops/ -race -count=1` | 0 | **36** 个顶层测试全 PASS，0 FAIL（`/tmp/r4-evidence/r4b-ops-race.out`，含 5 个加密快照测试 + 4 个新 HTTP 审计测试） |
| `go test ./... -count=1` | 0 | 全仓库包全部 ok（含 R4 前的既有包） |
| `go test ./... -race -count=1` | 0 | 全仓库 race 门禁通过（14 包 ok，0 FAIL；`/tmp/r4-evidence/r4b-full-tests-race.out`） |
| `bash -n scripts/*.sh` | 0 | 6 个脚本语法检查通过 |
| `scripts/load_smoke.sh`（5 并发 + 30 串行） | 0 | 35/35 COMPLETED，0 error，0 duplicates |
| `scripts/backup.sh` → `scripts/verify_backup.sh` | 0 / 0 | 快照 + manifest 校验通过 |
| `scripts/restore.sh`（正确 keyring） | 0 | integrity ok、counts 一致、35/35 secrets 解密 |
| `scripts/restore.sh`（错误 keyring） | 1（预期失败） | “target untouched”，目标不存在 |
| `scripts/backup.sh --encrypt --keyring …`（35-run 库） | 0 | `.sqlite.enc` + manifest：`encryption=aes-256-gcm`、`key_version=1`、`plaintext_sha256=…`、`plaintext_size=495616`（§10.9） |
| `verify_backup.sh` 加密快照无 keyring / 错 key | 1 / 1 | `requires a keyring` / `decrypt sealed snapshot: secret decryption failed` |
| `scripts/restore.sh` 加密快照正确 key / 错误 key | 0 / 1 | encrypted=true、35/35 secrets 解密 / 目标不创建（§10.9） |
| `go test ./internal/ops/ -race -count=1`（R4c） | 0 | **38** 个顶层测试全 PASS，0 FAIL（`/tmp/r4-evidence/r4c-ops-race.out`） |
| `go test ./... -race -count=1`（R4c） | 0 | 14 个包 ok，0 FAIL（含 `tests/e2e`；`/tmp/r4-evidence/r4c-full-race.out`） |
| `bash -n scripts/*.sh deploy/scripts/preflight.sh` | 0 | 7 个脚本语法检查通过 |
| `scripts/redact_scan.sh`（真实 `.sandbox/e2e/run-*/onboardd*.log` + `evidence*.json`） | 0 | 80 个文件 / 4163 行 / 667476 bytes，0 findings（§12.1） |
| `scripts/backup.sh --encrypt` + `verify_backup.sh`（真实 e2e 库） | 0 / 0 | `.sqlite.enc` + manifest 校验通过（§12.3） |
| `deploy/scripts/preflight.sh`（有效临时 secrets/data） | 0 | 密钥格式、权限、token、data dir 全通过；失败用例见 §12.2 |
| `deploy/scripts/preflight.sh`（仓库默认，无真实 secrets） | 1（预期失败） | 明确提示缺失 master key/admin token/data dir，不打印密钥 |
| `scripts/rotate_key.sh` dry-run / `--apply` | 0 / 0 | planned 4 → rotated 4，versions 1 → 2 |
| 轮换后用旧 key 恢复含 v2 数据的快照 | 1（预期失败） | `secret key version 2 unavailable`，目标未创建 |
| `redactscan` 负向 dirty 样本（编译后二进制） | 2（预期 findings） | 5 findings，值脱敏 |
| `scripts/redact_scan.sh`（真实压测日志 + DB） | 0 | 0 findings |
| `docker compose -f deploy/compose.yaml config` | 0 | 解析通过（无 daemon） |
| `docker info` / `docker compose build` | 1 / 1 | daemon 未运行 → build/run NOT_RUN |

`go run` 会把子进程的非零退出码归一为 1（Go 工具链行为）；需要精确退出码的验收场景使用编译后的二进制（§10.7）。

---

## 9. 残余风险与优先修复清单

### P0（立即阻断发布 / 数据泄露）

无。本审计未发现可导致凭据明文泄露、跨 host 凭据转发、任意 URL 请求或备份不可恢复的 P0 缺陷。

### P1（发布前必须关闭或取得书面豁免）

#### 已于 R4b 关闭（2026-10-24）

| ID | 原问题 | 关闭证据 | 备注 |
|---|---|---|---|
| P1-1 | 无请求速率限制 / 登录暴力破解防护 | `internal/api/ratelimit.go`；`TestHTTPRateLimitGeneral`、`TestHTTPRateLimitLogin`（429 + Retry-After ≥1s + `RATE_LIMITED`） | 60s 固定窗口、按客户端 IP、general 300/min + login 10/min、0 可禁用；内存态、单实例（P2-10） |
| P1-3 | 备份快照未加密（仅 0600 + checksum） | `internal/ops/sealed.go`；`TestEncryptedBackupVerifyRestoreRoundTrip/WrongKeyFailClosed/MultiVersionKeyring/TamperDetection`；§10.9 | AES-256-GCM + 自描述 header + manifest key_version/plaintext_sha256；无/错 key fail closed；仍建议 keyring 独立介质离机保存 |
| P1-4 | RBAC 缺失（单 admin token） | `ONBOARD_READONLY_TOKEN`；`TestHTTPViewerRoleReadOnly` | 最小双角色（admin/viewer），viewer 只读全部 GET、mutation 403；**仍是单 operator 服务**：无 per-user 账号/2FA/角色管理（P2-11） |

#### 仍开放

| ID | 问题 | 影响 | 建议修复 | 回归测试 |
|---|---|---|---|---|
| P1-2 | **TLS ingress 未落地** | 若把 `127.0.0.1:8788` 暴露到私网/公网而无 TLS 终止，bearer token/cookie 明文传输 | 明确部署契约：只允许反代（Caddy/nginx）终止 TLS（≥1.2），并给 compose/README 提供反代示例；HTTPS 下 cookie `Secure` 自动生效（已实现） | `curl -I https://…` 断言 TLS 版本 + HSTS；本环境 NOT_RUN（无 ingress） |
| P1-5 | **轮换/恢复操作要求停机，但工具无法证明进程已停** | 运行中对 live DB 轮换会与 service 写竞争；恢复会替换正在使用的 DB | 脚本已要求 `CONFIRM_ROTATE=1`/`CONFIRM_RESTORE=1` + 检查 `<db>-wal`；补充建议：通过 PID/lockfile 或 systemd `ExecStop` 编排 | 故障注入：服务运行时拒绝 apply/restore（当前仅靠 WAL 检查，可被绕过） |

### P2（可跟进，非阻断）

| ID | 问题 | 建议 |
|---|---|---|
| P2-1 | WorkBuddy authorization URL 不 pin host（adapter 不 fetch，仅展示） | 在 config 增加 `ONBOARD_WORKBUDDY_AUTHORIZATION_HOSTS` allowlist；不匹配时拒绝展示并要求人工确认 |
| P2-2 | **已关闭（R4b）**：cookie POST 现要求 Origin 或同 host Referer | 保留：非浏览器客户端仍可显式伪造两者，但还须持有 CSRF token + 会话 |
| P2-3 | 非 TLS 请求下 session cookie 无 `Secure`（TLS/可信反代下已设置）；显式登出会做**服务端撤销**，但撤销表是**进程内存**（重启后旧 cookie 在 TTL 内仍有效，多实例不共享） | 生产必须 TLS 终止；可增加 `ONBOARD_REQUIRE_TLS` 强制拒绝明文请求；需要强吊销时把撤销表落 SQLite 或引入 per-user 会话存储（超出 v0.1.0） |
| P2-4 | JSON API 无 CSP | 对 JSON 无执行语义；若使用 API 文档 UI 等渲染端点，按路由补 CSP |
| P2-5 | **已关闭（R4c，操作流程）**：compose 本身仍不自动校验 secret 文件，但 `deploy/scripts/preflight.sh` 会在 `up` 前检查 secret 文件存在/非空/0400-0600/占位符、master key 格式、admin/viewer token、data dir 可写性，并已写入 compose 头注释与 `deploy/README.md` | 建议 CI/runbook 把 preflight 作为 `up` 的前置步骤（P2-13） |
| P2-6 | 基础镜像 tag 未 pin digest、无 OCI labels、镜像含 shell | 供应链加固：digest pin + labels + 必要时 distroless；由 Lead 在 Dockerfile 处理 |
| P2-7 | 备份 manifest 只记录 basename、不记录 tool git commit | 可加 `tool_commit`/host 信息便于审计链；注意不要写入敏感路径 |
| P2-8 | 扫描器为启发式；secrets 表不扫描（设计如此） | 文档已声明边界；可增加可配置 allowlist/规则文件 |
| P2-9 | `ONBOARD_TRUST_PROXY_HEADERS=1` 时若客户端可直连，可伪造 `X-Forwarded-For`（绕过限流）或 `X-Forwarded-Proto`（误导 Secure 标记） | 仅允许在“只有反代可达、客户端不能直连”的部署中开启；`.env.example` 与 R4-audit 已警告；可在启动时校验 listen 仅 loopback/私网 |
| P2-10 | 限流器为进程内固定窗口：单实例有效、重启清零、不支持多副本共享计数 | 单实例自托管符合产品定位；多副本部署需外置限流（反代层） |
| P2-11 | viewer 令牌可读 `GET /v1/audit`、run/event 元数据（无密钥明文），且无 per-user 审计归属 | 共享只读令牌仅用于看板/巡检；如需按人审计应引入 per-user 账号（超出 v0.1.0 范围） |

---

## 10. 关键证据（已脱敏）

### 10.1 备份（35 runs 压测库）

```text
backup: source=/tmp/r4-evidence/load/db/state.sqlite out_dir=/tmp/r4-evidence/backups-full
{ "snapshot_path": ".../state-20261024T132514Z.sqlite",
  "manifest": { "format_version": 1, "tool": "account-onboarding-ops", "tool_version": "0.1.0-r4",
    "created_at": "2026-10-24T13:25:14Z", "source_db": "state.sqlite",
    "snapshot_file": "state-20261024T132514Z.sqlite",
    "sha256": "…(64 hex)…", "schema_version": 2, "source_journal_mode": "wal",
    "counts": { "runs": 35, "steps": 210, "events": 525, "resources": 105, "secrets": 35,
                "mail_receipts": 35, "audit_log": 40 } } }
backup: OK (snapshot, manifest and sha256 verified)
```

### 10.2 校验

```text
"checks": ["manifest_snapshot_name","size_bytes","sha256","integrity_check=ok",
           "foreign_key_check=0","schema_version=2","counts"]
verify_backup: OK
```

### 10.3 恢复（正确 keyring）

```text
"schema_version": 2, "integrity_check": "ok", "foreign_key_violations": 0,
"counts": { "runs": 35, "steps": 210, "events": 525, "resources": 105, "secrets": 35, … },
"secrets_verified": 35, "keyring_current_version": 1, "manifest_verified": true
restore: OK (integrity, counts and decryption verified)
```

### 10.4 错误 key / 轮换后旧 key（fail closed）

```text
ERROR: restore failed: … secret decryption proof failed; target untouched:
secret <uuid> (kind=sandbox_password, key_version=1) is not decryptable: secret decryption failed
（目标目录为空，源快照与活库未变）
```

```text
ERROR: restore failed: … secret <uuid> (kind=sandbox_password, key_version=2) is not decryptable:
secret key version 2 unavailable
```

### 10.5 轮换

```text
dry-run: { "current_version": 2, "total_secrets": 4, "already_current": 0, "planned": 4,
           "versions_before": {"1": 4}, "versions_after": {"1": 4} }
apply:   { "current_version": 2, "total_secrets": 4, "rotated": 4,
           "versions_before": {"1": 4}, "versions_after": {"2": 4}, "verified_after": 4 }
sqlite3: 2|4      -- secrets.key_version after rotation
keyring show: { "current": 2, "versions": [1, 2] }
```

### 10.6 5 并发 + 30 串行负载

```text
p50/p95 create = 0.65 / 5.53 ms；p50/p95 run = 11.86 / 30.75 ms（in-process HTTP + SQLite + fake providers）
expected=35 created=35 completed=35 failed=0 cancelled=0 missing=0
duplicate_runs=0 duplicate_resources=0 duplicate_identities=0 duplicate_operation_keys=0
idempotent_replay_same_id=true error_count=0 pass=true
```

### 10.7 敏感扫描

```text
clean（真实服务日志 + 事件行）: { "sources": 982, "lines": 1095, "findings": null }  exit 0
dirty 负向对照（编译后 CLI）: 5 findings（authorization_header / credential_assignment ×3 / known_secret），
                              值显示为 <redacted>，exit 2
```

### 10.8 Docker 清单

```text
$ docker compose -f deploy/compose.yaml config   # exit 0（无 daemon）
$ docker info                                    # exit 1: Cannot connect to the Docker daemon
$ docker compose -f deploy/compose.yaml build    # exit 1: daemon 未运行 → NOT_RUN
```

### 10.9 加密快照（R4b）

```text
backup --encrypt（35-run 库）
  file: state-20261024T133825Z.sqlite.enc（495854 bytes, mode 0600）
  manifest: encryption=aes-256-gcm, key_version=1,
            plaintext_sha256=cd213b44…（64 hex）, plaintext_size=495616,
            sha256=<ciphertext sha256>, counts: runs=35 secrets=35 …
verify --keyring <key>       → exit 0；checks 含 encryption=aes-256-gcm / key_version=1 / plaintext_sha256 / integrity_check=ok
verify 无 keyring           → exit 1: "encrypted snapshot requires a keyring"
verify 错误 keyring         → exit 1: "decrypt sealed snapshot: secret decryption failed"
restore 正确 keyring        → exit 0: encrypted=true, key_version=1, integrity_check=ok, runs=35, secrets_verified=35
restore 错误 keyring        → exit 1: 目标目录为空（target untouched）
```

实现要点：文件头 `AONSEAL1 + uint32(len) + JSON{format,cipher,key_version,nonce,plaintext_sha256,plaintext_size}`，
密文为 AES-256-GCM；AAD 由 `security.Box` 绑定 `kind=ops_snapshot`、`id=plaintext_sha256`、`version`。
manifest 的 `SHA256/SizeBytes` 是存储文件（密文）校验，`PlaintextSHA256/PlaintextSize` 是解密后 SQLite 负载校验；
verify/restore 均要求二者一致，任何一项不匹配 fail closed。

### 10.10 HTTP 新控制审计（R4b，真实 api.Server + httptest）

```text
go test ./internal/ops/ -race -count=1 -v
--- PASS: TestHTTPRateLimitGeneral      （limit=2：第 3 次 429 + Retry-After + RATE_LIMITED）
--- PASS: TestHTTPRateLimitLogin        （limit=3：第 4 次登录 429 + Retry-After）
--- PASS: TestHTTPViewerRoleReadOnly    （viewer GET /readyz、/v1/meta、/v1/providers、/v1/runs、/v1/audit = 200；
                                          POST create/resume/cancel = 403 read-only；admin POST = 202）
--- PASS: TestHTTPSecureCookieFlags     （loopback 无 Secure；trusted proxy + X-Forwarded-Proto=https 有 Secure；
                                          untrusted 栈忽略 X-Forwarded-Proto）
--- PASS: TestHTTPCookieCSRFAndOrigin   （无 Origin/Referer → 403；同 host Referer → 202；evil Origin → 403）
36 top-level tests PASS / 0 FAIL
```

---

## 11. 审计结论

- 备份/恢复/轮换/敏感扫描/压测门禁真实执行且通过（含负向用例），未发现 P0。
- HTTP 层 CSRF/CSP/Origin/401-403/常量时间比较/body limit/未知字段拒绝均通过 httptest 对真实 `api.Server` 的验证；
  未修改 API 代码。
- SSRF 只读审计未发现请求时任意 URL 或跨 host 凭据转发；WorkBuddy authorization URL 的展示 host 未 pin 为 P2。
- R4b 已闭环：RBAC（最小双角色）、请求速率限制（general + login）、cookie mutation 的 Origin/Referer 强制、可信反代 Secure cookie、
  快照静态加密（AES-256-GCM，含错 key/缺失版本 fail-closed 与篡改检测）。
- 部署清单静态合格；Docker build/run、GHCR、TLS ingress 仍为 NOT_RUN；轮换/恢复停机编排为开放 P1；单 operator、无 per-user 账号/2FA
  为已知产品边界（P2-11）。
- 因此 R4 总体状态仍为 **PARTIAL**：安全与可靠性工具链 PASS，但 TLS ingress 未部署/未测试、Docker build/run 与 GHCR 为 NOT_RUN，
  不能以 PASS 包装。

---

## 12. R4c（post-review closure，2026-10-24T14:10Z–14:25Z）

R4c 只处理独立审查发现的两类问题：敏感扫描器长路径误报、部署 secret/数据目录缺少前置校验。**不改变 R4 的 PARTIAL 判定**：
TLS ingress、Docker build/run、GHCR 发布、真实平台 live 仍为 NOT_RUN（§6）；reviewer 的代码级 P1（状态机提交缺口、mail receipt 原子性、
Cloudflare 重复地址 CONFLICT）由 Lead 在共享文件修复，本轮以全仓库 14 包 race 门禁为回归证据（`/tmp/r4-evidence/r4c-full-race.out`），本报告不重复审计其实现。

### 12.1 敏感扫描器误报修复（reviewer P2-1 → CLOSED）

**根因**：`long_token` 是唯一的启发式规则（`[A-Za-z0-9+/_-]{48,}`）。真实 R2 e2e 日志/证据中的长绝对路径
`/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding/.sandbox/e2e/run-20261024T134934-fe6306/onboardd.log`（63 字节）
和 API 路径 `/v1/runs/<uuid>/events`（52 字节）都在该字符集内，导致 7–9 条假阳性、脚本 exit 1。

**修复**（`internal/ops/scan.go`）：

- 新增 `pathLikeSpans()`：识别绝对/相对路径（`/`、`./`、`../`、`~/`）、Windows 路径、以及 `path/file/log/db_path/out_dir/url/uri`
  等字段中含 `/` 或 `\` 的值；**只有** `long_token` 规则跳过与这些 span 重叠的匹配。
- `jwt`、`authorization_header`、`credential_assignment`、`known_secret`、`private_key` 规则仍对整行原始文本生效，
  因此“路径中嵌 JWT/已知 secret”仍会检出（`TestScanHighConfidenceRulesInsidePaths`）。
- 敏感赋值 key 扩展：`action_token`、`login_action_token`、`action-token`、`api_key`、`apikey`、`x-api-key`、
  `execution`、`session_code`、`tab_id`、`key`（Keycloak action-token 形状），保留 enum/掩码降噪。

**回归测试**（`internal/ops/scan_test.go`）：

- `TestScanCleanE2ELogAndEvidenceArtifacts`：真实 e2e 日志形状（绝对路径、`/v1/runs/<uuid>/events`、log/db_path JSON）→ 0 findings；
- `TestScanHighConfidenceRulesInsidePaths`：路径内 JWT / known secret 仍检出；
- `TestScanDirtySamples` 新增 `action_token`、`execution`、`session_code`、`api_key`、`path-field-base64` 子用例；
- 原有 JWT/Bearer/password=/state=/token=/long base64/known secret/PEM 用例保持通过。

**真实证据**：

```text
$ scripts/redact_scan.sh --file .sandbox/e2e/run-*/onboardd*.log --file .sandbox/e2e/run-*/evidence*.json
{ "sources": 80, "lines": 4163, "bytes": 667476, "findings": null, "findings_by_kind": {} }
redact_scan: OK (no sensitive-data findings)        # exit 0
```

### 12.2 部署 preflight（`deploy/scripts/preflight.sh`）

检查项（不打印任何 secret 值）：

1. `deploy/compose.yaml` 存在且可读；
2. secrets 目录存在；master key / admin token 文件存在、非空、可读、owner-only（0400/0600）、非 `*.example` 占位符；
3. master key 格式：32 raw bytes、64 hex、base64（43/44 chars）或 JSON keyring（`current` 为正、current 有对应 key、每 key base64 32 bytes）；
4. admin token 非空（<16 字符 warning）；可选 readonly token 非空且与 admin token 不同；
5. data 目录存在、当前用户可写（临时文件探针），owner UID 非 10001 时给出 `chown 10001:10001` 建议（warning）。

使用位置：`deploy/compose.yaml` 头注释 + `secrets:` 注释、`deploy/README.md`（`up` 前置步骤）、`docs/acceptance/R4.md` §R4c。

真实执行：

| 场景 | 命令 | 退出码 |
|---|---|---|
| 仓库默认（无真实 secrets/data） | `deploy/scripts/preflight.sh` | **1**（3 个 FAIL，明确给出创建/复制/chown 指令） |
| 有效 JSON keyring（v1）+ admin/viewer token + 可写 data | `--secrets-dir /tmp/... --data-dir /tmp/...` | **0**（1 warning: UID 501） |
| 多版本 JSON keyring（current=2, keys 1+2） | `--master-key …v2` | **0** |
| 32 raw / 64 hex / base64 single key | `--master-key …raw/hex/b64` | **0 / 0 / 0** |
| 示例占位符 / 31-byte key / 0644 perms / readonly==admin / data dir 不可写 | 对应 flags | **1 / 1 / 1 / 1 / 1** |
| 未知参数 | `--bogus` | **2** |

### 12.3 R4c 重新执行证据

| 命令 | 退出码 | 结果 |
|---|---|---|
| `go test ./internal/ops/ -race -count=1` | 0 | 38 个顶层测试全 PASS，0 FAIL（`/tmp/r4-evidence/r4c-ops-race.out`） |
| `go test ./... -race -count=1` | 0 | 14 个包 ok（含 `tests/e2e`），0 FAIL（`/tmp/r4-evidence/r4c-full-race.out`） |
| `bash -n scripts/*.sh deploy/scripts/preflight.sh` | 0 | 7 个脚本语法通过 |
| `docker compose -f deploy/compose.yaml config` | 0 | 静态解析通过（仍无 daemon，build/run NOT_RUN） |
| `scripts/redact_scan.sh`（真实 e2e 日志 + evidence） | 0 | 80 文件 / 4163 行，0 findings |
| `scripts/backup.sh --encrypt`（真实 e2e `state.sqlite`） | 0 | `.sqlite.enc` + manifest，自校验通过 |
| `scripts/verify_backup.sh <enc> --keyring …` | 0 | sha256/integrity/FK/schema/counts + 解密校验通过 |
| `deploy/scripts/preflight.sh`（有效/默认） | 0 / 1 | 见 §12.2 |

### 12.4 R4c 结论

- reviewer P2-1（长路径误报）**CLOSED**；扫描器对真实 R2 e2e 产物 exit 0，同时对 dirty 样本保持全检出。
- `deploy/scripts/preflight.sh` 已加入并文档化，P2-5 在操作流程上 **CLOSED**（compose 不自动调用，P2-13 记录）。
- reviewer 代码级 P1/P2 由 Lead 在共享文件修复；本轮全仓库 14 包 race 门禁通过，R4 工具链未回退。
- 仍开放：P1-2 TLS ingress（NOT_RUN）、P1-5 停机编排；P2-1/2-3/2-4/2-6/2-7/2-8/2-9/2-10/2-11/2-13 见 §9。
- **R4 总体判定不变：PARTIAL**（工具链与已执行控制 PASS；TLS ingress/Docker build-run/GHCR/真实平台 live NOT_RUN）。

