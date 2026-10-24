package workflow

import (
	"io"
	"log/slog"
	"testing"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

// TestEveryStepOutcomeIsCommitLegal is the regression for the R4/REVIEW P1-1
// finding: every error classification the engine can produce must map onto a
// transition that the commit-time state matrix accepts. Otherwise the worker
// would keep re-executing the step (lease retained, no failure budget) until the
// absolute deadline.
func TestEveryStepOutcomeIsCommitLegal(t *testing.T) {
	eng := New(nil, provider.NewRegistry(), nil, slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{
		MaxAttempts:  8,
		PollInterval: 1e9,
		RunDeadline:  1e12,
	})
	classes := []errs.Class{
		errs.ClassValidation, errs.ClassNotAuthorized, errs.ClassAuthRequired,
		errs.ClassAuthExpired, errs.ClassRateLimited, errs.ClassUpstreamTransient,
		errs.ClassTimeoutUncertain, errs.ClassProviderBug, errs.ClassCancelled,
	}
	for _, tmpl := range eng.Templates() {
		for _, def := range tmpl.Steps {
			if err := state.CanTransition(def.ActiveState, def.Next); err != nil {
				t.Fatalf("template %s step %s success edge %s -> %s is illegal: %v",
					tmpl.Name, def.Name, def.ActiveState, def.Next, err)
			}
			claim := &storage.Claim{
				Run:        storage.Run{ID: "run", Template: tmpl.Name},
				StepName:   def.Name,
				StateAfter: string(def.ActiveState),
				Attempt:    1,
				Owner:      "test",
				Generation: 1,
			}
			for _, class := range classes {
				out := eng.classifyError(&def, claim, errs.New(class, "test", "test failure"))
				out = eng.safeNextState(def.ActiveState, out)
				result := eng.toStorageResult(&def, claim, out)
				if err := state.CanTransition(def.ActiveState, state.RunState(result.NextState)); err != nil {
					t.Fatalf("template %s step %s class %s -> illegal commit state %s: %v",
						tmpl.Name, def.Name, class, result.NextState, err)
				}
				if result.NextState == string(state.FailedTerminal) && !result.CountFailure {
					t.Fatalf("terminal outcome for %s/%s class %s must count a failure", tmpl.Name, def.Name, class)
				}
			}
		}
	}
}

// TestSafeNextStateFailsClosedOnIllegalEdge documents the fallback behaviour.
func TestSafeNextStateFailsClosedOnIllegalEdge(t *testing.T) {
	eng := New(nil, provider.NewRegistry(), nil, slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{})
	// MAILBOX_READY -> WAITING_EMAIL is illegal; the engine must fail closed
	// rather than commit an edge the matrix rejects.
	out := eng.safeNextState(state.MailboxReady, Outcome{NextState: state.WaitingEmail, StepStatus: state.StepWaitingEmail})
	if out.NextState != state.FailedTerminal || out.ErrorClass != string(errs.ClassProviderBug) || !out.CountFailure {
		t.Fatalf("expected fail-closed outcome, got %+v", out)
	}
	// A legal edge is preserved untouched.
	out = eng.safeNextState(state.Created, Outcome{NextState: state.Validating, StepStatus: state.StepSucceeded})
	if out.NextState != state.Validating || out.ErrorClass != "" {
		t.Fatalf("legal edge was rewritten: %+v", out)
	}
}

func TestSenderAllowedParsesAddressAndDomain(t *testing.T) {
	allow := []string{"moonbyte.test", "no-reply@github.com", "@idp.fake.test"}
	cases := []struct {
		sender string
		want   bool
	}{
		{"no-reply@moonbyte.test", true},
		{`"MoonByte Test (LOCAL SANDBOX)" <no-reply@moonbyte.test>`, true},
		{"NO-REPLY@MOONBYTE.TEST", true},
		{"no-reply@github.com", true},
		{"noreply@github.com", false},
		{"x@idp.fake.test", true},
		{"evil@evil-moonbyte.test", false},
		{"attacker@moonbyte.test.evil.example", false},
		{"<no-reply@moonbyte.test>", true},
		{"plain-text", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := senderAllowed(tc.sender, allow); got != tc.want {
			t.Fatalf("senderAllowed(%q) = %t, want %t", tc.sender, got, tc.want)
		}
	}
}
