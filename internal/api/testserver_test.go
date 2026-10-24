package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baicie/account-onboarding/internal/api"
)

// newTestServer starts a second HTTP server around an existing api.Server and
// registers its teardown.
func newTestServer(t *testing.T, srv *api.Server) string {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// postLoginRaw performs a form login without touching the shared cookie jar.
func postLoginRaw(t *testing.T, client *http.Client, base string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/ui/login", strings.NewReader("token="+adminToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
