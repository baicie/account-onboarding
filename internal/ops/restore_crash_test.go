package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/storage"
)

// tryOpen reports whether the service could open (and, if needed, create) the
// database at path.
func tryOpen(t *testing.T, path string) error {
	t.Helper()
	st, err := storage.Open(context.Background(), storage.Options{
		Path: path, HMACKey: []byte("restore-crash-test-key"), ReadConns: 1,
	})
	if err == nil {
		_ = st.Close()
	}
	return err
}

// crashRestore runs a restore whose publish panics at stage, which is what a
// kill -9 there would leave behind on disk.
func crashRestore(t *testing.T, stage string, opts RestoreOptions) {
	t.Helper()
	restorePublishHook = func(s string) {
		if s == stage {
			panic("simulated crash at " + s)
		}
	}
	defer func() { restorePublishHook = nil }()
	crashed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				crashed = true
			}
		}()
		_, _ = Restore(context.Background(), opts)
	}()
	if !crashed {
		t.Fatalf("restore did not stop at stage %q", stage)
	}
}

func restoreOpts(f restoreFixture, target string) RestoreOptions {
	return RestoreOptions{
		SnapshotPath: f.backup.SnapshotPath,
		TargetPath:   target,
		KeyRingPath:  f.keyFile,
		Force:        true,
		Now:          time.Now,
	}
}

// P2-2: a crash between the two publish renames must not look like a first
// install. Without the guard, storage.Open would create an empty schema-6
// database that reports healthy.
func TestRestoreCrashBetweenRenamesRefusesSilentFirstInstall(t *testing.T) {
	f := newRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "state.sqlite")
	crashRestore(t, "between-renames", restoreOpts(f, target))

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target should be absent after a crash between renames: %v", err)
	}
	if _, err := os.Stat(target + restoreMarkerSuffix); err != nil {
		t.Fatalf("restore marker missing after the crash: %v", err)
	}
	if err := tryOpen(t, target); err == nil {
		t.Fatal("storage.Open created a fresh database instead of refusing")
	} else if !strings.Contains(err.Error(), "interrupted restore") {
		t.Fatalf("refusal does not explain the interrupted restore: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("a refused open must not leave a database behind: %v", err)
	}
}

// P2-2: a crash before the publish leaves the old database in place and usable.
func TestRestoreCrashBeforePublishKeepsOldDatabase(t *testing.T) {
	f := newRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "state.sqlite")
	if _, err := Restore(context.Background(), restoreOpts(f, target)); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat published database: %v", err)
	}
	crashRestore(t, "before-publish", restoreOpts(f, target))
	after, err := os.Stat(target)
	if err != nil {
		t.Fatalf("old database disappeared after a crash before publish: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("old database was replaced even though the publish never started")
	}
	if err := tryOpen(t, target); err != nil {
		t.Fatalf("the old database must still open: %v", err)
	}
}

// P2-2: a crash after the publish leaves the restored database in place; the
// marker must not prevent opening an existing database.
func TestRestoreCrashAfterPublishLeavesUsableDatabase(t *testing.T) {
	f := newRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "state.sqlite")
	crashRestore(t, "after-publish", restoreOpts(f, target))

	if _, err := os.Stat(target); err != nil {
		t.Fatalf("published database missing after a crash after publish: %v", err)
	}
	if _, err := os.Stat(target + restoreMarkerSuffix); err != nil {
		t.Fatalf("marker should still be present (the publish never completed cleanly): %v", err)
	}
	if err := tryOpen(t, target); err != nil {
		t.Fatalf("an existing restored database must open even with a stale marker: %v", err)
	}
}

// The happy path must clear the marker, and a genuine first install must still
// create a database.
func TestRestoreSuccessClearsMarkerAndFirstInstallStillWorks(t *testing.T) {
	f := newRestoreFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "state.sqlite")
	if _, err := Restore(context.Background(), restoreOpts(f, target)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(target + restoreMarkerSuffix); !os.IsNotExist(err) {
		t.Fatalf("marker left behind after a successful restore: %v", err)
	}
	fresh := filepath.Join(dir, "fresh", "state.sqlite")
	if err := os.MkdirAll(filepath.Dir(fresh), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := tryOpen(t, fresh); err != nil {
		t.Fatalf("a first install must still create the database: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("first install did not create a database: %v", err)
	}
}
