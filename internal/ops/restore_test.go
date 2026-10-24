package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

type restoreFixture struct {
	stack     *Stack
	backup    BackupResult
	keyFile   string
	runID     string
	secrets   map[string]string // ref -> plaintext
	secretCnt int
}

func newRestoreFixture(t *testing.T) restoreFixture {
	t.Helper()
	st := newTestStack(t)
	ctx := context.Background()
	var runID string
	for i, idem := range []string{"restore-run-one", "restore-run-two"} {
		run := seedRun(t, st, idem)
		if i == 0 {
			runID = run.ID
		}
		seedEvent(t, st, run.ID)
	}
	plain := map[string]string{}
	for i := 0; i < 3; i++ {
		text := "restore-secret-" + string(rune('a'+i))
		ref := seedSecret(t, st, "sandbox_password", text)
		plain[ref] = text
	}
	res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return restoreFixture{
		stack: st, backup: res, keyFile: writeRawKeyFile(t, st.MasterKey),
		runID: runID, secrets: plain, secretCnt: len(plain),
	}
}

func TestRestoreRoundTripProofs(t *testing.T) {
	fx := newRestoreFixture(t)
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "restored.sqlite")
	res, err := Restore(ctx, RestoreOptions{
		SnapshotPath: fx.backup.SnapshotPath,
		TargetPath:   target,
		LiveDBPath:   fx.stack.DBPath,
		KeyRingPath:  fx.keyFile,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !res.ManifestVerified {
		t.Fatal("manifest was not verified")
	}
	if res.IntegrityCheck != "ok" || res.ForeignKeyViolations != 0 {
		t.Fatalf("integrity=%q fk=%d", res.IntegrityCheck, res.ForeignKeyViolations)
	}
	if !res.Counts.Equal(fx.backup.Manifest.Counts) {
		t.Fatalf("restored census %+v != manifest %+v", res.Counts, fx.backup.Manifest.Counts)
	}
	if res.SecretsVerified != fx.secretCnt {
		t.Fatalf("decrypted %d secrets, want %d", res.SecretsVerified, fx.secretCnt)
	}
	if res.SecretsByVersion[1] != fx.secretCnt {
		t.Fatalf("secret versions = %+v", res.SecretsByVersion)
	}
	if res.KeyringCurrentVersion != 1 {
		t.Fatalf("keyring current = %d, want 1", res.KeyringCurrentVersion)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat restored: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("restored mode = %o, want 0600", st.Mode().Perm())
	}

	// Reopen the restored database with the real storage layer and resolve every
	// secret with the supplied keyring: encrypted data survived the round trip.
	store, err := storage.Open(ctx, storage.Options{
		Path:     target,
		HMACKey:  security.DeriveKey(fx.stack.MasterKey, "storage-hmac"),
		Redactor: security.NewRedactor(),
	})
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer store.Close()
	if _, err := store.GetRun(ctx, fx.runID); err != nil {
		t.Fatalf("restored run missing: %v", err)
	}
	secretsSvc := secrets.New(store, security.NewBox(fx.stack.KeyRing))
	for ref, want := range fx.secrets {
		got, err := secretsSvc.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("resolve restored secret: %v", err)
		}
		if got != want {
			t.Fatalf("restored secret plaintext mismatch")
		}
	}
	if n, err := store.CountSecrets(ctx); err != nil || n != int64(fx.secretCnt) {
		t.Fatalf("restored secret count = %d, err=%v", n, err)
	}
}

func TestRestoreWrongKeyFailsClosedAndLeavesTargetUntouched(t *testing.T) {
	fx := newRestoreFixture(t)
	ctx := context.Background()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "restored.sqlite")
	wrongKey := writeRawKeyFile(t, bytes.Repeat([]byte{0x11}, 32))

	_, err := Restore(ctx, RestoreOptions{
		SnapshotPath: fx.backup.SnapshotPath,
		TargetPath:   target,
		KeyRingPath:  wrongKey,
	})
	if err == nil {
		t.Fatal("restore with the wrong keyring must fail")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("wrong-key error = %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target must not exist after a failed restore (stat err=%v)", statErr)
	}
	if n := countFiles(t, targetDir); n != 0 {
		t.Fatalf("target dir has %d leftovers after failed restore", n)
	}

	// The source snapshot is untouched and the live database still works.
	if _, err := VerifyBackup(ctx, fx.backup.SnapshotPath, ""); err != nil {
		t.Fatalf("snapshot damaged by failed restore: %v", err)
	}
	if _, err := fx.stack.Store.GetRun(ctx, fx.runID); err != nil {
		t.Fatalf("live db damaged by failed restore: %v", err)
	}
}

func TestRestoreRequiresKeyringForEncryptedSecrets(t *testing.T) {
	fx := newRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restored.sqlite")
	_, err := Restore(context.Background(), RestoreOptions{
		SnapshotPath: fx.backup.SnapshotPath,
		TargetPath:   target,
	})
	if err == nil || !strings.Contains(err.Error(), "keyring is required") {
		t.Fatalf("missing keyring error = %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("target must not be created when the keyring is missing")
	}
}

func TestRestoreRefusesOverwriteAndLiveDatabaseWithoutForce(t *testing.T) {
	fx := newRestoreFixture(t)
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "restored.sqlite")
	opts := RestoreOptions{
		SnapshotPath: fx.backup.SnapshotPath,
		TargetPath:   target,
		KeyRingPath:  fx.keyFile,
	}
	if _, err := Restore(ctx, opts); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	if _, err := Restore(ctx, opts); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("overwrite refusal error = %v", err)
	}

	// A live database path is refused with an explicit message unless forced.
	liveOpts := opts
	liveOpts.TargetPath = fx.stack.DBPath
	liveOpts.LiveDBPath = fx.stack.DBPath
	if _, err := Restore(ctx, liveOpts); err == nil || !strings.Contains(err.Error(), "live service database") {
		t.Fatalf("live db refusal error = %v", err)
	}
	if _, err := fx.stack.Store.GetRun(ctx, fx.runID); err != nil {
		t.Fatalf("live db changed by refused restore: %v", err)
	}

	// Forcing an overwrite is allowed and keeps the old copy when requested.
	forced := opts
	forced.Force = true
	forced.KeepOld = true
	res, err := Restore(ctx, forced)
	if err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	if res.PreRestoreCopy == "" {
		t.Fatal("KeepOld did not report the pre-restore copy")
	}
	if _, err := os.Stat(res.PreRestoreCopy); err != nil {
		t.Fatalf("pre-restore copy missing: %v", err)
	}
}

func TestRestoreRejectsMissingManifestAndCorruptSnapshot(t *testing.T) {
	fx := newRestoreFixture(t)
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "restored.sqlite")
	if _, err := Restore(ctx, RestoreOptions{
		SnapshotPath: fx.backup.SnapshotPath,
		ManifestPath: filepath.Join(t.TempDir(), "absent.json"),
		TargetPath:   target,
		KeyRingPath:  fx.keyFile,
	}); err == nil {
		t.Fatal("missing manifest must fail the restore")
	}

	corruptDir := t.TempDir()
	corruptPath := filepath.Join(corruptDir, "corrupt.sqlite")
	data, err := os.ReadFile(fx.backup.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 'X'
	if err := os.WriteFile(corruptPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(fx.backup.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	corruptManifest := ManifestPathFor(corruptPath)
	if err := os.WriteFile(corruptManifest, manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	target2 := filepath.Join(t.TempDir(), "restored2.sqlite")
	if _, err := Restore(ctx, RestoreOptions{
		SnapshotPath: corruptPath,
		ManifestPath: corruptManifest,
		TargetPath:   target2,
		KeyRingPath:  fx.keyFile,
	}); err == nil {
		t.Fatal("corrupt snapshot must fail the restore")
	}
	if _, statErr := os.Stat(target2); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("target must not exist after a refused restore")
	}
}
