package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

// RestoreOptions configures a verified restore into a target database path.
type RestoreOptions struct {
	// SnapshotPath is the snapshot produced by Backup (plaintext or encrypted).
	SnapshotPath string
	// ManifestPath defaults to ManifestPathFor(SnapshotPath).
	ManifestPath string
	// TargetPath is where the database is restored. It may be the path the
	// service uses; existing files are only replaced when Force is set.
	TargetPath string
	// LiveDBPath is the service's configured database path. It enables a
	// dedicated "this is the live database" refusal message.
	LiveDBPath string
	// Force allows replacing an existing target and is required for a live DB.
	Force bool
	// KeyRingPath is the keyring used to decrypt an encrypted snapshot and to
	// prove that encrypted secrets in it still decrypt. It is mandatory for an
	// encrypted snapshot and whenever the snapshot contains secrets.
	KeyRingPath string
	// KeepOld keeps the replaced target as <target>.pre-restore-<unixnano>.
	KeepOld bool
	// Now defaults to time.Now.
	Now func() time.Time
}

// RestoreResult is the restore evidence. It contains no secret material.
type RestoreResult struct {
	TargetPath            string      `json:"target_path"`
	SnapshotPath          string      `json:"snapshot_path"`
	ManifestPath          string      `json:"manifest_path"`
	SnapshotSHA256        string      `json:"snapshot_sha256"`
	Encrypted             bool        `json:"encrypted"`
	KeyVersion            int         `json:"key_version,omitempty"`
	PlaintextSHA256       string      `json:"plaintext_sha256"`
	PlaintextSize         int64       `json:"plaintext_size"`
	SchemaVersion         int         `json:"schema_version"`
	Counts                TableCounts `json:"counts"`
	IntegrityCheck        string      `json:"integrity_check"`
	ForeignKeyViolations  int64       `json:"foreign_key_violations"`
	SecretsVerified       int         `json:"secrets_verified"`
	SecretsByVersion      map[int]int `json:"secrets_by_version"`
	KeyringCurrentVersion int         `json:"keyring_current_version"`
	PreRestoreCopy        string      `json:"pre_restore_copy,omitempty"`
	ManifestVerified      bool        `json:"manifest_verified"`
}

const restoreStagingPattern = ".onboard-restore-*.sqlite"

// Restore verifies a snapshot and its manifest, stages a plaintext copy next to
// the target, proves integrity/counts/decryption on the staged copy, and only
// then swaps it into place. An encrypted snapshot is decrypted with the supplied
// keyring first; a wrong/missing key or a corrupt snapshot leaves the target
// untouched (fail closed, no plaintext fallback).
func Restore(ctx context.Context, opts RestoreOptions) (RestoreResult, error) {
	var res RestoreResult
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if strings.TrimSpace(opts.SnapshotPath) == "" {
		return res, errors.New("restore: snapshot path is required")
	}
	if strings.TrimSpace(opts.TargetPath) == "" {
		return res, errors.New("restore: target path is required")
	}
	snapAbs, err := filepath.Abs(opts.SnapshotPath)
	if err != nil {
		return res, fmt.Errorf("restore: resolve snapshot: %w", err)
	}
	targetAbs, err := filepath.Abs(opts.TargetPath)
	if err != nil {
		return res, fmt.Errorf("restore: resolve target: %w", err)
	}
	if samePath(snapAbs, targetAbs) {
		return res, errors.New("restore: target path is the snapshot itself")
	}

	// Load the keyring first: it is needed to verify an encrypted snapshot and to
	// prove that encrypted secrets still decrypt.
	var ring *security.KeyRing
	if opts.KeyRingPath != "" {
		ring, err = security.LoadKeyRing(opts.KeyRingPath)
		if err != nil {
			return res, fmt.Errorf("restore: load keyring: %w", err)
		}
		res.KeyringCurrentVersion = ring.CurrentVersion()
	}
	ver, err := VerifyBackupWithKeyring(ctx, snapAbs, opts.ManifestPath, ring)
	if err != nil {
		return res, err
	}
	res.SnapshotPath = snapAbs
	res.ManifestPath = ver.ManifestPath
	res.SnapshotSHA256 = ver.Snapshot.SHA256
	res.Encrypted = ver.Encrypted
	res.KeyVersion = ver.KeyVersion
	res.PlaintextSHA256 = ver.Manifest.PlaintextSHA256
	res.PlaintextSize = ver.Manifest.PlaintextSize
	res.ManifestVerified = true

	if ring == nil {
		switch {
		case ver.Manifest.Encryption != "":
			return res, errors.New("restore: encrypted snapshot requires a keyring")
		case ver.Manifest.Counts.Secrets > 0:
			return res, fmt.Errorf("restore: snapshot contains %d encrypted secrets; a keyring is required to prove decryption", ver.Manifest.Counts.Secrets)
		}
	}

	// A live database must never be replaced silently.
	if opts.LiveDBPath != "" {
		liveAbs, err := filepath.Abs(opts.LiveDBPath)
		if err == nil && samePath(liveAbs, targetAbs) && !opts.Force {
			return res, fmt.Errorf("restore: %q is the live service database; refusing without --force (stop the service first)", targetAbs)
		}
	}
	if info, err := os.Lstat(targetAbs); err == nil {
		if !opts.Force {
			return res, fmt.Errorf("restore: target %q already exists; refusing to overwrite without --force", targetAbs)
		}
		if info.IsDir() {
			return res, fmt.Errorf("restore: target %q is a directory", targetAbs)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("restore: stat target: %w", err)
	}

	targetDir := filepath.Dir(targetAbs)
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return res, fmt.Errorf("restore: create target dir: %w", err)
	}

	// Reserve a staging path in the target directory (same filesystem, so the
	// final rename is atomic).
	stageFile, err := os.CreateTemp(targetDir, restoreStagingPattern)
	if err != nil {
		return res, fmt.Errorf("restore: create staging file: %w", err)
	}
	staged := stageFile.Name()
	_ = stageFile.Close()
	_ = os.Remove(staged)
	defer func() { _ = os.Remove(staged) }()

	var stagedSHA string
	if ver.Encrypted {
		plainSHA, plainSize, err := decryptSealedToPath(snapAbs, staged, ring)
		if err != nil {
			return res, fmt.Errorf("restore: decrypt staged snapshot failed; target untouched: %w", err)
		}
		if !strings.EqualFold(plainSHA, ver.Manifest.PlaintextSHA256) {
			return res, fmt.Errorf("restore: staged plaintext sha256 %s does not match manifest %s", plainSHA, ver.Manifest.PlaintextSHA256)
		}
		if plainSize != ver.Manifest.PlaintextSize {
			return res, fmt.Errorf("restore: staged plaintext size %d != manifest %d", plainSize, ver.Manifest.PlaintextSize)
		}
		stagedSHA = plainSHA
	} else {
		copiedSHA, _, err := copyFileAtomic(snapAbs, staged, 0o600)
		if err != nil {
			return res, fmt.Errorf("restore: stage snapshot: %w", err)
		}
		if !strings.EqualFold(copiedSHA, ver.Manifest.PlaintextSHA256) {
			return res, fmt.Errorf("restore: staged copy sha256 %s does not match snapshot %s", copiedSHA, ver.Manifest.PlaintextSHA256)
		}
		stagedSHA = copiedSHA
	}

	stagedInfo, err := inspectSnapshot(ctx, staged)
	if err != nil {
		return res, fmt.Errorf("restore: staged snapshot verification failed: %w", err)
	}
	if stagedInfo.SchemaVersion != ver.Manifest.SchemaVersion {
		return res, fmt.Errorf("restore: staged schema version %d != manifest %d", stagedInfo.SchemaVersion, ver.Manifest.SchemaVersion)
	}
	if !stagedInfo.Counts.Equal(ver.Manifest.Counts) {
		return res, fmt.Errorf("restore: staged row census %+v != manifest %+v", stagedInfo.Counts, ver.Manifest.Counts)
	}
	if stagedInfo.SHA256 != stagedSHA {
		return res, fmt.Errorf("restore: staged file changed during verification")
	}
	secretsVerified := 0
	secretsByVersion := map[int]int{}
	if ring != nil && ver.Manifest.Counts.Secrets > 0 {
		secretsVerified, secretsByVersion, err = verifySecretsDecryptable(ctx, staged, ring)
		if err != nil {
			return res, fmt.Errorf("restore: secret decryption proof failed; target untouched: %w", err)
		}
		if int64(secretsVerified) != ver.Manifest.Counts.Secrets {
			return res, fmt.Errorf("restore: decrypted %d secrets but manifest records %d", secretsVerified, ver.Manifest.Counts.Secrets)
		}
	}

	// Mark the target before the first rename so a crash cannot be mistaken for a
	// first install: storage.Open refuses to create a database while this marker
	// exists and the database does not.
	if err := writeRestoreMarker(targetAbs, staged, opts.Now()); err != nil {
		return res, fmt.Errorf("restore: write restore marker: %w", err)
	}
	if hook := restorePublishHook; hook != nil {
		hook("before-publish")
	}
	// Publish: move the old DB (and its WAL sidecars) aside, then rename the
	// verified staging file into place. On failure the old files are restored.
	var preRestore string
	walPath := targetAbs + "-wal"
	shmPath := targetAbs + "-shm"
	if _, statErr := os.Lstat(targetAbs); statErr == nil {
		stamp := opts.Now().UTC().Format("20060102T150405.000000000Z")
		preRestore = targetAbs + ".pre-restore-" + stamp
		if err := os.Rename(targetAbs, preRestore); err != nil {
			_ = clearRestoreMarker(targetAbs)
			return res, fmt.Errorf("restore: move existing target aside: %w", err)
		}
		movedWAL := false
		if _, err := os.Lstat(walPath); err == nil {
			if err := os.Rename(walPath, preRestore+"-wal"); err != nil {
				_ = os.Rename(preRestore, targetAbs)
				_ = clearRestoreMarker(targetAbs)
				return res, fmt.Errorf("restore: move old WAL aside: %w", err)
			}
			movedWAL = true
		}
		if _, err := os.Lstat(shmPath); err == nil {
			if err := os.Rename(shmPath, preRestore+"-shm"); err != nil {
				if movedWAL {
					_ = os.Rename(preRestore+"-wal", walPath)
				}
				_ = os.Rename(preRestore, targetAbs)
				_ = clearRestoreMarker(targetAbs)
				return res, fmt.Errorf("restore: move old SHM aside: %w", err)
			}
		}
	}
	if hook := restorePublishHook; hook != nil {
		hook("between-renames")
	}
	if err := os.Rename(staged, targetAbs); err != nil {
		if preRestore != "" {
			_ = os.Rename(preRestore, targetAbs)
			if _, werr := os.Lstat(preRestore + "-wal"); werr == nil {
				_ = os.Rename(preRestore+"-wal", walPath)
			}
			if _, serr := os.Lstat(preRestore + "-shm"); serr == nil {
				_ = os.Rename(preRestore+"-shm", shmPath)
			}
		}
		_ = clearRestoreMarker(targetAbs)
		return res, fmt.Errorf("restore: publish restored database: %w", err)
	}
	if err := os.Chmod(targetAbs, 0o600); err != nil {
		return res, fmt.Errorf("restore: chmod restored database: %w", err)
	}
	if err := syncDir(targetDir); err != nil {
		return res, fmt.Errorf("restore: sync target dir: %w", err)
	}

	if hook := restorePublishHook; hook != nil {
		hook("after-publish")
	}
	if err := clearRestoreMarker(targetAbs); err != nil {
		return res, fmt.Errorf("restore: clear restore marker: %w", err)
	}
	// Post-restore proof: reopen the published file and repeat every check.
	postInfo, err := inspectSnapshot(ctx, targetAbs)
	if err != nil {
		return res, fmt.Errorf("restore: post-restore verification failed: %w", err)
	}
	if postInfo.SHA256 != ver.Manifest.PlaintextSHA256 {
		return res, fmt.Errorf("restore: post-restore sha256 %s != snapshot plaintext %s", postInfo.SHA256, ver.Manifest.PlaintextSHA256)
	}
	if !postInfo.Counts.Equal(ver.Manifest.Counts) {
		return res, fmt.Errorf("restore: post-restore row census mismatch")
	}
	if ring != nil && ver.Manifest.Counts.Secrets > 0 {
		postVerified, postVersions, err := verifySecretsDecryptable(ctx, targetAbs, ring)
		if err != nil {
			return res, fmt.Errorf("restore: post-restore decryption proof failed: %w", err)
		}
		if postVerified != secretsVerified {
			return res, fmt.Errorf("restore: post-restore decrypted %d secrets, staged copy had %d", postVerified, secretsVerified)
		}
		secretsByVersion = postVersions
	}

	if preRestore != "" {
		if opts.KeepOld {
			res.PreRestoreCopy = preRestore
		} else {
			_ = os.Remove(preRestore)
			_ = os.Remove(preRestore + "-wal")
			_ = os.Remove(preRestore + "-shm")
		}
	}

	res.TargetPath = targetAbs
	res.SchemaVersion = postInfo.SchemaVersion
	res.Counts = postInfo.Counts
	res.IntegrityCheck = postInfo.IntegrityCheck
	res.ForeignKeyViolations = postInfo.ForeignKeyErrors
	res.SecretsVerified = secretsVerified
	res.SecretsByVersion = secretsByVersion
	res.ManifestVerified = true
	return res, nil
}

// verifySecretsDecryptable opens a database read-only and decrypts every
// secrets row with the supplied keyring. It fails closed on the first row that
// cannot be authenticated; plaintext is zeroed immediately and never returned.
func verifySecretsDecryptable(ctx context.Context, dbPath string, ring *security.KeyRing) (int, map[int]int, error) {
	versions := map[int]int{}
	if ring == nil {
		return 0, versions, errors.New("nil keyring")
	}
	db, err := openReadOnly(ctx, dbPath)
	if err != nil {
		return 0, versions, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, secret_kind, ciphertext, nonce, key_version FROM secrets ORDER BY id`)
	if err != nil {
		return 0, versions, fmt.Errorf("list secrets: %w", err)
	}
	defer rows.Close()
	box := security.NewBox(ring)
	total := 0
	for rows.Next() {
		var (
			id       string
			kind     string
			ct, nonc []byte
			version  int
		)
		if err := rows.Scan(&id, &kind, &ct, &nonc, &version); err != nil {
			return total, versions, fmt.Errorf("scan secret: %w", err)
		}
		pt, err := box.Open(kind, id, security.Sealed{Ciphertext: ct, Nonce: nonc, KeyVersion: version})
		if err != nil {
			return total, versions, fmt.Errorf("secret %s (kind=%s, key_version=%d) is not decryptable: %w", id, kind, version, err)
		}
		for i := range pt {
			pt[i] = 0
		}
		total++
		versions[version]++
	}
	if err := rows.Err(); err != nil {
		return total, versions, err
	}
	return total, versions, nil
}

// samePath compares absolute paths after cleaning them.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// restoreMarkerSuffix marks a target whose publish started but did not finish.
// storage.Open refuses to create a fresh database next to it, so an interrupted
// restore can never masquerade as a first install.
const restoreMarkerSuffix = ".restore-in-progress"

// restorePublishHook is a test seam (nil in production). Tests use it to stop the
// publish at a named stage and inspect the state a crash would leave behind.
var restorePublishHook func(stage string)

func writeRestoreMarker(target, snapshot string, now time.Time) error {
	marker := target + restoreMarkerSuffix
	// O_EXCL, never O_TRUNC: a marker that already exists - including a symlink
	// planted at this path - must not be followed or truncated, because a restore
	// normally runs as root against a directory the service account can write.
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists; a previous restore did not complete - inspect it and remove it before retrying", marker)
		}
		return err
	}
	if _, err := fmt.Fprintf(f, "%s\n%s\n", filepath.Base(snapshot), now.UTC().Format(time.RFC3339)); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(target))
}

func clearRestoreMarker(target string) error {
	if err := os.Remove(target + restoreMarkerSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(target))
}
