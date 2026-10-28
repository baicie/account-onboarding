# 接入真实服务方（cloudflare_temp_email / GitHub / WorkBuddy）

本手册给出把链路从「模拟」切到「真实」的**精确步骤**。控制台的 **服务方** 页（侧边栏 → 服务方）会实时显示每个适配器是 `真实` 还是 `模拟`。

## 0. 先看当前状态

```bash
# 服务器上（把 <密码> 换成 /root/onboard-panel-credentials.txt 里的值）
curl -s -u onboard:<密码> -H 'Host: 82.156.234.84:18788' \
  http://127.0.0.1:18788/v1/providers -H "Authorization: Bearer $(sudo cat /opt/account-onboarding/secrets/onboard_admin_token)"
```

返回里每个条目带 `enabled` 与 `live`：`live=false` 就是还在用模拟实现。

## 1. Cloudflare 临时邮箱（cloudflare_temp_email）

适配器已实现真实协议：`POST /admin/new_address`（`x-admin-auth`，可选 `x-custom-auth`）、`GET /api/parsed_mails`、`GET /api/parsed_mail/{id}`（`Authorization: Bearer <地址 JWT>`）。

### 1.1 放凭据（在服务器上）

```bash
sudo sh -c 'printf %s "<管理员密码>" > /opt/account-onboarding/secrets-src/cloudflare_admin_password'
sudo sh -c 'printf %s "<站点密码>"   > /opt/account-onboarding/secrets-src/cloudflare_site_password'   # 若实例开启了私有站点
sudo chmod 0444 /opt/account-onboarding/secrets-src/cloudflare_admin_password /opt/account-onboarding/secrets-src/cloudflare_site_password
```

### 1.2 配置（compose 的环境变量）

```yaml
ONBOARD_CLOUDFLARE_ENABLED: "true"
ONBOARD_CLOUDFLARE_BASE_URL: "https://<你的实例>"      # 自建实例必须加入下面的 ALLOWED_HOSTS
ONBOARD_CLOUDFLARE_DOMAIN: "<你的域名>"
ONBOARD_CLOUDFLARE_ALLOWED_HOSTS: "<你的实例主机名>"
ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE: "/run/secrets/cloudflare_admin_password"
ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE: "/run/secrets/cloudflare_site_password"
```

### 1.3 验证

```bash
# 建一个地址（这一步会真的在你的实例上创建邮箱）
curl -s -X POST https://<你的实例>/admin/new_address \
  -H "x-admin-auth: $(cat /opt/account-onboarding/secrets-src/cloudflare_admin_password)" \
  -H 'Content-Type: application/json' \
  -d '{"enablePrefix":true,"name":"onboard-check","domain":"<你的域名>"}'
```
返回 `{"jwt":...,"address":...}` 即代表适配器的入参/出参与真实实例一致。

## 2. GitHub（只做凭据校验）

该适配器**不创建账号**，只做：`GET /user`、`GET /user/emails`、`POST /user/emails`（把邮箱标记为已验证）。自动化注册 GitHub 账号违反其条款，本项目不做。

```bash
sudo sh -c 'printf %s "<PAT，需 read:user + user:email>" > /opt/account-onboarding/secrets-src/github_token'
sudo chmod 0444 /opt/account-onboarding/secrets-src/github_token
```

```yaml
ONBOARD_GITHUB_ENABLED: "true"
ONBOARD_GITHUB_TOKEN_FILE: "/run/secrets/github_token"
# 企业版才需要：
# ONBOARD_GITHUB_BASE_URL: "https://ghe.example.com"
# ONBOARD_GITHUB_ALLOWED_HOSTS: "ghe.example.com"
```

## 3. WorkBuddy 国际版

```bash
sudo sh -c 'printf %s "<服务令牌>" > /opt/account-onboarding/secrets-src/workbuddy_token'
sudo chmod 0444 /opt/account-onboarding/secrets-src/workbuddy_token
```

```yaml
ONBOARD_WORKBUDDY_ENABLED: "true"
ONBOARD_WORKBUDDY_BASE_URL: "https://<你的网关>"
ONBOARD_WORKBUDDY_REALM: "<realm>"
ONBOARD_WORKBUDDY_TOKEN_FILE: "/run/secrets/workbuddy_token"
ONBOARD_WORKBUDDY_PERMISSION_ACK: "<书面授权说明，必填>"
# 网关不在本机/内网时必须显式放开：
ONBOARD_WORKBUDDY_ALLOW_REMOTE: "true"
ONBOARD_WORKBUDDY_ALLOWED_HOSTS: "<网关主机名>"
```

## 4. 生效与自检

```bash
# 重新材料化密钥并重建容器（tag 部署路径会自动做这一步）
sudo bash /opt/account-onboarding/scripts/update_account_onboarding.sh --root /opt/account-onboarding
# 再看一次服务方页/接口：live 应为 true
```

## 4.1 让运行详情显示收到的邮件（可选）

默认**不保存**邮件内容（`docs/08` 的约束）。若希望在运行详情页看到收到的验证邮件：

```yaml
ONBOARD_MAIL_SNAPSHOT: "true"
```

开启后，`wait_email` 会把匹配到的那封邮件的**发件人、主题、验证链接、收信时间**写入
`mail_snapshots` 表；**正文仍然不落库**。控制台运行详情会出现「收到的邮件」卡片，
`GET /v1/runs/{id}/mails` 返回同样内容并附带 `enabled` 字段。关闭时接口返回
`{"items":[],"enabled":false}`，一行都不写。

> 注意：验证链接本身是一次性凭据，能打开邮件链接的人就能完成验证。开这个开关等于把该凭据
> 存进数据库，请确保数据库访问与备份同样受控。
## 5. 边界（务必知悉）

- **不会自动注册 GitHub 账号**：只校验/绑定你已持有的账号。
- **不会自动完成第三方授权同意**：需要人工确认的步骤会在运行里停下并显示原因。
- 邮箱地址与邮件内容默认**不落库**：邮件正文只在步骤执行期间存在，符合 `docs/08` 的约束。因此运行详情页目前只能看到步骤与事件，看不到邮件正文——如需在 UI 展示邮件，需要新增一个受控的「运行期邮件快照」存储字段（尚未实现，属后续项）。
