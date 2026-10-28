package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baicie/account-onboarding/internal/provider"
)

// Recorded payloads from the WorkBuddy gateway (docs/10-integration-workbuddy.md):
// POST /panel/api/login/start answers {ok,url,state,realm} and GET .../login/poll answers
// {done,uid,nickname,realm,message}. These tests pin the parsing of those exact shapes.
const fixtureStartOK = `{"ok":true,"url":"https://workbuddy.example.test/login?state=abc123",` +
	`"state":"abc123","realm":"global"}`

const fixtureStartRefused = `{"ok":false,"error":"realm not enabled for this gateway"}`

func fixtureGateway(t *testing.T, payload string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "login/start") {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("start must be a POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixtureAdapter(t *testing.T, baseURL string) *Adapter {
	t.Helper()
	a, err := New(Config{
		Enabled:         true,
		BaseURL:         baseURL,
		AllowTestHTTP:   true,
		GatewayTokenRef: gwTokenRef,
	}, newTestStore())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestFixtureStartParsesTheGatewayResponse(t *testing.T) {
	srv := fixtureGateway(t, fixtureStartOK)
	a := fixtureAdapter(t, srv.URL)
	sess, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "subject-1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(sess.AuthorizationURL, "https://workbuddy.example.test/login") {
		t.Fatalf("authorization url = %q", sess.AuthorizationURL)
	}
	if sess.Ref == "" || strings.Contains(sess.Ref, "abc123") {
		t.Fatalf("session ref must be an opaque secret reference, got %q", sess.Ref)
	}
}

func TestFixtureStartRefusalIsAnError(t *testing.T) {
	srv := fixtureGateway(t, fixtureStartRefused)
	a := fixtureAdapter(t, srv.URL)
	if _, err := a.Start(context.Background(), provider.StartAuthRequest{SubjectID: "subject-1"}); err == nil {
		t.Fatal("a refused start must surface an error")
	}
}
