package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/baicie/account-onboarding/internal/security"
)

// Verification is the evidence produced by VerifyBackup.
type Verification struct {
	SnapshotPath string       `json:"snapshot_path"`
	ManifestPath string       `json:"manifest_path"`
	Snapshot     SnapshotInfo `json:"snapshot"`
	Manifest     Manifest     `json:"manifest"`
	Encrypted    bool         `json:"encrypted"`
	KeyVersion   int          `json:"key_version,omitempty"`
	Checks       []string     `json:"checks"`
}

// ReadManifest loads and validates a manifest envelope.
func ReadManifest(path string) (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("decode manifest: %w", err)
	}
	if m.FormatVersion != ManifestFormatVersion {
		return m, fmt.Errorf("manifest format_version %d is not supported (want %d)", m.FormatVersion, ManifestFormatVersion)
	}
	if m.SHA256 == "" {
		return m, errors.New("manifest has no sha256")
	}
	if m.SchemaVersion < 1 {
		return m, fmt.Errorf("manifest schema_version %d is invalid", m.SchemaVersion)
	}
	if m.Encryption != "" {
		if m.Encryption != sealedCipher {
			return m, fmt.Errorf("manifest encryption %q is not supported", m.Encryption)
		}
		if m.KeyVersion < 1 {
			return m, fmt.Errorf("manifest key_version %d is invalid for an encrypted snapshot", m.KeyVersion)
		}
		if len(m.PlaintextSHA256) != 64 {
			return m, errors.New("manifest has no valid plaintext_sha256 for an encrypted snapshot")
		}
		if m.PlaintextSize <= 0 {
			return m, errors.New("manifest has no valid plaintext_size for an encrypted snapshot")
		}
	}
	return m, nil
}

// VerifyBackup proves that a plaintext snapshot is intact and matches its
// manifest. It is the backward-compatible wrapper for VerifyBackupWithKeyring.
func VerifyBackup(ctx context.Context, snapshotPath, manifestPath string) (Verification, error) {
	return VerifyBackupWithKeyring(ctx, snapshotPath, manifestPath, nil)
}

// VerifyBackupWithKeyring proves that a snapshot (plaintext or AES-256-GCM
// encrypted) is intact and matches its manifest: sha256 of the stored file,
// size, schema_migrations version, PRAGMA integrity_check, foreign_key_check and
// the per-table row census. For encrypted snapshots the keyring is mandatory:
// decryption failure is a hard error (fail closed).
func VerifyBackupWithKeyring(ctx context.Context, snapshotPath, manifestPath string, ring *security.KeyRing) (Verification, error) {
	var v Verification
	if strings.TrimSpace(snapshotPath) == "" {
		return v, errors.New("verify: snapshot path is required")
	}
	snapAbs, err := filepath.Abs(snapshotPath)
	if err != nil {
		return v, fmt.Errorf("verify: resolve snapshot path: %w", err)
	}
	if manifestPath == "" {
		manifestPath = ManifestPathFor(snapAbs)
	}
	manifestAbs, err := filepath.Abs(manifestPath)
	if err != nil {
		return v, fmt.Errorf("verify: resolve manifest path: %w", err)
	}
	m, err := ReadManifest(manifestAbs)
	if err != nil {
		return v, err
	}
	v.Manifest = m
	v.ManifestPath = manifestAbs
	v.SnapshotPath = snapAbs

	if got, want := filepath.Base(snapAbs), m.SnapshotFile; got != want {
		return v, fmt.Errorf("verify: manifest names snapshot %q but got %q", want, got)
	}
	v.Checks = append(v.Checks, "manifest_snapshot_name")

	storedSHA, storedSize, err := hashFile(snapAbs)
	if err != nil {
		return v, fmt.Errorf("verify: hash snapshot: %w", err)
	}
	if storedSize != m.SizeBytes {
		return v, fmt.Errorf("verify: size mismatch (snapshot %d, manifest %d)", storedSize, m.SizeBytes)
	}
	v.Checks = append(v.Checks, "size_bytes")
	if !strings.EqualFold(storedSHA, m.SHA256) {
		return v, fmt.Errorf("verify: sha256 mismatch (snapshot %s, manifest %s)", storedSHA, m.SHA256)
	}
	v.Checks = append(v.Checks, "sha256")

	header, sealed, err := detectSealed(snapAbs)
	if err != nil {
		return v, fmt.Errorf("verify: %w", err)
	}
	if sealed && m.Encryption == "" {
		return v, errors.New("verify: snapshot is encrypted but the manifest does not record encryption")
	}
	if !sealed && m.Encryption != "" {
		return v, errors.New("verify: manifest says the snapshot is encrypted but it is plaintext")
	}

	v.Encrypted = sealed
	if sealed {
		if ring == nil {
			return v, errors.New("verify: encrypted snapshot requires a keyring")
		}
		if header.KeyVersion != m.KeyVersion {
			return v, fmt.Errorf("verify: sealed key_version %d != manifest key_version %d", header.KeyVersion, m.KeyVersion)
		}
		if !strings.EqualFold(header.PlaintextSHA256, m.PlaintextSHA256) {
			return v, errors.New("verify: sealed plaintext_sha256 != manifest plaintext_sha256")
		}
		if header.PlaintextSize != m.PlaintextSize {
			return v, fmt.Errorf("verify: sealed plaintext_size %d != manifest %d", header.PlaintextSize, m.PlaintextSize)
		}
		plaintext, h, err := decryptSealedFile(snapAbs, ring)
		if err != nil {
			return v, fmt.Errorf("verify: %w", err)
		}
		if h.PlaintextSHA256 != m.PlaintextSHA256 || h.PlaintextSize != m.PlaintextSize {
			zero(plaintext)
			return v, errors.New("verify: sealed header changed during decryption")
		}
		tmp, err := writeTempPlaintext(filepath.Dir(snapAbs), plaintext)
		zero(plaintext)
		if err != nil {
			return v, fmt.Errorf("verify: write decrypted snapshot: %w", err)
		}
		defer removeSensitiveFile(tmp)
		info, err := inspectSnapshot(ctx, tmp)
		if err != nil {
			return v, err
		}
		info.Path = snapAbs
		info.SizeBytes = storedSize
		info.SHA256 = storedSHA
		info.Encryption = sealedCipher
		info.KeyVersion = header.KeyVersion
		info.PlaintextSHA256 = header.PlaintextSHA256
		info.PlaintextSize = header.PlaintextSize
		v.Snapshot = info
		v.KeyVersion = header.KeyVersion
		v.Checks = append(v.Checks, "encryption="+sealedCipher)
		v.Checks = append(v.Checks, fmt.Sprintf("key_version=%d", header.KeyVersion))
		v.Checks = append(v.Checks, "plaintext_sha256")
		v.Checks = append(v.Checks, fmt.Sprintf("plaintext_size=%d", header.PlaintextSize))
	} else {
		info, err := inspectSnapshot(ctx, snapAbs)
		if err != nil {
			return v, err
		}
		v.Snapshot = info
	}

	v.Checks = append(v.Checks, "integrity_check="+v.Snapshot.IntegrityCheck)
	if v.Snapshot.ForeignKeyErrors != 0 {
		return v, fmt.Errorf("verify: %d foreign key violations in snapshot", v.Snapshot.ForeignKeyErrors)
	}
	v.Checks = append(v.Checks, "foreign_key_check=0")
	if v.Snapshot.SchemaVersion != m.SchemaVersion {
		return v, fmt.Errorf("verify: schema version mismatch (snapshot %d, manifest %d)", v.Snapshot.SchemaVersion, m.SchemaVersion)
	}
	v.Checks = append(v.Checks, fmt.Sprintf("schema_version=%d", v.Snapshot.SchemaVersion))
	if !v.Snapshot.Counts.Equal(m.Counts) {
		return v, fmt.Errorf("verify: row census mismatch (snapshot %+v, manifest %+v)", v.Snapshot.Counts, m.Counts)
	}
	v.Checks = append(v.Checks, "counts")
	return v, nil
}
