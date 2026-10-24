# 对抗性代码审查提示词

你是一名不参与实现的资深安全/可靠性 reviewer。对 `account-onboarding` 的实际源代码做 P0/P1 风险审查（不要依据 README 自称完成的能力），并实际运行项目可执行测试。重点审查：

1. 任务幂等：重复提交会不会创建重复的账户/邮件地址？SQLite lease fencing 是否真正排除过期执行者？
2. 原子性：HTTP 调用是否错误地持有数据库事务？崩溃窗口怎么 reconcile？OAuth `state` 是否可重放？
3. 身份安全：是否有攻击者选择任意地址/任意 URL 的 SSRF、OAuth redirect 跨域、CSRF、跨用户访问？
4. 保密性：Cloudflare Admin 密码、address JWT、GitHub Token、WorkBuddy Token、OTP、OAuth state 是否在日志/UI/错误消息/SQLite 明文出现？
5. 平台边界：有没有隐藏代码连接 GitHub 新账号机器人注册、DataDome/CAPTCHA 求解、浏览器指纹伪装、WorkBuddy 未授权真实接口或多免费账户轮转？如有，判为 P0、停止发布。
6. 测试真实性：Keycloak+Mailpit Sandbox 的 5/5 是否真实运行？是否混淆了 Fake 结果、Cloudflare Live 和真实 WorkBuddy 成功？
7. 运维：Compose 是否真正 127.0.0.1，Secret 是否与 DB 一起存储、镜像有没有 root/权限扩大，SQL 备份恢复是否包含 WAL 等问题？

输出格式：`P0/P1/P2` 问题表（文件/行号/复现路径/影响/建议/回归测试），修复优先序，真实测试命令+退出码；最终状态为 `PASS` / `PARTIAL` / `FAIL`，不允许用没有运行的测试替代证据。禁止在审查中擅自修改真实云服务、GitHub 用户或 WorkBuddy 账号。
