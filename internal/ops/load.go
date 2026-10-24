package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

// LoadOptions configures the offline load/concurrency smoke.
type LoadOptions struct {
	// Concurrency is the number of runs created simultaneously (R4 plan: 5).
	Concurrency int
	// Sequential is the number of back-to-back runs (R4 plan: 30).
	Sequential int
	// Timeout bounds the whole smoke (default 3m).
	Timeout time.Duration
	// Dir keeps the SQLite database for later inspection (evidence). When empty
	// a temporary directory is used and removed afterwards.
	Dir string
	// LogWriter receives the redacted service logs (optional).
	LogWriter io.Writer
	// KeyringOutPath, when set, writes the harness master key as a raw 32-byte
	// file (0600) so backup/restore/rotation evidence can be decrypted later.
	// This is test-only material: the CLI never prints it.
	KeyringOutPath string
	// Now defaults to time.Now.
	Now func() time.Time
}

// Percentiles is a latency summary in milliseconds.
type Percentiles struct {
	Count int     `json:"count"`
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}

// LoadReport is the evidence produced by RunLoadSmoke.
type LoadReport struct {
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	DurationMS int64  `json:"duration_ms"`

	Concurrency int `json:"concurrency"`
	Sequential  int `json:"sequential"`

	CreateLatency Percentiles `json:"create_latency"`
	RunLatency    Percentiles `json:"run_latency"`

	ExpectedRuns      int `json:"expected_runs"`
	CreatedRuns       int `json:"created_runs"`
	CompletedRuns     int `json:"completed_runs"`
	FailedRuns        int `json:"failed_runs"`
	CancelledRuns     int `json:"cancelled_runs"`
	MissingRuns       int `json:"missing_runs"`
	IdempotentReplays int `json:"idempotent_replays"`

	ErrorCount int      `json:"error_count"`
	Errors     []string `json:"errors,omitempty"`

	DuplicateRuns         int64 `json:"duplicate_runs"`
	DuplicateResources    int64 `json:"duplicate_resources"`
	DuplicateIdentities   int64 `json:"duplicate_identities"`
	DuplicateOperationKey int64 `json:"duplicate_operation_keys"`
	RowCountRuns          int64 `json:"row_count_runs"`

	IdempotentReplaySameID bool            `json:"idempotent_replay_same_id"`
	Checks                 map[string]bool `json:"checks"`
	Pass                   bool            `json:"pass"`

	DBPath      string `json:"db_path,omitempty"`
	KeyringPath string `json:"keyring_path,omitempty"`
}

// RunLoadSmoke creates runs through the real HTTP API against fake providers,
// waits for them to finish in the real workflow engine, and reports latency,
// errors and duplicate checks. Nothing leaves the process: the API is an
// httptest loopback server and every provider is in-memory.
func RunLoadSmoke(ctx context.Context, opts LoadOptions) (LoadReport, error) {
	rep := LoadReport{Checks: map[string]bool{}}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 5
	}
	if opts.Sequential <= 0 {
		opts.Sequential = 30
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	rep.Concurrency = opts.Concurrency
	rep.Sequential = opts.Sequential
	rep.ExpectedRuns = opts.Concurrency + opts.Sequential
	startedAt := opts.Now().UTC()
	rep.StartedAt = startedAt.Format(time.RFC3339)

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	workDir := opts.Dir
	if workDir == "" {
		tmp, err := os.MkdirTemp("", "onboard-load-smoke-*")
		if err != nil {
			return rep, fmt.Errorf("load smoke: create work dir: %w", err)
		}
		workDir = tmp
		defer os.RemoveAll(tmp)
	}
	rep.DBPath = workDir
	stack, err := NewStack(StackOptions{
		Dir:            workDir,
		MaxConcurrency: opts.Concurrency,
		TickInterval:   5 * time.Millisecond,
		LeaseTTL:       30 * time.Second,
		StepTimeout:    5 * time.Second,
		PollInterval:   20 * time.Millisecond,
		MaxAttempts:    4,
		RunDeadline:    2 * time.Minute,
		LogWriter:      opts.LogWriter,
	})
	if err != nil {
		return rep, err
	}
	defer stack.Close()
	stack.Start(runCtx)
	rep.DBPath = stack.DBPath
	if opts.KeyringOutPath != "" {
		if err := os.MkdirAll(filepath.Dir(opts.KeyringOutPath), 0o700); err != nil {
			return rep, fmt.Errorf("load smoke: create keyring dir: %w", err)
		}
		if err := os.WriteFile(opts.KeyringOutPath, stack.MasterKey, 0o600); err != nil {
			return rep, fmt.Errorf("load smoke: write test keyring: %w", err)
		}
		rep.KeyringPath = opts.KeyringOutPath
	}

	base := stack.HTTPBaseURL()
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 32,
		},
	}
	body, err := json.Marshal(loadCreateBody())
	if err != nil {
		return rep, err
	}
	nonce := security.RandomToken(8)
	idem := func(phase string, i int) string {
		return fmt.Sprintf("load-%s-%s-%02d", nonce, phase, i)
	}

	var (
		createMu        sync.Mutex
		createLatencies []time.Duration
		runLatencies    []time.Duration
		errs            []string
		errorCount      int
		completed       int
		failed          int
		cancelled       int
		created         int
		ids             []string
		starts          []time.Time
	)
	recordErr := func(err error) {
		createMu.Lock()
		defer createMu.Unlock()
		errorCount++
		if len(errs) < 20 {
			errs = append(errs, stack.Redactor.Redact(err.Error()))
		}
	}

	// Concurrent creates: all goroutines wait on a released barrier.
	concResults := make([]struct {
		id      string
		latency time.Duration
		start   time.Time
	}, opts.Concurrency)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-barrier
			start := opts.Now()
			id, latency, err := loadCreateRun(runCtx, client, base, stack.AdminToken, idem("c", i), body)
			concResults[i] = struct {
				id      string
				latency time.Duration
				start   time.Time
			}{id, latency, start}
			if err != nil {
				recordErr(err)
			}
		}(i)
	}
	close(barrier)
	wg.Wait()
	for _, r := range concResults {
		if r.id == "" {
			continue
		}
		created++
		createLatencies = append(createLatencies, r.latency)
		ids = append(ids, r.id)
		starts = append(starts, r.start)
	}
	concurrentIDs := append([]string(nil), ids...)
	concurrentStarts := append([]time.Time(nil), starts...)
	waitConcurrent := loadWaitAll(runCtx, client, base, stack.AdminToken, concurrentIDs, concurrentStarts)
	for _, w := range waitConcurrent {
		runLatencies = append(runLatencies, w.latency)
		switch w.status {
		case "COMPLETED":
			completed++
		case "FAILED_TERMINAL":
			failed++
			recordErr(fmt.Errorf("concurrent run ended FAILED_TERMINAL (class %s)", w.errClass))
		case "CANCELLED":
			cancelled++
		default:
			if w.err != nil {
				recordErr(w.err)
			}
		}
	}

	// Sequential runs: create then wait for terminal before the next one.
	var lastID, lastIdem string
	for i := 0; i < opts.Sequential; i++ {
		key := idem("s", i)
		start := opts.Now()
		id, latency, err := loadCreateRun(runCtx, client, base, stack.AdminToken, key, body)
		if err != nil {
			recordErr(err)
			continue
		}
		created++
		createLatencies = append(createLatencies, latency)
		w := loadWaitRun(runCtx, client, base, stack.AdminToken, id, 90*time.Second)
		runLatencies = append(runLatencies, opts.Now().Sub(start))
		switch w.status {
		case "COMPLETED":
			completed++
		case "FAILED_TERMINAL":
			failed++
			recordErr(fmt.Errorf("sequential run ended FAILED_TERMINAL (class %s)", w.errClass))
		case "CANCELLED":
			cancelled++
		default:
			if w.err != nil {
				recordErr(w.err)
			}
		}
		lastID, lastIdem = id, key
	}

	// Idempotent replay of the same request must return the same run and must not
	// create a row.
	if lastIdem != "" {
		replayID, _, err := loadCreateRun(runCtx, client, base, stack.AdminToken, lastIdem, body)
		if err != nil {
			recordErr(fmt.Errorf("idempotent replay failed: %w", err))
		} else {
			rep.IdempotentReplays = 1
			rep.IdempotentReplaySameID = replayID == lastID
		}
	}

	// Duplicate checks directly against SQLite (independent of HTTP responses).
	if dbErr := loadDuplicateChecks(runCtx, stack.DBPath, &rep); dbErr != nil {
		return rep, dbErr
	}

	rep.CreatedRuns = created
	rep.CompletedRuns = completed
	rep.FailedRuns = failed
	rep.CancelledRuns = cancelled
	if rep.ExpectedRuns > created {
		rep.MissingRuns = rep.ExpectedRuns - created
	}
	rep.ErrorCount = errorCount
	rep.Errors = errs
	rep.CreateLatency = computePercentiles(createLatencies)
	rep.RunLatency = computePercentiles(runLatencies)
	rep.FinishedAt = opts.Now().UTC().Format(time.RFC3339)
	rep.DurationMS = opts.Now().Sub(startedAt).Milliseconds()

	rep.Checks["all_runs_completed"] = completed == rep.ExpectedRuns
	rep.Checks["created_expected_runs"] = created == rep.ExpectedRuns
	rep.Checks["row_count_matches"] = rep.RowCountRuns == int64(rep.ExpectedRuns)
	rep.Checks["no_http_errors"] = rep.ErrorCount == 0
	rep.Checks["no_duplicate_runs"] = rep.DuplicateRuns == 0
	rep.Checks["no_duplicate_resources"] = rep.DuplicateResources == 0
	rep.Checks["no_duplicate_identities"] = rep.DuplicateIdentities == 0
	rep.Checks["no_duplicate_operation_keys"] = rep.DuplicateOperationKey == 0
	rep.Checks["idempotent_replay_same_id"] = rep.IdempotentReplaySameID
	rep.Pass = true
	for _, ok := range rep.Checks {
		if !ok {
			rep.Pass = false
			break
		}
	}
	return rep, nil
}

func loadCreateBody() map[string]any {
	return map[string]any{
		"template":            "sandbox-email-verify-oidc",
		"mode":                "sandbox-e2e",
		"mailbox_provider":    "fake-mailbox",
		"identity_provider":   "fake-identity",
		"oauth_provider":      "fake-oauth",
		"sandbox_provider":    "fake-sandbox",
		"credential_provider": "fake-credential",
	}
}

func loadCreateRun(ctx context.Context, client *http.Client, base, token, idem string, body []byte) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idem)
	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return "", latency, fmt.Errorf("create run: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusAccepted {
		return "", latency, fmt.Errorf("create run: unexpected HTTP status %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", latency, fmt.Errorf("create run: malformed response")
	}
	return out.ID, latency, nil
}

type loadWaitOutcome struct {
	status   string
	errClass string
	latency  time.Duration
	err      error
}

func loadWaitRun(ctx context.Context, client *http.Client, base, token, id string, timeout time.Duration) loadWaitOutcome {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return loadWaitOutcome{err: fmt.Errorf("run %s did not reach a terminal state within %s", id, timeout)}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/runs/"+id, nil)
		if err != nil {
			return loadWaitOutcome{err: err}
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return loadWaitOutcome{err: fmt.Errorf("poll run %s: %w", id, err)}
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return loadWaitOutcome{err: fmt.Errorf("poll run %s: HTTP %d", id, resp.StatusCode)}
		}
		var out struct {
			Status         string `json:"status"`
			LastErrorClass string `json:"last_error_class"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return loadWaitOutcome{err: fmt.Errorf("poll run %s: malformed response", id)}
		}
		switch out.Status {
		case "COMPLETED", "FAILED_TERMINAL", "CANCELLED":
			return loadWaitOutcome{status: out.Status, errClass: out.LastErrorClass}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return loadWaitOutcome{err: ctx.Err()}
		case <-timer.C:
		}
	}
}

func loadWaitAll(ctx context.Context, client *http.Client, base, token string, ids []string, starts []time.Time) []loadWaitOutcome {
	out := make([]loadWaitOutcome, len(ids))
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			w := loadWaitRun(ctx, client, base, token, ids[i], 90*time.Second)
			// Prefer the wall clock since the create returned; falls back to the
			// wait window if the caller did not supply a start time.
			if i < len(starts) && !starts[i].IsZero() {
				out[i] = loadWaitOutcome{status: w.status, errClass: w.errClass, latency: time.Since(starts[i]), err: w.err}
			} else {
				out[i] = loadWaitOutcome{status: w.status, errClass: w.errClass, latency: time.Since(start), err: w.err}
			}
		}(i)
	}
	wg.Wait()
	return out
}

func loadDuplicateChecks(ctx context.Context, dbPath string, rep *LoadReport) error {
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	count := func(query string) (int64, error) {
		var n int64
		err := db.QueryRowContext(ctx, query).Scan(&n)
		return n, err
	}
	if rep.DuplicateRuns, err = count(`SELECT COUNT(*) FROM (
		SELECT owner_id, template, idempotency_key FROM runs
		GROUP BY owner_id, template, idempotency_key HAVING COUNT(*) > 1)`); err != nil {
		return fmt.Errorf("load smoke: duplicate run query: %w", err)
	}
	if rep.DuplicateResources, err = count(`SELECT COUNT(*) FROM (
		SELECT run_id, resource_kind, external_id_hmac FROM resources
		GROUP BY run_id, resource_kind, external_id_hmac HAVING COUNT(*) > 1)`); err != nil {
		return fmt.Errorf("load smoke: duplicate resource query: %w", err)
	}
	if rep.DuplicateIdentities, err = count(`SELECT COUNT(*) FROM (
		SELECT external_id_hmac FROM resources WHERE resource_kind='identity'
		GROUP BY external_id_hmac HAVING COUNT(*) > 1)`); err != nil {
		return fmt.Errorf("load smoke: duplicate identity query: %w", err)
	}
	if rep.DuplicateOperationKey, err = count(`SELECT COUNT(*) FROM (
		SELECT operation_key FROM steps GROUP BY operation_key HAVING COUNT(*) > 1)`); err != nil {
		return fmt.Errorf("load smoke: duplicate operation key query: %w", err)
	}
	if rep.RowCountRuns, err = count(`SELECT COUNT(*) FROM runs`); err != nil {
		return fmt.Errorf("load smoke: run count query: %w", err)
	}
	var secrets int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets`).Scan(&secrets); err != nil {
		return fmt.Errorf("load smoke: secret count query: %w", err)
	}
	return nil
}

// computePercentiles returns nearest-rank percentiles in milliseconds.
func computePercentiles(d []time.Duration) Percentiles {
	p := Percentiles{Count: len(d)}
	if len(d) == 0 {
		return p
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	ms := func(v time.Duration) float64 { return float64(v) / float64(time.Millisecond) }
	rank := func(pct float64) float64 {
		idx := int(math.Ceil(pct/100*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return ms(sorted[idx])
	}
	p.P50MS = rank(50)
	p.P95MS = rank(95)
	p.P99MS = rank(99)
	p.MaxMS = ms(sorted[len(sorted)-1])
	return p
}
