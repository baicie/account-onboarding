package ops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
	"github.com/baicie/account-onboarding/internal/workflow"
)

// testMasterKey is deterministic so a test can reopen a restored database.
var testMasterKey = bytes.Repeat([]byte{0x5a}, 32)

func newTestStack(t *testing.T) *Stack {
	t.Helper()
	return newTestStackWith(t, nil)
}

func newTestStackWith(t *testing.T, mutate func(*StackOptions)) *Stack {
	t.Helper()
	opts := StackOptions{
		Dir:            t.TempDir(),
		MasterKey:      append([]byte(nil), testMasterKey...),
		AdminToken:     "test-admin-token-0123456789",
		MaxConcurrency: 2,
		TickInterval:   5 * time.Millisecond,
		PollInterval:   10 * time.Millisecond,
		LeaseTTL:       30 * time.Second,
		StepTimeout:    5 * time.Second,
		MaxAttempts:    3,
		RunDeadline:    2 * time.Minute,
	}
	if mutate != nil {
		mutate(&opts)
	}
	st, err := NewStack(opts)
	if err != nil {
		t.Fatalf("new stack: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func sandboxRunRequest() workflow.CreateRunRequest {
	return workflow.CreateRunRequest{
		Template:         "sandbox-email-verify-oidc",
		Mode:             "sandbox-e2e",
		MailboxProvider:  "fake-mailbox",
		IdentityProvider: "fake-identity",
		OAuthProvider:    "fake-oauth",
		SandboxProvider:  "fake-sandbox",
	}
}

// seedRun persists a run directly through the engine/storage layer.
func seedRun(t *testing.T, st *Stack, idem string) storage.Run {
	t.Helper()
	params, err := st.Engine.PrepareRun(sandboxRunRequest(), "operator", time.Now().UTC())
	if err != nil {
		t.Fatalf("prepare run: %v", err)
	}
	params.IdempotencyKey = idem
	run, created, err := st.Store.CreateRun(context.Background(), params)
	if err != nil || !created {
		t.Fatalf("create run: err=%v created=%v", err, created)
	}
	return run
}

// seedSecret stores an encrypted secret via the real secret service.
func seedSecret(t *testing.T, st *Stack, kind, plaintext string) string {
	t.Helper()
	ref, err := st.Secrets.PutNew(context.Background(), kind, plaintext)
	if err != nil {
		t.Fatalf("put secret: %v", err)
	}
	return ref
}

func seedEvent(t *testing.T, st *Stack, runID string) {
	t.Helper()
	if err := st.Store.AppendEvent(context.Background(), runID, "STEP_SUCCEEDED", "validate", map[string]any{
		"provider": "fake-mailbox", "attempt": int64(1),
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}
}

// writeRawKeyFile writes a 32-byte key file (accepted by security.LoadKeyRing).
func writeRawKeyFile(t *testing.T, key []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

// writeKeyringEnvelope writes the multi-version JSON keyring format.
func writeKeyringEnvelope(t *testing.T, current int, keys map[int][]byte) string {
	t.Helper()
	type envelope struct {
		Current int               `json:"current"`
		Keys    map[string]string `json:"keys"`
	}
	env := envelope{Current: current, Keys: map[string]string{}}
	for v, key := range keys {
		env.Keys[fmt.Sprintf("%d", v)] = base64.StdEncoding.EncodeToString(key)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal keyring: %v", err)
	}
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write keyring: %v", err)
	}
	return path
}

// readSecretRowsForTest returns the raw encrypted rows (tests only).
func readSecretRowsForTest(t *testing.T, dbPath string) []secretRow {
	t.Helper()
	rows, err := readSecretRows(context.Background(), dbPath, true, nil)
	if err != nil {
		t.Fatalf("read secret rows: %v", err)
	}
	return rows
}

func requireNoPlaintext(t *testing.T, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if len(secret) >= 4 && bytes.Contains([]byte(text), []byte(secret)) {
			t.Fatalf("report leaked a raw secret value")
		}
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	return len(entries)
}

var _ = security.RandomToken
