package state

import "testing"

func TestTransitionMatrix(t *testing.T) {
	allowed := map[string][]string{
		"CREATED":                {"VALIDATING", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
		"VALIDATING":             {"PREPARING_MAILBOX", "OAUTH_PENDING", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL", "RETRY_WAIT"},
		"PREPARING_MAILBOX":      {"MAILBOX_READY", "RETRY_WAIT", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
		"MAILBOX_READY":          {"VERIFICATION_REQUESTED", "RETRY_WAIT", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
		"VERIFICATION_REQUESTED": {"WAITING_EMAIL", "WAITING_USER_ACTION", "IDENTITY_VERIFIED", "RETRY_WAIT", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
		"WAITING_EMAIL":          {"WAITING_EMAIL", "IDENTITY_VERIFIED", "WAITING_USER_ACTION", "COMPLETED", "RETRY_WAIT", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL", "REAUTHORIZE_REQUIRED"},
		"IDENTITY_VERIFIED":      {"OAUTH_PENDING", "CREDENTIAL_VERIFIED", "COMPLETED", "RETRY_WAIT", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
		"OAUTH_PENDING":          {"OAUTH_PENDING", "WAITING_USER_ACTION", "CREDENTIAL_VERIFIED", "REAUTHORIZE_REQUIRED", "RETRY_WAIT", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
		"CREDENTIAL_VERIFIED":    {"COMPLETED", "RETRY_WAIT", "REAUTHORIZE_REQUIRED", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
		"CANCEL_REQUESTED":       {"CANCELLED"},
		"WAITING_USER_ACTION":    {"WAITING_USER_ACTION", "IDENTITY_VERIFIED", "WAITING_EMAIL", "VERIFICATION_REQUESTED", "RETRY_WAIT", "REAUTHORIZE_REQUIRED", "CANCEL_REQUESTED", "CANCELLED", "FAILED_TERMINAL"},
	}
	for from, targets := range allowed {
		for _, to := range targets {
			if err := CanTransition(RunState(from), RunState(to)); err != nil {
				t.Fatalf("expected allowed %s -> %s: %v", from, to, err)
			}
		}
	}
	denied := [][2]string{
		{"CREATED", "COMPLETED"},
		{"CREATED", "WAITING_EMAIL"},
		{"VALIDATING", "COMPLETED"},
		{"COMPLETED", "VALIDATING"},
		{"FAILED_TERMINAL", "RETRY_WAIT"},
		{"CANCELLED", "CANCEL_REQUESTED"},
		{"MAILBOX_READY", "WAITING_EMAIL"},
		{"RETRY_WAIT", "COMPLETED"},
		{"CREDENTIAL_VERIFIED", "IDENTITY_VERIFIED"},
		{"CANCEL_REQUESTED", "COMPLETED"},
		{"CANCEL_REQUESTED", "FAILED_TERMINAL"},
		{"UNKNOWN", "CREATED"},
	}
	for _, pair := range denied {
		if err := CanTransition(RunState(pair[0]), RunState(pair[1])); err == nil {
			t.Fatalf("expected denial %s -> %s", pair[0], pair[1])
		}
	}
}

func TestStatePredicates(t *testing.T) {
	if !Completed.IsTerminal() || !FailedTerminal.IsTerminal() || !Cancelled.IsTerminal() {
		t.Fatal("terminal states misclassified")
	}
	if Validating.IsTerminal() || RetryWait.IsTerminal() {
		t.Fatal("non-terminal states misclassified")
	}
	for _, s := range []RunState{WaitingEmail, WaitingUserAction, RetryWait, ReauthorizeRequired} {
		if !s.IsWait() {
			t.Fatalf("%s should be a wait state", s)
		}
	}
	if Completed.IsWait() {
		t.Fatal("completed is not a wait state")
	}
	if !WaitingUserAction.NeedsManualResume() || !ReauthorizeRequired.NeedsManualResume() || !RetryWait.NeedsManualResume() {
		t.Fatal("manual resume states misclassified")
	}
	if WaitingEmail.NeedsManualResume() {
		t.Fatal("waiting email is automatic, not manual")
	}
}

func TestAllRunStatesCovered(t *testing.T) {
	seen := map[RunState]bool{}
	for _, s := range AllRunStates {
		if seen[s] {
			t.Fatalf("duplicate state %s", s)
		}
		seen[s] = true
	}
	// Every state except the initial CREATED must be reachable from the table.
	for _, s := range AllRunStates {
		if s == Created {
			continue
		}
		reachable := false
		for _, from := range AllRunStates {
			if err := CanTransition(from, s); err == nil {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Fatalf("state %s is unreachable", s)
		}
	}
	// Terminal states have no outgoing edges.
	for _, s := range []RunState{Completed, FailedTerminal, Cancelled} {
		if len(AllowedTransitions(s)) != 0 {
			t.Fatalf("terminal state %s has outgoing edges", s)
		}
	}
}
