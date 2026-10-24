package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFiveSandboxIdentities is E2E-001: five independent onboarding runs go
// through the real Keycloak + Mailpit sandbox and reach COMPLETED, each with a
// distinct, verified identity and a valid OIDC credential.
func TestFiveSandboxIdentities(t *testing.T) {
	env := requireSandbox(t)
	testStart := time.Now()
	proc := startOnboardd(t, env, procOptions{Addr: "127.0.0.1:18788"})
	admin := newKCAdmin(env)
	mp := newMailpitClient(env)
	ctx := context.Background()
	const want = 5
	// Never leak sandbox identities when an assertion fails before teardown.
	t.Cleanup(func() { cleanupSandboxUsers(t, admin, nil, testStart.UnixMilli()) })

	// The catalog must explain the sandbox roles and their activation gate.
	providers := proc.providers()
	required := []struct{ kind, name string }{
		{"identity", keycloakName},
		{"sandbox", keycloakName},
		{"credential", keycloakName},
		{"mailbox", mailboxName},
	}
	for _, wantProvider := range required {
		found := false
		for _, entry := range providers {
			if entry.Kind == wantProvider.kind && entry.Name == wantProvider.name {
				found = true
				if !entry.Enabled {
					t.Fatalf("provider %s/%s is registered but disabled", entry.Kind, entry.Name)
				}
				if entry.Live {
					t.Fatalf("provider %s/%s reports live=true; the sandbox fixture must not be live", entry.Kind, entry.Name)
				}
			}
		}
		if !found {
			t.Fatalf("provider %s/%s is not registered", wantProvider.kind, wantProvider.name)
		}
	}
	t.Logf("providers: %d registered (%s)", len(providers), summariseProviders(providers))

	// --- create five runs with distinct idempotency keys ---------------------
	suffix := time.Now().UTC().Format("150405") + "-" + randomHex(t, 2)
	runIDs := make([]string, 0, want)
	created := make([]runView, 0, want)
	for i := 0; i < want; i++ {
		idem := fmt.Sprintf("r2-five-%s-%d", suffix, i)
		view, status := proc.createRun(idem)
		if status != 202 {
			t.Fatalf("run %d: create status %d, want 202", i, status)
		}
		runIDs = append(runIDs, view.ID)
		created = append(created, view)
		t.Logf("created run %d: id=%s status=%s", i+1, view.ID, view.Status)
	}
	if len(uniqueStrings(runIDs)) != want {
		t.Fatalf("expected %d distinct run ids, got %v", want, runIDs)
	}

	// --- wait for all five to reach a terminal state -------------------------
	deadline := time.Now().Add(4 * time.Minute)
	finals := make([]runView, 0, want)
	for {
		listed := proc.listRuns()
		byID := make(map[string]runView, len(listed))
		for _, view := range listed {
			byID[view.ID] = view
		}
		finals = finals[:0]
		terminal := 0
		for _, id := range runIDs {
			view := byID[id]
			finals = append(finals, view)
			switch view.Status {
			case "COMPLETED", "FAILED_TERMINAL", "CANCELLED":
				terminal++
			}
		}
		if terminal == want || time.Now().After(deadline) {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	completed := 0
	for i, view := range finals {
		if view.Status != "COMPLETED" {
			t.Errorf("run %d (%s) status=%s step=%s error=%s", i+1, view.ID, view.Status, view.CurrentStep, view.LastErrorClass)
			continue
		}
		completed++
	}

	// --- events: every run must show OAUTH_STARTED and RUN_COMPLETED ---------
	for i, id := range runIDs {
		events := proc.events(id)
		types := map[string]int{}
		for _, event := range events {
			types[event.Type]++
		}
		if types["OAUTH_STARTED"] < 1 {
			t.Errorf("run %d: no OAUTH_STARTED event (events: %v)", i+1, eventTypeCounts(events))
		}
		if types["RUN_COMPLETED"] < 1 {
			t.Errorf("run %d: no RUN_COMPLETED event (events: %v)", i+1, eventTypeCounts(events))
		}
	}
	if completed != want {
		proc.terminate()
		t.Fatalf("only %d/%d runs completed; evidence log: %s", completed, want, proc.logPath())
	}

	// --- stop the service and inspect the durable state ----------------------
	proc.terminate()
	probe := openProbe(t, proc)
	fixtures := make([]runFixture, 0, want)
	for _, id := range runIDs {
		fixtures = append(fixtures, probe.runFixture(t, id))
	}

	// --- Keycloak: five distinct, verified identities ------------------------
	users, err := admin.listUsers(ctx)
	if err != nil {
		t.Fatalf("list keycloak users: %v", err)
	}
	byID := map[string]kcUser{}
	for _, user := range users {
		byID[user.ID] = user
	}
	subjects := map[string]bool{}
	usernames := map[string]bool{}
	windowUsers := len(sandboxUsersSince(t, admin, testStart))
	for i, fixture := range fixtures {
		if fixture.SubjectID == "" {
			t.Errorf("run %d: no identity resource", i+1)
			continue
		}
		if subjects[fixture.SubjectID] {
			t.Errorf("run %d: duplicate subject %s", i+1, fixture.SubjectID)
		}
		subjects[fixture.SubjectID] = true
		user, ok := byID[fixture.SubjectID]
		if !ok {
			t.Errorf("run %d: keycloak user %s not found", i+1, fixture.SubjectID)
			continue
		}
		usernames[strings.ToLower(user.Username)] = true
		if !user.EmailVerified {
			t.Errorf("run %d: keycloak user %s emailVerified=false", i+1, user.Username)
		}
		if !strings.EqualFold(user.Email, fixture.MailboxAddress) {
			t.Errorf("run %d: keycloak email %q != mailbox %q", i+1, user.Email, fixture.MailboxAddress)
		}
	}
	if len(subjects) != want {
		t.Errorf("distinct subjects = %d, want %d", len(subjects), want)
	}
	if len(usernames) != want {
		t.Errorf("distinct usernames = %d, want %d", len(usernames), want)
	}
	if windowUsers != want {
		t.Errorf("keycloak users created during this test = %d, want %d (duplicate or leaked identity)", windowUsers, want)
	}

	// --- Mailpit: one verification email per identity ------------------------
	messages, err := mp.messages(ctx)
	if err != nil {
		t.Fatalf("list mailpit messages: %v", err)
	}
	perAddress := map[string]int{}
	for _, msg := range messages {
		for _, recipient := range messageRecipients(msg) {
			perAddress[recipient]++
		}
	}
	for i, fixture := range fixtures {
		if fixture.MailboxAddress == "" {
			t.Errorf("run %d: no mailbox address", i+1)
			continue
		}
		if perAddress[strings.ToLower(fixture.MailboxAddress)] < 1 {
			t.Errorf("run %d: no verification email observed for %s", i+1, fixture.MailboxAddress)
		}
	}

	// --- userinfo: five real OIDC token validations --------------------------
	userinfoOK := 0
	userinfoValid := make([]bool, len(fixtures))
	for i := range fixtures {
		fixture := &fixtures[i]
		envelope := probe.credential(t, fixture.CredentialRef)
		sub, email, verified, status, err := userinfo(ctx, env, envelope.AccessToken)
		if err != nil || status != 200 {
			t.Errorf("run %d: userinfo status=%d err=%v", i+1, status, err)
			continue
		}
		if sub != fixture.SubjectID {
			t.Errorf("run %d: userinfo sub %q != stored subject %q", i+1, sub, fixture.SubjectID)
			continue
		}
		if !strings.EqualFold(email, fixture.MailboxAddress) || !verified {
			t.Errorf("run %d: userinfo email=%q verified=%v (want %q true)", i+1, email, verified, fixture.MailboxAddress)
			continue
		}
		if envelope.Issuer != env.KeycloakURL+"/realms/"+env.Realm {
			t.Errorf("run %d: stored issuer %q", i+1, envelope.Issuer)
		}
		userinfoValid[i] = true
		userinfoOK++
	}

	// --- duration statistics -------------------------------------------------
	durations := make([]time.Duration, 0, want)
	rows := make([]evidenceRow, 0, want)
	for i, fixture := range fixtures {
		var elapsed time.Duration
		if fixture.FinishedAtMS > fixture.CreatedAtMS && fixture.FinishedAtMS > 0 {
			elapsed = time.Duration(fixture.FinishedAtMS-fixture.CreatedAtMS) * time.Millisecond
			durations = append(durations, elapsed)
		}
		user := byID[fixture.SubjectID]
		rows = append(rows, evidenceRow{
			Index:         i + 1,
			RunID:         fixture.RunID,
			Status:        fixture.Status,
			Subject:       fixture.SubjectID,
			Email:         fixture.MailboxAddress,
			EmailVerified: user.EmailVerified,
			UserinfoValid: userinfoValid[i],
			EmailCount:    perAddress[strings.ToLower(fixture.MailboxAddress)],
			DurationMS:    elapsed.Milliseconds(),
		})
	}
	p50 := percentile(durations, 0.50)
	p95 := percentile(durations, 0.95)

	var table strings.Builder
	table.WriteString("\n| # | run | status | subject | email | emailVerified | userinfo | emails | duration |\n")
	table.WriteString("|---|-----|--------|---------|-------|---------------|----------|--------|----------|\n")
	for _, row := range rows {
		fmt.Fprintf(&table, "| %d | %s | %s | %s | %s | %v | %v | %d | %dms |\n",
			row.Index, shortRun(row.RunID), row.Status, row.Subject, row.Email,
			row.EmailVerified, row.UserinfoValid, row.EmailCount, row.DurationMS)
	}
	table.WriteString(fmt.Sprintf("\nP50=%dms P95=%dms (n=%d)\n", p50.Milliseconds(), p95.Milliseconds(), len(durations)))
	t.Log(table.String())
	fmt.Print(table.String())

	evidence := map[string]any{
		"test":                  "E2E-001 five sandbox identities",
		"generated_at":          time.Now().UTC().Format(time.RFC3339),
		"environment":           "LOCAL SANDBOX (Keycloak " + env.KeycloakURL + " realm " + env.Realm + ", Mailpit " + env.MailpitURL + ")",
		"template":              templateName,
		"completed":             completed,
		"requested":             want,
		"userinfo_valid":        userinfoOK,
		"keycloak_window_users": windowUsers,
		"p50_ms":                p50.Milliseconds(),
		"p95_ms":                p95.Milliseconds(),
		"rows":                  rows,
		"data_dir":              proc.dataDir,
		"log":                   proc.logPath(),
	}
	writeEvidence(t, proc, "evidence-five-identities.json", evidence)

	// --- teardown ------------------------------------------------------------
	subjectIDs := make([]string, 0, want)
	addresses := make([]string, 0, want)
	for _, fixture := range fixtures {
		subjectIDs = append(subjectIDs, fixture.SubjectID)
		addresses = append(addresses, fixture.MailboxAddress)
	}
	cleanupSandboxUsers(t, admin, subjectIDs, testStart.UnixMilli())
	cleanupMailpit(t, mp, addresses)

	if completed != want || len(subjects) != want || userinfoOK != want {
		t.Fatalf("sandbox E2E incomplete: completed=%d/%d subjects=%d userinfo=%d/%d",
			completed, want, len(subjects), userinfoOK, want)
	}
}

type evidenceRow struct {
	Index         int    `json:"index"`
	RunID         string `json:"run_id"`
	Status        string `json:"status"`
	Subject       string `json:"subject"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	UserinfoValid bool   `json:"userinfo_valid"`
	EmailCount    int    `json:"email_count"`
	DurationMS    int64  `json:"duration_ms"`
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func shortRun(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func summariseProviders(providers []providerEntry) string {
	parts := make([]string, 0, len(providers))
	for _, entry := range providers {
		state := "disabled"
		if entry.Enabled {
			state = "enabled"
		}
		parts = append(parts, entry.Kind+"/"+entry.Name+"("+state+")")
	}
	return strings.Join(parts, ", ")
}

func eventTypeCounts(events []eventView) map[string]int {
	out := map[string]int{}
	for _, event := range events {
		out[event.Type]++
	}
	return out
}
