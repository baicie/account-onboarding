# 独立对抗性审查报告 — R9 Release Hardening（REVIEW-R9，复审）

- 复审时间：`2026-10-25T16:34–17:00`（Asia/Shanghai）
- 审查者：独立 reviewer（未参与实现；本轮只更新本文件，未改代码/文档/证据）
- 冻结对象：**代码/CI 证据 HEAD `b31e753`**（fix commit）与 **文档/证据 HEAD `08379a0`**，分支 `feat/r9-release-hardening`；base R8 `de89878`，main `9726fc1`；工作树审查时干净。
- 上一轮审查（`5f13d78`/`b199e2a`）发现 P1=1/P2=3；本轮冻结前修复已合入 `b31e753`（`fd80b54`/`3b46b99` 的 candidate scanner workflow 也包含在其祖先中）。
- 环境：本地 Docker 仍不可用（`docker info` exit 1，未启动）；Docker/TLS/Caddy/rollback 证据来自 GitHub Actions；本轮通过已认证 `gh api`（只读）独立核验 run/job/artifact，而不是只读提交 JSON。
- **最终结论：R9 = PASS；P0 = 0，P1 = 0，P2 = 0，P3 = 9。`RELEASE_CANDIDATE_READY` 成立；`LIVE_READY` 仍不宣告。**
  - 上一轮 P1-1（candidate 未真实扫描）、P2-1（live IN_FLIGHT resolve）、P2-2（CONFIRMED commit-window）、P2-3（Caddy 重定向端口）均已修复并有代码/测试/CI 证据（§1/§2）。
  - 真实 CI：staging 27/27 + container 63/63、supply-chain PASS、rollback 20/20、Caddy 17/17（1 WARN）、release-image Candidate PASS（真实 govulncheck/ShellCheck/Trivy，扫描文件来自 candidate 镜像）+ Publish skipped（默认 false/保护 Environment）；无 GHCR push/registry digest。
  - `LIVE_READY` 不成立：生产 TLS/ACME、GHCR publish/sign、真实 Cloudflare/GitHub/WorkBuddy、正式 Release/Tag 均 NOT_RUN。

---

## 1. 上一轮 P1/P2 闭环核验

| 上轮发现 | 修复 | 独立核验 |
|---|---|---|
| **P1-1** candidate 无真实扫描，扫描证据来自 supply run 的另一镜像 | candidate job 增加 `Candidate vulnerability / lint gates`：govulncheck v1.1.4、ShellCheck `-S error`、Trivy v0.76.1（checksum verified，CRITICAL blocking/HIGH report-only），归档到 `candidate/supply/`；publish 增加 `Assert candidate scan evidence before publish`（`release-image.yml:157-171`） | run **40176187349**（gh api）：candidate job steps 含该扫描步骤；candidate artifact `release-candidate-b31e753…`（38,075,834 B）存在；`docs/acceptance/evidence/r9-candidate/` 有 govulncheck/shellcheck/trivy-critical/trivy-high/trivy-version/scanner-policy；`R9-candidate-summary` 的 image ID `6355305…` 与 `image-id.txt`/`metadata.json`/`candidate-provenance.json` 一致；`trivy-critical.txt` target 明确为 `account-onboarding:candidate`；supply run 镜像不再被当作 candidate 扫描证据（**除 §3 P3-2 的 R9.md 旧索引文案**） |
| **P2-1** `resolve` 可关闭 live `IN_FLIGHT` | `ResolveExternalOperation` 先读 run `lease_until_ms`：live lease 拒绝；expired lease 才允许；新增测试：live lease 拒绝、expired lease 可 resolve | 独立探针：live → `conflict: operation is IN_FLIGHT under a live lease`；过期 lease → RESOLVED；测试通过 |
| **P2-2** CONFIRMED 后 step commit 失败无恢复路径 | `Resolve` 允许 CONFIRMED 的 `RESOURCE_CONFIRMED`/`MANUAL_COMPENSATION_COMPLETED`，拒绝 `NO_RESOURCE_CREATED`；新增 `TestResolveConfirmedRecoveryPath`；audit allowlist 增加 resolution/evidence_reference | 独立探针：CONFIRMED + `NO_RESOURCE_CREATED` → conflict；CONFIRMED + `RESOURCE_CONFIRMED` → RESOLVED；audit detail 同时含 `resolution`/`evidence_reference`/`reason`；测试通过 |
| **P2-3** Caddy 308 固定 8443 | `Caddyfile:38` 改为 `redir https://{env.R9_PUBLIC_HOST}{uri}`；`caddy_acceptance.sh:77` 断言动态 `https://localhost:$HTTPS_PORT/healthz` | CI run 40176187307 caddy log：`PASS Caddy HTTP 308 redirect to published HTTPS port`；17/17 + 1 WARN（oversize body connection-level，已在 R9.md 记录） |
| P3-3/P3-9 等 | `ExternalCompletionStatus` 改为 known-definite 白名单（Validation/NotAuthorized/RateLimited/NotFound）；`ops_external.sh` token 改为 0600 header file；迁移新增 `idx_external_ops_created` + 无过滤 query-plan 测试；resolve audit allowlist 增加 resolution/evidence | 代码/测试已核；剩余见 §3 |

**P0 门禁真实性（复核）**：PREPARED crash、并发 Mark CAS loser、completion failure、cancel、retry/restart 均未发现“未知/在途/已确认/已解决后再次调用 provider”的路径；timeout 用例验证 `CreateAddress` 调用次数 = 1；取消用例验证 →OUTCOME_UNKNOWN。CONFIRMED 后的 step commit 失败现在有人工恢复路径，且不会自动重试（P2-2 闭环）。

---

## 2. GitHub Actions 独立核验（gh api，只读）

| Run | 结果（API） | 证据 |
|---|---|---|
| staging-acceptance **40176187395** | success；head_sha `b31e753…`；jobs staging+container-regression 均 success | staging **27/27**、container **63/63**；artifacts `staging-acceptance-evidence-40176187395`（13,229 B）、`r8-container-regression-40176187395`（5,277 B） |
| supply-chain **40176187340** | success；head_sha `b31e753…`；job 全 steps success | govulncheck / ShellCheck / Syft / Trivy 全跑；artifact `supply-chain-evidence-40176187340`（367,915 B）；日志见 `r9-supply/` |
| r9-release **40176187307** | success；head_sha `b31e753…`；jobs old-image-rollback+caddy-proxy success | rollback **20/20**、Caddy **17/17 + 1 WARN**；artifacts `r9-old-image-rollback-evidence`（5,223 B）、`r9-caddy-proxy-evidence`（5,007 B） |
| release-image **40176187349** | success；head_sha `b31e753…`；candidate success，publish **skipped** | candidate job steps 含 `Candidate vulnerability / lint gates`；artifact `release-candidate-b31e753…`（38,075,834 B）；candidate 扫描文件在 `r9-candidate/`，summary 绑定 image ID/hash |

补充：`r9-candidate` 扫描文件 hash 与 `R9-candidate-summary.txt` 一致（govulncheck `3016e51e…`、trivy-critical `bba1c419…`、shellcheck 空文件 `e3b0c442…`）；`metadata.json` image ID `6355305…` 与 candidate `image-id.txt` 一致；`trivy-critical.txt` 的扫描 target 是 candidate 镜像。**P1-1 已闭环。**

---

## 3. 剩余 P3（9 项，不阻塞 RC）

| 编号 | 位置 | 内容 |
|---|---|---|
| P3-1 | [`internal/storage/migrations/0004_external_operations.sql:35`](../../internal/storage/migrations/0004_external_operations.sql) | 在已存在的 0004 迁移中**追加** `idx_external_ops_created`。若已有 schema-4 数据库（早期 R9 构建/开发库）不会自动补索引；迁移应保持不可变，新增 0005 或启动时 `CREATE INDEX IF NOT EXISTS`。R9 尚未发布，影响是升级/索引一致性而非当前 RC 运行时 |
| P3-2 | [`docs/acceptance/R9.md:120`](R9.md) | 证据索引仍把 `r9-supply/` 写成 “Candidate govulncheck/Trivy/ShellCheck/SBOM 摘要”；实际 candidate 扫描已在 `r9-candidate/`，supply run 是另一镜像。属索引文案错误，修复为指向 `r9-candidate/` |
| P3-3 | [`internal/storage/external_operations.go:451-459`](../../internal/storage/external_operations.go) | `resolve` 对 IN_FLIGHT 只检查 `lease_until_ms` 是否过期；lease 过期不等于 worker 必然已停止（慢 provider 调用可能仍在途）。这是上一轮 P2-1 的残余，当前实现已明显收窄；建议 runbook 要求“确认无本实例 worker/引擎已停止”或增加 owner/进程标记 |
| P3-4 | [`internal/storage/external_operations.go:247`](../../internal/storage/external_operations.go) | `CompleteExternalOperation` 仍只 CAS `status=IN_FLIGHT`，不重新校验 lease/fencing；单一 permit 下不会双调用，但完成路径的 fencing 防御可再加强（上轮 P3 未变） |
| P3-5 | [`.github/workflows/release-image.yml:194`](../../.github/workflows/release-image.yml) | publish job 的 cosign 二进制仍是 `curl` 下载后直接执行，未校验 checksum/签名；Publish 本轮 skipped，属发布路径未演练项。建议固定 hash 或改用官方 action |
| P3-6 | [`tests/r9/caddy_acceptance.sh:59`](../../tests/r9/caddy_acceptance.sh) | Caddy 容器以 root + `NET_BIND_SERVICE` 运行（官方 binary file capability 限制），Caddyfile Host 固定 `localhost`；测试夹具可接受，但不应被当作生产加固配置；R9.md 可注明 |
| P3-7 | [`tests/r9/old_image_rollback.sh:114,158`](../../tests/r9/old_image_rollback.sh) | 旧镜像拒绝 schema4/future 用固定 `sleep 5/4` + logs grep；CI 通过，内容 20/20 有意义（旧镜像拒绝、备份恢复、run/revocation 保留、wrong-key 不覆盖）。限制“仅相邻版本、非任意跨版本”已在 R9.md 注明 |
| P3-8 | [`internal/storage/metrics.go:18`](../../internal/storage/metrics.go) | `provider_error_classes` 实际来自 `steps.error_class`（含本地/引擎 class），不是纯 provider；固定标签/无 PII 成立，命名/文档可澄清。`SQLiteBusyRetryCount` 为 nil（未插桩）已注明 |
| P3-9 | [`internal/storage/external_operations.go:463-472`](../../internal/storage/external_operations.go) | resolve 的 audit actor 仍硬编码 `"operator"`，无管理员身份记录；resolution/evidence 已保留（P2-2 修复），若要审计到人需接入身份。 |

---

## 4. 其他复核

- **分页/计数**：201/1000/10000 行、重复时间戳 tie-break、global counts/`total_needs_reconciliation`、cursor 稳定性测试通过；新增无过滤 `idx_external_ops_created` query-plan 测试通过（P3-1 为迁移方式问题，非查询正确性）。
- **`/readyz`**：要求 schema>=4 + `external_operations` 表 + session revocations + WAL + writable；迁移 0004/health 检查存在；旧镜像对 schema4/future 拒绝（rollback 20/20）。
- **Caddy**：17/17 + 1 WARN；TLS1.1 拒绝/1.3 接受、HSTS、HTTP 308 动态端口、Host 421、readyz 401、cookie/CSRF、backend restart、证书轮换均真实；WARN 已记录（oversize body 连接级失败而非 413）。
- **发布**：candidate 真实扫描；publish 前置断言扫描证据（`govulncheck`/`shellcheck`/`trivy-critical`）并拒绝 CVE/ShellCheck error；publish skipped、无 GHCR push、无 registry digest；LIVE 未宣告。
- **文档**：R9.md 状态 `RELEASE_CANDIDATE_READY` 待 review；FINAL/README/index 均为 pending review；`LIVE_READY` 明确排除；rollback 限制、Caddy WARN、no digest 均如实。仅 P3-2 的旧 `r9-supply` 索引文案需要改。

---

## 5. 本地独立执行的命令与退出码

| 命令 | 退出码 | 结果 |
|---|---:|---|
| `make verify` | **0** | fmt/vet/build/全量测试（本地） |
| `go test ./... -race -count=1` | **0** | 29 包全绿 |
| `go test ./... -v` 计数 | **0** | **353 顶层（348 PASS + 5 SKIP）/665 含子测试/0 FAIL** — 与 R9.md 一致 |
| `go test ./internal/storage -run 'TestResolveRejectsLiveInFlight|TestResolveConfirmedRecovery|TestExternalOperationsUnfilteredQueryPlan' -race -count=3` | **0** | P2/P3 修复测试稳定 |
| `go test ./internal/workflow -run 'TestExternalOperation' -race -count=3` | **0** | confirmed/timeout/duplicate/cancel |
| `go test ./internal/api -run 'TestExternalOperation|TestOpsMetrics' -race` | **0** | resolve/RBAC/metrics |
| `tests/r9/old_image_rollback.sh`（无 Docker） | **3** | `NOT_RUN: Docker daemon unavailable`（符合预期） |
| `tests/r9/caddy_acceptance.sh`（无 Docker） | **3** | 同上 |
| `tests/r51/docker_acceptance.sh --self-test` | **0** | 11/11 |
| `docker info` | **1** | 本地 daemon 不可用（未启动） |
| R9 证据/文档 `redact_scan`（42 文件） | 全部 **0** | 0 findings |
| `gh api` 对 4 个 run/jobs/artifacts（只读） | **200** | 见 §2 |

独立探针（未入库）：live IN_FLIGHT resolve → conflict；expired lease → RESOLVED；CONFIRMED `NO_RESOURCE_CREATED` → conflict；CONFIRMED `RESOURCE_CONFIRMED` → RESOLVED，audit detail 含 `resolution`/`evidence_reference`/`reason`；missing id → not found；`ExternalCompletionStatus(ClassAuthRequired)` → OUTCOME_UNKNOWN。

> 未触发/重跑 Actions、未 push/部署、未安装/启动 Docker、未改代码/证据。

---

## 6. `RELEASE_CANDIDATE_READY` 判定与冻结

### 6.1 判定

| R9 关闭标准 | 结果 | 说明 |
|---|---|---|
| 外部写入 gate（P0） | **PASS** | 表权威 + CAS/lease + 粘性状态；无二次调用路径；completion/commit-window 有人工恢复路径 |
| 分页/全局计数 | **PASS** | 201/1000/10000、tie-break、global counts；created index（P3-1 迁移方式） |
| Resolve CSRF/RBAC/version/audit | **PASS** | admin-only/viewer 403/version CAS/重复 409/审计同事务；live IN_FLIGHT 守卫；audit 字段保留 |
| 旧镜像升级/回滚 | **PASS（范围限制已注明）** | 20/20；限制为非任意跨版本 |
| 真实 Caddy TLS | **PASS（WARN 已记录）** | 17/17 + 1 WARN；动态 308 端口 |
| Release candidate/publish | **PASS（Publish skipped 未演练）** | Candidate 真实扫描并归档；publish 前置断言；无 push/digest |
| 文档诚实性 | **PASS（P3-2 文案）** | LIVE 排除、rollback 限制、WARN、no digest 均如实；仅旧索引文案需改 |
| 独立审查 P0/P1 | **P0=0，P1=0，P2=0** | 本复审 |

**结论：`RELEASE_CANDIDATE_READY` 成立**（P0=0/P1=0/P2=0；真实 staging/container/rollback/Caddy/supply/candidate 证据；Publish 默认 skipped 且未 push）。`LIVE_READY` 仍不宣告：生产 TLS/ACME、GHCR publish/sign、真实 Cloudflare/GitHub/WorkBuddy、正式 Release/Tag 均 NOT_RUN；剩余 9 项 P3 建议在发布后/下一轮清理（其中 P3-1 迁移索引方式、P3-2 文档索引文案优先）。

### 6.2 冻结

| 项 | 值 |
|---|---|
| 代码/CI HEAD | `b31e753`；树 `352b2549…`；`git ls-tree -r b31e753 \| sha256sum` = `abeca721…31b1` |
| 文档/证据 HEAD | `08379a0`；树 `816d496f…`；`git ls-tree -r 08379a0 \| sha256sum` = `385390cd…2e7e` |
| 工作树 | 干净；本文件为唯一新增未跟踪文件 |
| `docs/acceptance/R9.md` | `4d209656…` |
| 证据 | staging log `5a87a792…`；container `586bf355…`；rollback `12a722db…`；caddy `8c0f8826…`；candidate summary `a7d590f3…`、govulncheck `e7589224…`、trivy-critical `b934a2fa…`、shellcheck `e69de29b…` |
| 代码 | `external_operations.go aa17dff3…`；`external_operations_test.go 1678a336…`；`0004_external_operations.sql bf86cfa2…`；`workflow/external_ops.go 359ab0a9…`；`security/redact.go ff6b5d8e…`；`api/ops.go a2b2ffcd…` |
| 脚本/工作流 | `scripts/ops_external.sh 25381308…`；`Caddyfile 32e43044…`；`caddy_acceptance.sh 28cb455f…`；`old_image_rollback.sh 42e58888…`；`release-image.yml 8f04bc55…` |
| 文档 | `FINAL-REPORT.md 30f5801c…`；`README.md 1566c24b…`；`index.md eb586dff…` |
