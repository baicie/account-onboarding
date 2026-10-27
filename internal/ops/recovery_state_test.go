package ops

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

func recoveryStore(t *testing.T, path string) *storage.Store {
	t.Helper()
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 7)
	}
	s, err := storage.Open(context.Background(), storage.Options{
		Path: path, HMACKey: security.DeriveKey(master, "hmac"), Redactor: security.NewRedactor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func seedRecoveryState(t *testing.T, s *storage.Store) (runs []string, ops map[string]string, auditBefore int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for i := range []string{"COMPLETED", "FAILED_TERMINAL", "WAITING_RETRY"} {
		id := "run-r14-" + string(rune('a'+i))
		_, created, err := s.CreateRun(ctx, storage.CreateRunParams{
			ID: id, OwnerID: "operator", Template: "test-template", Mode: "sandbox-e2e",
			IdempotencyKey: "r14-backup-" + id, ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp",
			DeadlineMS: now + 3600000, Steps: []storage.StepSeed{{Name: "validate"}},
		})
		if err != nil || !created {
			t.Fatalf("run seed: %v %v", err, created)
		}
		runs = append(runs, id)
	}
	db, err := sql.Open("sqlite3", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, status := range []string{"COMPLETED", "FAILED_TERMINAL", "WAITING_RETRY"} {
		if _, err := db.Exec(`UPDATE runs SET status=? WHERE id=?`, status, runs[i]); err != nil {
			t.Fatal(err)
		}
	}
	ops = map[string]string{}
	seedOp := func(name string, state string) {
		run, created, err := s.CreateRun(ctx, storage.CreateRunParams{
			ID: "oprun-" + name, OwnerID: "operator", Template: "test-template", Mode: "sandbox-e2e",
			IdempotencyKey: "r14-op-" + name, ProviderSnapshotJSN: `{}`, RequestFingerprint: "fp",
			DeadlineMS: now + 3600000, Steps: []storage.StepSeed{{Name: "validate"}},
		})
		if err != nil || !created {
			t.Fatalf("op run: %v %v", err, created)
		}
		claim, err := s.ClaimNext(ctx, storage.ClaimRequest{
			Owner: "worker-r14", LeaseTTL: time.Minute,
			RunnableStatuses: []string{"CREATED", "VALIDATING"},
			ActiveState:      func(template, step string) (string, bool) { return "VALIDATING", true },
		})
		if err != nil || claim == nil {
			t.Fatalf("claim: %v %v", err, claim)
		}
		op, err := s.BeginExternalOperation(ctx, storage.BeginExternalOperationParams{
			RunID: run.ID, StepName: claim.StepName, Provider: "fake-mailbox",
			OperationType: "mailbox.create", Fingerprint: "r14-" + name,
			LeaseOwner: claim.Owner, FencingGeneration: claim.Generation,
		})
		if err != nil {
			t.Fatal(err)
		}
		if state != "PREPARED" {
			op, err = s.MarkExternalOperationInFlight(ctx, op.OperationKey, claim.Owner, claim.Generation)
			if err != nil {
				t.Fatal(err)
			}
		}
		if state != "PREPARED" && state != "IN_FLIGHT" {
			op, err = s.CompleteExternalOperation(ctx, storage.CompleteExternalOperationParams{
				OperationKey: op.OperationKey, Status: state, ErrorClass: "R14_FIXTURE",
				LeaseOwner: claim.Owner, FencingGeneration: claim.Generation, OperationVersion: op.Version,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		ops[name] = op.ID
	}
	seedOp("prepared", storage.ExternalPrepared)
	seedOp("inflight", storage.ExternalInFlight)
	seedOp("unknown", storage.ExternalOutcomeUnknown)
	seedOp("recon", storage.ExternalReconciliationReq)
	seedOp("confirmed", storage.ExternalConfirmed)
	seedOp("resolved", storage.ExternalOutcomeUnknown)
	// Resolve the last op through the real operator path.
	op, err := s.GetExternalOperationByID(ctx, ops["resolved"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveExternalOperation(ctx, op.ID, storage.ExternalResolution{
		Resolution: "RESOURCE_CONFIRMED", Reason: "r14 backup fixture", EvidenceReference: "ticket-r14",
		ExpectedVersion: op.Version, Actor: "test:operator", RequestedAtMS: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit(ctx, "test:operator", "r14.fixture", runs[0], map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueAlertOutbox(ctx, []storage.AlertOutboxInput{{
		Fingerprint: "r14-backup-alert", Rule: "readyz_failing", Severity: "critical", Message: "restore fixture",
	}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, "session-r14", time.Now().UTC(), time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Create the expired revocation before backup so restore proves it is
	// preserved as expired (not just created afterwards).
	if err := s.RevokeSession(ctx, "session-expired-r14", time.Now().Add(-2*time.Hour).UTC(), time.Now().Add(-time.Hour).UTC()); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return runs, ops, n
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestEncryptedBackupRestorePreservesDurableState(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "source.sqlite")
	s := recoveryStore(t, dbPath)
	runs, ops, auditBefore := seedRecoveryState(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 7)
	}
	ring, err := security.NewKeyRing(master)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Backup(context.Background(), BackupOptions{
		DBPath: dbPath, OutDir: filepath.Join(dir, "backup"), Encrypt: true, Ring: ring,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backup.Manifest.Encryption != "aes-256-gcm" || backup.Manifest.SHA256 == "" {
		t.Fatalf("manifest not encrypted/checksummed: %+v", backup.Manifest)
	}
	keyPath := filepath.Join(dir, "master.key")
	if err := os.WriteFile(keyPath, master, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "restored.sqlite")
	res, err := Restore(context.Background(), RestoreOptions{
		SnapshotPath: backup.SnapshotPath, TargetPath: target, KeyRingPath: keyPath, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.ManifestVerified || res.SchemaVersion != 6 {
		t.Fatalf("restore result = %+v", res)
	}
	restored := recoveryStore(t, target)
	defer restored.Close()
	ctx := context.Background()
	for i, runID := range runs {
		run, err := restored.GetRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"COMPLETED", "FAILED_TERMINAL", "WAITING_RETRY"}[i]
		if run.Status != want {
			t.Fatalf("run %s status = %s want %s", runID, run.Status, want)
		}
	}
	revoked, err := restored.SessionRevoked(ctx, "session-r14", time.Now().UTC())
	if err != nil || !revoked {
		t.Fatalf("session revocation not restored: %v %v", revoked, err)
	}
	// The expired revocation was present in the backup and must remain expired.
	if expired, err := restored.SessionRevoked(ctx, "session-expired-r14", time.Now().UTC()); err != nil || expired {
		t.Fatalf("restored expired session revocation still active: %v %v", expired, err)
	}
	wantStates := map[string]string{
		"prepared": storage.ExternalPrepared, "inflight": storage.ExternalInFlight,
		"unknown": storage.ExternalOutcomeUnknown, "recon": storage.ExternalReconciliationReq,
		"confirmed": storage.ExternalConfirmed, "resolved": storage.ExternalResolved,
	}
	for name, want := range wantStates {
		op, err := restored.GetExternalOperationByID(ctx, ops[name])
		if err != nil {
			t.Fatal(err)
		}
		if op.Status != want {
			t.Fatalf("%s status = %s want %s", name, op.Status, want)
		}
	}
	var auditAfter int64
	db, err := sql.Open("sqlite3", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&auditAfter); err != nil {
		t.Fatal(err)
	}
	if auditAfter != auditBefore {
		t.Fatalf("audit rows changed: %d -> %d", auditBefore, auditAfter)
	}
	outbox, err := restored.ListAlertOutbox(ctx, 10)
	if err != nil || len(outbox) != 1 || outbox[0].Status != storage.AlertOutboxPending {
		t.Fatalf("durable alert outbox not restored: %+v err=%v", outbox, err)
	}
	// No automatic retry: the unresolved unknown op remains pending with its version.
	op, err := restored.GetExternalOperationByID(ctx, ops["unknown"])
	if err != nil || op.Status != storage.ExternalOutcomeUnknown || op.Resolution != "" {
		t.Fatalf("unknown op changed after restore: %+v err=%v", op, err)
	}
}

func TestWrongBackupKeyDoesNotOverwriteTarget(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "source.sqlite")
	s := recoveryStore(t, dbPath)
	seedRecoveryState(t, s)
	_ = s.Close()
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 1)
	}
	ring, _ := security.NewKeyRing(master)
	backup, err := Backup(context.Background(), BackupOptions{
		DBPath: dbPath, OutDir: filepath.Join(dir, "backup"), Encrypt: true, Ring: ring,
	})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "valid.sqlite")
	valid := recoveryStore(t, target)
	_ = valid.Close()
	before := fileSHA(t, target)
	wrongKey := filepath.Join(dir, "wrong.key")
	if err := os.WriteFile(wrongKey, bytes32(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), RestoreOptions{
		SnapshotPath: backup.SnapshotPath, TargetPath: target, KeyRingPath: wrongKey, Force: true,
	}); err == nil {
		t.Fatal("restore with wrong key succeeded")
	}
	if after := fileSHA(t, target); after != before {
		t.Fatal("wrong-key restore modified target")
	}
}

func TestCorruptSnapshotLeavesTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "source.sqlite")
	s := recoveryStore(t, dbPath)
	seedRecoveryState(t, s)
	_ = s.Close()
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 2)
	}
	ring, _ := security.NewKeyRing(master)
	backup, err := Backup(context.Background(), BackupOptions{
		DBPath: dbPath, OutDir: filepath.Join(dir, "backup"), Encrypt: true, Ring: ring,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(backup.SnapshotPath)
	b[len(b)/2] ^= 0xff
	if err := os.WriteFile(backup.SnapshotPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "master.key")
	_ = os.WriteFile(keyPath, master, 0o600)
	target := filepath.Join(dir, "target.sqlite")
	valid := recoveryStore(t, target)
	_ = valid.Close()
	before := fileSHA(t, target)
	if _, err := Restore(context.Background(), RestoreOptions{
		SnapshotPath: backup.SnapshotPath, TargetPath: target, KeyRingPath: keyPath, Force: true,
	}); err == nil {
		t.Fatal("corrupt snapshot restored")
	}
	if after := fileSHA(t, target); after != before {
		t.Fatal("failed restore polluted target")
	}
}

func bytes32() []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = 0x5a
	}
	return b
}

func TestSchema5BackupRestoresAndMigratesTo6(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	path := filepath.Join(dir, "schema5.sqlite")
	s, err := storage.Open(ctx, storage.Options{Path: path, HMACKey: []byte("schema5-restore-hmac")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueAlertOutbox(ctx, []storage.AlertOutboxInput{{Fingerprint: "legacy", Rule: "legacy", Severity: "warning", Message: "m"}}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	// Reconstruct a schema-5 fixture: drop migration-6 tables/version marker.
	raw, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE alert_deliveries; DROP TABLE alert_outbox; DELETE FROM schema_migrations WHERE version=6`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	backup, err := Backup(ctx, BackupOptions{DBPath: path, OutDir: dir, Now: func() time.Time { return time.Unix(1700000000, 0) }})
	if err != nil {
		t.Fatalf("schema5 backup: %v", err)
	}
	target := filepath.Join(dir, "restored.sqlite")
	res, err := Restore(ctx, RestoreOptions{SnapshotPath: backup.SnapshotPath, ManifestPath: backup.ManifestPath, TargetPath: target, Force: true})
	if err != nil || !res.ManifestVerified {
		t.Fatalf("restore schema5: %+v err=%v", res, err)
	}
	restored, err := storage.Open(ctx, storage.Options{Path: target, HMACKey: []byte("schema5-restore-hmac")})
	if err != nil {
		t.Fatalf("open migrated restore: %v", err)
	}
	defer restored.Close()
	h, err := restored.Health(ctx)
	if err != nil || h.SchemaVersion != 6 || !h.ExternalOperationsReady {
		t.Fatalf("migrated health = %+v err=%v", h, err)
	}
	outbox, err := restored.ListAlertOutbox(ctx, 10)
	if err != nil || len(outbox) != 0 {
		t.Fatalf("migration should start empty outbox (schema5 had none): %+v err=%v", outbox, err)
	}
}
