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

// makeSnapshots produces n real snapshots of the stack database, each with a
// distinct modification time so retention ordering is unambiguous.
func makeSnapshots(t *testing.T, st *Stack, dir string, times []time.Time) []string {
	t.Helper()
	ctx := context.Background()
	paths := make([]string, 0, len(times))
	for i, ts := range times {
		res, err := Backup(ctx, BackupOptions{DBPath: st.DBPath, OutDir: dir})
		if err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		if err := os.Chtimes(res.SnapshotPath, ts, ts); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		paths = append(paths, res.SnapshotPath)
	}
	return paths
}

func readRetentionReport(t *testing.T, path string) RetentionResult {
	t.Helper()
	var res RetentionResult
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	return res
}

func TestRetentionKeepsNewestAndHonoursCount(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	paths := makeSnapshots(t, st, dir, []time.Time{
		now.Add(-96 * time.Hour), now.Add(-72 * time.Hour), now.Add(-48 * time.Hour), now.Add(-1 * time.Hour),
	})

	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxCount: 2, MinValid: 1, Now: now,
	})
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if res.Scanned != 4 || res.Valid != 4 {
		t.Fatalf("scanned=%d valid=%d, want 4/4", res.Scanned, res.Valid)
	}
	if res.Deleted != 2 || res.Kept != 2 {
		t.Fatalf("deleted=%d kept=%d, want 2/2", res.Deleted, res.Kept)
	}
	for _, p := range paths[:2] {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("old snapshot %s should be gone", filepath.Base(p))
		}
		if _, err := os.Stat(ManifestPathFor(p)); !os.IsNotExist(err) {
			t.Fatalf("old manifest %s should be gone", filepath.Base(ManifestPathFor(p)))
		}
	}
	for _, p := range paths[2:] {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("newest snapshot %s must survive: %v", filepath.Base(p), err)
		}
		if _, err := os.Stat(ManifestPathFor(p)); err != nil {
			t.Fatalf("newest manifest %s must survive: %v", filepath.Base(ManifestPathFor(p)), err)
		}
	}
	if res.ReportPath == "" {
		t.Fatal("retention audit report was not written")
	}
	if got := readRetentionReport(t, res.ReportPath); got.Deleted != res.Deleted || got.MinValid != 1 {
		t.Fatalf("report does not match the result: %+v", got)
	}
}

func TestRetentionMinValidFloor(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	paths := makeSnapshots(t, st, dir, []time.Time{
		now.Add(-240 * time.Hour), now.Add(-120 * time.Hour), now.Add(-1 * time.Hour),
	})
	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxCount: 1, MinValid: 3, Now: now,
	})
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if res.Deleted != 0 || res.Kept != 3 {
		t.Fatalf("deleted=%d kept=%d, want 0/3 (MinValid floor)", res.Deleted, res.Kept)
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("snapshot %s must survive the floor: %v", filepath.Base(p), err)
		}
	}
}

func TestRetentionNeverDeletesTheLastRecoveryPoint(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-1000 * time.Hour)
	paths := makeSnapshots(t, st, dir, []time.Time{old})

	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxAge: time.Hour, MaxCount: 1, MinValid: 0, Now: now,
	})
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("deleted %d snapshots, want 0: the last recovery point must survive", res.Deleted)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Fatalf("only snapshot was removed: %v", err)
	}
}

func TestRetentionMaxAge(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	paths := makeSnapshots(t, st, dir, []time.Time{
		now.Add(-30 * 24 * time.Hour), now.Add(-3 * 24 * time.Hour), now.Add(-2 * time.Hour),
	})
	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxAge: 24 * time.Hour, MinValid: 1, Now: now,
	})
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("deleted=%d, want 2 (older than 24h)", res.Deleted)
	}
	if _, err := os.Stat(paths[2]); err != nil {
		t.Fatalf("recent snapshot must survive: %v", err)
	}
}

func TestRetentionReportsInvalidSnapshotsAndProtectsThem(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	paths := makeSnapshots(t, st, dir, []time.Time{now.Add(-48 * time.Hour), now.Add(-1 * time.Hour)})

	corrupt := paths[0]
	if err := os.WriteFile(corrupt, []byte("corrupted"), 0o600); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}
	orphan := filepath.Join(dir, "state-20200101T000000Z.sqlite")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxCount: 1, MinValid: 1, Now: now,
	})
	if err != nil {
		t.Fatalf("retention: %v", err)
	}
	if res.Valid != 1 {
		t.Fatalf("valid=%d, want 1", res.Valid)
	}
	var invalidCount int
	for _, c := range res.Candidates {
		if !c.Valid {
			invalidCount++
			if c.Reason == "" {
				t.Fatalf("invalid candidate %s has no reason", filepath.Base(c.SnapshotPath))
			}
		}
	}
	if invalidCount != 2 {
		t.Fatalf("invalid candidates=%d, want 2 (corrupted + orphan manifest)", invalidCount)
	}
	if _, err := os.Stat(corrupt); err != nil {
		t.Fatalf("an invalid snapshot must not be deleted without -purge-invalid: %v", err)
	}

	res, err = ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxCount: 1, MinValid: 1, PurgeInvalid: true, Now: now,
	})
	if err != nil {
		t.Fatalf("retention with purge: %v", err)
	}
	if _, err := os.Stat(corrupt); !os.IsNotExist(err) {
		t.Fatalf("corrupted snapshot should be purged: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan snapshot should be purged: %v", err)
	}
	if _, err := os.Stat(paths[1]); err != nil {
		t.Fatalf("valid snapshot must survive purge: %v", err)
	}
}

func TestRetentionRecordsDeleteFailure(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	paths := makeSnapshots(t, st, dir, []time.Time{now.Add(-72 * time.Hour), now.Add(-1 * time.Hour)})

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxCount: 1, MinValid: 1, Now: now,
	})
	if err == nil {
		t.Fatal("retention must report the delete failure")
	}
	if len(res.Failures) == 0 {
		t.Fatal("no failure recorded")
	}
	if !strings.Contains(strings.Join(res.Failures, " "), "remove") {
		t.Fatalf("failure does not name the remove step: %v", res.Failures)
	}
	for _, p := range paths {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("snapshot %s must survive a failed delete: %v", filepath.Base(p), statErr)
		}
	}
}

func TestRetentionDryRunChangesNothing(t *testing.T) {
	st := newTestStack(t)
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	paths := makeSnapshots(t, st, dir, []time.Time{now.Add(-72 * time.Hour), now.Add(-1 * time.Hour)})

	res, err := ApplyRetention(context.Background(), RetentionOptions{
		Dir: dir, MaxCount: 1, MinValid: 1, DryRun: true, Now: now,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("dry run deleted %d snapshots", res.Deleted)
	}
	for _, p := range paths {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("dry run removed %s: %v", filepath.Base(p), statErr)
		}
	}
}

func TestIsSnapshotFile(t *testing.T) {
	cases := map[string]bool{
		"state-20260101T000000Z.sqlite":                   true,
		"state-20260101T000000Z.sqlite.enc":               true,
		"state-20260101T000000Z.sqlite.manifest.json":     false,
		"state-20260101T000000Z.sqlite-wal":               false,
		"retention-20260101T000000Z.json":                 false,
		"other.sqlite":                                    false,
	}
	for name, want := range cases {
		if got := isSnapshotFile(name); got != want {
			t.Fatalf("isSnapshotFile(%q)=%v want %v", name, got, want)
		}
	}
}
