# 09 GitHub 已有账号邮箱验证

## 目标与边界

仅面向操作人**已合法持有并登录的 GitHub 账号**添加/验证自有 `moonbyte.top` 邮箱。GitHub 官方 REST API 是唯一允许的写入通道。

## 步骤

1. 管理员通过 OAuth/App 授权或 `gh auth login` 为现有 GitHub 身份授权 Email addresses 读/写权限；只存 secret reference。
2. Operator 选择自有 `@moonbyte.top` 地址（不能从任意客户邮箱读取）。
3. `POST /user/emails` 添加该地址（用户必须已明确授权）；如远端地址已存在视为幂等成功。
4. GitHub 发送验证邮件到该地址，Mailbox Provider 根据 mailbox scoped JWT 拉取邮件，只通知本人进行确认。
5. 真人打开 GitHub 官方验证链接并完成确认，不执行通过浏览器脚本代替用户确认的步骤。
6. `GET /user/emails` 获取 `verified=true`；显示完成与审计摘要。

## 示例（仅供操作者自己运行）

```bash
gh auth status
gh api -X POST /user/emails -f 'emails[]=github@moonbyte.top'
gh api user/emails --jq '.[] | {email,verified,primary}'
```

可能需要 `user:email` scope 或 GitHub App 对邮箱管理的对应权限；具体以调用时官方授权提示与 API 文档为准。已验证状态可能有传播延迟，应轮询有限次数。

## 禁止实现

- `github_signup()` / robot registration 等新账号注册流程。
- GitHub CAPTCHA / WAF / 二级挑战绕过。
- 自动填充/提交用于创建新 GitHub 身份的 launch code。
- 用测试账号生成器向真实 GitHub 注册。

## 测试

- Fake GitHub API：200 verified=true、未验证、401 scope不足、409 冲突、429 限流、5xx、超时。
- 真正 live：仅启用显式 flag，操作已有合法账号，且必须由持有人确认邮箱验证。
- 记录邮箱 alias/hash，不记录 GitHub PAT 或邮件正文。

官方参考：`https://docs.github.com/en/rest/users/emails`。
