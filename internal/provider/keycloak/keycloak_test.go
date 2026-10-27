package keycloak

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
)

const (
	testRealm    = "moonbyte-test"
	testClientID = "onboarding-test"
	testCallback = "http://127.0.0.1:8788/oidc/callback"
)

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:           baseURL,
		Realm:             testRealm,
		AdminUser:         "kcadmin",
		AdminPassword:     "admin-password",
		ClientID:          testClientID,
		RedirectURI:       testCallback,
		Timeout:           2 * time.Second,
		AllowLoopbackHTTP: true,
	}
}

func newTestAdapter(t *testing.T, f *fakeKeycloak, mutate func(*Config)) (*Adapter, *memSecrets) {
	t.Helper()
	cfg := testConfig(f.URL())
	if mutate != nil {
		mutate(&cfg)
	}
	sec := newMemSecrets()
	a, err := New(cfg, sec)
	if err != nil {
		t.Fatalf("new adapter: %v", err)
	}
	return a, sec
}

func register(t *testing.T, a *Adapter, subject, email string) provider.IdentityHandle {
	t.Helper()
	handle, err := a.Sandbox().Register(context.Background(), provider.SandboxRegisterRequest{
		SubjectID:   subject,
		Email:       email,
		DisplayName: subject,
		Password:    "Aa1!sandbox-password",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return handle
}

func seedCredential(t *testing.T, f *fakeKeycloak, sec *memSecrets, userID, accessToken string, expiresAt time.Time) provider.SandboxCredentialRef {
	t.Helper()
	f.mu.Lock()
	f.tokens[accessToken] = userID
	f.mu.Unlock()
	envelope := tokenEnvelope{
		AccessToken: accessToken,
		Issuer:      f.URL() + "/realms/" + f.realm,
		Subject:     userID,
		Audience:    testClientID,
		ExpiresAtMS: expiresAt.UnixMilli(),
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	ref := provider.SecretRefPrefix + secretKindOIDC + "/seeded0001"
	if err := sec.Put(context.Background(), ref, secretKindOIDC, string(raw)); err != nil {
		t.Fatal(err)
	}
	return provider.SandboxCredentialRef{Ref: ref, Issuer: envelope.Issuer, Subject: userID}
}

// ---------------------------------------------------------------------------
// Configuration and descriptors
// ---------------------------------------------------------------------------

func TestNewRejectsNonLoopbackAndUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	valid := testConfig("http://127.0.0.1:8180")
	secrets := newMemSecrets()
	tests := []struct {
		name  string
		cfg   Config
		class errs.Class
	}{
		{"empty base url", func() Config { c := valid; c.BaseURL = ""; return c }(), errs.ClassValidation},
		{"public idp host", func() Config { c := valid; c.BaseURL = "https://idp.example.com"; return c }(), errs.ClassValidation},
		{"non loopback port host", func() Config { c := valid; c.BaseURL = "https://keycloak.example.com:8443"; return c }(), errs.ClassValidation},
		{"credentials in url", func() Config { c := valid; c.BaseURL = "http://user:pass@127.0.0.1:8180"; return c }(), errs.ClassValidation},
		{"query in url", func() Config { c := valid; c.BaseURL = "http://127.0.0.1:8180?x=1"; return c }(), errs.ClassValidation},
		{"plain http without opt in", func() Config { c := valid; c.AllowLoopbackHTTP = false; return c }(), errs.ClassValidation},
		{"empty realm", func() Config { c := valid; c.Realm = ""; return c }(), errs.ClassValidation},
		{"realm path injection", func() Config { c := valid; c.Realm = "a/../b"; return c }(), errs.ClassValidation},
		{"empty client id", func() Config { c := valid; c.ClientID = ""; return c }(), errs.ClassValidation},
		{"no admin credentials", func() Config { c := valid; c.AdminUser = ""; c.AdminPassword = ""; return c }(), errs.ClassValidation},
		{"public redirect uri", func() Config { c := valid; c.RedirectURI = "https://app.example.com/callback"; return c }(), errs.ClassValidation},
		{"redirect with query", func() Config { c := valid; c.RedirectURI = testCallback + "?code=1"; return c }(), errs.ClassValidation},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.cfg, secrets); err == nil {
				t.Fatalf("expected configuration rejection")
			} else if got := errs.ClassOf(err); got != tc.class {
				t.Fatalf("class = %s, want %s (%v)", got, tc.class, err)
			}
		})
	}

	// A loopback https base URL is valid without the plain-http opt-in.
	if _, err := New(func() Config {
		c := valid
		c.BaseURL = "https://127.0.0.1:8443"
		c.RedirectURI = "https://127.0.0.1:8443/oidc/callback"
		c.AllowLoopbackHTTP = false
		return c
	}(), secrets); err != nil {
		t.Fatalf("loopback https rejected: %v", err)
	}
	// A nil secret store is rejected: credentials must be persisted out of band.
	if _, err := New(valid, nil); err == nil {
		t.Fatal("expected nil secret store rejection")
	}
}

func TestDescriptorsReportPerKindIdentity(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)

	cases := []struct {
		name string
		desc provider.Descriptor
		kind provider.Kind
		caps string
	}{
		{"identity", a.Identity().Descriptor(), provider.KindIdentity, "identity.request_verification"},
		{"sandbox", a.Sandbox().Descriptor(), provider.KindSandbox, "sandbox.register"},
		{"credential", a.Credential().Descriptor(), provider.KindCredential, "credential.verify"},
	}
	for _, tc := range cases {
		if tc.desc.Name != ProviderName {
			t.Fatalf("%s name = %q", tc.name, tc.desc.Name)
		}
		if tc.desc.Kind != tc.kind {
			t.Fatalf("%s kind = %q, want %q", tc.name, tc.desc.Kind, tc.kind)
		}
		if tc.desc.ContractVersion != ContractVersion {
			t.Fatalf("%s contract version = %q", tc.name, tc.desc.ContractVersion)
		}
		if !tc.desc.Enabled || tc.desc.Live {
			t.Fatalf("%s enabled=%v live=%v, want enabled sandbox fixture", tc.name, tc.desc.Enabled, tc.desc.Live)
		}
		if len(tc.desc.Capabilities) == 0 {
			t.Fatalf("%s has no capabilities", tc.name)
		}
		found := false
		for _, cap := range tc.desc.Capabilities {
			if cap == tc.caps {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s capabilities %v do not include %q", tc.name, tc.desc.Capabilities, tc.caps)
		}
	}
}

func TestDisabledAdapterFailsClosedBeforeAnyCall(t *testing.T) {
	t.Parallel()
	disabled := NewDisabled("keycloak sandbox is off for this test")
	ctx := context.Background()
	descriptors := []provider.Descriptor{
		disabled.Identity().Descriptor(),
		disabled.Sandbox().Descriptor(),
		disabled.Credential().Descriptor(),
	}
	for _, d := range descriptors {
		if d.Enabled {
			t.Fatalf("%s descriptor should be disabled", d.Kind)
		}
		if d.RestrictionReason != "keycloak sandbox is off for this test" {
			t.Fatalf("%s reason = %q", d.Kind, d.RestrictionReason)
		}
	}
	if _, err := disabled.Identity().CheckVerification(ctx, provider.CheckVerificationRequest{SubjectID: "x"}); errs.ClassOf(err) != errs.ClassNotAuthorized {
		t.Fatalf("check verification class = %s", errs.ClassOf(err))
	}
	if _, err := disabled.Sandbox().Register(ctx, provider.SandboxRegisterRequest{SubjectID: "x", Email: "x@moonbyte.top"}); errs.ClassOf(err) != errs.ClassNotAuthorized {
		t.Fatalf("register class = %s", errs.ClassOf(err))
	}
	if _, err := disabled.Credential().Verify(ctx, "secret://keycloak_oidc/x"); errs.ClassOf(err) != errs.ClassNotAuthorized {
		t.Fatalf("verify class = %s", errs.ClassOf(err))
	}
}

// ---------------------------------------------------------------------------
// Admin token cache
// ---------------------------------------------------------------------------

func TestAdminTokenPasswordGrantIsCachedUntilExpiry(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a.now = func() time.Time { return clock }

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := a.Identity().CheckVerification(ctx, provider.CheckVerificationRequest{SubjectID: "onboard-missing"}); err == nil {
			t.Fatal("expected not-found for a missing identity")
		}
	}
	f.mu.Lock()
	requests := f.tokenRequests
	form := f.adminTokenForms[len(f.adminTokenForms)-1]
	f.mu.Unlock()
	if requests != 1 {
		t.Fatalf("admin token requests = %d, want 1 (cached)", requests)
	}
	if form.Get("grant_type") != "password" || form.Get("client_id") != DefaultAdminClientID {
		t.Fatalf("unexpected admin grant form: %v", form)
	}
	if form.Get("username") != "kcadmin" || form.Get("password") != "admin-password" {
		t.Fatal("admin password grant did not carry the configured sandbox credentials")
	}

	// 60s expires_in minus the 30s safety window: the cache must refresh after 31s.
	clock = clock.Add(31 * time.Second)
	if _, err := a.Identity().CheckVerification(ctx, provider.CheckVerificationRequest{SubjectID: "onboard-missing"}); err == nil {
		t.Fatal("expected not-found")
	}
	f.mu.Lock()
	requests = f.tokenRequests
	f.mu.Unlock()
	if requests != 2 {
		t.Fatalf("admin token requests after expiry = %d, want 2", requests)
	}
}

func TestAdminTokenClientCredentialsWhenConfigured(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, func(c *Config) {
		c.AdminUser = ""
		c.AdminPassword = ""
		c.AdminClientID = "onboarding-admin"
		c.AdminClientSecret = "service-account-secret"
	})
	ctx := context.Background()
	if _, err := a.Identity().CheckVerification(ctx, provider.CheckVerificationRequest{SubjectID: "onboard-missing"}); err == nil {
		t.Fatal("expected not-found")
	}
	f.mu.Lock()
	form := f.adminTokenForms[len(f.adminTokenForms)-1]
	f.mu.Unlock()
	if form.Get("grant_type") != "client_credentials" {
		t.Fatalf("grant_type = %q, want client_credentials", form.Get("grant_type"))
	}
	if form.Get("client_id") != "onboarding-admin" || form.Get("client_secret") != "service-account-secret" {
		t.Fatal("service account credentials were not used")
	}
	f.mu.Lock()
	paths := append([]string(nil), f.requestPaths...)
	f.mu.Unlock()
	foundRealmToken := false
	for _, path := range paths {
		if path == "/realms/"+testRealm+"/protocol/openid-connect/token" {
			foundRealmToken = true
		}
		if path == "/realms/"+DefaultAdminRealm+"/protocol/openid-connect/token" {
			t.Fatalf("service-account grant was sent to the master realm token endpoint")
		}
	}
	if !foundRealmToken {
		t.Fatalf("service-account grant did not use the realm token endpoint: %v", paths)
	}
}

func TestAdminToken401RefreshesOnceAndSucceeds(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	register(t, a, "onboard-refresh", "refresh@moonbyte.top")
	f.mu.Lock()
	f.admin401Once = true
	f.mu.Unlock()

	state, err := a.Identity().CheckVerification(context.Background(), provider.CheckVerificationRequest{SubjectID: "onboard-refresh"})
	if err != nil {
		t.Fatalf("check verification after token refresh: %v", err)
	}
	if state != provider.VerificationPending {
		t.Fatalf("state = %s", state)
	}
	f.mu.Lock()
	requests := f.tokenRequests
	f.mu.Unlock()
	if requests != 2 {
		t.Fatalf("admin token requests = %d, want 2 (one forced refresh)", requests)
	}
}

// ---------------------------------------------------------------------------
// Identity provisioning and verification
// ---------------------------------------------------------------------------

func TestRegisterIsIdempotentOnStableSubject(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	ctx := context.Background()

	first := register(t, a, "onboard-idem", "idem@moonbyte.top")
	second, err := a.Sandbox().Register(ctx, provider.SandboxRegisterRequest{
		SubjectID:   "onboard-idem",
		Email:       "idem@moonbyte.top",
		DisplayName: "retry",
		Password:    "Aa1!another-password",
	})
	if err != nil {
		t.Fatalf("second register: %v", err)
	}
	if first.SubjectID != second.SubjectID {
		t.Fatalf("subject changed across retries: %q vs %q", first.SubjectID, second.SubjectID)
	}
	if first.Address != "idem@moonbyte.top" || second.Address != "idem@moonbyte.top" {
		t.Fatalf("address mismatch: %q / %q", first.Address, second.Address)
	}
	f.mu.Lock()
	created := f.createCount
	users := len(f.users)
	resets := len(f.resetPasswords)
	f.mu.Unlock()
	if created != 1 {
		t.Fatalf("create calls that succeeded = %d, want 1", created)
	}
	if users != 1 {
		t.Fatalf("sandbox users = %d, want 1 (no duplicate identity)", users)
	}
	if resets != 1 {
		t.Fatalf("password resets on retry = %d, want 1", resets)
	}
}

func TestRegisterRecoversFromConflictResponse(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	existing := f.seedUser("onboard-conflict", "conflict@moonbyte.top")
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-conflict", "conflict@moonbyte.top")
	if handle.SubjectID != existing {
		t.Fatalf("register subject = %q, want the existing identity %q", handle.SubjectID, existing)
	}
	if f.userCount() != 1 {
		t.Fatalf("users = %d, want 1 (no duplicate identity)", f.userCount())
	}
}

func TestRequestVerificationSendsVerifyEmail(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-verify", "verify@moonbyte.top")
	state, err := a.Identity().RequestVerification(context.Background(), provider.RequestVerificationRequest{
		SubjectID:      handle.SubjectID,
		Address:        handle.Address,
		IdempotencyKey: "run:verify",
	})
	if err != nil {
		t.Fatalf("request verification: %v", err)
	}
	if state != provider.VerificationPending {
		t.Fatalf("state = %s, want PENDING", state)
	}
	f.mu.Lock()
	actions := append([]url.Values(nil), f.executeActions...)
	body := append([]string(nil), f.executeActionsBody...)
	f.mu.Unlock()
	if len(actions) != 1 || len(body) != 1 {
		t.Fatalf("execute-actions calls = %d/%d, want 1/1", len(actions), len(body))
	}
	if body[0] != "VERIFY_EMAIL" {
		t.Fatalf("actions = %q, want VERIFY_EMAIL", body[0])
	}
	if actions[0].Get("client_id") != testClientID {
		t.Fatalf("client_id = %q", actions[0].Get("client_id"))
	}
	if actions[0].Get("lifespan") == "" {
		t.Fatal("action token lifespan was not bounded")
	}
}

func TestRequestVerificationSkipsVerifiedIdentity(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-done", "done@moonbyte.top")
	f.mu.Lock()
	f.users[handle.SubjectID].EmailVerified = true
	f.mu.Unlock()

	state, err := a.Identity().RequestVerification(context.Background(), provider.RequestVerificationRequest{
		SubjectID: handle.SubjectID, Address: handle.Address,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state != provider.VerificationVerified {
		t.Fatalf("state = %s, want VERIFIED", state)
	}
	f.mu.Lock()
	calls := len(f.executeActions)
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("execute-actions calls = %d, want 0 for an already verified identity", calls)
	}
}

func TestCheckVerificationReadsAuthoritativeFlag(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-check", "check@moonbyte.top")
	ctx := context.Background()

	state, err := a.Identity().CheckVerification(ctx, provider.CheckVerificationRequest{SubjectID: handle.SubjectID, Address: handle.Address})
	if err != nil || state != provider.VerificationPending {
		t.Fatalf("before verification: state=%s err=%v", state, err)
	}
	f.mu.Lock()
	f.users[handle.SubjectID].EmailVerified = true
	f.mu.Unlock()
	state, err = a.Identity().CheckVerification(ctx, provider.CheckVerificationRequest{SubjectID: handle.SubjectID, Address: handle.Address})
	if err != nil || state != provider.VerificationVerified {
		t.Fatalf("after verification: state=%s err=%v", state, err)
	}
}

func TestEmailMismatchIsRejected(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-mismatch", "mismatch@moonbyte.top")
	_, err := a.Identity().RequestVerification(context.Background(), provider.RequestVerificationRequest{
		SubjectID: handle.SubjectID, Address: "other@moonbyte.top",
	})
	if errs.ClassOf(err) != errs.ClassValidation {
		t.Fatalf("class = %s, want VALIDATION", errs.ClassOf(err))
	}
}

func TestEnsureIdentityIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	provisioner, ok := a.Identity().(provider.IdentityProvisioner)
	if !ok {
		t.Fatal("identity view does not implement IdentityProvisioner")
	}
	req := provider.EnsureIdentityRequest{SubjectID: "onboard-ensure", Email: "ensure@moonbyte.top", DisplayName: "ensure", Password: "Aa1!pw"}
	first, err := provisioner.EnsureIdentity(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provisioner.EnsureIdentity(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if first.SubjectID != second.SubjectID || f.userCount() != 1 {
		t.Fatalf("ensure was not idempotent: %q vs %q, users=%d", first.SubjectID, second.SubjectID, f.userCount())
	}
}

// ---------------------------------------------------------------------------
// Two-step email confirmation
// ---------------------------------------------------------------------------

func TestConfirmEmailFollowsInterstitialAndRechecksAdminAPI(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-confirm", "confirm@moonbyte.top")

	state, err := a.Sandbox().ConfirmEmail(context.Background(), provider.SandboxConfirmRequest{
		Identity:        handle,
		VerificationURL: f.issueActionToken(handle.SubjectID),
	})
	if err != nil {
		t.Fatalf("confirm email: %v", err)
	}
	if state != provider.VerificationVerified {
		t.Fatalf("state = %s, want VERIFIED", state)
	}
	user, ok := f.user(handle.SubjectID)
	if !ok || !user.EmailVerified {
		t.Fatal("admin API still reports emailVerified=false")
	}
	f.mu.Lock()
	second := f.confirmSecondHit
	f.mu.Unlock()
	if second != 1 {
		t.Fatalf("interstitial second step executed %d times, want 1", second)
	}
}

func TestConfirmEmailExpiredTokenIsNotReportedAsVerified(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-expired", "expired@moonbyte.top")

	f.mu.Lock()
	f.actionTokenExpired = true
	f.mu.Unlock()
	state, err := a.Sandbox().ConfirmEmail(context.Background(), provider.SandboxConfirmRequest{
		Identity:        handle,
		VerificationURL: f.issueActionToken(handle.SubjectID),
	})
	if err != nil {
		t.Fatalf("expired interstitial should not be a transport error: %v", err)
	}
	if state == provider.VerificationVerified {
		t.Fatal("expired action token was reported as verified")
	}

	f.mu.Lock()
	f.actionTokenExpired = false
	f.actionTokenStatus = 410
	f.mu.Unlock()
	_, err = a.Sandbox().ConfirmEmail(context.Background(), provider.SandboxConfirmRequest{
		Identity:        handle,
		VerificationURL: f.issueActionToken(handle.SubjectID),
	})
	if errs.ClassOf(err) != errs.ClassAuthExpired {
		t.Fatalf("410 class = %s, want AUTH_EXPIRED", errs.ClassOf(err))
	}
}

func TestConfirmEmailRejectsLinksOutsideTheSandbox(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-ssrf", "ssrf@moonbyte.top")
	ctx := context.Background()

	foreign := []string{
		"http://127.0.0.1:1/realms/" + testRealm + actionTokenPath + "?key=good",
		"https://evil.example.com/realms/" + testRealm + actionTokenPath + "?key=good",
		f.URL() + "/realms/" + testRealm + "/protocol/openid-connect/auth?client_id=x",
		"javascript:alert(1)",
		"",
	}
	f.mu.Lock()
	before := len(f.requestPaths)
	f.mu.Unlock()
	for _, link := range foreign {
		if _, err := a.Sandbox().ConfirmEmail(ctx, provider.SandboxConfirmRequest{Identity: handle, VerificationURL: link}); errs.ClassOf(err) != errs.ClassValidation {
			t.Fatalf("link %q class = %s, want VALIDATION", link, errs.ClassOf(err))
		}
	}
	f.mu.Lock()
	after := len(f.requestPaths)
	f.mu.Unlock()
	if before != after {
		t.Fatalf("rejected links still produced %d HTTP request(s)", after-before)
	}
}

// ---------------------------------------------------------------------------
// OIDC authorization code + PKCE
// ---------------------------------------------------------------------------

func TestAuthorizeOIDCFullPKCEFlow(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, sec := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-oidc", "oidc@moonbyte.top")

	cred, err := a.Sandbox().AuthorizeOIDC(context.Background(), provider.SandboxAuthorizeRequest{
		Identity:    handle,
		Scopes:      []string{"openid", "email", "profile"},
		RedirectURI: testCallback,
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if _, _, ok := splitRef(cred.Ref); !ok {
		t.Fatalf("credential ref %q is not opaque", cred.Ref)
	}
	if !strings.HasPrefix(cred.Ref, provider.SecretRefPrefix+secretKindOIDC+"/") {
		t.Fatalf("credential ref = %q, want secret://%s/<id>", cred.Ref, secretKindOIDC)
	}
	if cred.Subject != handle.SubjectID {
		t.Fatalf("subject = %q, want %q", cred.Subject, handle.SubjectID)
	}
	if cred.Issuer != f.URL()+"/realms/"+testRealm {
		t.Fatalf("issuer = %q", cred.Issuer)
	}
	if cred.Audience != testClientID {
		t.Fatalf("audience = %q, want %q", cred.Audience, testClientID)
	}
	if !cred.ExpiresAt.After(time.Now()) {
		t.Fatalf("expires_at = %s, want in the future", cred.ExpiresAt)
	}

	// Tokens only exist inside the secret store.
	stored := sec.all()
	if len(stored) != 1 {
		t.Fatalf("stored secrets = %d, want exactly one credential", len(stored))
	}
	raw, ok := stored[cred.Ref]
	if !ok {
		t.Fatalf("credential ref %q was not stored", cred.Ref)
	}
	if !strings.Contains(raw, "\"access_token\"") || !strings.Contains(raw, "\"id_token\"") {
		t.Fatal("stored credential envelope is missing the OIDC tokens")
	}
	if strings.Contains(cred.Ref, "access-token") || strings.Contains(cred.Issuer, "access-token") {
		t.Fatal("token material leaked into the public credential reference")
	}

	result, err := a.Sandbox().Validate(context.Background(), cred)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if result.State != provider.CredentialValid || result.SubjectID != handle.SubjectID {
		t.Fatalf("validate result = %+v", result)
	}
	verified, err := a.Credential().Verify(context.Background(), cred.Ref)
	if err != nil {
		t.Fatalf("verify credential: %v", err)
	}
	if verified.State != provider.CredentialValid || verified.SubjectID != handle.SubjectID {
		t.Fatalf("verify result = %+v", verified)
	}
	f.mu.Lock()
	auths := append([]string(nil), f.userinfoAuth...)
	f.mu.Unlock()
	foundBearer := false
	for _, h := range auths {
		if strings.HasPrefix(h, "Bearer access-token-") {
			foundBearer = true
		}
	}
	if !foundBearer {
		t.Fatal("userinfo was never called with the stored access token")
	}
}

func TestAuthorizeOIDCRejectsWrongVerifier(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-pkce", "pkce@moonbyte.top")
	f.mu.Lock()
	f.forceWrongVerifier = true
	f.mu.Unlock()

	var err error
	for attempt := 0; attempt < 2; attempt++ {
		_, err = a.Sandbox().AuthorizeOIDC(context.Background(), provider.SandboxAuthorizeRequest{
			Identity: handle, RedirectURI: testCallback,
		})
		// The fake IdP can see a transient connection reset under the full
		// parallel race suite; retry only that infrastructure class so the
		// semantic assertion below stays strict.
		if errs.ClassOf(err) != errs.ClassUpstreamTransient {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if errs.ClassOf(err) != errs.ClassAuthExpired {
		t.Fatalf("class = %s, want AUTH_EXPIRED for a rejected PKCE exchange (%v)", errs.ClassOf(err), err)
	}
}

func TestAuthorizeOIDCRejectsBadNonce(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-nonce", "nonce@moonbyte.top")
	f.mu.Lock()
	f.badNonce = true
	f.mu.Unlock()

	_, err := a.Sandbox().AuthorizeOIDC(context.Background(), provider.SandboxAuthorizeRequest{
		Identity: handle, RedirectURI: testCallback,
	})
	if errs.ClassOf(err) != errs.ClassProviderBug {
		t.Fatalf("class = %s, want PROVIDER_BUG for a bad id_token nonce (%v)", errs.ClassOf(err), err)
	}
}

func TestAuthorizeOIDCLoginRejectedMapsToAuthExpired(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-login", "login@moonbyte.top")
	f.mu.Lock()
	f.loginRejects = true
	f.mu.Unlock()

	_, err := a.Sandbox().AuthorizeOIDC(context.Background(), provider.SandboxAuthorizeRequest{
		Identity: handle, RedirectURI: testCallback,
	})
	if errs.ClassOf(err) != errs.ClassAuthExpired {
		t.Fatalf("class = %s, want AUTH_EXPIRED (%v)", errs.ClassOf(err), err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "password") {
		t.Fatalf("login error leaked credential details: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Credential validation and error taxonomy
// ---------------------------------------------------------------------------

func TestValidateExpiredCredential(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, sec := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-expcred", "expcred@moonbyte.top")
	// userinfo rejects the token with 401 and the envelope is past expiry.
	cred := seedCredential(t, f, sec, handle.SubjectID, "expired-token", time.Now().Add(-time.Minute))
	f.mu.Lock()
	delete(f.tokens, "expired-token")
	f.mu.Unlock()

	result, err := a.Sandbox().Validate(context.Background(), cred)
	if err != nil {
		t.Fatalf("validate expired credential should not error: %v", err)
	}
	if result.State != provider.CredentialExpired {
		t.Fatalf("state = %s, want EXPIRED", result.State)
	}
}

func TestValidateInvalidCredential(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, sec := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-badcred", "badcred@moonbyte.top")
	cred := seedCredential(t, f, sec, handle.SubjectID, "revoked-token", time.Now().Add(time.Hour))
	f.mu.Lock()
	delete(f.tokens, "revoked-token")
	f.mu.Unlock()

	result, err := a.Sandbox().Validate(context.Background(), cred)
	if err != nil {
		t.Fatalf("validate invalid credential: %v", err)
	}
	if result.State != provider.CredentialInvalid {
		t.Fatalf("state = %s, want INVALID", result.State)
	}
}

func TestValidateRejectsForeignReferenceKind(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	_, err := a.Sandbox().Validate(context.Background(), provider.SandboxCredentialRef{Ref: "secret://mailbox/abc"})
	if errs.ClassOf(err) != errs.ClassValidation {
		t.Fatalf("class = %s, want VALIDATION", errs.ClassOf(err))
	}
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mutate    func(*fakeKeycloak)
		timeout   time.Duration
		wantClass errs.Class
		wantRetry time.Duration
	}{
		{"rate limited with retry-after", func(f *fakeKeycloak) { f.userinfoStatus = 429; f.userinfoRetryAfter = "7" }, 0, errs.ClassRateLimited, 7 * time.Second},
		{"upstream 5xx", func(f *fakeKeycloak) { f.userinfoStatus = 503 }, 0, errs.ClassUpstreamTransient, 0},
		{"timeout", func(f *fakeKeycloak) { f.userinfoDelay = 500 * time.Millisecond }, 100 * time.Millisecond, errs.ClassTimeoutUncertain, 0},
		{"malformed json", func(f *fakeKeycloak) { f.userinfoMalformed = true }, 0, errs.ClassProviderBug, 0},
		{"cross origin redirect", func(f *fakeKeycloak) { f.userinfoCrossSite = true }, 0, errs.ClassProviderBug, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeKeycloak(t, testRealm, testClientID)
			a, sec := newTestAdapter(t, f, nil)
			handle := register(t, a, "onboard-mapping", "mapping@moonbyte.top")
			if tc.timeout > 0 {
				// Apply the short timeout only to the validation call: a cold CI
				// runner may legitimately spend >100ms registering the user.
				a.timeout = tc.timeout
				a.api.Timeout = tc.timeout
			}
			cred := seedCredential(t, f, sec, handle.SubjectID, "mapping-token", time.Now().Add(time.Hour))
			f.mu.Lock()
			tc.mutate(f)
			f.mu.Unlock()

			_, err := a.Sandbox().Validate(context.Background(), cred)
			if err == nil {
				t.Fatalf("expected a classified error")
			}
			if got := errs.ClassOf(err); got != tc.wantClass {
				t.Fatalf("class = %s, want %s (%v)", got, tc.wantClass, err)
			}
			if tc.wantRetry > 0 && errs.RetryAfterOf(err) != tc.wantRetry {
				t.Fatalf("retry-after = %s, want %s", errs.RetryAfterOf(err), tc.wantRetry)
			}
		})
	}

	for _, status := range []int{401, 403} {
		status := status
		t.Run("userinfo rejected token "+strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			f := newFakeKeycloak(t, testRealm, testClientID)
			a, sec := newTestAdapter(t, f, nil)
			handle := register(t, a, "onboard-rejected", "rejected@moonbyte.top")
			cred := seedCredential(t, f, sec, handle.SubjectID, "rejected-token", time.Now().Add(time.Hour))
			f.mu.Lock()
			f.userinfoStatus = status
			delete(f.tokens, "rejected-token")
			f.mu.Unlock()
			result, err := a.Sandbox().Validate(context.Background(), cred)
			if err != nil {
				t.Fatalf("rejected token must map to a credential state, got error: %v", err)
			}
			if result.State != provider.CredentialInvalid {
				t.Fatalf("state = %s, want INVALID", result.State)
			}
		})
	}

	t.Run("admin 403 not authorized", func(t *testing.T) {
		t.Parallel()
		f := newFakeKeycloak(t, testRealm, testClientID)
		a, _ := newTestAdapter(t, f, nil)
		f.mu.Lock()
		f.adminStatus = 403
		f.mu.Unlock()
		_, err := a.Identity().CheckVerification(context.Background(), provider.CheckVerificationRequest{SubjectID: "onboard-any"})
		if errs.ClassOf(err) != errs.ClassNotAuthorized {
			t.Fatalf("class = %s, want NOT_AUTHORIZED", errs.ClassOf(err))
		}
	})

	t.Run("token endpoint 5xx", func(t *testing.T) {
		t.Parallel()
		f := newFakeKeycloak(t, testRealm, testClientID)
		a, _ := newTestAdapter(t, f, nil)
		handle := register(t, a, "onboard-token500", "token500@moonbyte.top")
		f.mu.Lock()
		f.tokenStatus = 500
		f.mu.Unlock()
		_, err := a.Sandbox().AuthorizeOIDC(context.Background(), provider.SandboxAuthorizeRequest{Identity: handle, RedirectURI: testCallback})
		if errs.ClassOf(err) != errs.ClassUpstreamTransient {
			t.Fatalf("class = %s, want UPSTREAM_TRANSIENT", errs.ClassOf(err))
		}
	})

	t.Run("action token 5xx", func(t *testing.T) {
		t.Parallel()
		f := newFakeKeycloak(t, testRealm, testClientID)
		a, _ := newTestAdapter(t, f, nil)
		handle := register(t, a, "onboard-action500", "action500@moonbyte.top")
		f.mu.Lock()
		f.actionTokenStatus = 503
		f.mu.Unlock()
		_, err := a.Sandbox().ConfirmEmail(context.Background(), provider.SandboxConfirmRequest{
			Identity: handle, VerificationURL: f.issueActionToken(handle.SubjectID),
		})
		if errs.ClassOf(err) != errs.ClassUpstreamTransient {
			t.Fatalf("class = %s, want UPSTREAM_TRANSIENT", errs.ClassOf(err))
		}
	})
}

func TestSecretsNeverAppearInErrorsOrDTOs(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	handle := register(t, a, "onboard-leak", "leak@moonbyte.top")
	f.mu.Lock()
	f.tokenStatus = 400
	f.tokenSecretBody = "SUPER-SECRET-TOKEN-VALUE"
	f.mu.Unlock()

	_, err := a.Sandbox().AuthorizeOIDC(context.Background(), provider.SandboxAuthorizeRequest{Identity: handle, RedirectURI: testCallback})
	if err == nil {
		t.Fatal("expected token exchange failure")
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-TOKEN-VALUE") {
		t.Fatalf("upstream body leaked into the error: %v", err)
	}

	// Successful DTO must not serialize token material nor the opaque ref.
	f2 := newFakeKeycloak(t, testRealm, testClientID)
	a2, sec2 := newTestAdapter(t, f2, nil)
	h2 := register(t, a2, "onboard-dto", "dto@moonbyte.top")
	cred, err := a2.Sandbox().AuthorizeOIDC(context.Background(), provider.SandboxAuthorizeRequest{Identity: h2, RedirectURI: testCallback})
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := sec2.all()[cred.Ref]
	if !ok {
		t.Fatal("credential was not stored")
	}
	var envelope tokenEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatal(err)
	}
	dto, err := json.Marshal(cred)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dto), envelope.AccessToken) {
		t.Fatalf("access token leaked into the public DTO: %s", dto)
	}
	if strings.Contains(string(dto), "secret://") {
		t.Fatalf("opaque credential ref leaked into the public DTO: %s", dto)
	}
}

func TestLookupByEmailAndUsernameExactMatch(t *testing.T) {
	t.Parallel()
	f := newFakeKeycloak(t, testRealm, testClientID)
	a, _ := newTestAdapter(t, f, nil)
	register(t, a, "onboard-lookup", "lookup@moonbyte.top")

	state, err := a.Identity().CheckVerification(context.Background(), provider.CheckVerificationRequest{
		SubjectID: "onboard-lookup", Address: "lookup@moonbyte.top",
	})
	if err != nil || state != provider.VerificationPending {
		t.Fatalf("username lookup: state=%s err=%v", state, err)
	}
	state, err = a.Identity().CheckVerification(context.Background(), provider.CheckVerificationRequest{
		Address: "lookup@moonbyte.top",
	})
	if err != nil || state != provider.VerificationPending {
		t.Fatalf("email lookup: state=%s err=%v", state, err)
	}
}
