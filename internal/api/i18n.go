package api

import "strings"

// DefaultUILang is the console language unless ONBOARD_UI_LANG says otherwise.
const DefaultUILang = "zh-CN"

// messages is the operator-facing catalogue: {Chinese, English} per key, shared by the
// templates (through the T func) and by Go code (through localize).
var messages = map[string][2]string{
	"html.lang":           {"zh-CN", "en"},
	"brand.name":          {"账号开通控制台", "Account Onboarding"},
	"nav.runs":            {"运行", "Runs"},
	"nav.reconciliation":  {"外部对账", "Reconciliation"},
	"nav.alerts":          {"告警", "Alerts"},
	"nav.signout":         {"退出登录", "Sign out"},
	"footer.loopback":     {"本机回环运维控制台", "Loopback operator console"},
	"footer.redacted":     {"事件已脱敏，密钥永不显示", "Events are redacted; secrets are never displayed"},
	"page.signin":         {"登录", "Sign in"},
	"login.title":         {"运维登录", "Operator sign in"},
	"login.hint":          {"该控制台只监听本机回环接口。认证使用从密钥文件加载的管理员令牌，令牌不会写入浏览器。", "This console is bound to the loopback interface. Authentication uses the admin token loaded from a secret file; the token is never stored in the browser."},
	"login.token_label":   {"管理员令牌", "Admin token"},
	"login.submit":        {"登录", "Sign in"},
	"login.session_note":  {"界面会话是带 CSRF 保护的 HttpOnly、SameSite=Strict Cookie。", "The UI session is an HttpOnly, SameSite=Strict cookie with CSRF protection."},
	"error.origin":        {"来源校验失败。", "Origin check failed."},
	"error.invalid_token": {"管理员令牌无效。", "Invalid operator token."},
	"error.form":          {"表单提交无效。", "Invalid form submission."},
	"error.token_missing": {"未配置管理员令牌，控制台已锁定。", "Admin token is not configured; the console is locked."},
	"error.session":       {"无法创建会话。", "Could not create a session."},
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
