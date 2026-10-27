package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baicie/account-onboarding/internal/errs"
	"github.com/baicie/account-onboarding/internal/provider"
	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow/state"
)

// Config tunes the engine.
type Config struct {
	MaxConcurrency  int
	TickInterval    time.Duration
	LeaseTTL        time.Duration
	StepTimeout     time.Duration
	PollInterval    time.Duration
	MaxAttempts     int
	RunDeadline     time.Duration
	Domains         []string
	SenderAllowlist []string
	InstanceID      string
	FingerprintKey  []byte
	OIDCScopes      []string
	OIDCRedirectURI string
}

// Engine is the durable workflow runner. A worker claims one step, executes it
// without holding a database transaction, then commits with a fencing check.
type Engine struct {
	cfg       Config
	store     *storage.Store
	registry  *provider.Registry
	secrets   *secrets.Service
	log       *slog.Logger
	templates map[string]*Template
	now       func() time.Time

	// paused stops new step claims during a maintenance window. In-flight steps
	// finish; any lease they hold waits for the next probe after Resume.
	paused atomic.Bool

	lifecycleMu sync.Mutex
	lifecycle   engineLifecycle
	stopCh      chan struct{}
	stopDone    chan struct{}
	stopErr     error
	wg          sync.WaitGroup
}

type engineLifecycle uint8

const (
	engineNew engineLifecycle = iota
	engineStarting
	engineRunning
	engineStopping
	engineStopped
	engineStartFailed
)

var (
	ErrEngineAlreadyStarted = errors.New("workflow: engine already started")
	ErrEngineStopped        = errors.New("workflow: engine cannot be restarted after stop")
)

// claimableStatuses are the run states a worker may claim. Waiting states are
// only claimable when next_run_at has been set (resume or poll schedule).
var claimableStatuses = []string{
	string(state.Created), string(state.Validating), string(state.PreparingMailbox),
	string(state.MailboxReady), string(state.VerificationRequest), string(state.WaitingEmail),
	string(state.WaitingUserAction), string(state.IdentityVerified), string(state.OAuthPending),
	string(state.CredentialVerified), string(state.RetryWait), string(state.ReauthorizeRequired),
	string(state.CancelRequested),
}

// New builds an engine with the four built-in templates.
func New(store *storage.Store, registry *provider.Registry, sec *secrets.Service, log *slog.Logger, cfg Config) *Engine {
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if cfg.MaxConcurrency > 5 {
		cfg.MaxConcurrency = 5
	}
	if cfg.TickInterval <= 0 {
		cfg.TickInterval = 250 * time.Millisecond
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 90 * time.Second
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.RunDeadline <= 0 {
		cfg.RunDeadline = 30 * time.Minute
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = "onboardd-" + security.NewID()[:8]
	}
	if len(cfg.FingerprintKey) == 0 {
		cfg.FingerprintKey = []byte("account-onboarding-fingerprint")
	}
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		cfg:       cfg,
		store:     store,
		registry:  registry,
		secrets:   sec,
		log:       log,
		templates: map[string]*Template{},
		now:       time.Now,
		stopCh:    make(chan struct{}),
		stopDone:  make(chan struct{}),
	}
	for _, t := range e.buildTemplates() {
		e.templates[t.Name] = t
	}
	return e
}

// Config returns the effective engine configuration.
func (e *Engine) Config() Config { return e.cfg }

// Store returns the backing store (API handlers use it for reads).
func (e *Engine) Store() *storage.Store { return e.store }

// Registry returns the provider catalog.
func (e *Engine) Registry() *provider.Registry { return e.registry }

// Secrets returns the secret service.
func (e *Engine) Secrets() *secrets.Service { return e.secrets }

// Templates returns the registered templates sorted deterministically.
func (e *Engine) Templates() []*Template {
	out := make([]*Template, 0, len(e.templates))
	for _, name := range []string{
		"sandbox-email-verify-oidc", "cloudflare-mail-smoke",
		"github-existing-email", "oauth-existing-account",
	} {
		if t, ok := e.templates[name]; ok {
			out = append(out, t)
		}
	}
	return out
}

// Template looks up a template by name.
func (e *Engine) Template(name string) (*Template, bool) {
	t, ok := e.templates[name]
	return t, ok
}

// Start performs mandatory external-operation recovery before any worker can
// claim a step. Lifecycle transitions are serialized so Stop cannot race the
// recovery barrier or close stopCh before the workers have been registered.
func (e *Engine) Start(ctx context.Context) error {
	e.lifecycleMu.Lock()
	if e.lifecycle != engineNew {
		state := e.lifecycle
		e.lifecycleMu.Unlock()
		if state == engineRunning || state == engineStarting || state == engineStopping {
			return ErrEngineAlreadyStarted
		}
		return ErrEngineStopped
	}
	e.lifecycle = engineStarting

	// The previous process is known dead after the instance lock is acquired.
	// PREPARED never reached the provider; IN_FLIGHT may have and must remain
	// pending reconciliation.
	n, err := e.store.RecoverExternalOperations(ctx, "STARTUP_PREVIOUS_PROCESS")
	if err != nil {
		e.lifecycle = engineStartFailed
		e.lifecycleMu.Unlock()
		e.log.ErrorContext(ctx, "external operation startup recovery failed", "error", err)
		return fmt.Errorf("workflow: external operation startup recovery: %w", err)
	}
	if n > 0 {
		e.log.WarnContext(ctx, "recovered leftover external operations", "count", n)
	}

	for i := 0; i < e.cfg.MaxConcurrency; i++ {
		e.wg.Add(1)
		go func(worker int) {
			defer e.wg.Done()
			e.workerLoop(ctx, worker)
		}(i)
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-e.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A maintenance window must not mutate state behind the
				// restore: skip the sweep until the engine resumes.
				if e.paused.Load() {
					continue
				}
				ids, err := e.store.SweepDeadlines(ctx, 50)
				if err != nil {
					e.log.ErrorContext(ctx, "deadline sweep failed", "error", err)
					continue
				}
				for _, id := range ids {
					e.log.WarnContext(ctx, "run deadline exceeded", "run_id", id)
				}
			}
		}
	}()
	e.lifecycle = engineRunning
	e.lifecycleMu.Unlock()
	return nil
}

// Stop stops claiming work, drains workers, then performs the lifecycle recovery
// barrier. Concurrent/repeated callers observe the same terminal result; a
// stopped Engine is intentionally not restartable because its stopCh is closed.
func (e *Engine) Stop() error {
	e.lifecycleMu.Lock()
	switch e.lifecycle {
	case engineNew, engineStartFailed:
		e.lifecycle = engineStopped
		close(e.stopCh)
		close(e.stopDone)
		e.lifecycleMu.Unlock()
		return nil
	case engineStopped:
		err := e.stopErr
		e.lifecycleMu.Unlock()
		return err
	case engineStopping:
		done := e.stopDone
		e.lifecycleMu.Unlock()
		<-done
		e.lifecycleMu.Lock()
		err := e.stopErr
		e.lifecycleMu.Unlock()
		return err
	case engineRunning:
		e.lifecycle = engineStopping
		close(e.stopCh)
	default:
		// STARTING is protected by lifecycleMu during recovery and worker
		// registration, so a caller cannot observe it here.
		e.lifecycleMu.Unlock()
		return ErrEngineAlreadyStarted
	}
	e.lifecycleMu.Unlock()

	e.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if n, err := e.store.RecoverExternalOperations(ctx, "GRACEFUL_STOP"); err != nil {
		e.log.Error("external operation stop recovery failed", "error", err)
		e.lifecycleMu.Lock()
		e.stopErr = fmt.Errorf("workflow: external operation stop recovery: %w", err)
		e.lifecycle = engineStopped
		close(e.stopDone)
		result := e.stopErr
		e.lifecycleMu.Unlock()
		return result
	} else if n > 0 {
		e.log.Warn("recovered external operations on stop", "count", n)
	}

	e.lifecycleMu.Lock()
	e.lifecycle = engineStopped
	close(e.stopDone)
	result := e.stopErr
	e.lifecycleMu.Unlock()
	return result
}

// Pause stops new step claims for a maintenance window. In-flight steps are
// allowed to finish; their leases are honoured again (or reclaimed if expired)
// after Resume.
func (e *Engine) Pause() {
	e.paused.Store(true)
}

// Resume restarts step claiming.
func (e *Engine) Resume() {
	e.paused.Store(false)
}

// Paused reports whether step claiming is suspended.
func (e *Engine) Paused() bool { return e.paused.Load() }

func (e *Engine) workerLoop(ctx context.Context, worker int) {
	ticker := time.NewTicker(e.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				did, err := e.Tick(ctx)
				if err != nil {
					e.log.ErrorContext(ctx, "engine tick failed", "worker", worker, "error", err)
					break
				}
				if !did {
					break
				}
			}
		}
	}
}

// Tick claims and executes at most one step. It reports whether work was done.
func (e *Engine) Tick(ctx context.Context) (bool, error) {
	if e.paused.Load() {
		return false, nil
	}
	claim, err := e.store.ClaimNext(ctx, storage.ClaimRequest{
		Owner:            e.cfg.InstanceID,
		LeaseTTL:         e.cfg.LeaseTTL,
		RunnableStatuses: claimableStatuses,
		ActiveState:      e.activeState,
	})
	if err != nil {
		return false, err
	}
	if claim == nil {
		return false, nil
	}
	if claim.CancelRequested {
		return true, e.store.CommitStepResult(ctx, claim, storage.StepResult{
			NextState:  string(state.Cancelled),
			StepStatus: string(state.StepCancelled),
			Events:     []storage.EventDraft{{Type: "RUN_CANCELLED", Payload: map[string]any{"reason": "cancelled"}}},
		})
	}
	result := e.execute(ctx, claim)
	if err := e.store.CommitStepResult(ctx, claim, result); err != nil {
		if errors.Is(err, storage.ErrLeaseLost) {
			e.log.WarnContext(ctx, "lease lost before commit", "run_id", claim.Run.ID, "step", claim.StepName)
			return true, nil
		}
		return true, err
	}
	return true, nil
}

func (e *Engine) activeState(template, step string) (string, bool) {
	t, ok := e.templates[template]
	if !ok {
		return "", false
	}
	for _, def := range t.Steps {
		if def.Name == step {
			return string(def.ActiveState), true
		}
	}
	return "", false
}

// execute runs a single step under a timeout and normalizes the result. It must
// never hold a transaction: external HTTP calls happen here.
func (e *Engine) execute(ctx context.Context, claim *storage.Claim) (result storage.StepResult) {
	defer func() {
		if r := recover(); r != nil {
			result = storage.StepResult{
				NextState:    string(state.FailedTerminal),
				StepStatus:   string(state.StepFailedTerminal),
				ErrorClass:   string(errs.ClassProviderBug),
				ErrorSummary: "provider panicked",
			}
		}
	}()

	tmpl, ok := e.templates[claim.Run.Template]
	if !ok {
		return storage.StepResult{
			NextState:    string(state.FailedTerminal),
			StepStatus:   string(state.StepFailedTerminal),
			ErrorClass:   string(errs.ClassValidation),
			ErrorSummary: "unknown workflow template",
		}
	}
	def := tmpl.StepDef(claim.StepName)
	if def == nil {
		return storage.StepResult{
			NextState:    string(state.FailedTerminal),
			StepStatus:   string(state.StepFailedTerminal),
			ErrorClass:   string(errs.ClassProviderBug),
			ErrorSummary: "unknown workflow step",
		}
	}

	var latestStep storage.Step
	if step, err := e.store.LatestStep(ctx, claim.Run.ID, claim.StepName); err == nil {
		latestStep = step
		claim.Attempt = step.AttemptCount
	}

	_ = e.store.AppendEvent(ctx, claim.Run.ID, "STEP_STARTED", claim.StepName, map[string]any{
		"attempt":       claim.Attempt,
		"operation_key": claim.Run.ID + ":" + claim.StepName + ":v1",
		"resume_state":  string(def.ActiveState),
	})

	if mismatch, changed := e.contractChanged(claim.Run); changed {
		res := storage.StepResult{
			NextState:    string(state.FailedTerminal),
			StepStatus:   string(state.StepFailedTerminal),
			ErrorClass:   string(errs.ClassProviderBug),
			ErrorSummary: "provider contract version changed for " + mismatch,
			Events: []storage.EventDraft{{Type: "RUN_FAILED", Payload: map[string]any{
				"failure_class": string(errs.ClassProviderBug),
				"reason":        "provider contract version changed: " + mismatch,
			}}},
		}
		return res
	}

	rc := &RunContext{
		Engine:    e,
		Run:       claim.Run,
		Step:      latestStep,
		Template:  tmpl,
		Def:       def,
		Now:       e.now().UTC(),
		Deadline:  time.UnixMilli(claim.Run.DeadlineMS).UTC(),
		Selection: e.selectionFromSnapshot(claim.Run),
		Claim:     *claim,
		Log: e.log.With("run_id", claim.Run.ID, "step", claim.StepName,
			"template", claim.Run.Template, "attempt", claim.Attempt),
	}
	stepCtx, cancel := context.WithTimeout(ctx, e.cfg.StepTimeout)
	defer cancel()

	out, err := def.Handler(stepCtx, rc)
	if err != nil {
		out = e.classifyError(def, claim, err)
	}
	out = e.safeNextState(state.RunState(claim.StateAfter), out)
	return e.toStorageResult(def, claim, out)
}

func (e *Engine) contractChanged(run storage.Run) (string, bool) {
	snap := e.selectionFromSnapshot(run)
	var current provider.ProviderSnapshot
	if d, ok := e.registry.LookupDescriptor(provider.KindMailbox, snap.Mailbox.Name); ok {
		current.Mailbox = d
	}
	if d, ok := e.registry.LookupDescriptor(provider.KindIdentity, snap.Identity.Name); ok {
		current.Identity = d
	}
	if d, ok := e.registry.LookupDescriptor(provider.KindOAuth, snap.OAuth.Name); ok {
		current.OAuth = d
	}
	if d, ok := e.registry.LookupDescriptor(provider.KindEmail, snap.Email.Name); ok {
		current.Email = d
	}
	return snap.Mismatch(current)
}

// classifyError maps a domain error onto workflow states and retry policy.
func (e *Engine) classifyError(def *StepDef, claim *storage.Claim, err error) Outcome {
	class := errs.ClassOf(err)
	summary := errs.SummaryOf(err)
	active := def.ActiveState
	switch class {
	case errs.ClassAuthRequired:
		return Outcome{
			NextState:      state.WaitingUserAction,
			StepStatus:     state.StepSucceeded,
			ErrorClass:     string(class),
			ErrorSummary:   summary,
			Wait:           e.cfg.PollInterval,
			ActionRequired: true,
			ResumeState:    state.WaitingUserAction,
		}
	case errs.ClassAuthExpired:
		return Outcome{
			NextState:      state.ReauthorizeRequired,
			StepStatus:     state.StepReauthorizeRequired,
			ErrorClass:     string(class),
			ErrorSummary:   summary,
			ActionRequired: true,
			ResumeState:    active,
		}
	case errs.ClassRateLimited, errs.ClassUpstreamTransient, errs.ClassTimeoutUncertain:
		if claim.Failures+1 >= int64(e.cfg.MaxAttempts) {
			return Outcome{
				NextState:    state.FailedTerminal,
				StepStatus:   state.StepFailedTerminal,
				ErrorClass:   string(class),
				ErrorSummary: "retry budget exhausted: " + summary,
				CountFailure: true,
			}
		}
		return Outcome{
			NextState:    state.RetryWait,
			StepStatus:   state.StepRetryWait,
			ErrorClass:   string(class),
			ErrorSummary: summary,
			RetryAfter:   backoff(int(claim.Failures)+1, errs.RetryAfterOf(err)),
			ResumeState:  active,
			CountFailure: true,
		}
	case errs.ClassCancelled:
		return Outcome{
			NextState:    state.Cancelled,
			StepStatus:   state.StepCancelled,
			ErrorClass:   string(class),
			ErrorSummary: summary,
		}
	default:
		// VALIDATION, NOT_AUTHORIZED, PROVIDER_BUG and unknown classes are
		// terminal: never swap accounts or retry blindly.
		return Outcome{
			NextState:    state.FailedTerminal,
			StepStatus:   state.StepFailedTerminal,
			ErrorClass:   string(class),
			ErrorSummary: summary,
			CountFailure: true,
		}
	}
}

// safeNextState guarantees the commit is a legal edge from the state the worker
// executed under. An adapter/engine mapping that would be rejected by the state
// matrix is a mapping bug: fail terminal (and count the failure) instead of
// leaving the lease to spin until the deadline with no failure budget.
func (e *Engine) safeNextState(from state.RunState, out Outcome) Outcome {
	if out.NextState == "" {
		return out
	}
	if err := state.CanTransition(from, out.NextState); err == nil {
		return out
	}
	if state.CanTransition(from, state.FailedTerminal) == nil {
		out.NextState = state.FailedTerminal
		out.StepStatus = state.StepFailedTerminal
		out.ErrorClass = string(errs.ClassProviderBug)
		out.ErrorSummary = "illegal state transition for step outcome; failing closed"
		out.Wait = 0
		out.RetryAfter = 0
		out.ResumeState = ""
		out.ActionRequired = false
		out.CountFailure = true
		return out
	}
	return out
}

func (e *Engine) toStorageResult(def *StepDef, claim *storage.Claim, out Outcome) storage.StepResult {
	if out.NextState == "" {
		out.NextState = def.Next
	}
	if out.StepStatus == "" {
		out.StepStatus = state.StepSucceeded
	}
	res := storage.StepResult{
		NextState:      string(out.NextState),
		StepStatus:     string(out.StepStatus),
		ErrorClass:     out.ErrorClass,
		ErrorSummary:   out.ErrorSummary,
		Result:         out.Result,
		WaitingReason:  out.WaitingReason,
		ActionRequired: out.ActionRequired,
		ResumeState:    string(out.ResumeState),
		Events:         out.Events,
		Resources:      out.Resources,
		MailReceipts:   out.MailReceipts,
		CountFailure:   out.CountFailure,
	}
	if out.Wait > 0 {
		at := e.now().UTC().Add(out.Wait).UnixMilli()
		res.RetryAtMS = &at
	} else if out.RetryAfter > 0 {
		at := e.now().UTC().Add(out.RetryAfter).UnixMilli()
		res.RetryAtMS = &at
	}
	// Automatic, redacted event per outcome.
	switch {
	case out.StepStatus == state.StepSucceeded:
		res.Events = append(res.Events, storage.EventDraft{Type: "STEP_SUCCEEDED", Payload: map[string]any{
			"failure_class": "",
			"next_retry_at": int64(0),
		}})
	case out.StepStatus == state.StepRetryWait:
		res.Events = append(res.Events, storage.EventDraft{Type: "STEP_RETRY_SCHEDULED", Payload: map[string]any{
			"failure_class": out.ErrorClass,
			"next_retry_at": derefMS(res.RetryAtMS),
			"attempt":       claim.Attempt,
		}})
	case out.StepStatus == state.StepWaitingUserAction:
		res.Events = append(res.Events, storage.EventDraft{Type: "HUMAN_ACTION_REQUIRED", Payload: map[string]any{
			"waiting_for":  out.WaitingReason,
			"resume_state": string(out.ResumeState),
		}})
	case out.StepStatus == state.StepReauthorizeRequired:
		res.Events = append(res.Events, storage.EventDraft{Type: "OAUTH_EXPIRED", Payload: map[string]any{
			"failure_class": string(errs.ClassAuthExpired),
			"resume_state":  string(out.ResumeState),
		}})
	}
	return res
}

func derefMS(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// backoff implements exponential backoff with jitter: base 2s, cap 120s,
// +/-20%; a provider Retry-After takes precedence.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > 120*time.Second {
			retryAfter = 120 * time.Second
		}
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return retryAfter
	}
	d := 2 * time.Second
	for i := 1; i < attempt && d < 120*time.Second; i++ {
		d *= 2
	}
	if d > 120*time.Second {
		d = 120 * time.Second
	}
	jitter := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(d) * jitter)
}
