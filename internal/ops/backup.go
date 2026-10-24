package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

// Manifest is the sidecar JSON that travels with a snapshot. It records the
// checksum, schema version and row census observed inside the snapshot itself.
//
// For an encrypted snapshot, SHA256/SizeBytes describe the stored ciphertext
// file while PlaintextSHA256/PlaintextSize describe the verified SQLite payload;
// the payload checksum is also bound into the AES-GCM AAD.
type Manifest struct {
	FormatVersion     int         `json:"format_version"`
	Tool              string      `json:"tool"`
	ToolVersion       string      `json:"tool_version"`
	CreatedAt         string      `json:"created_at"`
	SourceDB          string      `json:"source_db"`
	SnapshotFile      string      `json:"snapshot_file"`
	SizeBytes         int64       `json:"size_bytes"`
	SHA256            string      `json:"sha256"`
	SchemaVersion     int         `json:"schema_version"`
	SourceJournalMode string      `json:"source_journal_mode"`
	Counts            TableCounts `json:"counts"`
	// Encryption is empty for plaintext snapshots or "aes-256-gcm".
	Encryption      string `json:"encryption,omitempty"`
	KeyVersion      int    `json:"key_version,omitempty"`
	PlaintextSHA256 string `json:"plaintext_sha256,omitempty"`
	PlaintextSize   int64  `json:"plaintext_size,omitempty"`
}

// BackupOptions configures an online snapshot.
type BackupOptions struct {
	// DBPath is the live SQLite database. It is opened read-only: a read-only
	// connection sees committed WAL frames and never mutates the service DB.
	DBPath string
	// OutDir defaults to the directory of DBPath.
	OutDir string
	// Encrypt seals the snapshot with AES-256-GCM under the keyring current
	// version (P1-3: encryption at rest).
	Encrypt bool
	// KeyRingPath is the JSON/raw master keyring used when Encrypt is set.
	KeyRingPath string
	// Ring injects a keyring directly (tests); overrides KeyRingPath.
	Ring *security.KeyRing
	// Now defaults to time.Now; tests inject a deterministic clock.
	Now func() time.Time
}

// BackupResult describes a completed snapshot.
type BackupResult struct {
	SnapshotPath string   `json:"snapshot_path"`
	ManifestPath string   `json:"manifest_path"`
	Manifest     Manifest `json:"manifest"`
}

// ManifestPathFor returns the default sidecar path for a snapshot.
func ManifestPathFor(snapshotPath string) string { return snapshotPath + ".manifest.json" }

// Backup creates a consistent SQLite snapshot of a live WAL database with
// VACUUM INTO, then verifies it, optionally encrypts it (AES-256-GCM with a
// self-describing header), renames it atomically, chmods it 0600 and writes a
// checksum manifest. The source DB may be actively written while the snapshot is
// taken.
func Backup(ctx context.Context, opts BackupOptions) (BackupResult, error) {
	var res BackupResult
	if strings.TrimSpace(opts.DBPath) == "" {
		return res, errors.New("backup: DBPath is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	var ring *security.KeyRing
	if opts.Encrypt {
		ring = opts.Ring
		if ring == nil {
			if strings.TrimSpace(opts.KeyRingPath) == "" {
				return res, errors.New("backup: Encrypt requires a keyring (KeyRingPath or Ring)")
			}
			loaded, err := security.LoadKeyRing(opts.KeyRingPath)
			if err != nil {
				return res, fmt.Errorf("backup: load keyring: %w", err)
			}
			ring = loaded
		}
	}
	dbPath, err := filepath.Abs(opts.DBPath)
	if err != nil {
		return res, fmt.Errorf("backup: resolve db path: %w", err)
	}
	st, err := os.Stat(dbPath)
	if err != nil {
		return res, fmt.Errorf("backup: stat source db: %w", err)
	}
	if !st.Mode().IsRegular() {
		return res, fmt.Errorf("backup: %s is not a regular file", dbPath)
	}
	outDir := strings.TrimSpace(opts.OutDir)
	if outDir == "" {
		outDir = filepath.Dir(dbPath)
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return res, fmt.Errorf("backup: resolve output dir: %w", err)
	}
	// Snapshots contain encrypted secrets plus workflow metadata; keep the
	// containing directory private to the operator account.
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return res, fmt.Errorf("backup: create output dir: %w", err)
	}

	src, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return res, err
	}
	defer src.Close()

	// Pre-flight: the source must be a valid account-onboarding schema before we
	// advertise a backup. This is cheap compared with VACUUM INTO.
	if _, err := integrityCheck(ctx, src); err != nil {
		return res, fmt.Errorf("backup: source database is not healthy: %w", err)
	}
	sourceVersion, err := schemaVersion(ctx, src)
	if err != nil {
		return res, err
	}
	if sourceVersion < 1 {
		return res, fmt.Errorf("backup: source database has no schema version")
	}
	sourceJournal, err := journalMode(ctx, src)
	if err != nil {
		return res, err
	}

	now := opts.Now().UTC()
	stamp := now.Format("20060102T150405Z")
	base := strings.TrimSuffix(filepath.Base(dbPath), filepath.Ext(dbPath))
	suffix := ".sqlite"
	if opts.Encrypt {
		suffix = ".sqlite.enc"
	}
	snapPath, err := uniqueSnapshotPath(outDir, base+"-"+stamp+suffix)
	if err != nil {
		return res, err
	}

	// VACUUM INTO refuses an existing destination, so use a private temp name and
	// remove the placeholder first (the sqlite3 build used here does not accept
	// bound parameters for the INTO filename).
	tmpFile, err := os.CreateTemp(outDir, "."+filepath.Base(snapPath)+".tmp-*")
	if err != nil {
		return res, fmt.Errorf("backup: create temp snapshot: %w", err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return res, err
	}
	if err := os.Remove(tmpPath); err != nil {
		return res, err
	}
	plaintextTmp := tmpPath
	defer func() { _ = os.Remove(plaintextTmp) }()

	quoted := strings.ReplaceAll(plaintextTmp, "'", "''")
	if _, err := src.ExecContext(ctx, `VACUUM INTO '`+quoted+`'`); err != nil {
		return res, fmt.Errorf("backup: VACUUM INTO: %w", err)
	}
	// Tighten permissions before anything else can read the snapshot.
	if err := os.Chmod(plaintextTmp, 0o600); err != nil {
		return res, fmt.Errorf("backup: chmod snapshot: %w", err)
	}

	info, err := inspectSnapshot(ctx, plaintextTmp)
	if err != nil {
		return res, fmt.Errorf("backup: self-verification failed: %w", err)
	}
	if info.SchemaVersion != sourceVersion {
		return res, fmt.Errorf("backup: snapshot schema version %d != source %d", info.SchemaVersion, sourceVersion)
	}

	manifest := Manifest{
		FormatVersion:     ManifestFormatVersion,
		Tool:              ToolName,
		ToolVersion:       ToolVersion,
		CreatedAt:         now.Format(time.RFC3339),
		SourceDB:          filepath.Base(dbPath),
		SnapshotFile:      filepath.Base(snapPath),
		SchemaVersion:     info.SchemaVersion,
		SourceJournalMode: sourceJournal,
		Counts:            info.Counts,
		PlaintextSHA256:   info.SHA256,
		PlaintextSize:     info.SizeBytes,
	}

	if opts.Encrypt {
		sealedTmp, err := sealedTempPath(outDir, snapPath)
		if err != nil {
			return res, fmt.Errorf("backup: reserve sealed temp: %w", err)
		}
		defer func() { _ = os.Remove(sealedTmp) }()
		header, sealedSHA, sealedSize, err := sealSnapshot(plaintextTmp, sealedTmp, ring)
		if err != nil {
			return res, err
		}
		if header.PlaintextSHA256 != info.SHA256 || header.PlaintextSize != info.SizeBytes {
			return res, errors.New("backup: sealed header does not match the verified snapshot")
		}
		if err := os.Rename(sealedTmp, snapPath); err != nil {
			return res, fmt.Errorf("backup: publish sealed snapshot: %w", err)
		}
		// The plaintext staging file is no longer needed: best-effort overwrite and
		// remove it so only the encrypted artifact remains.
		removeSensitiveFile(plaintextTmp)
		manifest.SizeBytes = sealedSize
		manifest.SHA256 = sealedSHA
		manifest.Encryption = sealedCipher
		manifest.KeyVersion = header.KeyVersion
	} else {
		if err := os.Rename(plaintextTmp, snapPath); err != nil {
			return res, fmt.Errorf("backup: publish snapshot: %w", err)
		}
		manifest.SizeBytes = info.SizeBytes
		manifest.SHA256 = info.SHA256
	}
	if err := syncDir(outDir); err != nil {
		return res, fmt.Errorf("backup: sync output dir: %w", err)
	}

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return res, fmt.Errorf("backup: encode manifest: %w", err)
	}
	raw = append(raw, '\n')
	manifestPath := ManifestPathFor(snapPath)
	if err := writeFileAtomic(manifestPath, raw, 0o600); err != nil {
		return res, fmt.Errorf("backup: write manifest: %w", err)
	}
	res.SnapshotPath = snapPath
	res.ManifestPath = manifestPath
	res.Manifest = manifest
	return res, nil
}

// uniqueSnapshotPath avoids overwriting an earlier snapshot taken within the
// same second. It never truncates existing evidence.
func uniqueSnapshotPath(dir, name string) (string, error) {
	candidate := filepath.Join(dir, name)
	for i := 1; i <= 100; i++ {
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
	}
	return "", fmt.Errorf("backup: too many snapshots named %q in %s", name, dir)
}
