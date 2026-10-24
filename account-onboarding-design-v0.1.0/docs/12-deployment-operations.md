# 12 部署、运维与故障恢复

## 部署策略

- 新建仓库 `baicie/account-onboarding`（仅命名建议，设计包未代你创建 GitHub 仓库）。
- CI：Go test + race + vet + lint + SAST + dependency review + Docker image build -> GHCR。
- Ubuntu：只新增 `account-onboarding` 容器，绑定 `127.0.0.1:8788`；原工作网关与 Cloudflare Worker 不动。
- 运行镜像最终由 CI 构建，`deploy/compose.yaml` 只定义待发布镜像位置，设计包不是可启动的已发布产品。
- 本地 Keycloak 和 Mailpit 仅测试时运行；不要在生产 Compose 默认启动。

## 环境变量与安全注入

`ONBOARD_LISTEN=127.0.0.1:8788`（容器内建议 `0.0.0.0:8788`，外层 Compose 映射 127.0.0.1）；`ONBOARD_DB_PATH=/app/data/state.sqlite`; `ONBOARD_MODE=sandbox`; `ONBOARD_MAX_CONCURRENCY=2`; `ONBOARD_CLOUDFLARE_BASE_URL=https://api.moonbyte.top`; `ONBOARD_CLOUDFLARE_ENABLED=false`; `ONBOARD_GITHUB_ENABLED=false`; `ONBOARD_WORKBUDDY_ENABLED=false`。

Secret 值优先采用 `/run/secrets/...` 文件路径，不写真实密码到普通环境变量。生产日志结构化输出并脱敏。

## Docker Compose

见 `deploy/compose.yaml`、`.env.example`。以下配置属于契约；实现时必须增加 Dockerfile、非 root 用户、HEALTHCHECK、read-only rootfs 与配置校验。

## 日常运维

- 健康探测：`GET /healthz` 仅表示进程存在；`/readyz` 需鉴权，检查 SQLite 可读写、schema 版本、密钥状态。
- 日志：按 `run_id`,`step`,`provider`,`event_type`,`failure_class` 搜索；禁止打印完整 URL query（可能含 OAuth state）。
- 告警：连续 Provider 5xx、run 堵塞/lease 过期、存储空间不足、真实邮件投递超时、备份失效、WorkBuddy adapter 出乎预期被开启。
- 指标：成功/失败数、run duration、step retries、mail delivery latency、secret decrypt errors；不记录邮箱正文。

## 备份 SOP

1. 暂停接收新任务，等待 SQLite 事务落盘；使用 SQLite backup API / `VACUUM INTO` 生成一致性数据库快照。
2. 快照加密后存放到不同持久介质；保存 checksum 和 schema version。
3. **密钥单独备份**，不可把密钥和数据库快照放在同一公开对象中。
4. 每月恢复到隔离目录执行 `PRAGMA integrity_check;` 与解密检查。
5. 如果发现真正的 OAuth credentials 泄露，在**原凭据拥有者系统**执行撤销/重登，不只删除本系统任务数据。

## 故障处理手册

| 现象 | 首选排查 | 禁止做法 |
|---|---|---|
| 邮件不来 | CF Email Routing MX/Catch-all、SMTP 发信日志、D1、地址 JWT | 先无限重复申请账号 |
| Cloudflare 401 | 检查 `x-admin-auth`、site password 以及绑定 Worker | 删除全站认证来“修复” |
| GitHub 403 | 检查官方 token scope、已授权账号所有权及平台限制 | 使用代理绕过或开新账号重试 |
| WorkBuddy 410 | 原网关 OAuth state 过期/服务重启，要求本人重新开始 | 重放旧 state |
| SQLite locked | busy timeout、事务时长、是否持有事务发 HTTP | 直接删除 WAL |
| 任务重复 | 幂等键/operation_key/租约 fencing | 手工删 run 表绕过 |
| 无法解密 | 检查密钥版本、从独立备份恢复 | 置空 SecretStore/打印密文密钥 |

## 灾备约束

Cloudflare Temp Email 的 D1 邮件内容不在本 SQLite 内，不应把备份编排任务数据库描述为“已备份邮箱邮件”。云端 D1 必须单独按照 Cloudflare D1 能力与数据保留政策备份。
