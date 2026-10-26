package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func applyMigrationsUpTo(t *testing.T, path string, upTo int) {
	t.Helper()
	raw, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for v := 1; v <= upTo; v++ {
		name := migrationName(v)
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := raw.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

func migrationName(v int) string {
	switch v {
	case 1:
		return "0001_initial.sql"
	case 2:
		return "0002_operator_fields.sql"
	case 3:
		return "0003_session_revocations.sql"
	case 4:
		return "0004_external_operations.sql"
	case 5:
		return "0005_external_operation_lifecycle.sql"
	case 6:
		return "0006_alert_outbox.sql"
	default:
		return ""
	}
}

func TestSchemaUpgradesTo5FromEmpty3And4(t *testing.T) {
	for _, from := range []int{0, 3, 4} {
		t.Run(map[int]string{0: "empty", 3: "schema3", 4: "schema4"}[from], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upgrade.sqlite")
			if from > 0 {
				applyMigrationsUpTo(t, path, from)
			}
			ctx := context.Background()
			s, err := Open(ctx, Options{Path: path, HMACKey: []byte("schema5-test-hmac-key")})
			if err != nil {
				t.Fatalf("open/migrate: %v", err)
			}
			defer s.Close()
			h, err := s.Health(ctx)
			if err != nil || h.SchemaVersion != 6 || !h.ExternalOperationsReady {
				t.Fatalf("health = %+v err=%v", h, err)
			}
			rows, err := s.readDB.Query(`PRAGMA table_info(external_operations)`)
			if err != nil {
				t.Fatal(err)
			}
			cols := map[string]bool{}
			for rows.Next() {
				var cid int
				var name, typ string
				var notnull, pk int
				var dflt sql.NullString
				if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				cols[name] = true
			}
			rows.Close()
			for _, want := range []string{"worker_id", "lease_generation", "resolved_by", "resolved_from_status", "resolution_version"} {
				if !cols[want] {
					t.Fatalf("migration 5 missing column %s (cols=%v)", want, cols)
				}
			}
		})
	}
}

func TestMigration5FailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.sqlite")
	applyMigrationsUpTo(t, path, 4)
	raw, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	// Pre-create a column that migration 5 adds; ALTER then fails and the
	// entire migration transaction (including the version row) must roll back.
	if _, err := raw.Exec(`ALTER TABLE external_operations ADD COLUMN worker_id TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := Open(ctx, Options{Path: path, HMACKey: []byte("schema5-test-hmac-key")}); err == nil {
		t.Fatal("Open succeeded despite broken migration")
	}
	check, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version int
	if err := check.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("failed migration advanced schema to %d, want 4", version)
	}
}
