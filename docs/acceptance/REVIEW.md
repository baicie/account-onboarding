# 独立对抗性审查报告 — `account-onboarding`

- 审查时间：`2026-10-24T13:55Z–14:12Z`（本机 CST `21:55–22:12`，Asia/Shanghai）
- 审查者：独立 reviewer（**未参与实现**；不修改任何代码/`go.mod`/他人文档/运行态）
- 审查对象：仓库当前工作树 + 真实可执行测试，**不依据 README 自述能力**
- 审查依据：`account-onboarding-design-v0.1.0/ai/REVIEW_PROMPT.md`（本文严格按其输出格式）
- 环境：macOS Darwin arm64、Go 1.25.3、cgo `mattn/go-sqlite3`；真实本机 Keycloak 26.8.1
  (`127.0.0.1:8180`，realm `moonbyte-test`) + Mailpit v1.32.0 (`127.0.0.1:8025/1025`)；Docker daemon
  **未运行**（`docker info` → 1，Docker build/run 只能 NOT_RUN）
- 结论（首轮，2026-10-24T14:12Z）：**PARTIAL**。**无 P0**。发现 **3 项 P1**（其中 2 项为代码级可靠性缺口，
  1 项为已声明的 TLS 交付前置）与 **11 项 P2**；§2 的 17 组门禁/探针命令全部实际执行并记录退出码。
- **Post-review 复核（2026-10-24T14:35Z，§8）**：Lead 已修复 P1-1、P1-2a、P1-2b 与多数 P2；我在
  **哈希稳定快照**（126 个文件前后一致）上重跑全部门禁与探针，逐项独立复核。**代码级 P0/P1 已清零**，
  最终状态 **PASS（范围限定）**：适用于 `LOCAL SANDBOX` 证据 + 仅回环自托管的 v0.1.0 交付；
  **不等于**"可公网/远程暴露"或生产就绪（TLS ingress 仍为 NOT_RUN，见 §8.6）。
- **交付树（2026-10-24T14:40Z，§8.8）**：快照后仅发生一次追加修改（`state.go`/`state_test.go` 的
  `CANCELLED` 边），`state.go` sha256 = `ed1d6895…422188`；126 文件 manifest sha256 =
  `1a65ea11…296186e`；交付树上 build/vet/`go test ./... -race`/sandbox e2e 全部复跑通过，**PASS（范围限定）维持**。

> 边界声明：本轮审查只读仓库代码与文档；所有新增运行态产物都在 `gitignored` 的 `.sandbox/review/`
> （临时 `onboardd`、诱饵 HTTP 服务、scratch SQLite、备份/恢复/轮换证据、过渡态探针）。
> 未 `git push`、未改生产配置、未联系任何真实第三方平台、未执行 `docker build`。
> 审查结束后 loopback 夹具已停止，Keycloak realm 用户回到 **0**，Mailpit 邮件数不变（5）。

---

## 1. 最终判定与问题总览（首轮快照，2026-10-24T14:12Z）

> 本节及 §2–§7 保留**首轮审查原文**作为审计快照；Lead 修复后的逐项复核状态与最终判定见 **§8**。
> 首轮 P1-1/P1-2 已关闭（§8.3），P1-3 已按运维前置重新分级（§8.6），P2 的关闭/部分/开放状态见 §8.3。

| 级别 | 数量 | 一句话 |
|---|---:|---|
| **P0** | **0** | 无隐藏注册/反爬绕过/指纹伪装/代理轮换/多账号轮转；无凭据明文泄漏到日志、事件或 SQLite |
| **P1** | **3** | ①状态机提交缺口导致"凭据过期"→重复执行外部步骤且不计重试预算；②外部副作用的重试/崩溃窗口不对账（邮件消费早于提交、Cloudflare 重复地址靠"再造一个"恢复）；③TLS ingress 未落地（R4 已声明，非代码缺陷） |
| **P2** | **11** | 扫描器长路径误报、raw master key 被 TrimSpace 破坏、会话不可吊销、viewer 可读 OAuth 授权 URL、邮件发送者子串预过滤、错误分类过宽、状态矩阵死边、无服务内补偿、R1 证据自相矛盾、template 过滤晚于分页、compose 首跑前置条件未校验 |

### 1.1 P0 / P1 / P2 问题表

约定：**复现路径**列只写"能在当前仓库/本机复现的最小路径"，每条都指明是我实跑还是代码级判定。

| 编号 | 级别 | 文件:行 | 复现路径 | 影响 | 建议修复 | 回归测试 |
|---|---|---|---|---|---|---|
| **P1-1** | P1（新，未被 R0–R4 记录） | `internal/workflow/engine.go:369-377`（AUTH_EXPIRED→`REAUTHORIZE_REQUIRED`）、`internal/workflow/state/state.go:118-135`（转移矩阵）、`internal/storage/claim.go:242-246`（提交期校验） | 实跑：`.sandbox/review/transition_probe`（`go run .`，side-module 通过 `replace` 引用本模块 internal 包；见 §5.4）。把 run 置于 `CREDENTIAL_VERIFIED`，提交 `REAUTHORIZE_REQUIRED` → `conflict: illegal transition`；run 仍为 `CREDENTIAL_VERIFIED` 且 `lease_owner=probe-worker`；租约到期后**同一步被再次领取**（attempt 2、failures **0**） | `CREDENTIAL_VERIFIED → REAUTHORIZE_REQUIRED` 不在矩阵内：任何在 OAuth 完成后凭据失效（sandbox access token 默认 5 分钟；崩溃/重启后重跑 `verify_credential` 即可触发）的 run，提交被拒→租约保留→每 ~90s 重复执行同一外部步骤（**不计 failure budget**）直到 30 分钟 deadline 被 `SweepDeadlines` 强制 `FAILED_TERMINAL/TIMEOUT_UNCERTAIN`；本应进入"等人工重新授权"的语义丢失，且对上游形成重试风暴 | ①矩阵补 `CREDENTIAL_VERIFIED → {REAUTHORIZE_REQUIRED, Cancelled, RetryWait}` 与 `WAITING_EMAIL → Cancelled`；②引擎对 `ErrConflict`（非法转移）做**终态化**处理：标记 `FAILED_TERMINAL/PROVIDER_BUG` 或映射到 `REAUTHORIZE_REQUIRED`，绝不保留租约；③对"提交被拒"路径计数（`failure_count+1`）以防风暴 | 新增 storage/workflow 测试：对模板中**每个 step × 每个 outcome**断言 `CanTransition(active, next)` 成立（穷举矩阵测试）；新增"提交被拒 → run 不再被重复领取"测试；把 `transition_probe` 场景固化为引擎级测试 |
| **P1-2** | P1（新；R1:166 的 PARTIAL 标注 + R1:363-364 的说明、R2 §9 E2E-008 已记为 PARTIAL，但其"engine 幂等键是主要去重保护"的说法不成立） | (a) `internal/workflow/steps.go:305` → `internal/storage/events.go:149-162` → `steps.go:310`；(b) `internal/provider/mailbox/cloudflare/cloudflare.go:303-321` + `:328-397` | (a) 代码级：`wait_email` 先 `ConsumeMessage`（独立写入 `mail_receipts`，立即提交）→ 再 HTTP 确认 → 最后 `CommitStepResult`。若进程在两步之间死掉，重试时 `MessageConsumed=true` 直接跳过该信，链接永久丢失；(b) 代码级 + 单测：`TIMEOUT_UNCERTAIN` 重试 `prepare_mailbox`（name 相同）→ Cloudflare 409 → 适配器**只做一次** `enablePrefix=true` 恢复，即**新建第二个随机前缀地址**；`tests` 覆盖 `TestCreateAddress_DuplicateRecovery` 等，live 未跑 | (a) 崩溃窗口（约百毫秒级）内的一封验证信被"消费"却未被使用 → run 永远 `WAITING_EMAIL`，30 分钟后 `FAILED_TERMINAL`；只能人工重跑整个 run。(b) 一次"外部已成功、提交未落"的重试会在真实 Cloudflare 上留下**孤儿地址**（配额/清理成本），与 `docs/03-workflows.md:65`"在任何非幂等远端副作用前先读取 provider 当前状态（reconciliation）"相悖；engine 幂等键只作用于**创建 run**，不抑制 step 重试 | (a) 把"消费"与步骤结果放进**同一个事务**（`StepResult` 增加 `ConsumedMessages`，由 `CommitStepResult` 写入 `mail_receipts`），或改为"确认成功后才消费"，并依靠 lease 的串行性避免并发重复；(b) 重复响应时改为**对账**（若 admin API 支持按 name/地址查询则复用既有地址），否则返回 `CONFLICT` 让操作者处理，而不是静默再建一个；如需保留恢复，须把孤儿地址写入资源记录并提供清理策略 | (a) 故障注入测试：在 `ConsumeMessage` 与提交之间注入崩溃（可复用 `mail_receipts` + 重启 harness）→ 断言重启后同一步能重新确认或至少进入可操作状态；(b) httptest 断言"409 后只发一次请求且**不产生第二个 address**"，或断言返回 `CONFLICT` |
| **P1-3** | P1（运维交付前置；R4 已列为 P1-2，本轮独立确认） | `cmd/onboardd/main.go:155-164`（纯 `http.Server`，无 `ListenAndServeTLS`）、`deploy/compose.yaml:57`（`127.0.0.1:8788:8788`） | 实跑：`grep -rn "tls\.\|ListenAndServeTLS" --include=*.go cmd internal` → **0 命中**；`grep -rniE "caddy|nginx|hsts|require_tls"` 只命中文档说明、**无实现/无 ingress 组件**；`docker compose config` 显示仅 `127.0.0.1:8788:8788`（不是缺陷，只是未部署） | 一旦把 8788 暴露到私网/公网而无 TLS 终止，bearer token 与 session cookie 明文传输；仓库无 ingress 组件、无 HSTS、无 `ONBOARD_REQUIRE_TLS` 强制项 | 在 `deploy/` 增加 Caddy/nginx 反代示例（TLS≥1.2 + HSTS）并把"只允许反代终止 TLS"写入部署契约；可选加 `ONBOARD_REQUIRE_TLS=1`（`X-Forwarded-Proto!=https` 时拒绝写操作） | `curl -sI https://<proxy>` 断言 TLS 版本 + HSTS；本环境 NOT_RUN（无 ingress，不得以 mock 代替） |
| **P2-1** | P2 | `internal/ops/scan.go:68`（`scanLong = [A-Za-z0-9+/_-]{48,}`） | 实跑：`scripts/redact_scan.sh --file .sandbox/e2e/run-…/onboardd-1.log` → **exit 1 / findings 3**；三条全部是长路径（`/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding/` 63B）与请求路径（`/v1/runs/<uuid>/events` 52B），`scripts/redact_scan.sh` 自己的 R2 证据即被误报 | 发布门禁不可用：任何真实部署路径 ≥48 字符就 exit 2；R4 §5.4 的"0 findings"只在短路径样本上成立（R4 自己的 P2-8 只说是启发式，未记录此可复现误报） | 扫描前把"文件路径/URL path"从 token 形态判定中排除（例如剔除 `(/[A-Za-z0-9._-]+){3,}` 片段、或在 `long_token` 命中后过滤含 `/` 且无 `=`/`.` 分隔的路径），并提供 allowlist 规则文件 | 把 `onboardd-1.log`（含绝对路径）加入 scanner clean 测试；断言 exit 0 |
| **P2-2** | P2 | `internal/config/config.go:413-419`（`strings.TrimSpace` 后 `DecodeMasterKey`）、`:476-493` | 实跑：`openssl rand 32 > master-key`（首字节 `0x0b`）→ `onboardd` 启动失败 `master key file: master key must be 32 bytes encoded as raw/hex/base64`；改为 64 hex 后正常 | `deploy/secrets/onboarding_master_key.example` 与 `deploy/compose.yaml` 注释都声称接受"32 raw bytes"；随机密钥首/尾字节落在空白字符（约 2–3% 概率）时**开机失败**且报错误导 | 与 `security.LoadKeyRing` 保持一致：先判 `len(raw)==32` 直接使用，再 `TrimSpace` 解析 hex/base64/JSON；错误信息区分"长度不符"与"编码无法识别" | 表驱动测试：32 字节首尾为 `0x00/0x0b/0x0a/0x20` 的 keyring 文件必须可加载 |
| **P2-3** | P2 | `internal/security/crypto.go:283-322`（无状态签名 cookie）、`internal/api/ui.go:238-245`（logout 只清客户端） | 代码级：cookie = base64(payload)+HMAC，服务端无会话表；logout 只 `Set-Cookie MaxAge=-1` | 会话不可吊销：cookie 泄漏后 12h（`SessionTTL`）内一直有效；无"登出全部设备"/强制下线 | 增加服务端会话版本号（存 DB，登出/改 token 时自增）或短 TTL + 刷新令牌；至少在文档中明确"泄漏需轮换 master key/管理员 token" | 测试：登出后旧 cookie 立即 401；轮换 admin token 后旧会话失效 |
| **P2-4** | P2 | `internal/workflow/prepare.go:247-261`（`Summarize` 解密授权 URL）+ `internal/api/server.go:86-94`（GET 全部身份可读） | 实跑：`P9a viewer GET /v1/runs → 200`（`readonly-token`）；代码级：`GET /v1/runs/{id}` 对 `WAITING_USER_ACTION` 返回 `authorization_url`（内含 OAuth `state`） | 共享只读令牌持有人可拿到待授权的完整 URL，从而**抢先完成**该会话的授权（state 单次有效，但抢到即用）；超出"只读看板"的意图 | 授权 URL 只返回给 admin 角色（viewer 返回 403 或置空）；或在 URL 中再加一层一次性确认 | `TestViewerRoleIsReadOnly` 增加断言：viewer 读 waiting run 不含 `authorization_url` |
| **P2-5** | P2 | `internal/workflow/steps.go:388-397`（`senderAllowed` 用 `strings.Contains`）对比 `internal/mailx/mailx.go:590`（严格 `senderRule` 解析） | 代码级：`senderAllowed("noreply@github.com.attacker.tld", ["github.com"])` → true | `cloudflare-mail-smoke` 模板的"已收到邮件"判定可被仿冒域名 From 欺骗（只影响 smoke 成功判定，不跟随链接、不消费凭据）；与 R1-03 声称的"From 伪造被拒"不一致（那条只针对 mailx 路径） | 复用 `mailx` 的发送者规则解析（精确地址/域/子域），或至少做 `strings.HasSuffix(lower, "@"+allowed)` 边界匹配 | 表驱动测试：`noreply@github.com.attacker.tld`、`github.com.evil` 必须拒绝 |
| **P2-6** | P2 | `internal/storage/runs.go:111-125` + `:448-458`（`isUniqueConstraint` 匹配任意 `"constraint failed"`） | 代码级：把 CHECK/FOREIGN KEY 违例当作幂等冲突 → 进入 `getRunTx(owner,template,key)` 查找 → 查不到时返回 `ErrNotFound`，API 变成 500 而不是明确的校验错误 | 未来 schema/校验变更时错误语义被吞掉，排障困难 | 只匹配 `UNIQUE constraint failed`；其他约束错误原样上抛并分类 | 测试：插入违反 CHECK 的 run 参数 → 断言返回明确错误而非 500 |
| **P2-7** | P2 | `internal/workflow/state/state.go:97`（`CancelRequested: {Cancelled, Completed, FailedTerminal}`）、`:118-135` | 实跑（probe 输出）：`matrix CANCEL_REQUESTED->COMPLETED: <nil>`；`WAITING_EMAIL->CANCELLED`、`CREDENTIAL_VERIFIED->CANCELLED` 均非法 | `CANCEL_REQUESTED→COMPLETED` 与"取消优先于在途结果"（`claim.go:231-238`）语义相反，属**当前不可达但危险**的死边；缺 `→Cancelled` 的边会让 `ClassCancelled` 的提交同样落入 P1-1 的"非法转移→无限重试" | 删除 `CancelRequested→Completed`；补齐各非终态到 `Cancelled` 的边（与 `classifyError(ClassCancelled)` 对齐） | 矩阵穷举测试：终态无出边、`CANCEL_REQUESTED` 只能到 `CANCELLED`（/`FAILED_TERMINAL`） |
| **P2-8** | P2 | `internal/provider/mailbox/mailpit/mailpit.go:301`（`DeleteAddress` 已实现）但全仓**无调用方**（`grep -rn "MailboxCleaner\|DeleteAddress" internal cmd \| grep -v _test` 只命中定义与 fake） | 代码级：取消/失败/deadline 超时后，sandbox 身份与邮件只由**测试 harness** 清理（R2 §7 明说 teardown 删除） | 生产式取消会在自控 Keycloak 里留下测试用户、在 Mailpit 留下邮件；`docs/03-workflows.md:70` 要求"测试资源按显式策略补偿"未在服务内实现 | 增加 `cleanup` 步骤/补偿队列（仅对 sandbox 与 mailpit 生效，Cloudflare 明确不自动删除真实地址，保持现状） | E2E：取消后断言 sandbox 身份被服务（非测试）删除；记录补偿事件 |
| **P2-9** | P2 | `docs/acceptance/R1.md:139`（R1-18 写 `GET /api/v1/messages/{id}` PASS）与同文件 `:422-429`（§14 集成补丁：原路径 404、真实端点为单数）自相矛盾；代码实际为 `mailpit.go:348` 单数 | 实跑：`grep -n "messages/{" docs/acceptance/R1.md` → 命中 L139；`grep -n "message/" internal/provider/mailbox/mailpit/mailpit.go` → `:348 /api/v1/message/`；另外 R1-40 行（`:166`）引用的“§12.3”在文中不存在（§12 是“外部观察”） | 测试真实性瑕疵：R1 的**单测 mock 曾编码了错误端点**，因此 R1-18 的 PASS 不能证明对真实 Mailpit 正确；文档内部不一致会让后续读者误判 | 修正 R1-18 表格行并注明"该端点由 R2 真实夹具纠正，单测 mock 同步修正"；修正 R1-40 的悬空 §12.3 引用；把"mock 与真实服务端点/字段不一致"列为 R1 的经验教训 | 文档一致性检查（R1-18 与 §14 集成补丁、R2 §8 三处描述一致） |
| **P2-10** | P2 | `internal/api/runs.go:103-125`（`handleListRuns` 在分页后按 template 过滤） | 实跑：`GET /v1/runs?limit=100` 正常；代码级：template 过滤发生在 `ListRuns(limit+1)` **之后** | 指定 template 时页大小变短、`next_cursor` 依据未过滤页生成，翻页语义不稳（不丢数据但会多翻） | `RunListFilter` 增加 `Template` 并在 SQL 中过滤 | 测试：20 条混合模板，`?template=x&limit=5` 每页 5 条、无重复/遗漏 |
| **P2-11** | P2 | `deploy/compose.yaml:63-84`（volume/secrets 未预检）、`deploy/data`、`deploy/secrets/{onboarding_master_key,onboard_admin_token}` 均不存在（只有 `*.example`） | 实跑：`docker compose -f deploy/compose.yaml config` → **0**（`config` 不校验 secret 文件存在）；`ls deploy/secrets` → 仅 `*.example` | 首次 `docker compose up` 会因缺少 secret 文件失败；若 Docker 自动创建 bind mount 目录，目录属 root，容器内 UID 10001 无法写 SQLite（compose 注释已提示 `chown`，但无预检脚本） | 增加 `deploy/preflight.sh`：校验 keyring 32B/格式、admin token 非空、`deploy/data` 属主 10001、权限 0600/0700；在 README 首跑段落前置执行 | shellcheck + 有 daemon 的 CI/flake 中跑 `preflight`（当前 environment 只能 NOT_RUN） |

> 已在 R4-audit 中记录、本轮独立复核但不重复计数的项：TLS ingress（R4 P1-2 = 本文 P1-3）、
> 基础镜像未 pin digest（R4 P2-6）、`TRUST_PROXY_HEADERS=1` 可伪造（P2-9）、viewer 可读 audit（P2-11）、
> 轮换/恢复需停机但工具无法证明进程已停（P1-5）、JSON API 无 CSP（P2-4）、限流器进程内（P2-10）。
> 这些条目的描述与代码**相符**，我未发现夸大或隐瞒。

### 1.2 修复优先序

1. **P1-1**（最小改动、最高收益）：补齐转移矩阵 + 引擎把"提交期非法转移"变成可终止结果/计数。这一条同时为 P2-7 提供结构性的兜底。
2. **P1-2a**：`mail_receipts` 消费与步骤提交原子化（或确认成功后再消费），消灭"邮件已消费但 run 卡死"。
3. **P1-2b**：Cloudflare 重复地址改"对账或 CONFLICT"，停止静默新建第二个地址（真实平台配额/清理成本）。
4. **P1-3**：TLS ingress 契约 + 反代示例 + `RequireTLS`（交付前置，可并与 R4 收口）。
5. **P2-2 → P2-1 → P2-3/P2-4**：三处小修（开机失败概率、发布门禁可用性、会话与授权 URL 暴露面）。
6. **P2-5 … P2-11**：按表中建议处理；P2-9 属证据卫生，建议与 R1 文档一起修。

> 修复后必须重跑 §2 的全部门禁（尤其 `go test ./... -race` 与 `ONBOARD_SANDBOX_E2E=1`），并为
> P1-1/P1-2 增加**故障注入**回归用例；不能用单测替代真实夹具证据。

---

## 2. 真实执行的命令与退出码（全部在本机实跑）

> 证据落盘：`.sandbox/review/logs/`（`gates.log`、`e2e-rerun.log`、`e2e-rerun-v.log`、`e2e-offline-v.log`、
> `probe.log`、`onboardd.log`、`redirect-hits.jsonl`）；备份/恢复/轮换证据在 `.sandbox/review/backups/`、`run/`。
> 下列命令均在仓库根目录执行。

| # | 命令 | 退出码 | 结果摘要 |
|---|---|---:|---|
| 1 | `gofmt -l $(find cmd internal -name '*.go')` | **0** | 无输出（已格式化） |
| 2 | `go build ./...` | **0** | 全仓可编译 |
| 3 | `go vet ./...` | **0** | 无输出 |
| 4 | `go test ./... -race -count=1` | **0** | 29 个包：**14 个有测试的包全部 `ok`**（api 2.2s、ops 5.5s、workflow 6.4s、provider/…均 ok），15 个 `[no test files]`。macOS 链接器 `LC_DYSYMTAB` warning 为环境噪声，不影响结果（`go test -list` 计 288 个顶层 Test） |
| 5 | `go test ./tests/e2e -count=1 -v`（未 opt-in） | **0** | 4 个 E2E **SKIP**（不是 PASS）：crash/five-identities/idempotency/cancel |
| 6 | `source tests/sandbox/env.sh && ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1 -timeout 900s` | **0** | **4/4 PASS**，`ok … 11.248s`（详见 §3） |
| 6b | 同上 `-v` | **0** | `TestCrashRecoveryRestartsInWaitingEmail 7.53s` / `TestFiveSandboxIdentities 0.97s` / `TestIdempotencyKeyCreatesOneRun 0.40s` / `TestCancelWhileWaitingEmail 0.42s` |
| 7 | `docker compose -f deploy/compose.yaml config` | **0** | 静态清单合法：`host_ip: 127.0.0.1`、`read_only: true`、`user: 10001:10001`、`cap_drop: ALL`、`no-new-privileges`、`pids_limit: 128`、`mem_limit: 256m`、`tmpfs /tmp noexec` |
| 8 | `docker info` | **1** | daemon 未运行 → Docker build/run 的 **NOT_RUN** 直接证据（未尝试启动 Docker） |
| 9 | `docker compose version` | **0** | CLI v5.5.0 可用（仅 CLI） |
| 10 | `scripts/backup.sh`（对**运行中的 WAL 库**，WAL 523 KB / 主库 131 KB） | **0** | 快照 `state-20261024T140333Z.sqlite`；`integrity_check=ok`、FK 0、`runs=4 events=54 secrets=3`（证明 WAL 内容被纳入） |
| 11 | `scripts/restore.sh --snapshot … --target … --force` | **0** | `secrets_verified=3`（逐行 AES-GCM 解密）、counts 与 manifest 完全一致、post-restore `integrity_check=ok` |
| 12 | `go run ./internal/ops/cmd/rotate -db … -keyring <仅 v2 的错 keyring>` | **1（预期 fail-closed）** | `secret … needs key version 1 which is unavailable or wrong; transaction rolled back`；随后 DB 仍为 3 行 `key_version=1`（未被破坏） |
| 13 | `go run ./internal/ops/cmd/rotate -db … -keyring <v1+v2> -dry-run` | **0** | `total_secrets=3 planned=3 already_current=0`（dry-run 零写入） |
| 14 | `scripts/redact_scan.sh --file …/onboardd-1.log` | **1**（内部工具 exit 2） | 3 findings，全部为长路径/请求路径误报（P2-1）；未回显原值 |
| 15 | scratch `onboardd`（127.0.0.1:18790，fake providers + mailpit 指向诱饵服务）+ 16 项对抗探针 | — | **15 PASS / 1 探针预期修正**（见 §5） |
| 16 | `.sandbox/review/transition_probe`（side-module 过渡态探针） | 0（脚本）/ 输出非法转移 | 复现 P1-1：`conflict: illegal transition CREDENTIAL_VERIFIED -> REAUTHORIZE_REQUIRED`、租约保留、重复领取 attempt 2 / failures 0 |

**未执行（NOT_RUN，与环境/授权一致，未用 mock 冒充）**：Docker build/run、GHCR、真实 Cloudflare
（无凭据）、真实 GitHub（无 PAT）、真实 WorkBuddy（无书面许可）、Playwright 浏览器自动化、TLS ingress、
多进程/多实例租约 fencing、Keycloak 邮件 120s 延迟注入、跨 CI runner 执行。

---

## 3. 七个审查域的对抗性结论

### (a) 任务幂等

**结论：run 级幂等 = PASS（实测）；step 级外部副作用幂等 = 有缺口（P1-2）。**

- 唯一约束真实存在：`internal/storage/migrations/0001_initial.sql:26` `UNIQUE(owner_id, template, idempotency_key)`，
  以及 `steps.operation_key` 唯一（同文件 `:45`）、`resources` 唯一（`:70`）、`mail_receipts` 唯一（`0002:21`）。
- 创建路径在**一个 `BEGIN IMMEDIATE` 事务**里插入 run + steps + `RUN_CREATED`（`runs.go:92-148`），
  冲突时比较 `request_fingerprint`（`prepare.go:75-79`）：同 fingerprint 返回既有 run，不同 fingerprint 返回 409。
- 实测（§5）：**8 个并发** `POST /v1/runs` 同 `Idempotency-Key` → 8×202、**同一 run id**、DB 只有 1 条 run；
  同 key 换 body → 409；**重启进程后重放同 key** → 202 + **原 run id**（DB 仍 4 条 run = 4 个唯一 key）。
- lease fencing 真实有效：`claim.go:147-158` 写入 `lease_owner/lease_until/fencing_generation`；
  `CommitStepResult` 先比对 `(lease_owner, fencing_generation)`（`claim.go:216-241`），不匹配即 `ErrLeaseLost`；
  `TestClaimLeaseAndFencing`、`TestConcurrentClaimsOnlyOneWins`（8 worker 只 1 个领到）在 `-race` 下通过。
- **但**：fencing 只保证"旧 worker 不能提交"，不保证"外部副作用只发生一次"——`prepare_mailbox`、
  `request_verification`（Keycloak 注册/发信）、`wait_email` 的邮件消费都可能在"外部已成功、提交未落"的窗口被
  重试（P1-2）。`operation_key` 被写入 DB 但**没有任何地方用它去抑制重跑**。

### (b) 原子性 / 崩溃窗口 / OAuth state 重放

**结论：HTTP 不在事务内 = PASS；崩溃窗口 reconcile = 有缺口（P1-2a）；OAuth state 重放 = 未发现可利用路径（PASS，live NOT_RUN）。**

- "DB 事务不跨 HTTP"是**代码结构**保证的：engine 只在 `Tick` 里 claim（`claim.go:40-164`）→ `execute`
  （`engine.go:256-333`，纯外部 I/O）→ 新事务 `CommitStepResult`（`claim.go:201-317`）。全仓非测试代码共 **10 处** `BeginTx`（9 处 storage 短写事务 + 1 处 ops 轮换），没有任何 `BeginTx`
  包住 provider 调用。
- 崩溃恢复在**已提交步骤**上被真实证明：`TestCrashRecoveryRestartsInWaitingEmail` SIGKILL → 同 data dir +
  master key 重启 → `COMPLETED`，`request_verification` attempts 仍为 1（未重复注册），`keycloak_users_after=1`。
- 未覆盖的窗口（P1-2a）：`wait_email` 的"消费邮件"（`steps.go:305`）先于"确认链接 + 提交结果"落库；
  崩溃会让链接永久不可用。R2 §9 已诚实标注 E2E-008 为 PARTIAL，但未识别该具体机制。
- OAuth state：sandbox 驱动每次 `AuthorizeOIDC` 生成新的 `state/nonce/PKCE`（`oidc.go:54-100`），
  从邮件提取的链接必须先通过 `parseActionTokenURL`（`oidc.go:498-527`：http(s)、无 userinfo、
  **path 含 action-token 段**、**同 loopback 服务 + 同端口**）才会发请求；WorkBuddy 侧 state 只存在
  加密 secret 里、`Poll` 有 terminal cache 防重放（`workbuddy.go:288-403,654-728`），`dropSession` 一次性删除。
  未发现 state 明文落盘（见 (d)）。**workbuddy live 无法执行 → 重放防护仅为代码级 + 单测证据**。

### (c) 身份安全（SSRF / redirect / CSRF / cookie / RBAC / traversal / 限额 / 限流）

**结论：PASS。** 逐项证据：

- **provider base URL（SSRF）**：配置层 allowlist（`config.go:565-609`：cloudflare=`api.moonbyte.top`、
  mailpit/keycloak=`127.0.0.1|localhost|::1`、github=`api.github.com`、workbuddy/oidc=loopback/RFC1918），
  **每个适配器构造时再校验一次**（`cloudflare.go:800-820`、`mailpit.go:parseBaseURL`、`keycloak.go:571-580`、
  `github/config.go:129-180`、`workbuddy.validateBaseURL`），并拒绝 userinfo/query/fragment/非 http(s)。
  实测：把 provider 名写成 URL（`http://127.0.0.1:9/evil`）→ 400，registry 只做名字查找，无任何取 URL 行为。
- **跨主机 redirect 凭证泄漏**：四个 live 适配器都有 `CheckRedirect` 同源守卫
  （`cloudflare.go:687-699`、`mailpit.go:448-462`、`github.go:74-86`、`workbuddy.go:100-112`），
  keycloak 的 `redirectGuard` 还额外在应用回调处**停止**（`keycloak.go:314-336`）。真实 HTTP 复验（§5 P10）：
  把 mailpit 指向会 302 到 `127.0.0.1:18792` 的诱饵服务，诱饵目标收到 **0** 个请求，run 以
  `PROVIDER_BUG / "mailpit attempted a cross-origin redirect"` 终止。github 的
  `TestCrossHostRedirectRefused` 进一步断言目标 0 请求且无 `Authorization`（`-race` 通过）。
- **CSRF / Origin**：`apiAuth` 对 cookie 认证的写操作强制 `csrfValid`（`server.go:235-240,269-281`），
  且 `sameOriginMutation` 要求 `Origin` 或同 host `Referer`（`server.go:294-303`）。实测：
  cookie+正确 CSRF 但无 Origin → **403**；cookie+CSRF+`Origin: https://evil.example` → **403**；
  cookie+CSRF+同源 Origin → **202**（对照）；`/ui/runs` 表单无 CSRF → **403**。
- **Cookie 属性**：`HttpOnly`、`SameSite=Strict`、`Path=/ui`（`ui.go:215-223`）；TLS 或
  `TRUST_PROXY_HEADERS=1`+`X-Forwarded-Proto=https` 时 `Secure`（`server.go:324-329`）。明文 loopback 不设
  `Secure` 是预期（P2-3/TLS 相关）。
- **RBAC**：`ONBOARD_READONLY_TOKEN` 只读（`server.go:230-234`）。实测 viewer：`GET /v1/runs` 200、
  `POST /v1/runs` **403**；未知 token 401；admin token 全量。
- **路径穿越**：所有来自 provider 的 ID 都 `url.PathEscape`（`mailpit.go:348,356`、`cloudflare.go:514`、
  `keycloak/admin.go:161,220,234,254,262`）；realm 有字符白名单（`keycloak.go:583 parseRealm`）；
  workbuddy uid 只允许 `[A-Za-z0-9_-]`（`workbuddy.go:638-653`，并有 `done true with path traversal uid` 单测）。
- **请求体/未知字段**：64 KiB 上限 + `DisallowUnknownFields`（`server.go:427-441`）。实测 70 KB body → 400；
  `account_count` 字段 → 400（结构性禁止"批量建号"类扩展）。
- **限流**：按客户端 IP 的 60s 固定窗口（`ratelimit.go:95-113`），general 300/min + login 10/min，
  429 带 `Retry-After`。实测登录桶：`401×9 → 429×3`，`Retry-After: 59`。
- 遗留 P2：P2-5（smoke 的 From 预过滤）、P2-4（viewer 可读授权 URL）、P2-3（会话不可吊销）。

### (d) 保密性（Cloudflare 密码 / address JWT / GitHub token / WorkBuddy token / OTP / OAuth state）

**结论：PASS（无 P0/P1 泄漏）。** 证据分三层：

1. **静态**：全树 grep（`eyJ…`/`ghp_`/`sk-`/`AKIA`/`BEGIN … PRIVATE KEY`/`xox[baprs]-`）
   只命中 `*_test.go` 的占位值（`internal/provider/github/github_test.go:23` 等）；无 `.env`/密钥文件被跟踪，
   `.sandbox/`、`deploy/secrets/*`、`*.sqlite` 均在 `.gitignore`/`.dockerignore` 中。
2. **运行态（我用 debug 级别跑真实服务）**：`admin-token`/`readonly-token`/`master-key` 三个真实值在
   `.sandbox/review/logs/onboardd.log` 中出现次数均为 **0**；日志经 `redactingHandler` 统一脱敏
   （`logging.go:24-45`），访问日志只记 path（`server.go:148-175` + `SanitizeURL` 去 query）。
3. **持久层语义**：scratch DB 的 `secrets` 表是 AES-256-GCM 密文（`sandbox_password` 行 `44 B` 密文、
   `key_version=1`，AAD 绑定 kind|id|version），不是明文；`resources` 只存 `external_id_hmac` + 不透明
   `secret://`；`events.public_payload_json` 经**严格 allowlist**（`redact.go:73-91`），实测 dump 只有
   `provider/resume_state/operation_key/attempt/failure_class/next_retry_at/deadline_at/reason` 等键，
   无邮件正文/链接/OTP/state/token；`steps.result_json` 同样过 redactor。
4. **事件与错误的公开面**：`errs.SummaryOf` 只返回发布安全的 summary（`errs.go:96-109`），provider 错误
   文本只含状态码/操作名（如 `cloudflare returned HTTP 503`），不含响应体。
5. **address JWT / gateway token**：只以不透明 ref 形式出现在 run 资源里，真实值仅加密存储；Cloudflare
   适配器写入前替换掉 secret-store 错误原因，避免回显明文（`cloudflare.go:381-383`）。
6. `redact_scan` 的 3 条命中全部是**长路径误报**（P2-1），不是凭据；我逐条打印了匹配子串确认。

### (e) 平台边界（隐藏注册 / CAPTCHA / 指纹 / 代理轮换 / 多账号 / WorkBuddy 真实调用）

**结论：PASS，无 P0。** 逐项检索与代码确认：

- 关键词扫描（`captcha|datadome|anti-bot|fingerprint|proxy rotat|multi-account|bypass|solve|register.*bot`）
  在非设计包代码中**零命中**（唯一 `FingerprintKey` 是 run 请求指纹的 HMAC key，`main.go:146`）。
- **没有第三方账号创建**：GitHub 适配器仅 `POST/GET /user/emails`（`do()` 有端点 allowlist，
  `github.go:384`；`AddEmail` 只给"已有 token 的账号"加地址，且要求真人确认邮件）；WorkBuddy 适配器只做
  设备授权 start/poll（不创建账号）；Keycloak 的 `createUser` 只在 **loopback 沙箱** adapter 中可达
  （`keycloak.go:571-580` 拒绝非 loopback base URL；`identity.go:115` 仅由 sandbox driver 调用）。
- Fake provider 不做任何网络 I/O（`internal/provider/fake/fake.go` 无 `net/http` 引用）。
- 依赖面最小：`go.mod` 只有 `mattn/go-sqlite3`；全仓非测试代码无 `os/exec`/`net.Dial`/裸 `http.Get`。
- WorkBuddy 真实 adapter：默认 disabled + 必须 `ONBOARD_WORKBUDDY_PERMISSION_ACK` + 仅 loopback/RFC1918
  （`config.go:599-609`、`live.go:118-140`）。实测无 live 适配器时 `/v1/providers` 返回
  `workbuddy enabled=false` 且请求 run 得到 **403 NOT_AUTHORIZED**（在 network 之前失败）。
- 未发现任何免费额度轮转、验证码求解、指纹伪装、代理池或登录态窃取代码。

### (f) 测试真实性

**结论：PASS（R2 5/5 为真实 LOCAL SANDBOX 运行，我独立复现）；Fake / Live / WorkBuddy 三层分离清晰；有 1 处证据瑕疵（P2-9）。**

- **独立复跑**：`source tests/sandbox/env.sh && ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1` →
  `4/4 PASS, ok 11.248s`（`-v` 版同样 4/4）。这不是缓存：本轮新生成 4 个证据目录
  `.sandbox/e2e/run-20261024T1359{07,15,16,17}-*`，其中 `evidence-five-identities.json` 记录
  `generated_at=2026-10-24T13:59:16Z`、`completed=5`、`keycloak_window_users=5`、`p50=727ms/p95=927ms`；
  crash 证据目录含 `onboardd-1.log`+`onboardd-2.log`（SIGKILL→重启）。
- **夹具真实性**：`tests/sandbox/status.sh` 显示真实 Keycloak 26.8.1（`/health/ready` 200、
  realm `moonbyte-test`、SMTP→`127.0.0.1:1025`、admin/服务账号 token 可用）+ 真实 Mailpit v1.32.0；
  测试结束后 `realm users: 0`。
- **三层分离**：`fake-*` provider 仅用于离线单测/quickstart；Cloudflare 是 `cloudflare-integration`
  mode 且 live smoke 默认 skip（`live_smoke_test.go` 显式门禁）；WorkBuddy 是 `oauth/workbuddy(disabled)`；
  R2/R3 报告都把"真实平台"标为 NOT_RUN。README/README-per-phase 的 PASS 均写明范围。
- **未 opt-in 时是 SKIP 而非 PASS**（我实测 `-v` 输出 4 个 `--- SKIP`），没有把 skip 冒充成功。
- **诚实性加分**：R2 §8 主动记录首轮 5 个 run 失败、三个真实夹具缺陷、修复过程，并保留失败轮次 SQLite/日志。
- **瑕疵**：R1-18 表格行仍写 `GET /api/v1/messages/{id}` PASS，与同文件 §14 集成补丁（`:422-429`）矛盾（P2-9）。
  这说明 R1 的 mock 曾在错误的端点上自洽通过，live 夹具才暴露——正好印证"单测不能替代 live 证据"。

### (g) 运维（compose / secret / 备份 / 轮换 / Docker 口径）

**结论：PASS（静态与离线可执行项）+ 明确的 NOT_RUN。** 见 §2 命令 #7–#13 与 §5.3：

- **Compose**：`docker compose config` 输出显示 `host_ip: 127.0.0.1`（仅回环发布）、`read_only: true`、
  `user: 10001:10001`、`cap_drop: [ALL]`、`security_opt: no-new-privileges`、`pids_limit: 128`、
  `mem_limit: 256m`、`tmpfs /tmp noexec,nosuid`；两个 secret 以**文件**挂载，密码不在 env；
  容器内 `ONBOARD_ALLOW_PUBLIC_LISTEN=1` 只为允许容器内 0.0.0.0 监听（host 侧仍 loopback）。
- **镜像**：Dockerfile 多阶段、`USER 10001:10001`、`HEALTHCHECK` 走 `/app/onboardd healthcheck`、
  `.dockerignore` 排除 `.sandbox/`、`data/`、`secrets/`、`*.sqlite*`（不会把测试凭据/库烤进镜像）。
  未 pin 基础镜像 digest（R4 P2-6，仍有效）；**build/run 未执行**（daemon 未运行）。
- **SQLite 备份含 WAL**：`VACUUM INTO`（`store.go:304-318`、`ops/backup.go:174`）在**运行中**对 WAL 523 KB
  的库取证，快照 census 与清单一致；恢复端逐行解密验证（`secrets_verified=3`），错误 key/篡改 fail-closed。
- **轮换 fail-closed**：缺失 key version 时整个事务回滚、数据不变（实跑 exit 1 后 DB 仍 3 行 v1）；
  dry-run 零写入（`planned=3`）。
- **Secret 与 DB 分离**：`scripts/backup.sh` 与 `restore.sh` 要求单独传 keyring，工具不把 keyring 写进快照目录。
- **未完成的运维项**：TLS ingress（P1-3）、首跑预检（P2-11）、轮换/恢复停机编排（R4 P1-5）。

---

## 4. 关键正向证据（用于判定"不是纸面实现"）

| 断言 | 证据（我实跑/实读） |
|---|---|
| run 级幂等 + 跨重启 | 8 并发同 key → 1 run id；重启后重放 → 原 run id；DB `UNIQUE(owner,template,idempotency_key)` |
| 租约 fencing 排除过期执行者 | `claim.go:216-241` owner+generation 双校验；`TestClaimLeaseAndFencing` / `TestConcurrentClaimsOnlyOneWins` 在 `-race` 下 PASS |
| 单条写连接 + BEGIN IMMEDIATE | `store.go:99,137-143`；迁移/创建/领取/提交全部走该连接 |
| HTTP 不在事务内 | `engine.go:210-239`（claim→execute→commit 三段）；全仓 10 处 `BeginTx` 均为短写事务 |
| 5/5 身份真实完成 | 我复跑：4/4 PASS；新证据 `completed=5, keycloak_window_users=5`，realm teardown 归零 |
| CSRF/Origin 真拦截 | 我实测 403/403/202/403 四组对照 |
| 跨源 redirect 不转发请求 | 我实测诱饵目标 0 命中 + run `PROVIDER_BUG`；github 单测断言无 Authorization |
| 限流 + Retry-After | 我实测 `401×9 → 429×3`，`Retry-After: 59` |
| 秘密零泄漏 | 三个真实 token 在 debug 日志命中 0；secrets 表为密文；events allowlist 实测 |
| 备份/恢复/轮换 | 我实跑 exit 0/0/1(fail-closed)/0，含逐行解密与数据不变证明 |
| 平台边界 | 全仓无 CAPTCHA/指纹/代理/多账号代码；Keycloak 用户创建仅 loopback 沙箱；GitHub 只 `/user/emails` |

---

## 5. 我自建的对抗性探针（细节与原始输出）

### 5.1 scratch 服务与探针（`.sandbox/review/`）

- 真实二进制：`go build -o .sandbox/review/onboardd ./cmd/onboardd`（未改任何源码）。
- 配置：`127.0.0.1:18790`、独立 SQLite、独立 64-hex master key 与 admin/viewer token（`openssl rand`，0600）、
  `ONBOARD_FAKE_PROVIDERS=true`、`ONBOARD_LOG_LEVEL=debug`、并启用 `mailpit` 指向 **诱饵 HTTP 服务**
  `127.0.0.1:18791`（对 `/api/v1/messages` 返回 302 → `127.0.0.1:18792`）。
- 结果：**15 PASS / 1 期望修正**（`probe.log`）：
  `P1` 并发幂等 PASS、`P2` 409 PASS、`P3` 缺 key 400 PASS、`P4` `account_count` 400 PASS、
  `P5` 70 KB 400 PASS、`P6` provider 名写成 URL → 400 PASS、`P7`（当次配置下 live provider 未注册 →
  400 "unknown mailbox provider"；随后用 `FAKE_PROVIDERS=false` 的第二实例复验"已注册但 disabled" →
  **403 NOT_AUTHORIZED**）、`P8a–e` CSRF/Origin PASS、`P9a/b` viewer RBAC PASS、`P11` 登录限流 PASS、
  `P10` 跨源 redirect 目标 0 命中 PASS。
- 额外：`P12` 两个 fake run 全部 `COMPLETED`，`events` 只含白名单键。

### 5.2 缺陷复现：`CREDENTIAL_VERIFIED → REAUTHORIZE_REQUIRED`

`.sandbox/review/transition_probe/`（独立 module，用 `replace` 引用本仓库以访问 `internal/*`，
**不写入仓库源码**；`go run .`）原始输出：

```text
created run status: CREATED
claimed step: verify_credential state-after-claim: CREDENTIAL_VERIFIED
commit CREDENTIAL_VERIFIED -> REAUTHORIZE_REQUIRED: err=conflict: illegal transition CREDENTIAL_VERIFIED -> REAUTHORIZE_REQUIRED
run status after rejected commit: CREDENTIAL_VERIFIED (lease_owner="probe-worker") | step failures=0 attempts=1
lease expiry re-claimed the same step: verify_credential attempt=2 failures=0 (no budget consumed)
matrix CREDENTIAL_VERIFIED->REAUTHORIZE_REQUIRED: illegal transition CREDENTIAL_VERIFIED -> REAUTHORIZE_REQUIRED
matrix WAITING_EMAIL->CANCELLED: illegal transition WAITING_EMAIL -> CANCELLED
matrix CREDENTIAL_VERIFIED->CANCELLED: illegal transition CREDENTIAL_VERIFIED -> CANCELLED
matrix CANCEL_REQUESTED->COMPLETED: <nil>
```

### 5.3 备份 / 恢复 / 轮换（真实文件，非 mock）

```text
backup: DB（运行中，WAL 523 KB）→ state-20261024T140333Z.sqlite
  counts: runs=4 steps=22 events=54 resources=10 secrets=3 mail_receipts=3 audit_log=25
  checks: manifest_snapshot_name,size_bytes,sha256,integrity_check=ok,foreign_key_check=0,schema_version=2,counts
restore: integrity_check=ok, foreign_key_violations=0, secrets_verified=3, keyring_current_version=1
rotate（错 keyring）: ERROR … needs key version 1 which is unavailable or wrong; transaction rolled back  → 数据仍 3 行 v1
rotate（dry-run v1+v2）: total_secrets=3 planned=3 already_current=0
```

### 5.4 说明

上面的 scratch 服务、诱饵服务与过渡态探针**不是**产品代码的一部分；仓库源码零改动
（`git status` 仍为初始未跟踪状态，我只新增了本文档与 `.sandbox/review/**`）。

---

## 6. 我认为不能算 PASS 的项（避免自欺）

1. 任何"真实平台成功"：Cloudflare live、GitHub live、WorkBuddy live 均 **NOT_RUN**，R1–R3 也没有把它们
   包装成 PASS（范围写法正确）。
2. Playwright 浏览器自动化：**NOT_RUN**（D5 偏差），Go OIDC 驱动是真跑，但不能等价为"浏览器 UI 可用"。
3. Docker build/run、GHCR、TLS ingress、多实例 fencing、120s 邮件延迟注入、E2E-007 跨进程租约：**NOT_RUN**。
4. 崩溃窗口"外部成功后、提交前"（E2E-008）：R2 标 PARTIAL，我确认**未**精确注入；并且发现该窗口与
   `mail_receipts` 先消费的语义叠加会造成不可恢复（P1-2a）。
5. 单测覆盖不等于真实：R1-18 的 mock 曾编码错误端点（P2-9）就是反例。

---

## 7. 首轮最终状态（快照，已被 §8 复核结论取代）

```
PASS / PARTIAL / FAIL  →  PARTIAL   # 2026-10-24T14:12Z 首轮判定
```

**理由**：

- 交付物是真实可运行、可复跑、证据可核对的实现：全部门禁 exit 0、R2 5/5 我独立复现、备份/恢复/轮换真实执行、
  CSRF/SSRF/redirect/RBAC/限流探针全部按预期拦截、无 P0、无凭据泄漏、平台边界干净。
- 存在 **2 项代码级 P1**（状态机提交缺口导致的外部步骤重复执行；外部副作用的重试/崩溃窗口不对账），
  它们恰好落在项目最核心的卖点（崩溃可恢复 + 幂等编排）上；另有 1 项已声明的 TLS 交付前置 P1。
  因此不能判 PASS；也不能判 FAIL——没有"可发布性被破坏"的 P0，绝大多数能力有真实证据。

**升到 PASS 的条件**（建议作为交付前收口项）：

1. 修复 **P1-1**：补齐 `state.go` 转移矩阵 + 引擎对提交期 `ErrConflict` 的终止化/计数处理，并加"每个
   step × outcome 合法"的矩阵测试。
2. 修复 **P1-2**：`mail_receipts` 消费与步骤提交原子化（或确认成功后再消费）；Cloudflare 重复地址改为
   对账/`CONFLICT`，不再静默建第二个地址；各加 1 个故障注入回归。
3. **P1-3**：给出 TLS ingress 契约与可执行示例（或明确 `ONBOARD_REQUIRE_TLS`），或经 Lead 书面确认
   "仅回环自用、不对外暴露"的交付边界，并从 P1 降级为 NOT_RUN 前置项。
4. 顺手修 **P2-2**（一行）与 **P2-1**（发布门禁可用性），并修正 **P2-9** 的 R1 证据行。
5. 全部修完后重跑 §2 的 #4、#6、#10–#13 并更新 `docs/acceptance/*`；本文档保持"审查时快照"，由 Lead
   在最终报告中引用其结论与残余风险。

---

## 8. Post-review verification (Lead fixes) — 2026-10-24T14:16Z–14:35Z

### 8.1 复核口径与可复现性（先说清一个坑）

- **复核窗口**：`2026-10-24T14:16Z–14:35Z`（本机 CST `22:16–22:35`）。审查者未修改任何代码，
  只新增本节与 `.sandbox/review/**` 证据。
- **第一次复核（14:17Z）失败，原因是并发编辑，不是产品缺陷**：`go test ./... -race` 报
  `TestCredentialExpiryAfterOAuthRequiresReauthorization` FAIL。核对文件时间戳可知当时
  `internal/workflow/engine_test.go`（22:17:48）与 `internal/workflow/state/state.go`（22:18:09）正在被 Lead 改写，
  失败日志里引用的 `engine_test.go:472 "tick=..."` 在当前文件中已不存在 → 我编译运行的是"半新半旧"的混合快照。
  该轮结果**作废**，不作为任何结论依据（记录在此以免读者误解）。
- **最终快照（可复现）**：对 `internal/ cmd/ tests/ deploy/ contracts/ scripts/` +
  `go.mod/go.sum/Makefile/Dockerfile/.github/` 共 **126 个文件**做 SHA-256 清单，
  门禁运行**前后各一次**比对：`TREE_STABLE: True`（changed=0 / added=0 / removed=0）。
  清单：`.sandbox/review/logs/tree-hash-before.json`、`tree-hash-after.json`。
  本节所有数字都来自该冻结快照（`gates3.log`/`gates4.log`/`e2e-*-3.log`/`p1-probe-reverify.log`）。
- **交付树（22:40Z 最终一致性检查）**：与上述冻结快照相比，**只有 2 个文件**发生变化——
  `internal/workflow/state/state.go` 与 `internal/workflow/state/state_test.go`（22:29:58Z，追加
  `CANCELLED` 出边；无其他代码改动，diff 由 126 文件 SHA-256 清单比对确认）。
  - `state.go` sha256 = `ed1d689506ca275a32d07f89511cc305794cb97ba17e7ac63be5ec6ae2242188`
  - 126 文件 manifest sha256 = `1a65ea11f2859c03ae18a28d11c2e5581288930d6376b3533a96366c9296186e`
    （清单算法：`sha256(concat(sorted("<path> <sha256>\n")))`；原始清单
    `.sandbox/review/logs/tree-manifest-delivery.txt`、digest `.sandbox/review/logs/tree-manifest-digest.txt`）
  - 门禁运行前后再次比对：`TREE_STABLE: True`（changed/added/removed 均 0）。
  - 说明：快照后的这次追加修改是**合法化修改**（只增加 `→CANCELLED` 边与对应测试断言），
    不影响 §8.3 已关闭项；交付树复跑结果见 §8.2 的 #24–#29。

### 8.2 复核命令与退出码（冻结快照上实跑）

| # | 命令 | 退出码 | 结果 |
|---|---|---:|---|
| 1 | `gofmt -l $(find cmd internal -name '*.go')` | **0** | 无输出 |
| 2 | `go build ./...` | **0** | 全仓可编译（含新 `internal/workflow/transition_internal_test.go`） |
| 3 | `go vet ./...` | **0** | 无输出 |
| 4 | `go test ./... -race -count=1` | **0** | 29 包：14 个 `ok`、15 个 `[no test files]`；**连续两次独立运行均 0**（`gates3.log`、`gates4.log`） |
| 5 | `go test ./internal/workflow ./internal/storage ./internal/api -race -count=2` | **0** | 3/3 `ok`、0 FAIL（新回归测试无抖动） |
| 6 | `go test ./tests/e2e -count=1 -v`（未 opt-in） | **0** | 4 个 **SKIP**（仍非 PASS） |
| 7 | `source tests/sandbox/env.sh && ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1 -v -timeout 900s` | **0** | **4/4 PASS，10.757s**；新证据目录 `run-20261024T1424{12,20,21}-*`，five-identities 记录 `completed=5, keycloak_window_users=5, generated=14:24:21Z`；keycloak teardown 后 `realm users: 0` |
| 8 | `go test ./internal/workflow -race -run 'TestEveryStepOutcomeIsCommitLegal\|TestSafeNextStateFailsClosedOnIllegalEdge\|TestCredentialExpiryAfterOAuthRequiresReauthorization' -v` | **0** | 3/3 PASS（2.372s） |
| 9 | `go test ./internal/storage -race -run TestMailReceiptsAreAtomicWithStepCommit -v` | **0** | PASS（0.02s） |
| 10 | `go test ./internal/provider/mailbox/cloudflare -race -run 'TestCreateAddress_Duplicate\|TestCreateAddress_EnablePrefix' -v` | **0** | 5/5 PASS（含 `DuplicateFailsClosed` 断言只发 1 次请求） |
| 11 | P2 定向：`TestRaw32ByteMasterKeyWithWhitespaceBytes` / `TestViewerCannotSeeAuthorizationURL` / `TestLogoutRevokesSession` / `TestTemplateFilterAppliedBeforePagination` / `TestScanCleanE2ELogAndEvidenceArtifacts` / `TestSenderAllowedParsesAddressAndDomain` | **0** | 6/6 PASS |
| 12 | `.sandbox/review/transition_probe`（side-module，重新指向修复后代码） | 0（脚本）/ 输出合法转移 | 见 §8.4：非法转移已不可达，租约释放、无重试自旋 |
| 13 | `scripts/redact_scan.sh --file <首轮误报的 R2 e2e 日志>` | **0** | 0 findings（首轮为 exit 1/3 findings） |
| 14 | `scripts/redact_scan.sh --file .sandbox/review/logs/onboardd.log --event-db .sandbox/review/run/state.sqlite` | **0** | 0 findings（首轮同命令：3 findings） |
| 15 | 负向对照：`scripts/redact_scan.sh --file /tmp/dirty-review.log`（Bearer/JWT/password=/PEM） | **1** | 4 findings：`authorization_header`、`credential_assignment`、`jwt`、`private_key` → 修复未削弱检出 |
| 16 | `deploy/scripts/preflight.sh`（仓库现状，无 secret 文件） | **1** | 3 个 FAIL：master key/admin token 缺失、data 目录缺失；给出可执行修复命令 |
| 17 | `deploy/scripts/preflight.sh --secrets-dir <scratch> --data-dir <scratch>`（0600 key + token） | **0** | 6 PASS + 1 INFO（viewer 未启用）+ 1 WARN（data 目录 UID 501，提示 `chown 10001:10001`） |
| 18 | 裸 32 字节 master key 动态复验：`openssl` 等价的 `0x0b` 开头 + `0x20` 结尾的 32B 原始 key 启动 `onboardd`（新二进制，127.0.0.1:18795） | **0** | `/healthz` 返回 `{"status":"ok"}`（首轮同形状 key **启动失败**，P2-2 已修复） |
| 19 | `scripts/backup.sh`（当前树代码，对运行中 WAL 库） | **0** | 快照 `state-20261024T142839Z.sqlite`；counts 与 manifest 一致、`integrity_check=ok` |
| 20 | `scripts/restore.sh --snapshot … --target … --force` | **0** | `secrets_verified=3`（逐行解密）、`schema_version=2`、`integrity_check=ok`（备份/恢复无回归） |
| 21 | `go run ./internal/ops/cmd/rotate -db … -keyring <v1+v2> -dry-run` | **0** | `total_secrets=3 planned=3 current_version=2`（轮换工具无回归；真实 rotation 仍要求停机，见 R4 P1-5） |
| 22 | `docker compose -f deploy/compose.yaml config` | **0** | 清单仍仅 `host_ip: 127.0.0.1` 发布、`read_only`、`user 10001`、`cap_drop ALL` |
| 23 | `docker info` | **1** | daemon 未运行：Docker build/run 依旧 **NOT_RUN** |

**交付树复跑（Lead 的 22:29 追加修改之后，2026-10-24T14:31:48Z，`gates-delivery.log`）：**

| # | 命令（交付树） | 退出码 | 结果 |
|---|---|---:|---|
| 24 | `go build ./...` | **0** | 全仓可编译 |
| 25 | `go vet ./...` | **0** | 无输出 |
| 26 | `go test ./... -race -count=1` | **0** | 29 包：14 个 `ok`（含 `internal/workflow/state` 的新 `CANCELLED` 断言）、15 个 `[no test files]` |
| 27 | `go test ./tests/e2e -count=1`（未 opt-in） | **0** | 4 个 SKIP（仍非 PASS） |
| 28 | `source tests/sandbox/env.sh && ONBOARD_SANDBOX_E2E=1 go test ./tests/e2e -race -count=1 -v -timeout 900s` | **0** | **4/4 PASS，13.207s**（crash 9.74s / five-identities 1.39s / idempotency 0.40s / cancel 0.41s）；新证据目录 `run-20261024T1432*` |
| 29 | `go test ./internal/workflow ./internal/workflow/state -race -run 'TestEveryStepOutcomeIsCommitLegal\|TestSafeNextStateFailsClosedOnIllegalEdge\|TestCredentialExpiryAfterOAuthRequiresReauthorization\|TestTransitionMatrix' -v` | **0** | 4/4 PASS（含穷举合法性测试；`TestTransitionMatrix` 断言新的 `CANCELLED` 边） |

> 所有 scratch 服务已停止；`tests/sandbox/status.sh` 复核：Keycloak realm users **0**、LOCAL SANDBOX healthy。

### 8.3 逐项关闭核验

| 编号 | 首轮级别 | Lead 修复（我读到的实现） | 我的独立验证 | 复核状态 |
|---|---|---|---|---|
| **P1-1** | P1 | `state.go`: 增加 `CREDENTIAL_VERIFIED→REAUTHORIZE_REQUIRED`（并补两处模板成功边，见 §8.5）；`CANCEL_REQUESTED` 只允许 `→CANCELLED`；`engine.go:332,422-442` 新增 `safeNextState`：非法映射 fail-closed 到 `FAILED_TERMINAL/PROVIDER_BUG` 且 `CountFailure=true` | §8.4 探针：`commit … err=nil`、run=`REAUTHORIZE_REQUIRED`、`lease_owner=""`、`action_required=true`，**未 resume 时再领取为 nil（无自旋）**；resume 后可正常领取；storage 仍拒绝真正非法边（纵深防御）。`TestEveryStepOutcomeIsCommitLegal` 穷举 **4 模板 × 全部 step × 9 个错误类 + 成功边**；`TestSafeNextStateFailsClosedOnIllegalEdge`；`TestCredentialExpiryAfterOAuthRequiresReauthorization` 端到端 PASS | **CLOSED** |
| **P1-2a** | P1 | `storage/claim.go:179-190,285-294`：`StepResult.MailReceipts` 与步骤结果**同一事务**写入；`workflow/context.go:32`；`steps.go:228-340` 不再调用 `ConsumeMessage`；`confirmSandboxEmail`（`steps.go:343-380`）先 `CheckVerification`（崩溃后远端已确认的快速路径）→ 确认 → 回执随成功提交落库 | `grep` 确认**非测试代码已无 `ConsumeMessage` 调用点**；`TestMailReceiptsAreAtomicWithStepCommit` PASS（回执随提交持久化、恰好一次、不跨 run 泄漏）；带新代码的 E2E crash/restart 与 5/5 全绿 | **CLOSED**（崩溃只会丢"远端确认"动作，由快速路径恢复） |
| **P1-2b** | P1 | `cloudflare.go:303-314`：重复地址直接 `CONFLICT`（"manual reconciliation required"），**不再** `enablePrefix` 二次创建 | 5 个 duplicate 单测 PASS；`DuplicateFailsClosed` 断言 `attempts==1`、0 secret、错误不含敏感值。Cloudflare live 仍 **NOT_RUN**（无凭据） | **代码级 CLOSED**；live 未验证（如实标注） |
| **P2-1** | P2 | `ops/scan.go:77-108`：`long_token` 规则对 path-like span 抑制，高置信规则（JWT/Bearer/赋值/PEM/known secret）不受影响；新增 e2e 日志 clean 测试 | 首轮误报的两份日志现在均 exit 0；负向脏样本仍 4/4 命中（§8.2 #13-#15） | **CLOSED** |
| **P2-2** | P2 | `config.go:416-424`：`len(raw)==32` 直接原样使用，再做 TrimSpace 解码 | 单测 PASS + 我用 `0x0b…0x20` 的 32 字节原始 key **实机启动成功**（§8.2 #18） | **CLOSED** |
| **P2-3** | P2 | `security/crypto.go:275-330` 增加 `SessionID`/`VerifyFull`；`api/server.go:46-47,269,460-486` 撤销表；`ui.go:239-242` logout 时服务端撤销 | `TestLogoutRevokesSession` PASS；代码级确认 cookie 带 `sid`，鉴权时检查撤销表 | **PARTIAL**：撤销表在**进程内存**，重启/多实例后旧 cookie 仍可能有效；admin token 轮换也不会自动吊销会话。残余风险已低于首轮（登出立即生效） |
| **P2-4** | P2 | `api/server.go:449-456` `summarizeFor(role)`；列表/详情按角色清空 `authorization_url` | `TestViewerCannotSeeAuthorizationURL` PASS；代码级确认 viewer 路由均走 `summarizeFor`（resume/cancel 为 admin-only） | **CLOSED** |
| **P2-5** | P2 | `steps.go` `senderAllowed` 改为解析 `Name <addr>` / 地址 / 域 / `@domain` | `TestSenderAllowedParsesAddressAndDomain` PASS（`attacker@moonbyte.test.evil.example` → false） | **CLOSED** |
| **P2-6** | P2 | `storage/runs.go:isUniqueConstraint` 只匹配 `UNIQUE constraint failed` | 全绿套件 + 代码级确认 | **CLOSED** |
| **P2-7** | P2 | 两步：①`CANCEL_REQUESTED` 只允许 `→CANCELLED`（删死边）；②交付树（22:29）为**所有 13 个非终态**补上 `→CANCELLED`（`state.go` + `state_test.go`），使 `classifyError(ClassCancelled)` 在任何 step 上都不会被提交期拒绝 | `state_test` 断言拒绝 `CANCEL_REQUESTED→COMPLETED/FAILED_TERMINAL`；我的 side-module 探针在**交付树**上枚举：13/13 非终态 `→CANCELLED` **全部 legal**（含 `WAITING_EMAIL`/`CREDENTIAL_VERIFIED`/`IDENTITY_VERIFIED`/`CANCEL_REQUESTED`），终态无出边，`CANCEL_REQUESTED→COMPLETED`、`CANCEL_REQUESTED→FAILED_TERMINAL`、`OAUTH_PENDING→COMPLETED`、`MAILBOX_READY→WAITING_EMAIL`、`CREDENTIAL_VERIFIED→IDENTITY_VERIFIED` 仍被拒（`.sandbox/review/logs/matrix-probe-delivery.log`） | **CLOSED**（首轮残余已消除：不再依赖 `safeNextState` 兜底） |
| **P2-8** | P2 | 未变更 | `grep -rn "MailboxCleaner\|DeleteAddress" internal cmd \| grep -v _test` 仍只有定义/接口，无调用方 | **OPEN**（取消/失败后的 sandbox 身份/邮件仍只由测试 harness 清理） |
| **P2-9** | P2 | 交付树文档：`R1.md:139` 的 R1-18 行已改为**单数** `GET /api/v1/message/{id}` 并注明 mock 曾编码复数、见 §14；另新增 §14 后补说明（`:447`） | 表格行内容现在**正确**（与 `mailpit.go:348` 单数端点一致），`TestGetMessage_Success`（`mailpit_test.go:458`）确实断言 `/api/v1/message/m1`。**但**该行引用的 `TestGetMessage_RealEndpointShape` 在仓库中**不存在**（`grep -rn "RealEndpointShape" --include=*.go` → 0 命中） | **PARTIAL（证据引用错误）**：内容已修，但需把引用改成已存在的 `TestGetMessage_Success`（或补上同名测试），否则属于"引用不存在的证据" |
| **P2-10** | P2 | `storage/runs.go:219-221` 增加 `Template` 过滤进 SQL；API 传入过滤条件 | `TestTemplateFilterAppliedBeforePagination` PASS | **CLOSED** |
| **P2-11** | P2 | 新增 `deploy/scripts/preflight.sh`（secret 存在性/权限/格式/placeholder/data 目录）+ `deploy/README.md` 首跑流程 | §8.2 #16/#17 实跑：无 secret → exit 1（3 条可执行提示）；scratch secret → exit 0 + UID 警告 | **CLOSED（静态/离线范围）**；`docker compose up` 仍 NOT_RUN（无 daemon） |
| **P1-3 → OPS-1** | P1 → **P2/部署前置** | 未改代码（有意）：仓库仍只提供 loopback 绑定 + `http.Server`；`deploy/README.md` 补了首跑/预检契约 | 复核 `grep -rn "tls\.\|ListenAndServeTLS" cmd internal` 仍 0；`docker compose config` 仍只发布 `127.0.0.1:8788` | **降级并保留为 OPS-1**：理由与边界见 §8.6 |

### 8.4 P1-1 探针：修复前 vs 修复后（同一 side-module 探针）

修复前（首轮，`REVIEW` §5.2 原文）：

```text
commit CREDENTIAL_VERIFIED -> REAUTHORIZE_REQUIRED: err=conflict: illegal transition CREDENTIAL_VERIFIED -> REAUTHORIZE_REQUIRED
run status after rejected commit: CREDENTIAL_VERIFIED (lease_owner="probe-worker") | step failures=0 attempts=1
lease expiry re-claimed the same step: verify_credential attempt=2 failures=0 (no budget consumed)
```

修复后（本轮，`.sandbox/review/logs/p1-probe-reverify.log`）：

```text
[1] claimed step: verify_credential state-after-claim: CREDENTIAL_VERIFIED
[1] commit CREDENTIAL_VERIFIED -> REAUTHORIZE_REQUIRED: err=<nil>
[1] run status=REAUTHORIZE_REQUIRED lease_owner="" action_required=true | step status=REAUTHORIZE_REQUIRED failures=0 attempts=1
[1] re-claim before resume: nil (no retry spin)  ✅
[1] after operator resume: claim=true err=<nil>
[2] commit OAUTH_PENDING -> COMPLETED (illegal): err=conflict: illegal transition OAUTH_PENDING -> COMPLETED
[2] run status after rejected illegal commit: OAUTH_PENDING (lease_owner="probe-worker-3")
[3] matrix CREDENTIAL_VERIFIED->REAUTHORIZE_REQUIRED: <nil>
[3] matrix CANCEL_REQUESTED->COMPLETED: illegal transition CANCEL_REQUESTED -> COMPLETED
[3] matrix CANCEL_REQUESTED->FAILED_TERMINAL: illegal transition CANCEL_REQUESTED -> FAILED_TERMINAL
[3] matrix CANCEL_REQUESTED->CANCELLED: <nil>
```

结论：同一探针从"提交被拒 + 租约保留 + 无限重试 + 无预算消耗"变为"提交成功 + 租约释放 + 等人工 resume"，
且 storage 层仍拒绝真正非法的边（引擎的 `safeNextState` 只是把引擎映射错误在提交前转为 fail-closed）。

### 8.5 修复工作额外发现的缺陷（我首轮漏报，如实记录）

新回归测试 `TestEveryStepOutcomeIsCommitLegal` 不只覆盖错误类，还断言**每个模板的"成功边"**都合法，
因此暴露了首轮审查漏掉的两个同类 P1-1 级缺陷（旧矩阵下这两条成功边会被提交期校验拒绝 → 同样的自旋）：

| 模板 | 步骤（成功边） | 旧矩阵 | 现矩阵 | 影响 |
|---|---|---|---|---|
| `cloudflare-mail-smoke` | `wait_email`: `WAITING_EMAIL → COMPLETED` | **缺失** | 已加入 `state.go:7` | 一旦该 live smoke 真的收到邮件，run 会在最后一步被拒并自旋到 deadline（R1 的 live smoke 一直是 NOT_RUN，所以从未暴露） |
| `oauth-existing-account` | `wait_oauth`: `WAITING_USER_ACTION → CREDENTIAL_VERIFIED` | **缺失** | 已加入 `state.go:8` | 授权成功后同样无法提交终态；WorkBuddy live 未运行，故未暴露 |

这两条**证实 P1-1 的实际影响面比我首轮报告的更广**（我首轮只报告了 `CREDENTIAL_VERIFIED→REAUTHORIZE_REQUIRED`
和 `WAITING_EMAIL→CANCELLED`），也说明"穷举 step × outcome 合法性"的回归测试是比逐点修复更根本的门禁。
我未能发现它们，原因已记录在此，供后续 review 参考。

### 8.6 仍然开放的事项（不阻塞本次范围，但必须随交付披露）

1. **OPS-1（原 P1-3，重新分级为 P2/部署前置）：TLS ingress 未落地。**
   - 保留事实：代码仍只有纯 `http.Server`（`cmd/onboardd/main.go:155-164`），仓库无 ingress/HSTS/`ONBOARD_REQUIRE_TLS`；
     `docker compose config` 仅发布 `127.0.0.1:8788`。
   - **降级理由**（与首轮不同的判断）：默认配置拒绝非回环监听（`config.go` `ONBOARD_ALLOW_PUBLIC_LISTEN` 门禁）、
     compose 只发布回环、反代头信任是显式 opt-in、cookie `Secure` 在 TLS/可信反代下自动生效；
     本环境无证书/无 ingress 可测，属**部署环境 NOT_RUN** 而非可修复的代码缺陷。
   - **仍未闭合的边界**：任何把 8788 暴露到私网/公网的部署**必须先有 TLS 终止**（否则 bearer/cookie 明文）。
     若交付方计划远程访问，此项应回到 P1 并补齐反代示例/`REQUIRE_TLS`。
2. **P2-3（部分）**：登出撤销是**进程内存**表；重启后旧 cookie 在 TTL 内仍有效，多实例不共享；admin token 轮换不吊销会话。
3. **P2-8（开放）**：服务内仍无 sandbox 资源补偿/清理（`MailboxCleaner` 无调用方）。
4. **P2-9（部分，仅剩文档证据引用）**：R1-18 行内容已改为单数端点（与实现一致），但引用的
   `TestGetMessage_RealEndpointShape` 不存在；应改引 `TestGetMessage_Success` 或补测试。
   （首轮"表格行与 §14 自相矛盾"的问题已消除。）
5. **R4 已披露、本轮未变的 NOT_RUN/限制**：Docker build/run、GHCR、真实 Cloudflare/GitHub/WorkBuddy live、
   Playwright、多实例 fencing、TLS ingress；以及 R4 自列的 P2（WorkBuddy 授权 URL host 未 pin、镜像 digest 未 pin、
   `TRUST_PROXY_HEADERS` 伪造风险、限流器进程内、轮换/恢复停机编排）。
6. **交付树复跑后无新增缺陷**：build/vet/`go test ./... -race`/sandbox e2e 全部通过；`CANCELLED` 边的追加
   使 P2-7 的残余风险归零，未发现回归。

### 8.7 最终状态（复核后，对当前工作树）

```
首轮（14:12Z）：      PASS / PARTIAL / FAIL  →  PARTIAL
复核（14:35Z）：      PASS / PARTIAL / FAIL  →  PASS（范围限定）
交付树（14:40Z）：    PASS / PARTIAL / FAIL  →  PASS（范围限定，维持）
```

**交付树指纹（不可变证据）**：

| 项 | 值 |
|---|---|
| 清单范围 | `internal/ cmd/ tests/ deploy/ contracts/ scripts/` + `go.mod go.sum Makefile Dockerfile .github/`（**126 files**） |
| manifest sha256 | `1a65ea11f2859c03ae18a28d11c2e5581288930d6376b3533a96366c9296186e` |
| `internal/workflow/state/state.go` | `ed1d689506ca275a32d07f89511cc305794cb97ba17e7ac63be5ec6ae2242188` |
| `internal/workflow/state/state_test.go` | `ed17af4b78f78e74fb8cfcecc102a08fbc6ca99fcbf07423ae0b9de31e0112bf` |
| 与 14:22Z 快照差异 | **仅**上述 2 个文件（22:29:58Z 追加 `CANCELLED` 边）；其余 124 个文件字节一致 |
| 门禁前后 TREE_STABLE | **True**（changed/added/removed 均 0） |

**判定依据（我的独立证据，不引用 Lead 的测试结论）**：

- **代码级 P0/P1 = 0**：首轮 3 项 P1 中，2 项代码级问题（P1-1、P1-2a/b）已修复并由我在冻结快照上独立复核关闭
  （穷举合法性测试、原子回执测试、Cloudflare 单请求测试、独立 side-module 探针、两轮全仓 `-race` 绿）；
  第 3 项（TLS）按 §8.6 重新分级为部署前置。
- **真实夹具证据仍在**：`ONBOARD_SANDBOX_E2E=1` 4/4 PASS、5/5 身份、crash/restart、idempotency、cancel 全绿
  （本轮新证据 `run-20261024T1424*`）；离线运行仍是 SKIP 而非 PASS。
- **保密性/平台边界/SSRF/CSRF/RBAC/限流**：首轮实测结论未被本轮改动削弱；重跑全仓 `-race` 与 redaction 扫描
  仍 clean（且负向对照证明检测未被削弱）。
- **运维**：compose 静态加固仍在；备份/恢复/轮换在本轮**重新实跑**（§8.2 #19–#21，exit 0/0/0，逐行解密证明）；
  新增 `preflight.sh` 实测通过（无 secret → exit 1，有 secret → exit 0）；Docker/TLS 等仍如实 NOT_RUN。

**该 PASS 的范围声明（不可外推）**：

- 适用于：`LOCAL SANDBOX` 上真实 Keycloak + Mailpit 的端到端证据，以及**仅回环**的自托管 v0.1.0 交付；
- **不**代表：可公网/远程暴露（需先落 TLS ingress）、真实 Cloudflare/GitHub/WorkBuddy 已联通、Docker 镜像可构建、
  Playwright 浏览器 UI 已验证、多实例/多进程部署已验证；
- 若后续把服务暴露到非回环地址而无 TLS，或声称任何 live 平台"已通过"，本 PASS 不成立，应按 §8.6 重新评估。

**结论**：在"代码级 P1 清零 + 全门禁实测绿 + 残余项均如实披露"的标准下，本轮把状态从 **PARTIAL 提升为
PASS（范围限定）**；剩余 `OPS-1`/P2-3/P2-8/P2-9 与 R4 的 NOT_RUN 项必须在最终交付说明中保持可见。

### 8.8 交付树最终一致性检查（2026-10-24T14:40Z，回应 Lead 的 22:29 追加修改）

Lead 在 22:29:58Z 做了**最后一次追加修改**：为所有剩余非终态补 `→CANCELLED` 合法边（`state.go` + `state_test.go`），
使 `classifyError(ClassCancelled)` 在任何 step 上都不会被提交期拒绝（首轮 P2-7 建议）。我逐项确认：

1. **代码差异与哈希**：126 文件 SHA-256 清单与 14:22Z 快照比对 → **只有** `state.go`、`state_test.go` 变化，
   无新增/删除文件、无其他代码改动（`docs/*` 仅文档更新，不在代码清单内）。
   交付树 manifest sha256 = `1a65ea11f2859c03ae18a28d11c2e5581288930d6376b3533a96366c9296186e`，
   `state.go` sha256 = `ed1d689506ca275a32d07f89511cc305794cb97ba17e7ac63be5ec6ae2242188`；
   门禁运行前后再次比对 `TREE_STABLE: True`。
2. **交付树门禁（实跑，§8.2 #24–#29）**：`go build ./...` **0**；`go vet ./...` **0**；
   `go test ./... -race -count=1` **0**（29 包，14 ok）；离线 e2e **0**（4 SKIP）；
   `ONBOARD_SANDBOX_E2E=1 … -race` **0，4/4 PASS，13.207s**（新证据目录 `run-20261024T1432*`）；
   穷举合法性/矩阵测试 **0，4/4 PASS**。
3. **状态矩阵探针（交付树，`.sandbox/review/logs/matrix-probe-delivery.log`）**：
   - 13/13 非终态 `→CANCELLED` 全部 **legal**（`CREATED`、`VALIDATING`、`PREPARING_MAILBOX`、`MAILBOX_READY`、
     `VERIFICATION_REQUESTED`、`WAITING_EMAIL`、`WAITING_USER_ACTION`、`IDENTITY_VERIFIED`、`OAUTH_PENDING`、
     `CREDENTIAL_VERIFIED`、`RETRY_WAIT`、`REAUTHORIZE_REQUIRED`、`CANCEL_REQUESTED`）；
   - 终态（`COMPLETED`/`FAILED_TERMINAL`/`CANCELLED`）无任何出边；
   - 真非法边仍被拒：`CANCEL_REQUESTED→COMPLETED`、`CANCEL_REQUESTED→FAILED_TERMINAL`、
     `OAUTH_PENDING→COMPLETED`、`MAILBOX_READY→WAITING_EMAIL`、`CREDENTIAL_VERIFIED→IDENTITY_VERIFIED`；
   - P1-1 的行为未回归：合法提交 `CREDENTIAL_VERIFIED→REAUTHORIZE_REQUIRED` 成功、租约释放、未 resume 不再领取、
     resume 后可领取；非法提交仍 `conflict`。
4. **残余项变化**（相对 §8.6）：
   - **P2-7 → CLOSED**（首轮残余"缺 `→CANCELLED` 边"已消除，不再依赖 `safeNextState` 兜底）。
   - **P2-9 仍 PARTIAL（仅剩证据引用）**：`R1.md:139` 的 R1-18 行已改为单数端点（与 `mailpit.go:348` 一致），
     但它引用的 `TestGetMessage_RealEndpointShape` **在仓库中不存在**（0 命中）；应为 `TestGetMessage_Success`
     （`mailpit_test.go:458`，确实断言 `/api/v1/message/m1`）或补上该测试。**代码行为本身正确**。
   - **OPS-1（TLS ingress）、P2-3（内存态会话撤销）、P2-8（无服务内补偿）不变**；
     `FINAL-REPORT.md` 已按 §8.6 披露这三项（我核对过其表格措辞与本文一致）。
   - R4 既有 NOT_RUN/P2（Docker、GHCR、live 平台、Playwright、多实例、URL host pin、digest pin、
     trusted-proxy、内存限流、轮换停机）**不变**。
5. **结论**：交付树与我 14:35Z 的判定**无实质差异**；`CANCELLED` 边的追加只降风险、不引入回归。
   最终状态 **PASS（范围限定）维持**——范围与 §8.7 的声明相同（LOCAL SANDBOX 证据 + 仅回环自托管），
   不包含"可公网/远程暴露"或任何 live 平台成功。

> 本轮复核未修改任何代码；只更新本文件（`docs/acceptance/REVIEW.md`）。
> 交付树清单与原始日志：`.sandbox/review/logs/tree-manifest-delivery.txt`、`tree-manifest-digest.txt`、
> `gates-delivery.log`、`matrix-probe-delivery.log`。

---

### 8.9 Lead 交付说明（2026-10-24T22:36Z，仅文档事实澄清）

审查者在 §8.3 将 P2-9 标为 PARTIAL 的唯一原因，是 `docs/acceptance/R1.md:139` 的 R1-18 行引用了当时不存在的
`TestGetMessage_RealEndpointShape`。该行已在交付前修正为引用真实存在的 `TestGetMessage_Success`
（`internal/provider/mailbox/mailpit/mailpit_test.go:458`，断言单数端点 `/api/v1/message/m1`），全仓库已无
`RealEndpointShape` 引用。因此**交付树上的 P2-9 证据引用错误已闭环**；§8.3 保留审查时的历史记录，不修改
审查者原文。其余结论（PASS 范围限定、OPS-1/P2-3/P2-8 残余、live 平台 NOT_RUN）不变。
