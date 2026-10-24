package ops

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// TableCounts is the row census captured in a backup manifest. It is also the
// expected-count contract checked after a restore.
type TableCounts struct {
	Runs         int64 `json:"runs"`
	Steps        int64 `json:"steps"`
	Events       int64 `json:"events"`
	Resources    int64 `json:"resources"`
	Secrets      int64 `json:"secrets"`
	MailReceipts int64 `json:"mail_receipts"`
	AuditLog     int64 `json:"audit_log"`
}

// Equal reports whether two censuses are identical.
func (c TableCounts) Equal(o TableCounts) bool { return c == o }

// IsZero reports whether every table was empty.
func (c TableCounts) IsZero() bool { return c == TableCounts{} }

// SnapshotInfo is the verified description of one SQLite snapshot. For an
// encrypted snapshot, SizeBytes/SHA256 describe the stored (ciphertext) file
// while PlaintextSHA256/PlaintextSize describe the verified SQLite payload.
type SnapshotInfo struct {
	Path             string      `json:"path"`
	SizeBytes        int64       `json:"size_bytes"`
	SHA256           string      `json:"sha256"`
	SchemaVersion    int         `json:"schema_version"`
	JournalMode      string      `json:"journal_mode"`
	IntegrityCheck   string      `json:"integrity_check"`
	ForeignKeyErrors int64       `json:"foreign_key_violations"`
	Counts           TableCounts `json:"counts"`
	Encryption       string      `json:"encryption,omitempty"`
	KeyVersion       int         `json:"key_version,omitempty"`
	PlaintextSHA256  string      `json:"plaintext_sha256,omitempty"`
	PlaintextSize    int64       `json:"plaintext_size,omitempty"`
}

// sqliteDSN builds a file: URI DSN. readOnly opens with mode=ro, which is how
// every snapshot is inspected and how the live database is snapshotted: a
// read-only connection still sees the WAL and never mutates the source.
func sqliteDSN(path string, readOnly bool) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{}
	q.Set("_busy_timeout", "5000")
	if readOnly {
		q.Set("mode", "ro")
	}
	return "file:" + filepath.ToSlash(abs) + "?" + q.Encode()
}

// sqliteWriteDSN is used only by key rotation. It mirrors the service pragmas
// (WAL, full sync, foreign keys, immediate write transactions).
func sqliteWriteDSN(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	q := url.Values{}
	q.Set("_busy_timeout", "5000")
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "FULL")
	q.Set("_foreign_keys", "on")
	q.Set("_txlock", "immediate")
	return "file:" + filepath.ToSlash(abs) + "?" + q.Encode()
}

func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", sqliteDSN(path, true))
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	return db, nil
}

func openReadWrite(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", sqliteWriteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return db, nil
}

// integrityCheck runs PRAGMA integrity_check and returns a stable summary. An
// empty result set or anything other than "ok" is a verification failure.
func integrityCheck(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return "", fmt.Errorf("integrity_check: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("integrity_check scan: %w", err)
		}
		results = append(results, line)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("integrity_check rows: %w", err)
	}
	if len(results) == 0 {
		return "", errors.New("integrity_check returned no rows")
	}
	joined := strings.Join(results, "; ")
	if joined != "ok" {
		return joined, fmt.Errorf("integrity_check failed: %s", joined)
	}
	return joined, nil
}

// foreignKeyViolations counts PRAGMA foreign_key_check rows.
func foreignKeyViolations(ctx context.Context, db *sql.DB) (int64, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return 0, fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	var n int64
	for rows.Next() {
		var (
			table  string
			rowid  sql.NullInt64
			parent string
			fkid   int64
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return 0, fmt.Errorf("foreign_key_check scan: %w", err)
		}
		n++
	}
	return n, rows.Err()
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema_migrations: %w", err)
	}
	return version, nil
}

func journalMode(ctx context.Context, db *sql.DB) (string, error) {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		return "", fmt.Errorf("read journal_mode: %w", err)
	}
	return strings.ToLower(mode), nil
}

// countingTables is the fixed, non-injectable census. Tables introduced by
// future migrations are simply absent and count as zero.
var countingTables = []struct {
	name string
	set  func(*TableCounts, int64)
}{
	{"runs", func(c *TableCounts, n int64) { c.Runs = n }},
	{"steps", func(c *TableCounts, n int64) { c.Steps = n }},
	{"events", func(c *TableCounts, n int64) { c.Events = n }},
	{"resources", func(c *TableCounts, n int64) { c.Resources = n }},
	{"secrets", func(c *TableCounts, n int64) { c.Secrets = n }},
	{"mail_receipts", func(c *TableCounts, n int64) { c.MailReceipts = n }},
	{"audit_log", func(c *TableCounts, n int64) { c.AuditLog = n }},
}

func tableCounts(ctx context.Context, db *sql.DB) (TableCounts, error) {
	var counts TableCounts
	for _, t := range countingTables {
		var n int64
		// The table name comes from the fixed slice above, never from input.
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.name).Scan(&n)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such table") {
				continue
			}
			return counts, fmt.Errorf("count %s: %w", t.name, err)
		}
		t.set(&counts, n)
	}
	return counts, nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// inspectSnapshot opens a snapshot read-only and proves its integrity, schema
// version and row census. Any integrity failure is an error, never a warning.
func inspectSnapshot(ctx context.Context, path string) (SnapshotInfo, error) {
	var info SnapshotInfo
	info.Path = path
	st, err := os.Stat(path)
	if err != nil {
		return info, fmt.Errorf("stat snapshot: %w", err)
	}
	if !st.Mode().IsRegular() {
		return info, fmt.Errorf("snapshot %s is not a regular file", path)
	}
	info.SizeBytes = st.Size()

	db, err := openReadOnly(ctx, path)
	if err != nil {
		return info, err
	}
	defer db.Close()

	sum, size, err := hashFile(path)
	if err != nil {
		return info, fmt.Errorf("hash snapshot: %w", err)
	}
	info.SHA256 = sum
	info.SizeBytes = size
	if info.IntegrityCheck, err = integrityCheck(ctx, db); err != nil {
		return info, err
	}
	if info.ForeignKeyErrors, err = foreignKeyViolations(ctx, db); err != nil {
		return info, err
	}
	if info.SchemaVersion, err = schemaVersion(ctx, db); err != nil {
		return info, err
	}
	if info.JournalMode, err = journalMode(ctx, db); err != nil {
		return info, err
	}
	if info.Counts, err = tableCounts(ctx, db); err != nil {
		return info, err
	}
	// For a plaintext snapshot the stored file is the SQLite payload.
	info.PlaintextSHA256 = info.SHA256
	info.PlaintextSize = info.SizeBytes
	return info, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFileAtomic writes data to a temp file in the same directory, fsyncs it,
// renames it into place and fsyncs the directory. The final path either has the
// complete content or does not exist.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// copyFileAtomic copies src to dst via a same-directory temp file. The returned
// SHA-256 is of the copied bytes.
func copyFileAtomic(src, dst string, perm os.FileMode) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	f, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), in)
	if err != nil {
		cleanup()
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", 0, err
	}
	if err := f.Chmod(perm); err != nil {
		cleanup()
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if err := syncDir(dir); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
