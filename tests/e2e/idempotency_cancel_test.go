package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestIdempotencyKeyCreatesOneRun is E2E-002: submitting the same template with
// the same Idempotency-Key twice returns the same run id and produces exactly
// one sandbox identity.
func TestIdempotencyKeyCreatesOneRun(t *testing.T) {
	env := requireSandbox(t)
	testStart := time.Now()
	proc := startOnboardd(t, env, procOptions{Addr: "127.0.0.1:18790"})
	admin := newKCAdmin(env)
	mp := newMailpitClient(env)
	ctx := context.Background()
	t.Cleanup(func() { cleanupSandboxUsers(t, admin, nil, testStart.UnixMilli()) })

	idem := "r2-idempotency-" + randomHex(t, 4)
	first, firstStatus := proc.createRun(idem)
	second, secondStatus := proc.createRun(idem)
	t.Logf("first create: status=%d run=%s", firstStatus, first.ID)
	t.Logf("second create (same key): status=%d run=%s", secondStatus, second.ID)
	if firstStatus != 202 || secondStatus != 202 {
		t.Fatalf("create statuses = %d/%d, want 202/202", firstStatus, secondStatus)
	}
	if first.ID != second.ID {
		t.Fatalf("same idempotency key returned different runs: %s vs %s", first.ID, second.ID)
	}

	final := proc.waitForStatus(first.ID, 3*time.Minute, "COMPLETED", "FAILED_TERMINAL")
	if final.Status != "COMPLETED" {
		t.Fatalf("run %s: status=%s error=%s", first.ID, final.Status, final.LastErrorClass)
	}

	// Exactly one RUN_CREATED event proves the duplicate POST was not persisted.
	runCreated := 0
	for _, event := range proc.events(first.ID) {
		if event.Type == "RUN_CREATED" {
			runCreated++
		}
	}
	if runCreated != 1 {
		t.Fatalf("RUN_CREATED events = %d, want 1", runCreated)
	}

	proc.terminate()
	probe := openProbe(t, proc)
	fixture := probe.runFixture(t, first.ID)
	envelope := probe.credential(t, fixture.CredentialRef)
	_, _, verified, status, err := userinfo(ctx, env, envelope.AccessToken)
	if err != nil || status != 200 || !verified {
		t.Fatalf("credential userinfo: status=%d verified=%v err=%v", status, verified, err)
	}

	users := sandboxUsersSince(t, admin, testStart)
	if len(users) != 1 {
		t.Fatalf("keycloak sandbox users = %d, want exactly 1 for a duplicated idempotency key", len(users))
	}
	if users[0].ID != fixture.SubjectID {
		t.Fatalf("keycloak user %s != run subject %s", users[0].ID, fixture.SubjectID)
	}
	if !users[0].EmailVerified {
		t.Fatalf("keycloak user %s is not emailVerified", users[0].Username)
	}

	messages, err := mp.messages(ctx)
	if err != nil {
		t.Fatalf("list mailpit messages: %v", err)
	}
	deliveries := 0
	for _, msg := range messages {
		for _, recipient := range messageRecipients(msg) {
			if strings.EqualFold(recipient, fixture.MailboxAddress) {
				deliveries++
			}
		}
	}
	if deliveries != 1 {
		t.Fatalf("verification emails for %s = %d, want exactly 1", fixture.MailboxAddress, deliveries)
	}

	evidence := map[string]any{
		"test":                       "E2E-002 idempotent run creation",
		"generated_at":               time.Now().UTC().Format(time.RFC3339),
		"environment":                "LOCAL SANDBOX (Keycloak " + env.KeycloakURL + ", Mailpit " + env.MailpitURL + ")",
		"idempotency_key":            idem,
		"first_status":               firstStatus,
		"second_status":              secondStatus,
		"same_run_id":                first.ID == second.ID,
		"run_id":                     first.ID,
		"run_created_events":         runCreated,
		"keycloak_users":             len(users),
		"keycloak_subject":           users[0].ID,
		"mailbox":                    fixture.MailboxAddress,
		"verification_emails":        deliveries,
		"final_status":               final.Status,
		"credential_userinfo_status": status,
	}
	writeEvidence(t, proc, "evidence-idempotency.json", evidence)

	cleanupSandboxUsers(t, admin, []string{fixture.SubjectID}, testStart.UnixMilli())
	cleanupMailpit(t, mp, []string{fixture.MailboxAddress})
}

// TestCancelWhileWaitingEmail is the E2E-006 surrogate: cancel a run while it is
// durably WAITING_EMAIL. The run becomes CANCELLED, no credential is stored and
// no later step executes.
//
// The wait is deterministic: the engine's mailx sender allowlist omits the
// Keycloak sender domain, so the delivered verification email cannot match and
// the run stays in WAITING_EMAIL until it is cancelled.
func TestCancelWhileWaitingEmail(t *testing.T) {
	env := requireSandbox(t)
	testStart := time.Now()
	proc := startOnboardd(t, env, procOptions{
		Addr:            "127.0.0.1:18791",
		Tick:            100 * time.Millisecond,
		SenderAllowlist: "idp.fake.test",
	})
	admin := newKCAdmin(env)
	mp := newMailpitClient(env)
	t.Cleanup(func() { cleanupSandboxUsers(t, admin, nil, testStart.UnixMilli()) })

	run, _ := proc.createRun("r2-cancel-" + randomHex(t, 4))
	waiting := proc.waitForStatus(run.ID, 90*time.Second, "WAITING_EMAIL")
	t.Logf("cancelling run %s while status=%s", run.ID, waiting.Status)

	canceled := proc.cancel(run.ID)
	if canceled.Status != "CANCEL_REQUESTED" && canceled.Status != "CANCELLED" {
		t.Fatalf("cancel response status = %s, want CANCEL_REQUESTED or CANCELLED", canceled.Status)
	}
	final := proc.waitForStatus(run.ID, 60*time.Second, "CANCELLED")
	if final.Status != "CANCELLED" {
		t.Fatalf("final status = %s, want CANCELLED", final.Status)
	}

	// No OAuth work may start after cancellation.
	events := proc.events(run.ID)
	startedLater := 0
	oauthStarted := 0
	cancelledEvent := 0
	for _, event := range events {
		switch event.Type {
		case "OAUTH_STARTED":
			oauthStarted++
		case "RUN_CANCELLED":
			cancelledEvent++
		case "STEP_STARTED":
			if event.Step == "oauth" || event.Step == "verify_credential" {
				startedLater++
			}
		}
	}
	if oauthStarted != 0 {
		t.Errorf("OAUTH_STARTED events = %d, want 0 after cancellation", oauthStarted)
	}
	if startedLater != 0 {
		t.Errorf("post-identity steps started after cancellation: %d", startedLater)
	}
	if cancelledEvent != 1 {
		t.Errorf("RUN_CANCELLED events = %d, want 1", cancelledEvent)
	}

	proc.terminate()
	probe := openProbe(t, proc)
	fixture := probe.runFixture(t, run.ID)
	if fixture.Status != "CANCELLED" {
		t.Fatalf("durable status = %s, want CANCELLED", fixture.Status)
	}
	if fixture.CredentialRef != "" {
		t.Fatalf("cancelled run has a stored credential reference %q", fixture.CredentialRef)
	}
	if count := probe.credentialResourceCount(t, run.ID); count != 0 {
		t.Fatalf("credential_ref resources = %d, want 0 for a cancelled run", count)
	}
	steps := probe.stepStatuses(t, run.ID)
	for _, name := range []string{"oauth", "verify_credential"} {
		step, ok := steps[name]
		if !ok {
			continue
		}
		// Cancellation finalizes untouched steps as CANCELLED (or leaves them
		// PENDING if the run was cancelled before they were seeded); neither may
		// ever have been attempted.
		if step.Status != "PENDING" && step.Status != "CANCELLED" {
			t.Errorf("step %s status = %s, want PENDING/CANCELLED (never executed)", name, step.Status)
		}
		if step.AttemptCount != 0 {
			t.Errorf("step %s attempts = %d, want 0", name, step.AttemptCount)
		}
	}
	if steps["request_verification"].AttemptCount != 1 {
		t.Errorf("request_verification attempts = %d, want 1", steps["request_verification"].AttemptCount)
	}

	users := sandboxUsersSince(t, admin, testStart)
	if len(users) != 1 {
		t.Errorf("keycloak sandbox users = %d, want 1 (identity created before the cancel)", len(users))
	}
	messages, err := mp.messages(context.Background())
	if err != nil {
		t.Fatalf("list mailpit messages: %v", err)
	}
	deliveries := 0
	for _, msg := range messages {
		for _, recipient := range messageRecipients(msg) {
			if strings.EqualFold(recipient, fixture.MailboxAddress) {
				deliveries++
			}
		}
	}

	evidence := map[string]any{
		"test":                          "E2E-006 surrogate cancel while WAITING_EMAIL",
		"generated_at":                  time.Now().UTC().Format(time.RFC3339),
		"environment":                   "LOCAL SANDBOX (Keycloak " + env.KeycloakURL + ", Mailpit " + env.MailpitURL + ")",
		"run_id":                        run.ID,
		"status_when_cancelled":         waiting.Status,
		"status_after_cancel_request":   canceled.Status,
		"final_status":                  final.Status,
		"oauth_started_events":          oauthStarted,
		"post_identity_step_starts":     startedLater,
		"run_cancelled_events":          cancelledEvent,
		"credential_resources":          probe.credentialResourceCount(t, run.ID),
		"oauth_step_status":             steps["oauth"].Status,
		"verify_credential_step_status": steps["verify_credential"].Status,
		"keycloak_identity_created":     len(users),
		"verification_emails_delivered": deliveries,
		"data_dir":                      proc.dataDir,
	}
	writeEvidence(t, proc, "evidence-cancel.json", evidence)

	subject := fixture.SubjectID
	if subject == "" && len(users) == 1 {
		subject = users[0].ID
	}
	cleanupSandboxUsers(t, admin, []string{subject}, testStart.UnixMilli())
	cleanupMailpit(t, mp, []string{fixture.MailboxAddress})
}
