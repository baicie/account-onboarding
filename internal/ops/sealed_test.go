package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baicie/account-onboarding/internal/secrets"
	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

func TestEncryptedBackupVerifyRestoreRoundTrip(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	run := seedRun(t, st, "sealed-run-one")
	seedEvent(t, st, run.ID)
	plain := map[string]string{}
	for i := 0; i < 2; i++ {
		text := "sealed-secret-" + string(rune('a'+i))
		ref := seedSecret(t, st, "sandbox_password", text)
		plain[ref] = text
	}
	keyFile := writeRawKeyFile(t, st.MasterKey)
	outDir := t.TempDir()
	res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: outDir, Encrypt: true, KeyRingPath: keyFile})
	if err != nil {
		t.Fatalf("encrypted backup: %v", err)
	}
	if !strings.HasSuffix(res.SnapshotPath, ".sqlite.enc") {
		t.Fatalf("encrypted snapshot name = %q, want *.sqlite.enc", res.SnapshotPath)
	}
	m := res.Manifest
	if m.Encryption != sealedCipher || m.KeyVersion != 1 {
		t.Fatalf("manifest encryption fields = %+v", m)
	}
	if len(m.PlaintextSHA256) != 64 || m.PlaintextSize <= 0 {
		t.Fatalf("manifest plaintext fields = %+v", m)
	}
	if m.SHA256 == m.PlaintextSHA256 {
		t.Fatal("stored ciphertext checksum equals plaintext checksum")
	}
	stat, err := os.Stat(res.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0o600 || stat.Size() != m.SizeBytes {
		t.Fatalf("sealed snapshot mode=%o size=%d manifest=%d", stat.Mode().Perm(), stat.Size(), m.SizeBytes)
	}
	// The plaintext staging file must not remain next to the encrypted artifact.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("output dir has %d entries, want 2 (one .enc + one manifest)", len(entries))
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".enc") && !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("unexpected plaintext artifact left behind: %s", e.Name())
		}
	}
	header, sealed, err := detectSealed(res.SnapshotPath)
	if err != nil || !sealed {
		t.Fatalf("detect sealed: sealed=%v err=%v", sealed, err)
	}
	if header.PlaintextSHA256 != m.PlaintextSHA256 || header.KeyVersion != m.KeyVersion || header.PlaintextSize != m.PlaintextSize {
		t.Fatalf("sealed header mismatch: %+v vs %+v", header, m)
	}

	ring, err := security.LoadKeyRing(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ver, err := VerifyBackupWithKeyring(ctx, res.SnapshotPath, "", ring)
	if err != nil {
		t.Fatalf("verify encrypted backup: %v", err)
	}
	if !ver.Encrypted || ver.KeyVersion != 1 || ver.Snapshot.Encryption != sealedCipher {
		t.Fatalf("verification encryption fields = %+v", ver)
	}
	if ver.Snapshot.IntegrityCheck != "ok" || !ver.Snapshot.Counts.Equal(m.Counts) {
		t.Fatalf("verified snapshot = %+v", ver.Snapshot)
	}
	joined := strings.Join(ver.Checks, ",")
	for _, want := range []string{"encryption=" + sealedCipher, "key_version=1", "plaintext_sha256", "integrity_check=ok"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("checks %q missing %q", joined, want)
		}
	}

	// Missing keyring must fail closed at verification.
	if _, err := VerifyBackup(ctx, res.SnapshotPath, ""); err == nil || !strings.Contains(err.Error(), "requires a keyring") {
		t.Fatalf("verify without keyring error = %v", err)
	}

	// Restore: decrypts, verifies and reopens as a plaintext SQLite database.
	target := filepath.Join(t.TempDir(), "restored.sqlite")
	rr, err := Restore(ctx, RestoreOptions{
		SnapshotPath: res.SnapshotPath,
		TargetPath:   target,
		LiveDBPath:   st.DBPath,
		KeyRingPath:  keyFile,
	})
	if err != nil {
		t.Fatalf("restore encrypted: %v", err)
	}
	if !rr.Encrypted || rr.KeyVersion != 1 || rr.PlaintextSHA256 != m.PlaintextSHA256 {
		t.Fatalf("restore result = %+v", rr)
	}
	if rr.IntegrityCheck != "ok" || !rr.Counts.Equal(m.Counts) || rr.SecretsVerified != len(plain) {
		t.Fatalf("restore proofs = %+v", rr)
	}
	if header, sealed, err := detectSealed(target); err != nil || sealed {
		t.Fatalf("restored target should be plaintext SQLite (header=%+v sealed=%v err=%v)", header, sealed, err)
	}
	store, err := storage.Open(ctx, storage.Options{
		Path:     target,
		HMACKey:  security.DeriveKey(st.MasterKey, "storage-hmac"),
		Redactor: security.NewRedactor(),
	})
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer store.Close()
	if _, err := store.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("restored run missing: %v", err)
	}
	svc := secrets.New(store, security.NewBox(ring))
	for ref, want := range plain {
		got, err := svc.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("resolve restored secret: %v", err)
		}
		if got != want {
			t.Fatal("restored secret plaintext mismatch")
		}
	}
}

func TestEncryptedBackupWrongKeyFailClosed(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	run := seedRun(t, st, "sealed-wrong-key")
	seedEvent(t, st, run.ID)
	seedSecret(t, st, "sandbox_password", "sealed-wrong-key-secret")
	res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: t.TempDir(), Encrypt: true, Ring: st.KeyRing})
	if err != nil {
		t.Fatalf("encrypted backup: %v", err)
	}
	wrong := writeRawKeyFile(t, bytes.Repeat([]byte{0x77}, 32))

	if _, err := VerifyBackupWithKeyring(ctx, res.SnapshotPath, "", mustRing(t, wrong)); err == nil {
		t.Fatal("verify with the wrong keyring must fail")
	}
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "restored.sqlite")
	_, err = Restore(ctx, RestoreOptions{
		SnapshotPath: res.SnapshotPath,
		TargetPath:   target,
		KeyRingPath:  wrong,
	})
	if err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("wrong-key restore error = %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("target must not exist after a failed encrypted restore")
	}
	if n := countFiles(t, targetDir); n != 0 {
		t.Fatalf("target dir has %d leftover files after failed restore", n)
	}
	// The encrypted artifact and live database are untouched.
	if _, err := VerifyBackupWithKeyring(ctx, res.SnapshotPath, "", st.KeyRing); err != nil {
		t.Fatalf("snapshot damaged by failed restore: %v", err)
	}
	if _, err := st.Store.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("live db damaged: %v", err)
	}
}

func TestEncryptedBackupMultiVersionKeyring(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	seedRun(t, st, "sealed-multiversion")
	seedSecret(t, st, "sandbox_password", "sealed-multiversion-secret")
	key1 := append([]byte(nil), st.MasterKey...)
	key2 := bytes.Repeat([]byte{0x22}, 32)
	ring2, err := security.NewKeyRingWithVersions(key2, map[int][]byte{1: key1, 2: key2})
	if err != nil {
		t.Fatal(err)
	}
	keyringPath := writeKeyringEnvelope(t, 2, map[int][]byte{1: key1, 2: key2})
	res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: t.TempDir(), Encrypt: true, KeyRingPath: keyringPath})
	if err != nil {
		t.Fatalf("backup with v2 ring: %v", err)
	}
	if res.Manifest.KeyVersion != 2 {
		t.Fatalf("manifest key version = %d, want 2", res.Manifest.KeyVersion)
	}
	if _, err := VerifyBackupWithKeyring(ctx, res.SnapshotPath, "", ring2); err != nil {
		t.Fatalf("verify with v1+v2 ring: %v", err)
	}
	// A ring with only v1 cannot decrypt a v2 snapshot: fail closed.
	ring1, err := security.NewKeyRing(key1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackupWithKeyring(ctx, res.SnapshotPath, "", ring1); err == nil {
		t.Fatal("verify with a ring missing key version 2 must fail")
	}
	target := filepath.Join(t.TempDir(), "restored.sqlite")
	if _, err := Restore(ctx, RestoreOptions{SnapshotPath: res.SnapshotPath, TargetPath: target, KeyRingPath: writeRawKeyFile(t, key1)}); err == nil {
		t.Fatal("restore with a ring missing key version 2 must fail")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("target must not exist after missing-version restore")
	}
}

func TestEncryptedBackupTamperDetection(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	seedRun(t, st, "sealed-tamper")
	seedSecret(t, st, "sandbox_password", "sealed-tamper-secret")
	res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: t.TempDir(), Encrypt: true, Ring: st.KeyRing})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	dir := t.TempDir()
	tampered := filepath.Join(dir, filepath.Base(res.SnapshotPath))
	data, err := os.ReadFile(res.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the ciphertext (after the header) and keep the manifest.
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(tampered, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	tamperedManifest := ManifestPathFor(tampered)
	if err := os.WriteFile(tamperedManifest, manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackupWithKeyring(ctx, tampered, tamperedManifest, st.KeyRing); err == nil {
		t.Fatal("tampered ciphertext must fail verification")
	}
	target := filepath.Join(t.TempDir(), "restored.sqlite")
	if _, err := Restore(ctx, RestoreOptions{SnapshotPath: tampered, ManifestPath: tamperedManifest, TargetPath: target, KeyRingPath: writeRawKeyFile(t, st.MasterKey)}); err == nil {
		t.Fatal("tampered ciphertext must fail restore")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("target must not exist after tampered restore")
	}

	// A manifest that claims a different key version is rejected even if the
	// ciphertext is intact.
	var m Manifest
	if err := json.Unmarshal(manifestRaw, &m); err != nil {
		t.Fatal(err)
	}
	m.KeyVersion = m.KeyVersion + 1
	badManifest, _ := json.Marshal(m)
	badPath := filepath.Join(dir, "bad-version.json")
	if err := os.WriteFile(badPath, badManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackupWithKeyring(ctx, res.SnapshotPath, badPath, st.KeyRing); err == nil {
		t.Fatal("manifest key-version mismatch must fail verification")
	}
}

func TestBackupEncryptRequiresKeyring(t *testing.T) {
	st := newTestStack(t)
	_, err := Backup(context.Background(), BackupOptions{DBPath: st.DBPath, OutDir: t.TempDir(), Encrypt: true})
	if err == nil || !strings.Contains(err.Error(), "requires a keyring") {
		t.Fatalf("encrypt without keyring error = %v", err)
	}
}

func mustRing(t *testing.T, path string) *security.KeyRing {
	t.Helper()
	ring, err := security.LoadKeyRing(path)
	if err != nil {
		t.Fatalf("load keyring: %v", err)
	}
	return ring
}
