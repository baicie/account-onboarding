package api

// The console is a React single-page app, so server-side copy is limited to the operator
// messages the login handler returns. The catalogue keeps a Chinese default with an English
// fallback, selected by ONBOARD_UI_LANG.

// DefaultUILang is the language the console ships with.
const DefaultUILang = "zh-CN"

var messages = map[string][2]string{
	"error.form":          {"登录表单无法解析，请重试。", "The login form could not be parsed; try again."},
	"error.origin":        {"来源校验失败。请通过控制台地址访问，不要跨站提交。", "Origin check failed. Open the console from its own address; do not post cross-site."},
	"error.token_missing": {"服务端未配置管理员令牌。", "The server has no operator token configured."},
	"error.invalid_token": {"管理员令牌无效。", "Invalid operator token."},
	"error.session":       {"无法签发会话，请检查服务端日志。", "Could not issue a session; check the server log."},
}

// normalizeUILang maps a configured language onto the two catalogs we ship.
func normalizeUILang(v string) string {
	if len(v) >= 2 && (v[0] == 'e' || v[0] == 'E') && (v[1] == 'n' || v[1] == 'N') {
		return "en"
	}
	return DefaultUILang
}

// localize returns the message for the language, falling back to the Chinese default.
func localize(lang, key string) string {
	m, ok := messages[key]
	if !ok {
		return key
	}
	if normalizeUILang(lang) == "en" {
		return m[1]
	}
	return m[0]
}

// t localizes with the server's configured language.
func (s *Server) t(key string) string {
	return localize(s.Cfg.UILang, key)
}
