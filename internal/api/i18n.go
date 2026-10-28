package api

import "strings"

// DefaultUILang is the console language unless ONBOARD_UI_LANG says otherwise.
const DefaultUILang = "zh-CN"

// messages is the operator-facing catalogue: {Chinese, English} per key, shared by the
// templates (through the T func) and by Go code (through localize).
var messages = map[string][2]string{
	"html.lang":                 {"zh-CN", "en"},
	"brand.name":                {"账号开通控制台", "Account Onboarding"},
	"nav.runs":                  {"运行", "Runs"},
	"nav.reconciliation":        {"外部对账", "Reconciliation"},
	"nav.alerts":                {"告警", "Alerts"},
	"nav.signout":               {"退出登录", "Sign out"},
	"footer.loopback":           {"本机回环运维控制台", "Loopback operator console"},
	"footer.redacted":           {"事件已脱敏，密钥永不显示", "Events are redacted; secrets are never displayed"},
	"page.signin":               {"登录", "Sign in"},
	"login.title":               {"运维登录", "Operator sign in"},
	"login.hint":                {"该控制台只监听本机回环接口。认证使用从密钥文件加载的管理员令牌，令牌不会写入浏览器。", "This console is bound to the loopback interface. Authentication uses the admin token loaded from a secret file; the token is never stored in the browser."},
	"login.token_label":         {"管理员令牌", "Admin token"},
	"login.submit":              {"登录", "Sign in"},
	"login.session_note":        {"界面会话是带 CSRF 保护的 HttpOnly、SameSite=Strict Cookie。", "The UI session is an HttpOnly, SameSite=Strict cookie with CSRF protection."},
	"error.origin":              {"来源校验失败。", "Origin check failed."},
	"error.invalid_token":       {"管理员令牌无效。", "Invalid operator token."},
	"error.form":                {"表单提交无效。", "Invalid form submission."},
	"error.token_missing":       {"未配置管理员令牌，控制台已锁定。", "Admin token is not configured; the console is locked."},
	"error.session":             {"无法创建会话。", "Could not create a session."},
	"dash.runs":                 {"\u8fd0\u884c", "Runs"},
	"dash.no_runs":              {"\u6682\u65e0\u8fd0\u884c", "No runs yet"},
	"dash.create":               {"\u521b\u5efa\u8fd0\u884c", "Create a run"},
	"dash.mailbox":              {"\u90ae\u7bb1\u670d\u52a1\u63d0\u4f9b\u65b9", "Mailbox provider"},
	"dash.identity":             {"\u8eab\u4efd\u670d\u52a1\u63d0\u4f9b\u65b9", "Identity provider"},
	"dash.oauth":                {"OAuth \u670d\u52a1\u63d0\u4f9b\u65b9", "OAuth provider"},
	"dash.email":                {"\u90ae\u4ef6\u670d\u52a1\u63d0\u4f9b\u65b9\uff08GitHub\uff09", "Email provider (GitHub)"},
	"dash.credential":           {"\u51ed\u636e\u6821\u9a8c\u5668", "Credential verifier"},
	"dash.sandbox":              {"\u6c99\u7bb1\u9a71\u52a8", "Sandbox driver"},
	"dash.existing_ref":         {"\u65e2\u6709\u8eab\u4efd\u5f15\u7528\uff08\u4ec5\u771f\u5b9e\u8d26\u53f7\u6a21\u677f\uff09", "Existing identity reference (real-account only)"},
	"dash.create_button":        {"\u521b\u5efa\u8fd0\u884c", "Create run"},
	"dash.providers":            {"\u670d\u52a1\u63d0\u4f9b\u65b9\u4e0e\u542f\u7528\u6761\u4ef6", "Providers and activation gates"},
	"th.kind":                   {"\u7c7b\u578b", "Kind"},
	"th.name":                   {"\u540d\u79f0", "Name"},
	"th.version":                {"\u7248\u672c", "Version"},
	"th.enabled":                {"\u542f\u7528\u72b6\u6001", "Enabled"},
	"th.restriction":            {"\u9650\u5236", "Restriction"},
	"pill.enabled":              {"\u5df2\u542f\u7528", "enabled"},
	"pill.disabled":             {"\u672a\u542f\u7528", "disabled"},
	"dash.find":                 {"\u67e5\u627e\u8fd0\u884c", "Find a run"},
	"dash.run_id":               {"\u8fd0\u884c ID", "Run ID"},
	"dash.open":                 {"\u6253\u5f00\u8fd0\u884c", "Open run"},
	"dash.filter_status":        {"\u72b6\u6001\u7b5b\u9009", "Status filter"},
	"opt.all_statuses":          {"\u5168\u90e8\u72b6\u6001", "All statuses"},
	"dash.filter_template":      {"\u6a21\u677f\u7b5b\u9009", "Template filter"},
	"opt.all_templates":         {"\u5168\u90e8\u6a21\u677f", "All templates"},
	"dash.apply":                {"\u5e94\u7528\u7b5b\u9009", "Apply filters"},
	"dash.clear":                {"\u6e05\u9664", "Clear"},
	"dash.recent":               {"\u6700\u8fd1\u8fd0\u884c", "Recent runs"},
	"th.created":                {"\u521b\u5efa\u65f6\u95f4\uff08UTC\uff09", "Created (UTC)"},
	"pill.operator":             {"\u9700\u4eba\u5de5\u5904\u7406", "operator action"},
	"dash.next":                 {"\u4e0b\u4e00\u9875", "Next page"},
	"dash.first":                {"\u2190 \u7b2c\u4e00\u9875", "\u2190 First page"},
	"dash.template":             {"\u6a21\u677f", "Template"},
	"dash.empty_recent":         {"\u6682\u65e0\u8fd0\u884c \u2014\u2014 \u7528\u4e0a\u65b9\u7684\u5047\u670d\u52a1\u521b\u5efa\u4e00\u4e2a\uff0c\u5373\u53ef\u79bb\u7ebf\u8dd1\u901a\u5f15\u64ce\u3002", "No runs yet \u2014 create one above with the fake providers to exercise the engine offline."},
	"dash.real_note":            {"\u771f\u5b9e\u8d26\u53f7\u6a21\u677f\u53ea\u4f5c\u7528\u4e8e\u4f60\u5df2\u6301\u6709\u7684\u8d26\u53f7\u3002\u672c\u670d\u52a1\u4e0d\u4f1a\u521b\u5efa\u7b2c\u4e09\u65b9\u8d26\u53f7\uff0c\u4e5f\u4e0d\u4f1a\u81ea\u52a8\u5316\u6388\u6743\u540c\u610f\u6d41\u7a0b\u3002", "Real-account templates operate only on accounts you already hold. This service never creates third-party accounts and never automates consent."},
	"dash.existing_placeholder": {"\u65e2\u6709\u8d26\u53f7\u7684\u5f15\u7528\uff0c\u7edd\u4e0d\u586b\u5199\u5bc6\u7801\u6216\u4ee4\u724c", "existing account reference, never a password or token"},
}

// localize resolves key for lang, falling back to English and then to the key itself.
func localize(lang, key string) string {
	entry, ok := messages[key]
	if !ok {
		return key
	}
	if strings.HasPrefix(strings.ToLower(lang), "en") {
		return entry[1]
	}
	return entry[0]
}

// normalizeUILang maps a configured value onto a supported language tag; Chinese is
// the default for anything unrecognised.
func normalizeUILang(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	switch {
	case strings.HasPrefix(v, "en"):
		return "en"
	default:
		return DefaultUILang
	}
}

// t localizes a key for the server language.
func (s *Server) t(key string) string {
	return localize(s.Cfg.UILang, key)
}
