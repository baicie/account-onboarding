package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCrashRecoveryRestartsInWaitingEmail is E2E-003/E2E-008: a run that is
// durably waiting for its verification email survives SIGKILL and a restart on
// the same data dir + master key. The run resumes from its persisted state,
// never registers a second identity, and still completes.
//
// The run is held in WAITING_EMAIL deterministically: while the process is up,
// the engine's mailx sender allowlist intentionally omits the Keycloak sender
// domain, so the already-delivered verification email cannot match yet. After
// the restart the normal allowlist is restored and the same email is picked up,
// which is exactly the "resume from the persisted step" behaviour under test.
func TestCrashRecoveryRestartsInWaitingEmail(t *testing.T) {
	env := requireSandbox(t)
	testStart := time.Now()
	proc := startOnboardd(t, env, procOptions{
		Addr:            "127.0.0.1:18789",
		Tick:            100 * time.Millisecond,
		Poll:            time.Second,
		StepTimeout:     20 * time.Second,
		LeaseTTL:        30 * time.Second,
		SenderAllowlist: "idp.fake.test", // deliberately cannot match the Keycloak sender
	})
	admin := newKCAdmin(env)
	mp := newMailpitClient(env)
	ctx := context.Background()
	t.Cleanup(func() { cleanupSandboxUsers(t, admin, nil, testStart.UnixMilli()) })

	run, _ := proc.createRun("r2-crash-" + randomHex(t, 4))
	waiting := proc.waitForStatus(run.ID, 90*time.Second, "WAITING_EMAIL")
	t.Logf("run %s durably waiting: status=%s step=%s", run.ID, waiting.Status, waiting.CurrentStep)

	// The sandbox identity must exist before the crash: that is the external
	// side effect the resumed run must not repeat.
	var identity kcUser
	identityDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(identityDeadline) {
		users := sandboxUsersSince(t, admin, testStart)
		if len(users) == 1 {
			identity = users[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if identity.ID == "" {
		t.Fatalf("sandbox identity was not provisioned before the crash")
	}
	if identity.EmailVerified {
		t.Fatalf("identity %s was already verified before the crash; the wait window is not deterministic", identity.Username)
	}
	t.Logf("pre-crash identity: subject=%s email=%s emailVerified=%v", identity.ID, identity.Email, identity.EmailVerified)

	// --- crash (SIGKILL) -----------------------------------------------------
	proc.kill()
	crashAt := time.Now()
	t.Logf("SIGKILL delivered at %s", crashAt.UTC().Format(time.RFC3339Nano))

	// Durable state must already be WAITING_EMAIL, and the identity resource
	// must be committed with the subject id (no half-written step).
	probeBefore := openProbe(t, proc)
	before := probeBefore.runFixture(t, run.ID)
	stepsBefore := probeBefore.stepStatuses(t, run.ID)
	probeBefore.Close()
	if before.Status != "WAITING_EMAIL" {
		t.Fatalf("durable status after crash = %s, want WAITING_EMAIL", before.Status)
	}
	if before.SubjectID != identity.ID {
		t.Fatalf("durable subject %q != keycloak subject %q", before.SubjectID, identity.ID)
	}
	if got := stepsBefore["request_verification"].AttemptCount; got != 1 {
		t.Fatalf("request_verification attempts before crash = %d, want 1", got)
	}
	t.Logf("durable state after crash: status=%s subject=%s request_verification_attempts=%d",
		before.Status, before.SubjectID, stepsBefore["request_verification"].AttemptCount)

	// --- restart on the SAME data dir, master key and admin token ------------
	proc.restart("moonbyte.test,idp.fake.test")
	restartedAt := time.Now()
	final := proc.waitForStatus(run.ID, 3*time.Minute, "COMPLETED", "FAILED_TERMINAL")
	if final.Status != "COMPLETED" {
		t.Fatalf("run %s after restart: status=%s step=%s error=%s (log %s)",
			run.ID, final.Status, final.CurrentStep, final.LastErrorClass, proc.logPath())
	}
	resumedAfter := final.UpdatedAt
	t.Logf("run resumed and completed: status=%s updated_at=%s (resume latency ~%s)",
		final.Status, resumedAfter, resumedAfter)

	// --- final assertions ----------------------------------------------------
	proc.terminate()
	probe := openProbe(t, proc)
	after := probe.runFixture(t, run.ID)
	stepsAfter := probe.stepStatuses(t, run.ID)
	if after.SubjectID != before.SubjectID {
		t.Fatalf("subject changed across restart: %q -> %q", before.SubjectID, after.SubjectID)
	}
	if got := stepsAfter["request_verification"].AttemptCount; got != 1 {
		t.Fatalf("request_verification attempts after restart = %d, want 1 (no duplicate registration)", got)
	}
	if got := stepsAfter["wait_email"].AttemptCount; got <= 1 {
		t.Fatalf("wait_email attempts after restart = %d, want >1 (proof the same step resumed)", got)
	}
	if after.CredentialRef == "" {
		t.Fatal("no credential reference after resume")
	}

	users := sandboxUsersSince(t, admin, testStart)
	if len(users) != 1 {
		t.Fatalf("keycloak sandbox users after restart = %d, want exactly 1 (no duplicate identity)", len(users))
	}
	if users[0].ID != before.SubjectID {
		t.Fatalf("keycloak user %s != persisted subject %s", users[0].ID, before.SubjectID)
	}
	if !users[0].EmailVerified {
		t.Fatalf("keycloak user %s is not verified after resume", users[0].Username)
	}
	envelope := probe.credential(t, after.CredentialRef)
	sub, email, verified, status, err := userinfo(ctx, env, envelope.AccessToken)
	if err != nil || status != 200 || sub != before.SubjectID || !verified {
		t.Fatalf("userinfo after resume: status=%d sub=%q verified=%v err=%v", status, sub, verified, err)
	}
	if !strings.EqualFold(email, after.MailboxAddress) {
		t.Fatalf("userinfo email %q != mailbox %q", email, after.MailboxAddress)
	}

	evidence := map[string]any{
		"test":                          "E2E-003/E2E-008 crash recovery",
		"generated_at":                  time.Now().UTC().Format(time.RFC3339),
		"environment":                   "LOCAL SANDBOX (Keycloak " + env.KeycloakURL + ", Mailpit " + env.MailpitURL + ")",
		"run_id":                        run.ID,
		"status_before_crash":           before.Status,
		"status_after_resume":           final.Status,
		"subject_before":                before.SubjectID,
		"subject_after":                 after.SubjectID,
		"request_verification_attempts": stepsAfter["request_verification"].AttemptCount,
		"wait_email_attempts":           stepsAfter["wait_email"].AttemptCount,
		"crash_at":                      crashAt.UTC().Format(time.RFC3339Nano),
		"restart_at":                    restartedAt.UTC().Format(time.RFC3339Nano),
		"keycloak_users_after":          len(users),
		"email_verified":                users[0].EmailVerified,
		"userinfo_status":               status,
		"data_dir":                      proc.dataDir,
		"logs":                          fmt.Sprintf("%s/onboardd-1.log (pre-crash), %s/onboardd-2.log (post-restart)", proc.dataDir, proc.dataDir),
	}
	writeEvidence(t, proc, "evidence-crash-recovery.json", evidence)

	cleanupSandboxUsers(t, admin, []string{before.SubjectID}, testStart.UnixMilli())
	cleanupMailpit(t, mp, []string{after.MailboxAddress})
}
