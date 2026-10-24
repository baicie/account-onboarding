// Package e2e drives the real onboardd binary against the LOCAL SANDBOX
// Keycloak + Mailpit fixture. Every test is skipped unless
// ONBOARD_SANDBOX_E2E=1, so `go test ./...` stays green without the fixture.
//
// The tests never talk to a public IdP: the adapter rejects non-loopback base
// URLs, and this harness only uses 127.0.0.1 endpoints exported by
// tests/sandbox/env.sh.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

const (
	templateName   = "sandbox-email-verify-oidc"
	templateMode   = "sandbox-e2e"
	mailboxName    = "mailpit"
	keycloakName   = "keycloak-test"
	mailboxDomain  = "moonbyte.top"
	adminSecretEnv = "SANDBOX_ADMIN_CLIENT_SECRET"
)

// sandboxEnv is the resolved loopback fixture configuration.
type sandboxEnv struct {
	RepoRoot          string
	KeycloakURL       string
	Realm             string
	ClientID          string
	RedirectURI       string
	AdminUser         string
	AdminPassword     string
	AdminClientID     string
	AdminClientSecret string
	MailpitURL        string
	SingleUserPW      string
}

// requireSandbox returns the fixture settings or skips the test.
func requireSandbox(t *testing.T) sandboxEnv {
	t.Helper()
	if os.Getenv("ONBOARD_SANDBOX_E2E") != "1" {
		t.Skip("SANDBOX E2E skipped: set ONBOARD_SANDBOX_E2E=1 and source tests/sandbox/env.sh")
	}
	env := sandboxEnv{
		RepoRoot:          repoRoot(t),
		KeycloakURL:       strings.TrimRight(firstNonEmpty(os.Getenv("KEYCLOAK_URL"), "http://127.0.0.1:8180"), "/"),
		Realm:             firstNonEmpty(os.Getenv("KEYCLOAK_REALM"), "moonbyte-test"),
		ClientID:          firstNonEmpty(os.Getenv("KEYCLOAK_CLIENT_ID"), "onboarding-test"),
		RedirectURI:       firstNonEmpty(os.Getenv("ONBOARDING_OIDC_REDIRECT_URI"), "http://127.0.0.1:8788/oidc/callback"),
		AdminUser:         firstNonEmpty(os.Getenv("SANDBOX_ADMIN_USER"), "kcadmin"),
		AdminPassword:     os.Getenv("SANDBOX_ADMIN_PASSWORD"),
		AdminClientID:     firstNonEmpty(os.Getenv("SANDBOX_ADMIN_CLIENT_ID"), "onboarding-admin"),
		AdminClientSecret: os.Getenv(adminSecretEnv),
		MailpitURL:        strings.TrimRight(firstNonEmpty(os.Getenv("MAILPIT_HTTP_URL"), "http://127.0.0.1:8025"), "/"),
		SingleUserPW:      os.Getenv("SANDBOX_TEST_USER_PASSWORD"),
	}
	// Fall back to the generated fixture credentials when the caller did not
	// source tests/sandbox/env.sh (still loopback + test-only credentials).
	if env.AdminPassword == "" || env.AdminClientSecret == "" {
		fileVals := loadKeyValueFile(filepath.Join(env.RepoRoot, ".sandbox", "secrets", "sandbox.env"))
		if env.AdminPassword == "" {
			env.AdminPassword = fileVals["SANDBOX_ADMIN_PASSWORD"]
		}
		if env.AdminClientSecret == "" {
			env.AdminClientSecret = fileVals["SANDBOX_ADMIN_CLIENT_SECRET"]
		}
		if env.AdminUser == "" {
			env.AdminUser = fileVals["SANDBOX_ADMIN_USER"]
		}
		if env.AdminClientID == "" {
			env.AdminClientID = fileVals["SANDBOX_ADMIN_CLIENT_ID"]
		}
		if env.SingleUserPW == "" {
			env.SingleUserPW = fileVals["SANDBOX_TEST_USER_PASSWORD"]
		}
	}
	if env.AdminPassword == "" && env.AdminClientSecret == "" {
		t.Fatalf("ONBOARD_SANDBOX_E2E=1 but no sandbox admin credentials were found; source tests/sandbox/env.sh " +
			"(or run bash tests/sandbox/up.sh to generate .sandbox/secrets/sandbox.env)")
	}
	requireLoopback(t, "KEYCLOAK_URL", env.KeycloakURL)
	requireLoopback(t, "MAILPIT_HTTP_URL", env.MailpitURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if status, err := rawGET(ctx, env.KeycloakURL+"/realms/"+env.Realm+"/.well-known/openid-configuration"); err != nil || status != http.StatusOK {
		t.Fatalf("sandbox Keycloak realm %q is not serving OIDC discovery (status=%d err=%v); run bash tests/sandbox/up.sh",
			env.Realm, status, err)
	}
	if status, err := rawGET(ctx, env.MailpitURL+"/api/v1/messages?limit=1"); err != nil || status != http.StatusOK {
		t.Fatalf("sandbox Mailpit API is not reachable (status=%d err=%v); run bash tests/sandbox/up.sh", status, err)
	}
	return env
}

func requireLoopback(t *testing.T, label, raw string) {
	t.Helper()
	lower := strings.ToLower(raw)
	if !strings.Contains(lower, "127.0.0.1") && !strings.Contains(lower, "localhost") && !strings.Contains(lower, "[::1]") {
		t.Fatalf("%s=%q is not loopback; this harness is for the local sandbox only", label, raw)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func loadKeyValueFile(path string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return out
}

// ---------------------------------------------------------------------------
// Repository paths and the real onboardd binary
// ---------------------------------------------------------------------------

var (
	repoRootOnce sync.Once
	repoRootPath string
	buildOnce    sync.Once
	binaryPath   string
	buildErr     error
)

func repoRoot(t *testing.T) string {
	t.Helper()
	repoRootOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			return
		}
		repoRootPath = filepath.Dir(filepath.Dir(filepath.Dir(file)))
	})
	if repoRootPath == "" {
		t.Fatal("could not determine the repository root")
	}
	return repoRootPath
}

func onboarddBinary(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	buildOnce.Do(func() {
		dir := filepath.Join(root, ".sandbox", "e2e", "bin")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			buildErr = err
			return
		}
		out := filepath.Join(dir, "onboardd")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/onboardd")
		cmd.Dir = root
		cmd.Env = os.Environ()
		combined, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build ./cmd/onboardd: %w: %s", err, strings.TrimSpace(string(combined)))
			return
		}
		binaryPath = out
	})
	if buildErr != nil {
		t.Fatalf("build onboardd: %v", buildErr)
	}
	return binaryPath
}

// ---------------------------------------------------------------------------
// onboardd subprocess
// ---------------------------------------------------------------------------

type procOptions struct {
	Addr            string
	DataDir         string
	Tick            time.Duration
	Poll            time.Duration
	StepTimeout     time.Duration
	LeaseTTL        time.Duration
	SenderAllowlist string
	MaxConcurrency  int
}

type onboardd struct {
	t               *testing.T
	env             sandboxEnv
	bin             string
	addr            string
	baseURL         string
	dataDir         string
	adminToken      string
	masterKeyHex    string
	tick            time.Duration
	poll            time.Duration
	stepTimeout     time.Duration
	leaseTTL        time.Duration
	senderAllowlist string
	maxConcurrency  int
	spawns          int

	cmd     *exec.Cmd
	logFile *os.File
}

func startOnboardd(t *testing.T, env sandboxEnv, opts procOptions) *onboardd {
	t.Helper()
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:18788"
	}
	if opts.Tick <= 0 {
		opts.Tick = 150 * time.Millisecond
	}
	if opts.Poll <= 0 {
		opts.Poll = time.Second
	}
	if opts.StepTimeout <= 0 {
		opts.StepTimeout = 45 * time.Second
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = 90 * time.Second
	}
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 2
	}
	if opts.SenderAllowlist == "" {
		opts.SenderAllowlist = "moonbyte.test,idp.fake.test"
	}
	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = filepath.Join(env.RepoRoot, ".sandbox", "e2e",
			"run-"+time.Now().UTC().Format("20060102T150405")+"-"+randomHex(t, 3))
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create sandbox e2e data dir: %v", err)
	}
	o := &onboardd{
		t:               t,
		env:             env,
		bin:             onboarddBinary(t),
		addr:            opts.Addr,
		baseURL:         "http://" + opts.Addr,
		dataDir:         dataDir,
		tick:            opts.Tick,
		poll:            opts.Poll,
		stepTimeout:     opts.StepTimeout,
		leaseTTL:        opts.LeaseTTL,
		senderAllowlist: opts.SenderAllowlist,
		maxConcurrency:  opts.MaxConcurrency,
	}
	o.adminToken = o.ensureSecretFile("admin-token", 24)
	o.masterKeyHex = o.ensureMasterKey()
	t.Cleanup(func() { o.terminate() })
	o.spawn()
	return o
}

// ensureSecretFile writes a random 0600 secret file once and returns the value.
func (o *onboardd) ensureSecretFile(name string, nbytes int) string {
	path := filepath.Join(o.dataDir, name)
	if raw, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(raw))
	}
	value := randomHex(o.t, nbytes)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		o.t.Fatalf("write %s: %v", name, err)
	}
	return value
}

// ensureMasterKey writes a real 32-byte master key (64 hex chars, 0600).
func (o *onboardd) ensureMasterKey() string {
	path := filepath.Join(o.dataDir, "master-key")
	if raw, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(raw)); len(v) == 64 {
			return v
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		o.t.Fatalf("generate master key: %v", err)
	}
	value := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		o.t.Fatalf("write master key: %v", err)
	}
	return value
}

// envVars builds a hermetic environment: any inherited ONBOARD_* variable is
// dropped so the fixture cannot be silently redirected by the caller shell.
func (o *onboardd) envVars() []string {
	out := make([]string, 0, len(os.Environ())+32)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "ONBOARD_") {
			continue
		}
		out = append(out, entry)
	}
	adminPasswordPath := filepath.Join(o.dataDir, "keycloak-admin-password")
	if o.env.AdminPassword != "" {
		if err := os.WriteFile(adminPasswordPath, []byte(o.env.AdminPassword+"\n"), 0o600); err != nil {
			o.t.Fatalf("write keycloak admin password file: %v", err)
		}
	}
	adminClientSecretPath := filepath.Join(o.dataDir, "keycloak-admin-client-secret")
	if o.env.AdminClientSecret != "" {
		if err := os.WriteFile(adminClientSecretPath, []byte(o.env.AdminClientSecret+"\n"), 0o600); err != nil {
			o.t.Fatalf("write keycloak admin client secret file: %v", err)
		}
	}
	out = append(out,
		"ONBOARD_LISTEN="+o.addr,
		"ONBOARD_DB_PATH="+filepath.Join(o.dataDir, "state.sqlite"),
		"ONBOARD_MODE=sandbox",
		"ONBOARD_LOG_LEVEL=info",
		"ONBOARD_DOMAINS="+mailboxDomain,
		"ONBOARD_SENDER_ALLOWLIST="+o.senderAllowlist,
		"ONBOARD_MAX_CONCURRENCY="+strconv.Itoa(o.maxConcurrency),
		"ONBOARD_POLL_INTERVAL="+o.poll.String(),
		"ONBOARD_TICK_INTERVAL="+o.tick.String(),
		"ONBOARD_STEP_TIMEOUT="+o.stepTimeout.String(),
		"ONBOARD_LEASE_TTL="+o.leaseTTL.String(),
		"ONBOARD_RUN_DEADLINE=5m",
		// The harness polls the control plane; keep its own limiter out of the way.
		"ONBOARD_RATE_LIMIT_PER_MINUTE=6000",
		"ONBOARD_LOGIN_RATE_LIMIT_PER_MINUTE=60",
		"ONBOARD_ADMIN_TOKEN_FILE="+filepath.Join(o.dataDir, "admin-token"),
		"ONBOARD_MASTER_KEY_FILE="+filepath.Join(o.dataDir, "master-key"),
		"ONBOARD_KEYCLOAK_ENABLED=true",
		"ONBOARD_KEYCLOAK_BASE_URL="+o.env.KeycloakURL,
		"ONBOARD_KEYCLOAK_REALM="+o.env.Realm,
		"ONBOARD_KEYCLOAK_ADMIN_USER="+o.env.AdminUser,
		"ONBOARD_KEYCLOAK_CLIENT_ID="+o.env.ClientID,
		"ONBOARD_KEYCLOAK_REDIRECT_URI="+o.env.RedirectURI,
		"ONBOARD_OIDC_REDIRECT_URI="+o.env.RedirectURI,
		"ONBOARD_MAILPIT_ENABLED=true",
		"ONBOARD_MAILPIT_BASE_URL="+o.env.MailpitURL,
		"ONBOARD_MAILPIT_DOMAIN="+mailboxDomain,
	)
	if o.env.AdminPassword != "" {
		out = append(out, "ONBOARD_KEYCLOAK_ADMIN_PASSWORD_FILE="+adminPasswordPath)
	}
	// Optional config fields (Lead owns internal/config): when present the
	// adapter prefers the service-account client_credentials grant.
	out = append(out,
		"ONBOARD_KEYCLOAK_ADMIN_CLIENT_ID="+o.env.AdminClientID,
		"ONBOARD_KEYCLOAK_ADMIN_CLIENT_SECRET_FILE="+adminClientSecretPath,
	)
	return out
}

func (o *onboardd) spawn() {
	o.spawns++
	logPath := filepath.Join(o.dataDir, fmt.Sprintf("onboardd-%d.log", o.spawns))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		o.t.Fatalf("open onboardd log: %v", err)
	}
	cmd := exec.Command(o.bin)
	cmd.Env = o.envVars()
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Dir = o.dataDir
	if err := cmd.Start(); err != nil {
		f.Close()
		o.t.Fatalf("start onboardd: %v", err)
	}
	o.cmd = cmd
	o.logFile = f
	o.waitReady()
}

func (o *onboardd) waitReady() {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if status, err := rawGET(context.Background(), o.baseURL+"/healthz"); err == nil && status == http.StatusOK {
			if status, body, err := o.tryRequest(http.MethodGet, "/readyz", nil); err == nil && status == http.StatusOK {
				var payload struct {
					Ready bool `json:"ready"`
				}
				if json.Unmarshal(body, &payload) == nil && payload.Ready {
					return
				}
			}
		}
		if o.cmd.ProcessState != nil && o.cmd.ProcessState.Exited() {
			o.t.Fatalf("onboardd exited during startup; log: %s", o.logPath())
		}
		time.Sleep(50 * time.Millisecond)
	}
	o.terminate()
	o.t.Fatalf("onboardd did not become ready within 60s; log: %s", o.logPath())
}

func (o *onboardd) logPath() string {
	return filepath.Join(o.dataDir, "onboardd.log")
}

// terminate stops the process gracefully (SIGTERM) and waits for it.
func (o *onboardd) terminate() {
	if o.cmd == nil || o.cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = o.cmd.Wait()
		close(done)
	}()
	_ = o.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = o.cmd.Process.Kill()
		<-done
	}
	o.cmd = nil
	if o.logFile != nil {
		_ = o.logFile.Close()
		o.logFile = nil
	}
}

// kill delivers SIGKILL, simulating a crash with no graceful shutdown.
func (o *onboardd) kill() {
	if o.cmd == nil || o.cmd.Process == nil {
		return
	}
	_ = o.cmd.Process.Kill()
	_, _ = o.cmd.Process.Wait()
	o.cmd = nil
	if o.logFile != nil {
		_ = o.logFile.Close()
		o.logFile = nil
	}
}

// restart starts a new process on the SAME data dir and master key.
func (o *onboardd) restart(senderAllowlist string) {
	o.terminate()
	if senderAllowlist != "" {
		o.senderAllowlist = senderAllowlist
	}
	o.spawn()
}

// ---------------------------------------------------------------------------
// API helpers
// ---------------------------------------------------------------------------

type httpResult struct {
	Status int
	Body   []byte
}

func (o *onboardd) tryRequest(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, o.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+o.adminToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, payload, nil
}

// request performs an authenticated API call and fails the test on an
// unexpected status code.
func (o *onboardd) request(method, path string, body any, allow ...int) httpResult {
	o.t.Helper()
	status, payload, err := o.tryRequest(method, path, body)
	if err != nil {
		o.t.Fatalf("%s %s: %v (log: %s)", method, path, err, o.logPath())
	}
	ok := len(allow) == 0 && status >= 200 && status < 300
	for _, want := range allow {
		if status == want {
			ok = true
		}
	}
	if !ok {
		o.t.Fatalf("%s %s: unexpected status %d (want %v): %s", method, path, status, allow, truncate(payload, 600))
	}
	return httpResult{Status: status, Body: payload}
}

func truncate(raw []byte, max int) string {
	s := strings.TrimSpace(string(raw))
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// listRuns returns the newest run summaries in one API call, which keeps the
// E2E polling well below the control-plane rate limiter.
func (o *onboardd) listRuns() []runView {
	o.t.Helper()
	res := o.request(http.MethodGet, "/v1/runs?limit=100", nil)
	var payload struct {
		Items []runView `json:"items"`
	}
	if err := json.Unmarshal(res.Body, &payload); err != nil {
		o.t.Fatalf("decode run list: %v", err)
	}
	return payload.Items
}

type runView struct {
	ID             string     `json:"id"`
	Status         string     `json:"status"`
	Template       string     `json:"template"`
	Mode           string     `json:"mode"`
	CurrentStep    string     `json:"current_step"`
	ActionRequired bool       `json:"action_required"`
	LastErrorClass string     `json:"last_error_class"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
	DeadlineAt     string     `json:"deadline_at"`
	Steps          []stepView `json:"steps"`
}

type stepView struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	AttemptCount  int64  `json:"attempt_count"`
	FailureCount  int64  `json:"failure_count"`
	ErrorClass    string `json:"error_class"`
	ErrorSummary  string `json:"error_summary"`
	WaitingReason string `json:"waiting_reason"`
}

type eventView struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	Step      string         `json:"step"`
	Payload   map[string]any `json:"payload"`
	CreatedAt string         `json:"created_at"`
}

func (o *onboardd) createRun(idem string) (runView, int) {
	o.t.Helper()
	body := map[string]any{
		"template":            templateName,
		"mode":                templateMode,
		"mailbox_provider":    mailboxName,
		"identity_provider":   keycloakName,
		"oauth_provider":      keycloakName,
		"sandbox_provider":    keycloakName,
		"credential_provider": keycloakName,
	}
	var req *http.Request
	raw, err := json.Marshal(body)
	if err != nil {
		o.t.Fatalf("encode run request: %v", err)
	}
	req, err = http.NewRequest(http.MethodPost, o.baseURL+"/v1/runs", bytes.NewReader(raw))
	if err != nil {
		o.t.Fatalf("build run request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+o.adminToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idem)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		o.t.Fatalf("create run: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted {
		o.t.Fatalf("create run %q: status %d: %s", idem, resp.StatusCode, truncate(payload, 600))
	}
	var view runView
	if err := json.Unmarshal(payload, &view); err != nil {
		o.t.Fatalf("decode run response: %v", err)
	}
	return view, resp.StatusCode
}

func (o *onboardd) getRun(id string) runView {
	o.t.Helper()
	res := o.request(http.MethodGet, "/v1/runs/"+id, nil)
	var view runView
	if err := json.Unmarshal(res.Body, &view); err != nil {
		o.t.Fatalf("decode run: %v", err)
	}
	return view
}

func (o *onboardd) events(id string) []eventView {
	o.t.Helper()
	res := o.request(http.MethodGet, "/v1/runs/"+id+"/events", nil)
	var payload struct {
		Items []eventView `json:"items"`
	}
	if err := json.Unmarshal(res.Body, &payload); err != nil {
		o.t.Fatalf("decode events: %v", err)
	}
	return payload.Items
}

func (o *onboardd) cancel(id string) runView {
	o.t.Helper()
	res := o.request(http.MethodPost, "/v1/runs/"+id+"/cancel", nil, http.StatusAccepted)
	var view runView
	if err := json.Unmarshal(res.Body, &view); err != nil {
		o.t.Fatalf("decode cancel response: %v", err)
	}
	return view
}

// waitForStatus polls the API until the run reaches one of the wanted states.
func (o *onboardd) waitForStatus(id string, timeout time.Duration, want ...string) runView {
	o.t.Helper()
	wanted := map[string]bool{}
	for _, w := range want {
		wanted[w] = true
	}
	deadline := time.Now().Add(timeout)
	var last runView
	for time.Now().Before(deadline) {
		last = o.getRun(id)
		if wanted[last.Status] {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	o.t.Fatalf("run %s did not reach %v within %s (last status=%s step=%s error=%s; log: %s)",
		id, want, timeout, last.Status, last.CurrentStep, last.LastErrorClass, o.logPath())
	return last
}

type providerEntry struct {
	Name              string   `json:"name"`
	Kind              string   `json:"kind"`
	ContractVersion   string   `json:"contract_version"`
	Enabled           bool     `json:"enabled"`
	Live              bool     `json:"live"`
	Capabilities      []string `json:"capabilities"`
	RestrictionReason string   `json:"restriction_reason"`
}

func (o *onboardd) providers() []providerEntry {
	o.t.Helper()
	res := o.request(http.MethodGet, "/v1/providers", nil)
	var payload struct {
		Items []providerEntry `json:"items"`
	}
	if err := json.Unmarshal(res.Body, &payload); err != nil {
		o.t.Fatalf("decode providers: %v", err)
	}
	return payload.Items
}

// ---------------------------------------------------------------------------
// Keycloak Admin REST helper (test assertions and teardown only)
// ---------------------------------------------------------------------------

type kcUser struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Email            string `json:"email"`
	EmailVerified    bool   `json:"emailVerified"`
	Enabled          bool   `json:"enabled"`
	CreatedTimestamp int64  `json:"createdTimestamp"`
}

type kcAdmin struct {
	base      string
	realm     string
	user      string
	password  string
	clientID  string
	secret    string
	client    *http.Client
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newKCAdmin(env sandboxEnv) *kcAdmin {
	return &kcAdmin{
		base:     env.KeycloakURL,
		realm:    env.Realm,
		user:     env.AdminUser,
		password: env.AdminPassword,
		clientID: env.AdminClientID,
		secret:   env.AdminClientSecret,
		client:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (k *kcAdmin) accessToken(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Now().Before(k.expiresAt) {
		return k.token, nil
	}
	form := map[string]string{}
	tokenRealm := "master"
	if k.clientID != "" && k.secret != "" {
		// The service-account client lives in the sandbox realm.
		form["grant_type"] = "client_credentials"
		form["client_id"] = k.clientID
		form["client_secret"] = k.secret
		tokenRealm = k.realm
	} else {
		form["grant_type"] = "password"
		form["client_id"] = "admin-cli"
		form["username"] = k.user
		form["password"] = k.password
	}
	encoded := encodeForm(form)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		k.base+"/realms/"+tokenRealm+"/protocol/openid-connect/token", strings.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := k.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("admin token status %d", resp.StatusCode)
	}
	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(payload, &tokenResp); err != nil || tokenResp.AccessToken == "" {
		return "", fmt.Errorf("admin token response is invalid")
	}
	k.token = tokenResp.AccessToken
	ttl := time.Duration(tokenResp.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	k.expiresAt = time.Now().Add(ttl - 30*time.Second)
	return k.token, nil
}

func encodeForm(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, urlQueryEscape(key)+"="+urlQueryEscape(values[key]))
	}
	return strings.Join(parts, "&")
}

func (k *kcAdmin) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	token, err := k.accessToken(ctx)
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, payload, err
}

func (k *kcAdmin) listUsers(ctx context.Context) ([]kcUser, error) {
	status, payload, err := k.do(ctx, http.MethodGet,
		"/admin/realms/"+k.realm+"/users?max=500&briefRepresentation=false", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list users status %d", status)
	}
	var users []kcUser
	if err := json.Unmarshal(payload, &users); err != nil {
		return nil, fmt.Errorf("decode users: %w", err)
	}
	return users, nil
}

func (k *kcAdmin) getUser(ctx context.Context, id string) (kcUser, int, error) {
	status, payload, err := k.do(ctx, http.MethodGet, "/admin/realms/"+k.realm+"/users/"+id, nil)
	if err != nil {
		return kcUser{}, 0, err
	}
	if status != http.StatusOK {
		return kcUser{}, status, nil
	}
	var user kcUser
	if err := json.Unmarshal(payload, &user); err != nil {
		return kcUser{}, status, fmt.Errorf("decode user: %w", err)
	}
	return user, status, nil
}

func (k *kcAdmin) deleteUser(ctx context.Context, id string) error {
	status, _, err := k.do(ctx, http.MethodDelete, "/admin/realms/"+k.realm+"/users/"+id, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("delete user status %d", status)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Mailpit helper
// ---------------------------------------------------------------------------

type mpAddress struct {
	Name    string `json:"Name"`
	Address string `json:"Address"`
}

type mpMessage struct {
	ID        string      `json:"ID"`
	MessageID string      `json:"MessageID"`
	From      mpAddress   `json:"From"`
	To        []mpAddress `json:"To"`
	Subject   string      `json:"Subject"`
	Created   string      `json:"Created"`
}

type mailpitClient struct {
	base   string
	client *http.Client
}

func newMailpitClient(env sandboxEnv) *mailpitClient {
	return &mailpitClient{base: env.MailpitURL, client: &http.Client{Timeout: 20 * time.Second}}
}

func (m *mailpitClient) messages(ctx context.Context) ([]mpMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/api/v1/messages?limit=500", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mailpit list status %d", resp.StatusCode)
	}
	var wrapper struct {
		Messages []mpMessage `json:"messages"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return nil, fmt.Errorf("decode mailpit messages: %w", err)
	}
	return wrapper.Messages, nil
}

// deleteMessages removes only the given Mailpit message ids. Mailpit has no
// per-message DELETE route; its documented batch form is
// DELETE /api/v1/messages with {"IDs":[...]}. The shared sink is never purged
// wholesale, so other fixtures keep their mail.
func (m *mailpitClient) deleteMessages(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += 100 {
		end := start + 100
		if end > len(ids) {
			end = len(ids)
		}
		payload, err := json.Marshal(map[string]any{"IDs": ids[start:end]})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.base+"/api/v1/messages", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := m.client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("mailpit delete messages status %d", resp.StatusCode)
		}
	}
	return nil
}

func messageRecipients(msg mpMessage) []string {
	out := make([]string, 0, len(msg.To))
	for _, to := range msg.To {
		out = append(out, strings.ToLower(strings.TrimSpace(to.Address)))
	}
	return out
}

// ---------------------------------------------------------------------------
// Local SQLite probe (same code paths the service uses)
// ---------------------------------------------------------------------------

type dbProbe struct {
	store     *storage.Store
	sec       *secrets.Service
	closeOnce sync.Once
}

// Close releases the probe's database handles so a restarted server can reopen
// the same file without contention.
func (p *dbProbe) Close() {
	p.closeOnce.Do(func() { _ = p.store.Close() })
}

// openProbe opens the orchestrator database after the process stopped. The
// server never runs concurrently with this helper, so no second writer exists.
func openProbe(t *testing.T, o *onboardd) *dbProbe {
	t.Helper()
	if o != nil && o.cmd != nil {
		o.terminate()
	}
	master, err := hex.DecodeString(o.masterKeyHex)
	if err != nil || len(master) != 32 {
		t.Fatalf("invalid master key for probe")
	}
	hmacKey := security.DeriveKey(master, "storage-hmac")
	store, err := storage.Open(context.Background(), storage.Options{
		Path:    filepath.Join(o.dataDir, "state.sqlite"),
		HMACKey: hmacKey,
	})
	if err != nil {
		t.Fatalf("open orchestrator database for assertions: %v", err)
	}
	probe := &dbProbe{store: store}
	t.Cleanup(probe.Close)
	ring, err := security.NewKeyRing(master)
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	probe.sec = secrets.New(store, security.NewBox(ring))
	return probe
}

type runFixture struct {
	RunID          string
	Status         string
	MailboxAddress string
	SubjectID      string
	CredentialRef  string
	CreatedAtMS    int64
	FinishedAtMS   int64
}

func (p *dbProbe) runFixture(t *testing.T, runID string) runFixture {
	t.Helper()
	ctx := context.Background()
	run, err := p.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	fx := runFixture{RunID: runID, Status: run.Status, CreatedAtMS: run.CreatedAtMS}
	if run.FinishedAtMS != nil {
		fx.FinishedAtMS = *run.FinishedAtMS
	}
	if res, err := p.store.LatestResource(ctx, runID, "mailbox"); err == nil {
		var extra struct {
			Address string `json:"address"`
		}
		_ = json.Unmarshal([]byte(res.ExtraJSON), &extra)
		fx.MailboxAddress = firstNonEmpty(extra.Address, res.DisplayAlias)
	}
	if res, err := p.store.LatestResource(ctx, runID, "identity"); err == nil {
		var extra struct {
			SubjectID string `json:"subject_id"`
			Address   string `json:"address"`
		}
		_ = json.Unmarshal([]byte(res.ExtraJSON), &extra)
		fx.SubjectID = extra.SubjectID
		if fx.MailboxAddress == "" {
			fx.MailboxAddress = extra.Address
		}
	}
	if res, err := p.store.LatestResource(ctx, runID, "credential_ref"); err == nil {
		fx.CredentialRef = res.SecretRef
	}
	return fx
}

type tokenEnvelope struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	Audience    string `json:"audience"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

func (p *dbProbe) credential(t *testing.T, ref string) tokenEnvelope {
	t.Helper()
	if ref == "" {
		t.Fatal("run has no stored credential reference")
	}
	raw, err := p.sec.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolve credential reference: %v", err)
	}
	var envelope tokenEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode stored credential: %v", err)
	}
	if envelope.AccessToken == "" {
		t.Fatal("stored credential has no access token")
	}
	return envelope
}

func (p *dbProbe) stepStatuses(t *testing.T, runID string) map[string]stepView {
	t.Helper()
	steps, err := p.store.ListSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	out := make(map[string]stepView, len(steps))
	for _, step := range steps {
		out[step.Name] = stepView{Name: step.Name, Status: step.Status, AttemptCount: step.AttemptCount,
			FailureCount: step.FailureCount, ErrorClass: step.ErrorClass, WaitingReason: step.WaitingReason}
	}
	return out
}

func (p *dbProbe) credentialResourceCount(t *testing.T, runID string) int {
	t.Helper()
	resources, err := p.store.ListResources(context.Background(), runID, "credential_ref")
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	return len(resources)
}

// ---------------------------------------------------------------------------
// Keycloak userinfo + evidence + stats
// ---------------------------------------------------------------------------

func userinfo(ctx context.Context, env sandboxEnv, accessToken string) (sub, email string, verified bool, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		env.KeycloakURL+"/realms/"+env.Realm+"/protocol/openid-connect/userinfo", nil)
	if err != nil {
		return "", "", false, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", "", false, 0, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", false, resp.StatusCode, nil
	}
	var info struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &info); err != nil {
		return "", "", false, resp.StatusCode, err
	}
	return info.Sub, info.Email, info.EmailVerified, resp.StatusCode, nil
}

func writeEvidence(t *testing.T, o *onboardd, name string, payload any) string {
	t.Helper()
	path := filepath.Join(o.dataDir, name)
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("encode evidence: %v", err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
	t.Logf("evidence: %s", path)
	return path
}

// percentile returns the nearest-rank percentile of the durations.
func percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted))*p+0.9999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(buf)
}

// sandboxUsersSince returns controlled sandbox users created during this test
// window ("onboard-" prefix, the workflow subject naming contract).
func sandboxUsersSince(t *testing.T, admin *kcAdmin, since time.Time) []kcUser {
	t.Helper()
	users, err := admin.listUsers(context.Background())
	if err != nil {
		t.Fatalf("list keycloak users: %v", err)
	}
	cutoff := since.Add(-10 * time.Second).UnixMilli()
	out := make([]kcUser, 0, len(users))
	for _, user := range users {
		if !strings.HasPrefix(strings.ToLower(user.Username), "onboard-") {
			continue
		}
		if user.CreatedTimestamp > 0 && user.CreatedTimestamp < cutoff {
			continue
		}
		out = append(out, user)
	}
	return out
}

func cleanupSandboxUsers(t *testing.T, admin *kcAdmin, exactIDs []string, sinceMS int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	deleted := 0
	for _, id := range exactIDs {
		if id == "" {
			continue
		}
		if err := admin.deleteUser(ctx, id); err != nil {
			t.Logf("teardown: delete keycloak user %s: %v", id, err)
			continue
		}
		deleted++
	}
	// Safety net for runs that failed before their ids could be collected:
	// only controlled sandbox usernames created during this test window.
	users, err := admin.listUsers(ctx)
	if err != nil {
		t.Logf("teardown: list keycloak users: %v", err)
		return
	}
	for _, user := range users {
		if !strings.HasPrefix(strings.ToLower(user.Username), "onboard-") {
			continue
		}
		if sinceMS > 0 && user.CreatedTimestamp > 0 && user.CreatedTimestamp < sinceMS-10_000 {
			continue
		}
		if err := admin.deleteUser(ctx, user.ID); err != nil {
			t.Logf("teardown: delete leftover keycloak user %s: %v", user.Username, err)
			continue
		}
		deleted++
	}
	t.Logf("teardown: deleted %d keycloak sandbox user(s)", deleted)
}

func cleanupMailpit(t *testing.T, mp *mailpitClient, addresses []string) {
	t.Helper()
	if len(addresses) == 0 {
		return
	}
	wanted := map[string]bool{}
	for _, address := range addresses {
		if address != "" {
			wanted[strings.ToLower(address)] = true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	messages, err := mp.messages(ctx)
	if err != nil {
		t.Logf("teardown: list mailpit messages: %v", err)
		return
	}
	ids := make([]string, 0, len(messages))
	for _, msg := range messages {
		for _, recipient := range messageRecipients(msg) {
			if wanted[recipient] {
				ids = append(ids, msg.ID)
				break
			}
		}
	}
	if err := mp.deleteMessages(ctx, ids); err != nil {
		t.Logf("teardown: delete mailpit messages: %v", err)
		return
	}
	t.Logf("teardown: deleted %d mailpit message(s)", len(ids))
}

// ---------------------------------------------------------------------------
// Tiny helpers
// ---------------------------------------------------------------------------

func rawGET(ctx context.Context, rawURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func urlQueryEscape(value string) string {
	replacer := strings.NewReplacer(
		" ", "+", "!", "%21", "\"", "%22", "#", "%23", "$", "%24", "&", "%26",
		"'", "%27", "(", "%28", ")", "%29", "*", "%2A", "+", "%2B", ",", "%2C",
		"/", "%2F", ":", "%3A", ";", "%3B", "=", "%3D", "?", "%3F", "@", "%40",
		"[", "%5B", "]", "%5D",
	)
	return replacer.Replace(value)
}
