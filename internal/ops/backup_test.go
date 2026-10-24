package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBackupSnapshotWhileWALActive(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()

	health, err := st.Store.Health(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !strings.EqualFold(health.WALMode, "wal") {
		t.Fatalf("source is not in WAL mode: %q", health.WALMode)
	}

	for i := 0; i < 4; i++ {
		run := seedRun(t, st, "wal-seed-"+string(rune('a'+i)))
		seedEvent(t, st, run.ID)
	}
	for i := 0; i < 2; i++ {
		seedSecret(t, st, "sandbox_password", "backup-secret-value")
	}
	initialRuns, err := st.Store.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	initialTotal := 0
	for _, n := range initialRuns {
		initialTotal += n
	}

	outDir := t.TempDir()
	// Keep writing while the snapshot is being taken: VACUUM INTO must see a
	// consistent WAL snapshot, not a torn file copy.
	var (
		wg      sync.WaitGroup
		stop    = make(chan struct{})
		writeEr error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			params, err := st.Engine.PrepareRun(sandboxRunRequest(), "operator", time.Now().UTC())
			if err != nil {
				writeEr = err
				return
			}
			params.IdempotencyKey = "wal-live-" + time.Now().Format("150405.000000") + string(rune('a'+i%26))
			if _, _, err := st.Store.CreateRun(ctx, params); err != nil {
				writeEr = err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: outDir})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if writeEr != nil {
		t.Fatalf("concurrent writer failed: %v", writeEr)
	}

	// Snapshot and manifest must be private, complete and side-by-side.
	stat, err := os.Stat(res.SnapshotPath)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if stat.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %o, want 0600", stat.Mode().Perm())
	}
	mstat, err := os.Stat(res.ManifestPath)
	if err != nil {
		t.Fatalf("stat manifest: %v", err)
	}
	if mstat.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %o, want 0600", mstat.Mode().Perm())
	}
	if n := countFiles(t, outDir); n != 2 {
		t.Fatalf("out dir has %d files, want 2 (snapshot + manifest): no temp leftovers", n)
	}

	m := res.Manifest
	if m.Tool != ToolName || m.FormatVersion != ManifestFormatVersion {
		t.Fatalf("manifest metadata = %+v", m)
	}
	if m.SourceJournalMode != "wal" {
		t.Fatalf("manifest source journal mode = %q, want wal", m.SourceJournalMode)
	}
	if m.SchemaVersion != health.SchemaVersion {
		t.Fatalf("manifest schema version = %d, want %d", m.SchemaVersion, health.SchemaVersion)
	}
	if m.Counts.Runs < int64(initialTotal) {
		t.Fatalf("snapshot saw %d runs, fewer than the %d written before backup", m.Counts.Runs, initialTotal)
	}
	if m.Counts.Secrets != 2 || m.Counts.Events == 0 {
		t.Fatalf("unexpected census: %+v", m.Counts)
	}
	if len(m.SHA256) != 64 {
		t.Fatalf("manifest sha256 = %q", m.SHA256)
	}

	ver, err := VerifyBackup(ctx, res.SnapshotPath, "")
	if err != nil {
		t.Fatalf("verify backup: %v", err)
	}
	if !ver.Snapshot.Counts.Equal(m.Counts) {
		t.Fatalf("verify census mismatch: %+v vs %+v", ver.Snapshot.Counts, m.Counts)
	}
	joined := strings.Join(ver.Checks, ",")
	for _, want := range []string{"sha256", "integrity_check=ok", "counts", "schema_version="} {
		if !strings.Contains(joined, want) {
			t.Fatalf("verify checks %q missing %q", joined, want)
		}
	}
}

func TestBackupRejectsInvalidSources(t *testing.T) {
	ctx := context.Background()
	if _, err := Backup(ctx, BackupOptions{}); err == nil {
		t.Fatal("empty DBPath must fail")
	}
	missing := filepath.Join(t.TempDir(), "nope.sqlite")
	if _, err := Backup(ctx, BackupOptions{DBPath: missing}); err == nil {
		t.Fatal("missing source must fail")
	}
	garbage := filepath.Join(t.TempDir(), "garbage.sqlite")
	if err := os.WriteFile(garbage, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(ctx, BackupOptions{DBPath: garbage, OutDir: t.TempDir()}); err == nil {
		t.Fatal("non-sqlite source must fail")
	}
}

func TestVerifyBackupDetectsTamperingAndMissingManifest(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	run := seedRun(t, st, "tamper-run")
	seedEvent(t, st, run.ID)
	seedSecret(t, st, "sandbox_password", "tamper-check")

	res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: t.TempDir()})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := VerifyBackup(ctx, res.SnapshotPath, ""); err != nil {
		t.Fatalf("clean verify: %v", err)
	}

	// 1. Missing manifest.
	if _, err := VerifyBackup(ctx, res.SnapshotPath, filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("missing manifest must fail verification")
	}

	// 2. Manifest checksum mismatch.
	raw, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), res.Manifest.SHA256, strings.Repeat("0", 64), 1)
	tamperManifest := filepath.Join(t.TempDir(), filepath.Base(res.ManifestPath))
	if err := os.WriteFile(tamperManifest, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(ctx, res.SnapshotPath, tamperManifest); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("checksum mismatch error = %v", err)
	}

	// 3. Snapshot byte corruption with an internally consistent manifest.
	corruptDir := t.TempDir()
	corruptPath := filepath.Join(corruptDir, filepath.Base(res.SnapshotPath))
	data, err := os.ReadFile(res.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(corruptPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	corruptManifest := ManifestPathFor(corruptPath)
	if err := os.WriteFile(corruptManifest, manifestRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(ctx, corruptPath, corruptManifest); err == nil {
		t.Fatal("corrupt snapshot must fail verification")
	}
}

// TestInspectStorageHelperBackupInterop proves the ops verifier accepts a
// snapshot produced by the in-process storage helper (same VACUUM INTO contract).
func TestInspectStorageHelperBackupInterop(t *testing.T) {
	st := newTestStack(t)
	ctx := context.Background()
	run := seedRun(t, st, "storage-backup-run")
	seedEvent(t, st, run.ID)

	dest := filepath.Join(t.TempDir(), "store-backup.sqlite")
	if err := st.Store.Backup(ctx, dest); err != nil {
		t.Fatalf("storage.Backup: %v", err)
	}
	info, err := inspectSnapshot(ctx, dest)
	if err != nil {
		t.Fatalf("inspect storage backup: %v", err)
	}
	if info.SchemaVersion != 2 {
		t.Fatalf("schema version = %d, want 2", info.SchemaVersion)
	}
	if info.Counts.Runs != 1 || info.Counts.Steps != 6 || info.Counts.Events < 2 {
		t.Fatalf("interop census = %+v", info.Counts)
	}
}
