package storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

func openSessionStore(t *testing.T, path string) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Options{
		Path: path, HMACKey: []byte("session-test-key"), Redactor: security.NewRedactor(),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

// TestSessionRevocationSurvivesReopen covers the durability contract directly:
// a revocation written by one process is visible to a completely new Store
// instance on the same database file.
func TestSessionRevocationSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	ctx := context.Background()
	first := openSessionStore(t, path)
	now := time.Now()
	if err := first.RevokeSession(ctx, "sid-durable", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := openSessionStore(t, path)
	defer second.Close()
	revoked, err := second.SessionRevoked(ctx, "sid-durable", now)
	if err != nil || !revoked {
		t.Fatalf("revocation lost across reopen: revoked=%v err=%v", revoked, err)
	}
	if n, err := second.CountSessionRevocations(ctx); err != nil || n != 1 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

// TestSessionRevocationConcurrentWritesAreIdempotent asserts the primary-key
// upsert never creates duplicate rows under concurrent logout.
func TestSessionRevocationConcurrentWritesAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.sqlite")
	s := openSessionStore(t, path)
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.RevokeSession(ctx, "sid-race", now, now.Add(time.Hour))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent revoke %d: %v", i, err)
		}
	}
	if n, _ := s.CountSessionRevocations(ctx); n != 1 {
		t.Fatalf("concurrent revocations created %d rows", n)
	}
	revoked, err := s.SessionRevoked(ctx, "sid-race", now)
	if err != nil || !revoked {
		t.Fatalf("revoked=%v err=%v", revoked, err)
	}
}

// TestSessionRevocationValidation covers malformed calls.
func TestSessionRevocationValidation(t *testing.T) {
	s := openSessionStore(t, filepath.Join(t.TempDir(), "sessions.sqlite"))
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.RevokeSession(ctx, "", now, now.Add(time.Hour)); err == nil {
		t.Fatal("empty session id accepted")
	}
	if err := s.RevokeSession(ctx, "sid", now, now); err == nil {
		t.Fatal("non-positive TTL accepted")
	}
	// Unknown ids are not revoked.
	if revoked, err := s.SessionRevoked(ctx, "unknown", now); err != nil || revoked {
		t.Fatalf("unknown id: revoked=%v err=%v", revoked, err)
	}
}

// TestMigration0003AppliesToExistingV2Database proves backward compatibility:
// a database migrated only through 0002 gains the revocation table and version
// 3 on the next Open, without touching existing data.
func TestMigration0003AppliesToExistingV2Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	ctx := context.Background()
	s := openSessionStore(t, path)
	if err := s.RevokeSession(ctx, "sid-before-upgrade", time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a v0.1.0 (schema 2) database: drop the new tables and their
	// migration markers, keeping the rest of the data.
	raw, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TABLE session_revocations`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TABLE external_operations`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version IN (3,4)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded := openSessionStore(t, path)
	defer upgraded.Close()
	health, err := upgraded.Health(ctx)
	if err != nil || health.SchemaVersion != 4 || !health.ExternalOperationsReady {
		t.Fatalf("schema after upgrade = %d ext=%v err=%v", health.SchemaVersion, health.ExternalOperationsReady, err)
	}
	// The pre-upgrade revocation row is of course gone (table was dropped), but
	// the upgraded store must accept and persist a new revocation.
	if err := upgraded.RevokeSession(ctx, "sid-after-upgrade", time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if revoked, err := upgraded.SessionRevoked(ctx, "sid-after-upgrade", time.Now()); err != nil || !revoked {
		t.Fatalf("post-upgrade revocation: revoked=%v err=%v", revoked, err)
	}
}

// TestHealthProbesSessionRevocationTable verifies /readyz's underlying health
// signal: a schema-3 database whose revocation table was dropped must not report
// session-revocation readiness (the UI would otherwise silently fail closed on
// every login while reporting ready).
func TestHealthProbesSessionRevocationTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.sqlite")
	ctx := context.Background()
	s := openSessionStore(t, path)
	health, err := s.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.SchemaVersion != 4 || !health.SessionRevocationsReady || !health.ExternalOperationsReady {
		t.Fatalf("health = %+v", health)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TABLE session_revocations`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening does not re-apply migration 0003 (its version is recorded), so
	// the missing table must be visible in health instead of silently healing.
	degraded := openSessionStore(t, path)
	defer degraded.Close()
	health, err = degraded.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.SchemaVersion != 4 || health.SessionRevocationsReady {
		t.Fatalf("degraded health = %+v (want schema 4, revocations not ready)", health)
	}
	if err := degraded.RevokeSession(ctx, "sid", time.Now(), time.Now().Add(time.Hour)); err == nil {
		t.Fatal("revocation succeeded without the revocation table")
	}
}

// TestSingleWriterInstanceLock enforces the documented single-instance contract:
// a second Store on the same database path is refused while the first is open,
// and is accepted again after Close. The kernel releases the lock on process
// death (verified at the process level in tests/r51/failure_modes.sh).
func TestSingleWriterInstanceLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "single.sqlite")
	first := openSessionStore(t, path)
	second, err := storage.Open(context.Background(), storage.Options{
		Path: path, HMACKey: []byte("session-test-key"), Redactor: security.NewRedactor(),
	})
	if err == nil {
		second.Close()
		t.Fatal("second Store acquired the same database while the first was open")
	}
	if !strings.Contains(err.Error(), "single-writer") {
		t.Fatalf("unexpected lock error: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again := openSessionStore(t, path)
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}
