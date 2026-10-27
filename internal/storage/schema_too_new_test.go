package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/baicie/account-onboarding/internal/security"
)

// TestMigrateRejectsFutureSchema is the rollback safety gate: an older binary
// must refuse to start on a database migrated by a newer binary instead of
// silently running with a schema it does not understand.
func TestMigrateRejectsFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	s, err := Open(context.Background(), Options{Path: path, HMACKey: []byte("k"), Redactor: security.NewRedactor()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version, applied_at_ms) VALUES(999, 1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	_, err = Open(context.Background(), Options{Path: path, HMACKey: []byte("k"), Redactor: security.NewRedactor()})
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open future schema error = %v, want ErrSchemaTooNew", err)
	}
}
