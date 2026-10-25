package ops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanCleanArtifacts(t *testing.T) {
	sha := strings.Repeat("8f14e45f", 8) // 64 hex chars: an integrity digest
	clean := strings.Join([]string{
		`{"time":"2026-10-24T13:00:00Z","level":"INFO","msg":"http request","request_id":"req-1","method":"GET","path":"/healthz","status":200,"latency_ms":1}`,
		`{"level":"INFO","msg":"database ready","schema_version":2,"wal":"wal"}`,
		`{"status":"COMPLETED","resume_state":"OAUTH_PENDING","waiting_for":"verification_email"}`,
		`manifest checksum=` + sha,
		`provider=fake-mailbox contract_version=fake-1 attempt=2`,
		`state=OAUTH_PENDING task=completed`,
		`path=/v1/runs/2f9b8c1e-6f5c-4a8f-9d0b-1c2d3e4f5a6b`,
	}, "\n")
	rep, err := ScanReader("clean.log", strings.NewReader(clean), ScanOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("clean sample produced findings: %+v", rep.Findings)
	}
	if rep.Lines != 7 {
		t.Fatalf("scanned lines = %d, want 7", rep.Lines)
	}

	// The same content through ScanFile must agree.
	path := filepath.Join(t.TempDir(), "clean.log")
	if err := os.WriteFile(path, []byte(clean), 0o600); err != nil {
		t.Fatal(err)
	}
	fileRep, err := ScanFile(path, ScanOptions{})
	if err != nil {
		t.Fatalf("scan file: %v", err)
	}
	if !fileRep.Clean() || fileRep.Lines != rep.Lines {
		t.Fatalf("file scan mismatch: %+v", fileRep)
	}
}

func TestScanCleanE2ELogAndEvidenceArtifacts(t *testing.T) {
	// Representative shapes from the real R2 sandbox run artifacts
	// (.sandbox/e2e/run-*/onboardd*.log and evidence*.json): long absolute
	// paths, API paths, log paths and UUIDs must not look like bearer tokens.
	clean := strings.Join([]string{
		`{"time":"2026-10-24T13:49:34Z","level":"INFO","msg":"starting onboardd","config":{"listen":"127.0.0.1:18788","db_path":"/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding/.sandbox/e2e/run-20261024T134934-fe6306/state.sqlite","mode":"sandbox","max_concurrency":2,"cloudflare_enabled":false,"github_enabled":false,"workbuddy_enabled":false,"mailpit_enabled":true,"keycloak_enabled":true,"fake_providers":false,"admin_token_set":true,"master_key_set":true,"allow_public_listen":false,"dev_ephemeral_key":false}}`,
		`{"level":"INFO","msg":"http request","request_id":"req-1760000000000000000-1","method":"GET","path":"/v1/runs/b18bed82-5544-4106-a34d-2be835cbafc6/events","status":200,"latency_ms":1}`,
		`{"level":"INFO","msg":"http request","request_id":"req-1760000000000000000-2","method":"POST","path":"/v1/runs/b18bed82-5544-4106-a34d-2be835cbafc6/cancel","status":202,"latency_ms":2}`,
		`{"level":"INFO","msg":"database ready","schema_version":2,"wal":"wal","path":"/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding/.sandbox/e2e/run-20261024T134934-fe6306/state.sqlite"}`,
		`{"log":"/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding/.sandbox/e2e/run-20261024T135532-79c483/onboardd-1.log","state_db":"/Users/liuzhiwei/Desktop/workspace/git-code/account-onboarding/.sandbox/e2e/run-20261024T135532-79c483/state.sqlite"}`,
		`{"run_id":"b18bed82-5544-4106-a34d-2be835cbafc6","template":"sandbox-email-verify-oidc","status":"COMPLETED","current_step":"verify_credential"}`,
	}, "\n")
	rep, err := ScanReader("e2e.log", strings.NewReader(clean), ScanOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("clean e2e artifact shapes produced false positives: %+v", rep.Findings)
	}
}

func TestScanDirtySamples(t *testing.T) {
	jwtValue := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	longToken := "QWxhZGRpbjpvcGVuIHNlc2FtZQ1234567890abcdefXYZ0123456789"
	known := "SUPER-SECRET-VALUE-42"
	cases := []struct {
		name     string
		line     string
		wantKind string
	}{
		{"jwt", `auth failed for eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U`, "jwt"},
		{"bearer", `header Authorization: Bearer abcdefghijklmnop`, "authorization_header"},
		{"password", `request body password=hunter2 leaked`, "credential_assignment"},
		{"state", `callback state=Zx9Qw2Lm4Pk7 received`, "credential_assignment"},
		{"token", `token=ab12cd34ef56gh78`, "credential_assignment"},
		{"long_token", `leaked="` + longToken + `"`, "long_token"},
		{"known_secret", `verbose log: ` + known, "known_secret"},
		{"private_key", `-----BEGIN RSA PRIVATE KEY-----`, "private_key"},
		{"action_token", `action_token=3f2a9c1e-77b4-4d21-9f38-0a1b2c3d4e5f`, "credential_assignment"},
		{"keycloak_execution", `{"execution":"9a8b7c6d-5e4f-3210-9abc-def012345678"}`, "credential_assignment"},
		{"session_code", `session_code=Ab12Cd34Ef56Gh78Ij90`, "credential_assignment"},
		{"api_key", `x-api-key=ak_live_51H8xYz0123456789abcdef`, "credential_assignment"},
		{"path_field_base64", `{"path":"QWxhZGRpbjpvcGVuIHNlc2FtZQ1234567890abcdefXYZ0123456789"}`, "long_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := ScanReader("dirty.log", strings.NewReader(tc.line+"\n"), ScanOptions{
				KnownSecrets: []string{known},
			})
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if rep.Clean() {
				t.Fatalf("line was not detected: %q", tc.line)
			}
			if rep.FindingsByKind[tc.wantKind] == 0 {
				t.Fatalf("kind %s missing from %+v", tc.wantKind, rep.FindingsByKind)
			}
			for _, f := range rep.Findings {
				for _, raw := range []string{jwtValue, longToken, known, "hunter2", "Zx9Qw2Lm4Pk7", "ab12cd34ef56gh78", "abcdefghijklmnop"} {
					if strings.Contains(f.Detail, raw) {
						t.Fatalf("finding detail leaked a raw value: %+v", f)
					}
				}
			}
		})
	}
}

func TestScanHighConfidenceRulesInsidePaths(t *testing.T) {
	// Path masking must only suppress the heuristic long_token rule; a JWT or a
	// known secret embedded in a URL path is still a leak.
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	known := "KNOWN-SECRET-IN-PATH-42"
	line := `{"path":"/v1/verify/` + jwt + `","referer":"/Users/example/` + known + `/callback"}`
	rep, err := ScanReader("dirty-path.log", strings.NewReader(line+"\n"), ScanOptions{KnownSecrets: []string{known}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep.FindingsByKind["jwt"] == 0 {
		t.Fatalf("JWT inside a path was not detected: %+v", rep.FindingsByKind)
	}
	if rep.FindingsByKind["known_secret"] == 0 {
		t.Fatalf("known secret inside a path was not detected: %+v", rep.FindingsByKind)
	}
}

func TestScanDatabaseFindingsAndAllowlist(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	run := seedRun(t, st, "scan-clean-run")
	seedEvent(t, st, run.ID)

	// Clean persisted artifacts must scan clean before injecting legacy leaks.
	rep, err := ScanDatabase(ctx, st.DBPath, ScanOptions{})
	if err != nil {
		t.Fatalf("scan database: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("clean database produced findings: %+v", rep.Findings)
	}

	db, err := openReadWrite(ctx, st.DBPath)
	if err != nil {
		t.Fatalf("open write db: %v", err)
	}
	defer db.Close()
	now := time.Now().UTC().UnixMilli()
	// Simulate a legacy row written outside the allowlist/sanitizer.
	if _, err := db.ExecContext(ctx, `INSERT INTO events
		(run_id, event_seq, event_type, step_name, public_payload_json, created_at_ms)
		VALUES (?,?,?,?,?,?)`,
		run.ID, 999, "LEGACY_DEBUG", "", `{"leaked":"Bearer abcdefghijklmnop","password":"hunter2"}`, now); err != nil {
		t.Fatalf("insert dirty event: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO audit_log
		(actor, action, run_id, detail_json, created_at_ms) VALUES (?,?,?,?,?)`,
		"legacy", "debug.dump", run.ID, `{"token":"ab12cd34ef56gh78"}`, now); err != nil {
		t.Fatalf("insert dirty audit row: %v", err)
	}

	dirty, err := ScanDatabase(ctx, st.DBPath, ScanOptions{})
	if err != nil {
		t.Fatalf("scan dirty database: %v", err)
	}
	for _, kind := range []string{"event_key_not_allowlisted", "authorization_header", "credential_assignment"} {
		if dirty.FindingsByKind[kind] == 0 {
			t.Fatalf("kind %s missing from dirty scan: %+v", kind, dirty.FindingsByKind)
		}
	}
	requireNoPlaintext(t, mustJSON(t, dirty), "hunter2", "ab12cd34ef56gh78", "abcdefghijklmnop")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return string(raw)
}

// TestScanCleanBuildMetadataShapes covers R6 false positives: Go import paths,
// SCREAMING_SNAKE env names and file-based secret configuration are metadata,
// not credentials; real secrets in the same lines must still be found.
func TestScanCleanBuildMetadataShapes(t *testing.T) {
	clean := strings.Join([]string{
		"ok  github.com/baicie/account-onboarding/internal/provider/mailbox/cloudflare  2.490s",
		"# isolated override requires R51_IMAGE/R51_DATA/R51_MASTER_KEY/R51_ADMIN_TOKEN",
		"ONBOARD_CLOUDFLARE_ADMIN_PASSWORD_FILE=/run/secrets/admin-pass",
		"ONBOARD_CLOUDFLARE_SITE_PASSWORD_FILE=/run/secrets/site-pass",
		"docker inspect --format '{{json .HostConfig.CapDrop}}' app",
		" Container onboarding-r51-r51-1792901234-11967-onboarding-1  Creating",
	}, "\n")
	rep, err := ScanReader("metadata.log", strings.NewReader(clean), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Clean() {
		t.Fatalf("metadata produced findings: %+v", rep.Findings)
	}
	// High-confidence detections must still work on the same shapes.
	dirty := strings.Join([]string{
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345",
		"password=supersecretvalue",
		"github.com/evil/ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}, "\n")
	rep, err = ScanReader("dirty.log", strings.NewReader(dirty), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Clean() {
		t.Fatal("dirty samples were not detected")
	}
}
