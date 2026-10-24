package workflow_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/provider/fake"
	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

type harness struct {
	store  *storage.Store
	world  *fake.World
	sec    *secrets.Service
	reg    *provider.Registry
	engine *workflow.Engine
}

func newHarness(t *testing.T, mutate func(*workflow.Config, *provider.Registry)) *harness {
	t.Helper()
	master := bytes.Repeat([]byte{7}, 32)
	ring, err := security.NewKeyRing(master)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(context.Background(), storage.Options{
		Path:     filepath.Join(t.TempDir(), "state.sqlite"),
		HMACKey:  security.DeriveKey(master, "storage-hmac"),
		Redactor: security.NewRedactor(),
		TransitionValidator: func(from, to string) error {
			return state.CanTransition(state.RunState(from), state.RunState(to))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sec := secrets.New(store, security.NewBox(ring))
	world := fake.NewWorld(sec)
	reg := provider.NewRegistry()
	reg.AddMailbox(fake.NewMailbox(world))
	reg.AddIdentity(fake.NewIdentity(world))
	reg.AddOAuth(fake.NewOAuth(world))
	reg.AddCredential(fake.NewCredential(world))
	reg.AddSandbox(fake.NewSandbox(world))
	reg.AddEmailManager(fake.NewEmailManager(world))
	cfg := workflow.Config{
		MaxConcurrency:  1,
		TickInterval:    10 * time.Millisecond,
		LeaseTTL:        10 * time.Second,
		StepTimeout:     3 * time.Second,
		PollInterval:    20 * time.Millisecond,
		MaxAttempts:     2,
		RunDeadline:     time.Minute,
		Domains:         []string{"moonbyte.test"},
		SenderAllowlist: []string{"no-reply", "idp.fake.test"},
		FingerprintKey:  []byte("test-fingerprint"),
	}
	if mutate != nil {
		mutate(&cfg, reg)
	}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return &harness{store: store, world: world, sec: sec, reg: reg, engine: workflow.New(store, reg, sec, log, cfg)}
}

func (h *harness) create(t *testing.T, req workflow.CreateRunRequest) storage.Run {
	t.Helper()
	params, err := h.engine.PrepareRun(req, "operator", time.Now().UTC())
	if err != nil {
		t.Fatalf("prepare run: %v", err)
	}
	run, created, err := h.store.CreateRun(context.Background(), params)
	if err != nil || !created {
		t.Fatalf("create run: %v created=%v", err, created)
	}
	return run
}

func (h *harness) tick(t *testing.T) bool {
	t.Helper()
	done, err := h.engine.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	return done
}

func (h *harness) waitTerminal(t *testing.T, runID string, maxTicks int) storage.Run {
	t.Helper()
	for i := 0; i < maxTicks; i++ {
		run, err := h.store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if state.RunState(run.Status).IsTerminal() {
			return run
		}
		h.tick(t)
		time.Sleep(10 * time.Millisecond)
	}
	run, _ := h.store.GetRun(context.Background(), runID)
	t.Fatalf("run did not reach a terminal state: status=%s step=%s", run.Status, run.CurrentStep)
	return run
}

func (h *harness) waitStatus(t *testing.T, runID, want string, maxTicks int) storage.Run {
	t.Helper()
	for i := 0; i < maxTicks; i++ {
		run, err := h.store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == want {
			return run
		}
		if state.RunState(run.Status).IsTerminal() {
			t.Fatalf("run reached terminal %s while waiting for %s", run.Status, want)
		}
		h.tick(t)
		time.Sleep(10 * time.Millisecond)
	}
	run, _ := h.store.GetRun(context.Background(), runID)
	t.Fatalf("status %s not reached, current %s", want, run.Status)
	return run
}

func sandboxRequest(idem string) workflow.CreateRunRequest {
	return workflow.CreateRunRequest{
		Template:         "sandbox-email-verify-oidc",
		Mode:             "sandbox-e2e",
		MailboxProvider:  "fake-mailbox",
		IdentityProvider: "fake-identity",
		OAuthProvider:    "fake-oauth",
		SandboxProvider:  "fake-sandbox",
		IdempotencyKey:   idem,
	}
}

func TestSandboxTemplateCompletesWithFakeProviders(t *testing.T) {
	h := newHarness(t, nil)
	run := h.create(t, sandboxRequest("sandbox-e2e-1"))
	final := h.waitTerminal(t, run.ID, 300)
	if final.Status != "COMPLETED" {
		t.Fatalf("status = %s error=%s", final.Status, final.LastErrorClass)
	}
	if final.ActionRequired {
		t.Fatal("completed run still requires action")
	}
	events, err := h.store.ListEvents(context.Background(), run.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, ev := range events {
		types[ev.Type]++
		for k, v := range ev.Payload {
			if k == "operation_key" {
				continue // opaque run:step:version identifier, not a secret
			}
			if s, ok := v.(string); ok && (len(s) > 40 || containsSecretish(s)) {
				t.Fatalf("event payload key %s may leak a secret: %q", k, s)
			}
		}
	}
	for _, want := range []string{"RUN_CREATED", "STEP_STARTED", "STEP_SUCCEEDED", "OAUTH_STARTED", "RUN_COMPLETED"} {
		if types[want] == 0 {
			t.Fatalf("missing event %s in %v", want, types)
		}
	}
	steps, err := h.store.ListSteps(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	tmpl, ok := h.engine.Template("sandbox-email-verify-oidc")
	if !ok {
		t.Fatal("template missing")
	}
	if len(steps) != len(tmpl.Steps) {
		t.Fatalf("unexpected step count %d", len(steps))
	}
	for _, st := range steps {
		if st.Status != "SUCCEEDED" {
			t.Fatalf("step %s status %s", st.Name, st.Status)
		}
	}
}

func containsSecretish(s string) bool {
	return bytes.Contains([]byte(s), []byte("secret://")) || bytes.Contains([]byte(s), []byte("eyJ"))
}

func TestIdempotentRunCreation(t *testing.T) {
	h := newHarness(t, nil)
	req := sandboxRequest("same-key-001")
	first := h.create(t, req)
	params, err := h.engine.PrepareRun(req, "operator", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := h.store.CreateRun(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("idempotency broken: created=%v %s != %s", created, second.ID, first.ID)
	}
	req.MailboxProvider = "fake-mailbox"
	req.EmailProvider = "fake-github"
	if _, err := h.engine.PrepareRun(req, "operator", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledProviderRejectedBeforeNetwork(t *testing.T) {
	h := newHarness(t, func(cfg *workflow.Config, reg *provider.Registry) {
		// Replace the enabled fake with a disabled wrapper.
		reg.AddMailbox(disabledMailbox{MailboxProvider: fake.NewMailbox(fake.NewWorld(nil))})
	})
	_, err := h.engine.PrepareRun(sandboxRequest("disabled-1"), "operator", time.Now().UTC())
	if err == nil {
		t.Fatal("disabled provider accepted")
	}
	if class := errs.ClassOf(err); class != errs.ClassNotAuthorized {
		t.Fatalf("class = %s, want NOT_AUTHORIZED (%v)", class, err)
	}
}

type disabledMailbox struct{ provider.MailboxProvider }

func (d disabledMailbox) Descriptor() provider.Descriptor {
	desc := d.MailboxProvider.Descriptor()
	desc.Enabled = false
	desc.RestrictionReason = "disabled in test"
	return desc
}

func TestOAuthExpiredRequiresReauthorization(t *testing.T) {
	h := newHarness(t, nil)
	h.world.OAuthExpire = true
	run := h.create(t, workflow.CreateRunRequest{
		Template:            "oauth-existing-account",
		Mode:                "real-account",
		MailboxProvider:     "fake-mailbox",
		IdentityProvider:    "fake-identity",
		OAuthProvider:       "fake-oauth",
		ExistingIdentityRef: "existing-account-1",
		IdempotencyKey:      "oauth-expire-1",
	})
	waiting := h.waitStatus(t, run.ID, "REAUTHORIZE_REQUIRED", 400)
	if !waiting.ActionRequired {
		t.Fatal("reauthorization state must require action")
	}
	// Resuming must open a new authorization session rather than replaying the
	// expired state.
	if _, err := h.store.RequestResume(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	again := h.waitStatus(t, run.ID, "REAUTHORIZE_REQUIRED", 400)
	if again.Status != "REAUTHORIZE_REQUIRED" {
		t.Fatalf("status = %s", again.Status)
	}
}

func TestOAuthDeniedCancelsRun(t *testing.T) {
	h := newHarness(t, nil)
	h.world.OAuthDeny = true
	run := h.create(t, workflow.CreateRunRequest{
		Template: "oauth-existing-account", Mode: "real-account",
		MailboxProvider: "fake-mailbox", IdentityProvider: "fake-identity",
		OAuthProvider: "fake-oauth", ExistingIdentityRef: "existing-account-2",
		IdempotencyKey: "oauth-deny-1",
	})
	final := h.waitTerminal(t, run.ID, 400)
	if final.Status != "CANCELLED" {
		t.Fatalf("status = %s (want CANCELLED)", final.Status)
	}
}

func TestGitHubHumanVerificationResume(t *testing.T) {
	h := newHarness(t, nil)
	run := h.create(t, workflow.CreateRunRequest{
		Template: "github-existing-email", Mode: "real-account",
		MailboxProvider: "fake-mailbox", IdentityProvider: "fake-identity",
		OAuthProvider: "fake-oauth", EmailProvider: "fake-github",
		ExistingIdentityRef: "existing-account-3", IdempotencyKey: "github-1",
	})
	waiting := h.waitStatus(t, run.ID, "WAITING_USER_ACTION", 400)
	if !waiting.ActionRequired {
		t.Fatal("github flow must require a human action")
	}
	mailbox, err := h.store.LatestResource(context.Background(), run.ID, "mailbox")
	if err != nil {
		t.Fatal(err)
	}
	if mailbox.DisplayAlias == "" {
		t.Fatal("mailbox alias missing")
	}
	h.world.VerifyEmail(mailbox.DisplayAlias)
	if _, err := h.store.RequestResume(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	final := h.waitTerminal(t, run.ID, 400)
	if final.Status != "COMPLETED" {
		t.Fatalf("status = %s error=%s", final.Status, final.LastErrorClass)
	}
}

type rateLimitedMailbox struct{ provider.MailboxProvider }

func (r rateLimitedMailbox) Descriptor() provider.Descriptor {
	return r.MailboxProvider.Descriptor()
}

func (r rateLimitedMailbox) CreateAddress(ctx context.Context, req provider.CreateAddressRequest) (provider.Mailbox, error) {
	return provider.Mailbox{}, errs.New(errs.ClassRateLimited, "test.mailbox", "rate limited by provider")
}

func TestRetryBudgetThenTerminal(t *testing.T) {
	h := newHarness(t, nil)
	h.reg.AddMailbox(rateLimitedMailbox{MailboxProvider: fake.NewMailbox(h.world)})
	run := h.create(t, workflow.CreateRunRequest{
		Template: "cloudflare-mail-smoke", Mode: "cloudflare-integration",
		MailboxProvider: "fake-mailbox", IdentityProvider: "fake-identity",
		OAuthProvider: "fake-oauth", IdempotencyKey: "retry-1",
	})
	final := h.waitTerminal(t, run.ID, 700)
	if final.Status != "FAILED_TERMINAL" {
		t.Fatalf("status = %s", final.Status)
	}
	if final.LastErrorClass != string(errs.ClassRateLimited) {
		t.Fatalf("error class = %s", final.LastErrorClass)
	}
	steps, _ := h.store.ListSteps(context.Background(), run.ID)
	for _, st := range steps {
		if st.Name == "prepare_mailbox" {
			if st.FailureCount != 2 {
				t.Fatalf("failure count = %d (want 2 failures counted)", st.FailureCount)
			}
			if st.Status != "FAILED_TERMINAL" {
				t.Fatalf("step status = %s", st.Status)
			}
		}
	}
}

type versionedMailbox struct{ provider.MailboxProvider }

func (v versionedMailbox) Descriptor() provider.Descriptor {
	desc := v.MailboxProvider.Descriptor()
	desc.ContractVersion = "fake-2"
	return desc
}

func TestProviderContractMismatchFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	run := h.create(t, sandboxRequest("contract-1"))
	// Advance one step so the run has a snapshot and a claimable step.
	h.tick(t)

	// Build a second engine whose registry claims a new contract version.
	reg2 := provider.NewRegistry()
	reg2.AddMailbox(versionedMailbox{MailboxProvider: fake.NewMailbox(h.world)})
	reg2.AddIdentity(fake.NewIdentity(h.world))
	reg2.AddOAuth(fake.NewOAuth(h.world))
	reg2.AddCredential(fake.NewCredential(h.world))
	reg2.AddSandbox(fake.NewSandbox(h.world))
	reg2.AddEmailManager(fake.NewEmailManager(h.world))
	engine2 := workflow.New(h.store, reg2, h.sec, slog.New(slog.NewJSONHandler(io.Discard, nil)), workflow.Config{
		MaxConcurrency: 1, LeaseTTL: 10 * time.Second, StepTimeout: 3 * time.Second,
		PollInterval: 20 * time.Millisecond, MaxAttempts: 2, RunDeadline: time.Minute,
		Domains: []string{"moonbyte.test"}, FingerprintKey: []byte("fp"),
	})
	for i := 0; i < 40; i++ {
		current, _ := h.store.GetRun(context.Background(), run.ID)
		if state.RunState(current.Status).IsTerminal() {
			break
		}
		if _, err := engine2.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	final, _ := h.store.GetRun(context.Background(), run.ID)
	if final.Status != "FAILED_TERMINAL" {
		t.Fatalf("status = %s (want FAILED_TERMINAL)", final.Status)
	}
	if final.LastErrorClass != string(errs.ClassProviderBug) {
		t.Fatalf("error class = %s", final.LastErrorClass)
	}
}

func TestPrepareRunRejectsUnknownTemplateAndMode(t *testing.T) {
	h := newHarness(t, nil)
	req := sandboxRequest("bad-1")
	req.Template = "does-not-exist"
	if _, err := h.engine.PrepareRun(req, "operator", time.Now().UTC()); err == nil {
		t.Fatal("unknown template accepted")
	}
	req = sandboxRequest("bad-2")
	req.Mode = "real-account"
	if _, err := h.engine.PrepareRun(req, "operator", time.Now().UTC()); err == nil {
		t.Fatal("template/mode mismatch accepted")
	}
	req = sandboxRequest("bad-3")
	req.MailboxProvider = "unknown-provider"
	if _, err := h.engine.PrepareRun(req, "operator", time.Now().UTC()); err == nil {
		t.Fatal("unknown provider accepted")
	}
	req = workflow.CreateRunRequest{
		Template: "github-existing-email", Mode: "real-account",
		MailboxProvider: "fake-mailbox", IdentityProvider: "fake-identity", OAuthProvider: "fake-oauth",
	}
	if _, err := h.engine.PrepareRun(req, "operator", time.Now().UTC()); err == nil {
		t.Fatal("real-account run without existing_identity_ref accepted")
	}
}

func TestCancelWinsOverInFlightStep(t *testing.T) {
	h := newHarness(t, nil)
	run := h.create(t, sandboxRequest("cancel-1"))
	if _, err := h.store.RequestCancel(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	final := h.waitTerminal(t, run.ID, 300)
	if final.Status != "CANCELLED" {
		t.Fatalf("status = %s", final.Status)
	}
	if _, err := h.store.RequestResume(context.Background(), run.ID); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("resume on cancelled run should conflict, got %v", err)
	}
}

// expiredCredential makes verify_credential fail with AUTH_EXPIRED after a
// successful OAuth flow. Regression for REVIEW P1-1: this outcome must commit
// as REAUTHORIZE_REQUIRED instead of spinning until the deadline.
type expiredCredential struct{ provider.CredentialVerifier }

func (e expiredCredential) Descriptor() provider.Descriptor {
	d := e.CredentialVerifier.Descriptor()
	d.Name = "fake-credential"
	return d
}

func (e expiredCredential) Verify(context.Context, string) (provider.CredentialResult, error) {
	return provider.CredentialResult{State: provider.CredentialExpired}, nil
}

func TestCredentialExpiryAfterOAuthRequiresReauthorization(t *testing.T) {
	h := newHarness(t, nil)
	h.reg.AddCredential(expiredCredential{CredentialVerifier: fake.NewCredential(h.world)})
	run := h.create(t, workflow.CreateRunRequest{
		Template: "oauth-existing-account", Mode: "real-account",
		MailboxProvider: "fake-mailbox", IdentityProvider: "fake-identity",
		OAuthProvider: "fake-oauth", ExistingIdentityRef: "existing-account-expired",
		IdempotencyKey: "credential-expired-1",
	})
	deadline := time.Now().Add(15 * time.Second)
	var final storage.Run
	for time.Now().Before(deadline) {
		final, _ = h.store.GetRun(context.Background(), run.ID)
		if final.Status == "REAUTHORIZE_REQUIRED" {
			break
		}
		if state.RunState(final.Status).IsTerminal() {
			steps, _ := h.store.ListSteps(context.Background(), run.ID)
			for _, st := range steps {
				t.Logf("step %s status=%s error=%s summary=%s attempts=%d", st.Name, st.Status, st.ErrorClass, st.ErrorSummary, st.AttemptCount)
			}
			t.Fatalf("run reached terminal %s (class %s) step=%s waiting for REAUTHORIZE_REQUIRED",
				final.Status, final.LastErrorClass, final.CurrentStep)
		}
		h.tick(t)
		time.Sleep(20 * time.Millisecond)
	}
	if final.Status != "REAUTHORIZE_REQUIRED" {
		t.Fatalf("status = %s (want REAUTHORIZE_REQUIRED)", final.Status)
	}
	if !final.ActionRequired {
		t.Fatal("reauthorization must require an operator action")
	}
	if final.LastErrorClass != string(errs.ClassAuthExpired) {
		t.Fatalf("last_error_class = %s", final.LastErrorClass)
	}
	steps, err := h.store.ListSteps(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range steps {
		if st.Name == "verify_credential" && st.Status != "REAUTHORIZE_REQUIRED" {
			t.Fatalf("verify_credential status = %s", st.Status)
		}
	}
}
