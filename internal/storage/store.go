// Package storage is the only package allowed to mutate workflow state.
//
// SQLite configuration follows docs/05-data-model.md: WAL, foreign keys on,
// busy_timeout 5000, synchronous FULL, `_txlock=immediate` for the single write
// connection. External HTTP calls never happen while a transaction is held:
// the engine claims in one transaction, performs I/O outside, then commits with
// a fencing check in a new transaction.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/baicie/account-onboarding/internal/security"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrLeaseLost is returned when a worker tries to commit with a stale lease.
var ErrLeaseLost = errors.New("lease lost: another worker owns this step")

// ErrConflict is returned for illegal state transitions or duplicate requests.
var ErrConflict = errors.New("conflict")

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrIdempotencyConflict means the same idempotency key was reused with a
// different request body.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")

// Options configures the store.
type Options struct {
	Path string
	// HMACKey is used for irreversible external resource identifiers and
	// message dedupe keys.
	HMACKey []byte
	// Redactor filters event payloads before persistence.
	Redactor *security.Redactor
	// Now defaults to time.Now.
	Now func() time.Time
	// ReadConns is the read pool size (default 8).
	ReadConns int
	// TransitionValidator is called inside the commit transaction to enforce the
	// workflow state machine. It must reject unknown edges.
	TransitionValidator func(from, to string) error
}

// Store is the SQLite persistence layer.
type Store struct {
	writeDB  *sql.DB
	readDB   *sql.DB
	lock     *instanceLock
	path     string
	hmacKey  []byte
	redactor *security.Redactor
	now      func() time.Time
	validate func(from, to string) error
	// sweepHook is an optional test-only synchronization point used by
	// SweepDeadlines race tests; nil in production.
	sweepHook func()
}

// Open opens the database, applies pragmas and runs migrations.
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, errors.New("storage: empty path")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Redactor == nil {
		opts.Redactor = security.NewRedactor()
	}
	if len(opts.HMACKey) == 0 {
		return nil, errors.New("storage: HMAC key is required")
	}
	if opts.ReadConns <= 0 {
		opts.ReadConns = 8
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o750); err != nil {
		return nil, fmt.Errorf("storage: create data dir: %w", err)
	}
	writeDSN := dsn(opts.Path, true)
	readDSN := dsn(opts.Path, false)

	var lock *instanceLock
	var err error
	if opts.Path != ":memory:" {
		lock, err = acquireInstanceLock(opts.Path)
		if err != nil {
			return nil, err
		}
	}
	releaseLock := func() {
		if lock != nil {
			lock.release()
			lock = nil
		}
	}
	writeDB, err := sql.Open("sqlite3", writeDSN)
	if err != nil {
		releaseLock()
		return nil, fmt.Errorf("storage: open write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)
	if err := writeDB.PingContext(ctx); err != nil {
		writeDB.Close()
		releaseLock()
		return nil, fmt.Errorf("storage: ping write db: %w", err)
	}

	readDB, err := sql.Open("sqlite3", readDSN)
	if err != nil {
		writeDB.Close()
		releaseLock()
		return nil, fmt.Errorf("storage: open read db: %w", err)
	}
	readDB.SetMaxOpenConns(opts.ReadConns)
	readDB.SetMaxIdleConns(opts.ReadConns / 2)
	readDB.SetConnMaxLifetime(0)
	if err := readDB.PingContext(ctx); err != nil {
		writeDB.Close()
		readDB.Close()
		releaseLock()
		return nil, fmt.Errorf("storage: ping read db: %w", err)
	}

	s := &Store{
		writeDB:  writeDB,
		readDB:   readDB,
		lock:     lock,
		path:     opts.Path,
		hmacKey:  append([]byte(nil), opts.HMACKey...),
		redactor: opts.Redactor,
		now:      opts.Now,
		validate: opts.TransitionValidator,
	}
	if err := s.Migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	if err := hardenDatabaseFiles(opts.Path); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// hardenDatabaseFiles restricts the SQLite database and its WAL/SHM sidecars to
// the owner (0600). SQLite creates these files with the process umask (0644 on a
// default umask), which would let other local users read encrypted secrets and
// workflow metadata. The lock file is already created with 0600.
func hardenDatabaseFiles(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // WAL/SHM may not exist yet; SQLite copies the DB mode.
			}
			return fmt.Errorf("storage: restrict %s permissions: %w", filepath.Base(p), err)
		}
	}
	return nil
}

func dsn(path string, write bool) string {
	params := "_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL&_foreign_keys=on"
	if write {
		params += "&_txlock=immediate"
	}
	return "file:" + path + "?" + params
}

// Close closes both pools.
func (s *Store) Close() error {
	var errs []error
	if s.writeDB != nil {
		errs = append(errs, s.writeDB.Close())
	}
	if s.readDB != nil {
		errs = append(errs, s.readDB.Close())
	}
	if s.lock != nil {
		s.lock.release()
		s.lock = nil
	}
	return errors.Join(errs...)
}

// Path returns the database path.
func (s *Store) Path() string { return s.path }

// Migrate applies embedded migrations in one explicit transaction. Migration
// scripts are additive; no DROP TABLE is used as an upgrade strategy.
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("storage: read migrations: %w", err)
	}
	type migration struct {
		version int
		name    string
		body    []byte
	}
	var list []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		name := e.Name()
		idx := strings.IndexByte(name, '_')
		if idx <= 0 {
			return fmt.Errorf("storage: bad migration name %q", name)
		}
		v, err := strconv.Atoi(name[:idx])
		if err != nil {
			return fmt.Errorf("storage: bad migration version %q: %w", name, err)
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("storage: read migration %q: %w", name, err)
		}
		list = append(list, migration{version: v, name: name, body: body})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin migration tx: %w", err)
	}
	defer tx.Rollback()

	// 0001 is idempotent (CREATE IF NOT EXISTS / INSERT OR IGNORE) and creates
	// schema_migrations, so it is always safe to execute.
	for _, m := range list {
		if m.version == 1 {
			if _, err := tx.ExecContext(ctx, string(m.body)); err != nil {
				return fmt.Errorf("storage: apply migration %s: %w", m.name, err)
			}
		}
	}
	var applied int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&applied); err != nil {
		return fmt.Errorf("storage: read schema version: %w", err)
	}
	for _, m := range list {
		if m.version <= applied {
			continue
		}
		if _, err := tx.ExecContext(ctx, string(m.body)); err != nil {
			return fmt.Errorf("storage: apply migration %s: %w", m.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit migrations: %w", err)
	}
	return nil
}

// Health is the readiness payload.
type Health struct {
	SchemaVersion int    `json:"schema_version"`
	WALMode       string `json:"wal_mode"`
	Writable      bool   `json:"writable"`
	// SessionRevocationsReady reports that the durable session-revocation table
	// from migration 0003 is present. A missing table means cookie sessions
	// cannot be revoked reliably, so /readyz must not report ready.
	SessionRevocationsReady bool `json:"session_revocations_ready"`
}

// Health verifies schema version, WAL mode and write capability.
func (s *Store) Health(ctx context.Context) (Health, error) {
	var h Health
	if err := s.readDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&h.SchemaVersion); err != nil {
		return h, fmt.Errorf("read schema version: %w", err)
	}
	if err := s.readDB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&h.WALMode); err != nil {
		return h, fmt.Errorf("read journal mode: %w", err)
	}
	var tables int
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_revocations'`).Scan(&tables); err != nil {
		return h, fmt.Errorf("probe session revocation table: %w", err)
	}
	h.SessionRevocationsReady = tables == 1
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return h, fmt.Errorf("begin write probe: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS _ready_probe(x INTEGER)`); err != nil {
		tx.Rollback()
		return h, fmt.Errorf("write probe: %w", err)
	}
	if err := tx.Rollback(); err != nil {
		return h, fmt.Errorf("rollback write probe: %w", err)
	}
	h.Writable = true
	return h, nil
}

// Timestamp helpers -----------------------------------------------------------

func (s *Store) nowMS() int64 { return s.now().UTC().UnixMilli() }

func nullableMS(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func ptrMS(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func nullableString(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func strFromNull(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// resourceHMAC returns the irreversible external identifier digest.
func (s *Store) resourceHMAC(kind, externalID string) string {
	return security.HMACHex(s.hmacKey, []byte("resource|"+kind+"|"+externalID))
}

// messageHMAC returns the irreversible mail dedupe key digest.
func (s *Store) messageHMAC(provider, key string) string {
	return security.HMACHex(s.hmacKey, []byte("mail|"+provider+"|"+key))
}

// Backup writes a consistent snapshot with VACUUM INTO. The destination must
// not exist. This is the only supported online backup path (copying the main
// file alone can miss WAL contents).
func (s *Store) Backup(ctx context.Context, destPath string) error {
	if destPath == "" {
		return errors.New("storage: empty backup destination")
	}
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("storage: backup destination %q already exists", destPath)
	}
	// VACUUM INTO quotes the literal path itself; bind parameters are not
	// supported by the SQLite version used here.
	quoted := strings.ReplaceAll(destPath, "'", "''")
	if _, err := s.writeDB.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil {
		return fmt.Errorf("storage: vacuum into: %w", err)
	}
	return nil
}

// IntegrityCheck runs PRAGMA integrity_check on the live database.
func (s *Store) IntegrityCheck(ctx context.Context) (string, error) {
	var result string
	if err := s.readDB.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return "", err
	}
	return result, nil
}

// ListSecrets returns every encrypted secret row (used by re-encryption jobs).
func (s *Store) ListSecrets(ctx context.Context) ([]SecretRecord, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT id, secret_kind, ciphertext, nonce, key_version,
		created_at_ms, updated_at_ms FROM secrets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretRecord
	for rows.Next() {
		var rec SecretRecord
		if err := rows.Scan(&rec.ID, &rec.Kind, &rec.Ciphertext, &rec.Nonce, &rec.KeyVersion,
			&rec.CreatedAtMS, &rec.UpdatedAtMS); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
