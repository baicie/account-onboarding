package storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
	"github.com/baicie/account-onboarding/internal/storage"
)

// TestSQLiteBusyTimeoutWaitsForLock proves the write pool waits through a
// transient external write lock (busy_timeout=5000) instead of failing fast or
// corrupting state: a second connection holds BEGIN IMMEDIATE, releases it after
// 150ms, and the store write must succeed after waiting.
func TestSQLiteBusyTimeoutWaitsForLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.sqlite")
	s, err := storage.Open(context.Background(), storage.Options{
		Path: path, HMACKey: []byte("busy-test-hmac-key"), Redactor: security.NewRedactor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	raw, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := raw.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("hold write lock: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = raw.ExecContext(context.Background(), `COMMIT`)
		close(released)
	}()
	start := time.Now()
	if err := s.Audit(ctx, "operator", "busy_probe", "", nil); err != nil {
		t.Fatalf("store write during external lock: %v", err)
	}
	elapsed := time.Since(start)
	<-released
	if elapsed < 100*time.Millisecond {
		t.Fatalf("write returned in %v; expected to wait for the external lock", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("write waited too long (%v)", elapsed)
	}
}
