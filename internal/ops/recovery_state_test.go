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
	if err := s.RevokeSession(ctx, "session-r14", time.Now().UTC(), time.Now().UTC().Add(time.Hour)); err != nil {
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
	if !res.ManifestVerified || res.SchemaVersion != 5 {
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
	// An expired revocation must no longer block future sessions.
	if err := restored.RevokeSession(ctx, "session-expired-r14", time.Now().Add(-2*time.Hour).UTC(), time.Now().Add(-time.Hour).UTC()); err != nil {
		t.Fatal(err)
	}
	if expired, err := restored.SessionRevoked(ctx, "session-expired-r14", time.Now().UTC()); err != nil || expired {
		t.Fatalf("expired session revocation still active: %v %v", expired, err)
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
