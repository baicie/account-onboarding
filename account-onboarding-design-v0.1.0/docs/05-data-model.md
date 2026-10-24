# 05 数据模型与事务

## SQLite 配置

- `PRAGMA foreign_keys=ON`; `journal_mode=WAL`; `busy_timeout=5000`; `synchronous=FULL`（可在压测确认后评估 NORMAL）。
- 使用 UTC ISO8601/RFC3339Nano 归一化存储，减少文本时间比较问题；更推荐使用 `INTEGER Unix epoch milliseconds`，初版 schema 已统一 `INTEGER`。
- 单实例多 goroutine，独立 DB pool 限制写竞争；迁移需要 schema version，不允许启动时静默 DROP 数据表。

## 实体

- `runs`：所有者、模式、模板、状态、截止时间、幂等键、lease 和 fencing generation。
- `steps`：运行 step 结果、操作键、尝试次数、下一次尝试、错误类别、完成时间。
- `events`：只追加，可审计的不敏感事件。
- `resources`：邮箱、身份、授权 session 的不可逆外部标识（必要时 HMAC 摘要）、SecretStore 引用。
- `secrets`：仅在选用内建加密 SecretStore 时启用，保存密文、nonce、key_version；生产优先外部 Secret 注入。

具体 SQL 见 `db/migrations/0001_initial.sql`。

## 独有索引

- `UNIQUE(owner_id, template, idempotency_key)` —— 避免重复创建任务。
- `UNIQUE(run_id, step_name)` —— 同一个 run 的某个步骤只有一个当前状态；历史进 events。
- `UNIQUE(run_id, event_seq)` —— 事件严格排序。
- `UNIQUE(run_id, resource_kind, external_id_hmac)` —— 避免重复关联。

## 外部副作用与补偿

状态提交和外部 REST 操作不能放入同一事务，故采取可恢复的 Saga：
1. 在事务中标记 step CLAIMED，分配 lease token，记录 `operation_key`。
2. 提交事务，向 Provider 发起外部操作。
3. 在事务中确认 lease token 仍有效后，记录结果及 event。
4. 如果崩溃在 2/3 之间，新 worker 先查询远端是否已成功；无法确定时标记 `TIMEOUT_UNCERTAIN`，不得重复创建真实身份。

## 数据保留

- 审计事件默认保留 90 天，可配置；不包含邮件正文。
- SQLite 的 Test Sandbox 地址和用户在 7 天后可清理，但真实 `moonbyte.top` 长期邮箱不因工作流完成自动删除。
- `state` / `code` / `access_token` 不进入数据库普通列，只可存受保护 SecretStore 的短期引用。
- `DELETE /runs` 不在 v0.1.0 暴露；只支持取消与受控清理，避免误删审计证据。

## 备份及恢复

必须使用 SQLite Backup API 或 `VACUUM INTO`；不要直接在 WAL 活跃时只复制 `.db` 而遗漏 `-wal`。密钥独立备份，恢复后检测解密能力、表版本及完整性，再允许 worker 领任务。
