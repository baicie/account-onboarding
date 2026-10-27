package ops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSnapshot writes a snapshot file that cannot verify: either without a
// manifest (unverifiable) or with a manifest whose checksum cannot match (invalid).
func fakeSnapshot(t *testing.T, dir, name string, withManifest bool) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not-a-sqlite-database"), 0o600); err != nil {
		t.Fatalf("write fake snapshot: %v", err)
	}
	if withManifest {
		raw := "{\"format_version\":1,\"sha256\":\"deadbeef\",\"schema_version\":6}"
		if err := os.WriteFile(path+".manifest.json", []byte(raw), 0o600); err != nil {
			t.Fatalf("write fake manifest: %v", err)
		}
	}
	return path
}

func countSnapshots(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && isSnapshotFile(e.Name()) {
			n++
		}
	}
	return n
}

// P1-2: -purge-invalid must never be able to empty the recovery set.
func TestRetentionPurgeRefusesWhenNothingVerifies(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	for i, name := range []string{"state-20260101T000000Z.sqlite", "state-20260102T000000Z.sqlite", "state-20260103T000000Z.sqlite"} {
		fakeSnapshot(t, dir, name, false)
		_ = os.Chtimes(filepath.Join(dir, name), now.Add(-time.Duration(72-i*24)*time.Hour), now.Add(-time.Duration(72-i*24)*time.Hour))
	}
	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, PurgeInvalid: true, MinValid: 1, Now: now,
	})
	if err == nil {
		t.Fatal("retention must fail closed when nothing verifies")
	}
	if !strings.Contains(strings.Join(res.Failures, " "), "refusing to purge") {
		t.Fatalf("failure does not explain the refusal: %v", res.Failures)
	}
	if got := countSnapshots(t, dir); got != 3 {
		t.Fatalf("snapshots left = %d, want 3 (nothing may be purged)", got)
	}
	if res.Deleted != 0 {
		t.Fatalf("deleted = %d, want 0", res.Deleted)
	}
	for _, c := range res.Candidates {
		if c.Action == retentionDelete {
			t.Fatalf("candidate %s marked for delete", filepath.Base(c.SnapshotPath))
		}
		if c.Action != retentionUnverifiable {
			t.Fatalf("candidate %s action = %q, want %q", filepath.Base(c.SnapshotPath), c.Action, retentionUnverifiable)
		}
	}
}

// P1-2: a wrong (here: absent) keyring must not turn into data loss.
func TestRetentionWrongKeyringKeepsEncryptedSnapshots(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	for i, name := range []string{"state-20260101T000000Z.sqlite.enc", "state-20260102T000000Z.sqlite.enc"} {
		fakeSnapshot(t, dir, name, true)
		_ = os.Chtimes(filepath.Join(dir, name), now.Add(-time.Duration(48-i*24)*time.Hour), now.Add(-time.Duration(48-i*24)*time.Hour))
	}
	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, PurgeInvalid: true, MinValid: 2, Now: now,
	})
	if err == nil {
		t.Fatal("retention must fail closed without a keyring")
	}
	if got := countSnapshots(t, dir); got != 2 {
		t.Fatalf("encrypted snapshots left = %d, want 2", got)
	}
	if res.Deleted != 0 {
		t.Fatalf("deleted = %d, want 0", res.Deleted)
	}
}

// P1-2: the floor is measured against the whole set, not just the verified files.
func TestRetentionPurgeRespectsFloorOverAllCandidates(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	paths := makeSnapshots(t, st, dir, []time.Time{now.Add(-2 * time.Hour)})
	fakeSnapshot(t, dir, "state-20260101T000000Z.sqlite", false)
	fakeSnapshot(t, dir, "state-20260102T000000Z.sqlite", true)

	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, PurgeInvalid: true, MinValid: 2, MaxCount: 10, Now: now,
	})
	if err == nil {
		t.Fatal("retention must refuse: only 1 verified snapshot but MinValid is 2")
	}
	if res.Deleted != 0 {
		t.Fatalf("deleted = %d, want 0", res.Deleted)
	}
	if got := countSnapshots(t, dir); got != 3 {
		t.Fatalf("snapshots left = %d, want 3", got)
	}
	if _, statErr := os.Stat(paths[0]); statErr != nil {
		t.Fatalf("verified snapshot must survive: %v", statErr)
	}
}

// P1-2: when the floor allows it, purging unverifiable files still works - the
// fix must not disable retention.
func TestRetentionPurgeAllowedAboveTheFloor(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	valid := makeSnapshots(t, st, dir, []time.Time{now.Add(-3 * time.Hour), now.Add(-1 * time.Hour)})
	fakeSnapshot(t, dir, "state-20260101T000000Z.sqlite", false)
	fakeSnapshot(t, dir, "state-20260102T000000Z.sqlite", true)

	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, PurgeInvalid: true, MinValid: 1, MaxCount: 10, Now: now,
	})
	if err != nil {
		t.Fatalf("retention should succeed here: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2 unverifiable snapshots", res.Deleted)
	}
	if got := countSnapshots(t, dir); got != 2 {
		t.Fatalf("snapshots left = %d, want the 2 verified ones", got)
	}
	for _, p := range valid {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("verified snapshot %s must survive purge: %v", filepath.Base(p), statErr)
		}
	}
}

// P1-2: without -purge-invalid nothing unverifiable is ever deleted.
func TestRetentionKeepsUnverifiableByDefault(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	fakeSnapshot(t, dir, "state-20260101T000000Z.sqlite", false)
	fakeSnapshot(t, dir, "state-20260102T000000Z.sqlite", true)
	res, err := ApplyRetention(context.Background(), RetentionOptions{Dir: dir, MinValid: 1, Now: now})
	if err != nil {
		t.Fatalf("retention without purge must not fail: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("deleted = %d, want 0", res.Deleted)
	}
	for _, c := range res.Candidates {
		if c.Action == retentionDelete {
			t.Fatalf("unverifiable snapshot marked for delete without -purge-invalid")
		}
	}
}
