# 10 WorkBuddy 现有网关集成

## 来源检查

目标仓库：`https://github.com/baicie/workbuddy2api-panel`（2026-10-24 勘查）。

- `internal/panel/panel.go` 注册了 `POST /panel/api/login/start` 和 `GET /panel/api/login/poll`，均通过 `p.withAuth()`。
- `internal/panel/login.go`：`{"realm":"global"}` 指向国际版，返回 `url`, `state`；poll 返回等待/完成状态，并在原网关内部执行 `auth.SaveAtomic()` 与热加载。
- `internal/panel/panel.go` 中 `loginTTL = 15*time.Minute`，会话在进程内存中；未知/过期 `state` 返回 410。

## 最终集成方式

编排服务对原网关只做**控制面适配**：

```text
start  -> workbuddy2api-panel POST /panel/api/login/start (realm=global)
       <- {url,state}
user   -> 浏览器官方网页，由本人登录/同意
poll   -> workbuddy2api-panel GET /panel/api/login/poll?state=...
       <- done=false / done=true / 410
```

OAuth Token、Refresh Token、文件 `auths/workbuddy-<uid>.json` 一律由现有网关写入；本项目绝不再创建第二份 Token JSON，也不保存 GitHub 或 WorkBuddy 的第三方密码。

## 许可门禁

真实 WorkBuddy 兼容服务存在平台条款限制。**WorkBuddyAdapter 默认 `enabled=false`**。R3 的真实调用必须有经核实的授权依据；在此之前只允许 Fake 适配器用于合同与状态机测试。不得把此编排层用于规避账号配额或与他人共享平台会话。

## 状态映射

| 现有网关结果 | 新引擎状态 | 操作 |
|---|---|---|
| start `{ok:true,url,state}` | OAUTH_PENDING | 展示官方授权链接，进入 `WAITING_USER_ACTION` |
| poll `{done:false}` | OAUTH_PENDING | 延迟轮询，有截止时间 |
| poll `{done:true}` | CREDENTIAL_VERIFIED | 使用原网关状态摘要验证，避免凭据复制 |
| poll `410` | REAUTHORIZE_REQUIRED | 用户重新开始授权，不自动复用 state |
| 401/403 | FAILED_TERMINAL | 检查面板权限/平台授权，不切账户重试 |
| 5xx/网络错误 | RETRY_WAIT | 有上限，遵守熔断与 backoff |

## 关键坑

- 现有 WorkBuddy Login state 在网关内存；编排服务可以记录 state 的 secret reference，但**网关重启会让原 state 无效**。与真正的 durable OAuth 不同。
- 原网关 `loginPoll` 的成功分支包含副作用（凭据落盘等）；轮询必须由单任务串行执行，不得高并发重复调同一 state。
- 源仓库 README 说明其反代为非官方接口，不应将“代码可调用”理解为“平台允许调用”。
- 自用也不能自动推定合法合规；如果拿不到授权，停止于 Fake adapter 门禁。

## 测试

Fake mock 可以测试成功、等待、410、401、429、502、并发双 poll。真实网关的联通探测（readiness）只确认网关在私有网络可达，**不得无许可调用实际 WorkBuddy 非公开接口**。
