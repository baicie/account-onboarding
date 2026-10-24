package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

// RotationOptions configures a re-encryption pass over the secrets table.
type RotationOptions struct {
	// DBPath is the SQLite database. Stop the service before a real rotation so
	// that no worker writes a secret while the transaction is open; a dry-run is
	// read-only and safe against a running service.
	DBPath string
	// KeyRingPath is the JSON envelope containing the new current key and every
	// older key version that must still be readable:
	//   {"current":2,"keys":{"1":"<base64>","2":"<base64>"}}
	KeyRingPath string
	// Ring may be supplied directly instead of KeyRingPath (tests).
	Ring *security.KeyRing
	// DryRun reports what would change without writing anything.
	DryRun bool
	// Now defaults to time.Now.
	Now func() time.Time
}

// RotationResult is the rotation evidence. It contains no key material or
// plaintext.
type RotationResult struct {
	DBPath          string      `json:"db_path"`
	DryRun          bool        `json:"dry_run"`
	CurrentVersion  int         `json:"current_version"`
	TotalSecrets    int         `json:"total_secrets"`
	AlreadyCurrent  int         `json:"already_current"`
	Rotated         int         `json:"rotated"`
	Planned         int         `json:"planned"`
	VersionsBefore  map[int]int `json:"versions_before"`
	VersionsAfter   map[int]int `json:"versions_after"`
	VerifiedAfter   int         `json:"verified_after"`
	MissingVersions []int       `json:"missing_key_versions,omitempty"`
}

type secretRow struct {
	id         string
	kind       string
	ciphertext []byte
	nonce      []byte
	version    int
}

// RotateSecrets re-encrypts every secrets row under the current key version in
// one BEGIN IMMEDIATE transaction. It is idempotent: rows already on the current
// version are decrypted (proof of readability) and left byte-for-byte intact.
//
// A key version missing from the keyring fails the entire transaction and rolls
// back, so no row is lost and nothing is left half-rotated.
func RotateSecrets(ctx context.Context, opts RotationOptions) (RotationResult, error) {
	res := RotationResult{
		DBPath:         opts.DBPath,
		DryRun:         opts.DryRun,
		VersionsBefore: map[int]int{},
		VersionsAfter:  map[int]int{},
	}
	if strings.TrimSpace(opts.DBPath) == "" {
		return res, errors.New("rotate: DBPath is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ring := opts.Ring
	if ring == nil {
		if strings.TrimSpace(opts.KeyRingPath) == "" {
			return res, errors.New("rotate: KeyRingPath or Ring is required")
		}
		loaded, err := security.LoadKeyRing(opts.KeyRingPath)
		if err != nil {
			return res, fmt.Errorf("rotate: load keyring: %w", err)
		}
		ring = loaded
	}
	dbPath, err := filepathAbs(opts.DBPath)
	if err != nil {
		return res, err
	}
	if _, err := os.Stat(dbPath); err != nil {
		return res, fmt.Errorf("rotate: stat database: %w", err)
	}
	res.DBPath = dbPath
	res.CurrentVersion = ring.CurrentVersion()
	box := security.NewBox(ring)

	if opts.DryRun {
		rows, err := readSecretRows(ctx, dbPath, true, nil)
		if err != nil {
			return res, err
		}
		for _, row := range rows {
			res.TotalSecrets++
			res.VersionsBefore[row.version]++
			if row.version == res.CurrentVersion {
				if _, err := box.Open(row.kind, row.id, sealedOf(row)); err != nil {
					return res, fmt.Errorf("rotate: dry-run: current-version secret %s is not decryptable: %w", row.id, err)
				}
				res.AlreadyCurrent++
				continue
			}
			plaintext, err := box.Open(row.kind, row.id, sealedOf(row))
			if err != nil {
				return res, fmt.Errorf("rotate: dry-run: secret %s needs key version %d which is not usable: %w", row.id, row.version, err)
			}
			if _, err := box.Seal(row.kind, row.id, plaintext); err != nil {
				return res, fmt.Errorf("rotate: dry-run: cannot seal secret %s under version %d: %w", row.id, res.CurrentVersion, err)
			}
			zero(plaintext)
			res.Planned++
		}
		res.VersionsAfter = copyVersions(res.VersionsBefore)
		return res, nil
	}

	db, err := openReadWrite(ctx, dbPath)
	if err != nil {
		return res, err
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, nil) // _txlock=immediate => BEGIN IMMEDIATE
	if err != nil {
		return res, fmt.Errorf("rotate: begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := readSecretRows(ctx, dbPath, false, tx)
	if err != nil {
		return res, err
	}
	nowMS := opts.Now().UTC().UnixMilli()
	for _, row := range rows {
		res.TotalSecrets++
		res.VersionsBefore[row.version]++
		if row.version == res.CurrentVersion {
			// Idempotency plus a readability proof for already-rotated rows.
			plaintext, err := box.Open(row.kind, row.id, sealedOf(row))
			if err != nil {
				return res, fmt.Errorf("rotate: secret %s is already on version %d but does not decrypt; refusing to continue: %w", row.id, row.version, err)
			}
			zero(plaintext)
			res.AlreadyCurrent++
			continue
		}
		plaintext, err := box.Open(row.kind, row.id, sealedOf(row))
		if err != nil {
			res.MissingVersions = append(res.MissingVersions, row.version)
			return res, fmt.Errorf("rotate: secret %s needs key version %d which is unavailable or wrong; transaction rolled back, no data changed: %w", row.id, row.version, err)
		}
		sealed, err := box.Seal(row.kind, row.id, plaintext)
		zero(plaintext)
		if err != nil {
			return res, fmt.Errorf("rotate: seal secret %s: %w", row.id, err)
		}
		update, err := tx.ExecContext(ctx, `UPDATE secrets
			SET ciphertext=?, nonce=?, key_version=?, updated_at_ms=?
			WHERE id=? AND key_version=?`,
			sealed.Ciphertext, sealed.Nonce, sealed.KeyVersion, nowMS, row.id, row.version)
		if err != nil {
			return res, fmt.Errorf("rotate: update secret %s: %w", row.id, err)
		}
		affected, err := update.RowsAffected()
		if err != nil {
			return res, fmt.Errorf("rotate: update secret %s: %w", row.id, err)
		}
		if affected != 1 {
			return res, fmt.Errorf("rotate: secret %s changed concurrently (rows=%d); transaction rolled back", row.id, affected)
		}
		res.Rotated++
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("rotate: commit: %w", err)
	}

	// Post-commit proof: every row must now be on the current version and
	// decryptable with the current key.
	after, err := readSecretRows(ctx, dbPath, true, nil)
	if err != nil {
		return res, fmt.Errorf("rotate: post-rotation read failed (data was committed): %w", err)
	}
	for _, row := range after {
		res.VersionsAfter[row.version]++
		if row.version != res.CurrentVersion {
			return res, fmt.Errorf("rotate: post-rotation row %s still on version %d (want %d)", row.id, row.version, res.CurrentVersion)
		}
		plaintext, err := box.Open(row.kind, row.id, sealedOf(row))
		if err != nil {
			return res, fmt.Errorf("rotate: post-rotation row %s does not decrypt: %w", row.id, err)
		}
		zero(plaintext)
		res.VerifiedAfter++
	}
	if len(after) != res.TotalSecrets {
		return res, fmt.Errorf("rotate: secret count changed during rotation (%d -> %d)", res.TotalSecrets, len(after))
	}
	return res, nil
}

// readSecretRows loads all rows either on an existing transaction or on a fresh
// read-only connection.
func readSecretRows(ctx context.Context, dbPath string, readOnly bool, tx *sql.Tx) ([]secretRow, error) {
	const query = `SELECT id, secret_kind, ciphertext, nonce, key_version FROM secrets ORDER BY id`
	var (
		rows *sql.Rows
		err  error
	)
	if tx != nil {
		rows, err = tx.QueryContext(ctx, query)
	} else {
		var db *sql.DB
		if readOnly {
			db, err = openReadOnly(ctx, dbPath)
		} else {
			db, err = openReadWrite(ctx, dbPath)
		}
		if err != nil {
			return nil, err
		}
		defer db.Close()
		rows, err = db.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, fmt.Errorf("rotate: list secrets: %w", err)
	}
	defer rows.Close()
	var out []secretRow
	for rows.Next() {
		var row secretRow
		if err := rows.Scan(&row.id, &row.kind, &row.ciphertext, &row.nonce, &row.version); err != nil {
			return nil, fmt.Errorf("rotate: scan secret: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func sealedOf(row secretRow) security.Sealed {
	return security.Sealed{Ciphertext: row.ciphertext, Nonce: row.nonce, KeyVersion: row.version}
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func copyVersions(in map[int]int) map[int]int {
	out := make(map[int]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func filepathAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	return abs, nil
}

// SortedVersions is a small helper for deterministic CLI output.
func SortedVersions(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
