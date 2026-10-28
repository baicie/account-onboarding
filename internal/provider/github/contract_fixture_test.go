package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Recorded payloads from the GitHub REST API (docs: /user, /user/emails). The adapter must parse
// them exactly as GitHub sends them, including the absence of a public email on /user and the
// per-address primary/verified flags.
const (
	fixtureUser = `{"login":"operator-handle","id":583231,"type":"User","email":null,` +
		`"name":"Operator","html_url":"https://github.com/operator-handle"}`
	fixtureEmails = `[` +
		`{"email":"primary@example.test","primary":true,"verified":true,"visibility":"public"},` +
		`{"email":"secondary@example.test","primary":false,"verified":false,"visibility":null}` +
		`]`
	fixtureRateLimited = `{"message":"API rate limit exceeded for 203.0.113.7.",` +
		`"documentation_url":"https://docs.github.com/rest/overview/resources-in-the-rest-api#rate-limiting"}`
)

func fixtureAdapter(t *testing.T, status int) *Adapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("authorization header = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got == "" {
			t.Errorf("missing the pinned API version header")
		}
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(fixtureRateLimited))
			return
		}
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(fixtureUser))
		case "/user/emails":
			_, _ = w.Write([]byte(fixtureEmails))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a, err := New(Config{BaseURL: srv.URL, AllowLoopbackHTTP: true, TokenRef: testRef}, newResolver())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestFixtureListEmailsParsesPrimaryAndVerified(t *testing.T) {
	a := fixtureAdapter(t, 0)
	list, err := a.ListEmails(context.Background())
	if err != nil {
		t.Fatalf("ListEmails: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("emails = %d, want 2", len(list))
	}
	if !list[0].Primary || !list[0].Verified || list[0].Email != "primary@example.test" {
		t.Fatalf("primary entry parsed wrong: %+v", list[0])
	}
	if list[1].Primary || list[1].Verified {
		t.Fatalf("unverified entry must stay unverified: %+v", list[1])
	}
}

func TestFixtureVerifyResolvesTheAccount(t *testing.T) {
	a := fixtureAdapter(t, 0)
	res, err := a.Verify(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.SubjectID == "" {
		t.Fatalf("verify result has no subject: %+v", res)
	}
	if res.Issuer == "" {
		t.Fatalf("verify result has no issuer: %+v", res)
	}
}

func TestFixtureRateLimitIsNotTreatedAsSuccess(t *testing.T) {
	a := fixtureAdapter(t, http.StatusForbidden)
	if _, err := a.ListEmails(context.Background()); err == nil {
		t.Fatal("a rate-limited response must be an error, not an empty list")
	}
}
