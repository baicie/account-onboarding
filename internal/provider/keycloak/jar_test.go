package keycloak

import (
	"net/http"
	"net/url"
	"testing"
)

func TestLoopbackSecureJarSendsSecureCookieOverHTTP(t *testing.T) {
	jar, err := newJar()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("http://127.0.0.1:8180/realms/moonbyte-test/login-actions/action-token")
	jar.SetCookies(u, []*http.Cookie{{Name: "AUTH_SESSION_ID", Value: "abc", Path: "/realms/moonbyte-test/", Secure: true}})
	cookies := jar.Cookies(u)
	found := false
	for _, c := range cookies {
		if c.Name == "AUTH_SESSION_ID" && c.Value == "abc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("secure loopback cookie not returned over http: %+v", cookies)
	}
}
