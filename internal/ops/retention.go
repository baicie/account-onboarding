package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/security"
)

// RetentionOptions configures pruning of a snapshot directory.
//
// The policy is deliberately conservative: the newest valid snapshot is never
// deleted, a floor of valid recovery points is always preserved, and a snapshot
// is only removed when *every* configured limit says it may go.
type RetentionOptions struct {
	// Dir is the snapshot directory.
	Dir string
	// MaxAge deletes snapshots older than this. Zero disables the age limit.
	MaxAge time.Duration
	// MaxCount keeps at most this many valid snapshots. Zero disables it.
	MaxCount int
	// MinValid is the floor of valid snapshots that must survive. Values below
	// one are treated as one: retention must never delete the last recovery
	// point.
	MinValid int
	// KeyRingPath is required to verify encrypted (.sqlite.enc) snapshots.
	KeyRingPath string
	// PurgeInvalid also removes snapshots that fail verification. Off by
	// default: an unreadable snapshot is reported, not silently destroyed.
	PurgeInvalid bool
	// DryRun reports the plan without deleting anything.
	DryRun bool
	// Now overrides the clock (tests).
	Now time.Time
	// ReportPath overrides where the JSON report is written.
	ReportPath string
}

// SnapshotCandidate is one snapshot file and the decision taken about it.
type SnapshotCandidate struct {
	SnapshotPath  string    `json:"snapshot_path"`
	ManifestPath  string    `json:"manifest_path"`
	SizeBytes     int64     `json:"size_bytes"`
	ModifiedAt    time.Time `json:"modified_at"`
	Valid         bool      `json:"valid"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Action        string    `json:"action"`
}

// RetentionResult is the evidence produced by ApplyRetention.
type RetentionResult struct {
	Dir        string              `json:"dir"`
	MaxAge     string              `json:"max_age,omitempty"`
	MaxCount   int                 `json:"max_count,omitempty"`
	MinValid   int                 `json:"min_valid"`
	DryRun     bool                `json:"dry_run"`
	Scanned    int                 `json:"scanned"`
	Valid      int                 `json:"valid"`
	Kept       int                 `json:"kept"`
	Deleted    int                 `json:"deleted"`
	Failures   []string            `json:"failures,omitempty"`
	Candidates []SnapshotCandidate `json:"candidates"`
	ReportPath string              `json:"report_path,omitempty"`
	FinishedAt time.Time           `json:"finished_at"`
}

// Actions recorded per candidate.
const (
	retentionKeep    = "keep"
	retentionDelete  = "delete"
	retentionDryRun  = "would-delete"
	retentionInvalid = "invalid"
	// retentionUnverifiable marks a snapshot that could not be checked at all
	// (missing manifest, missing or wrong keyring). It is kept: unverifiable does
	// not mean worthless.
	retentionUnverifiable = "unverifiable"
)

// ApplyRetention prunes a snapshot directory under the configured policy.
func ApplyRetention(ctx context.Context, opts RetentionOptions) (RetentionResult, error) {
	res := RetentionResult{Dir: opts.Dir, MaxCount: opts.MaxCount, DryRun: opts.DryRun}
	if strings.TrimSpace(opts.Dir) == "" {
		return res, errors.New("retention: snapshot directory is required")
	}
	info, err := os.Stat(opts.Dir)
	if err != nil {
		return res, fmt.Errorf("retention: stat directory: %w", err)
	}
	if !info.IsDir() {
		return res, fmt.Errorf("retention: %q is not a directory", opts.Dir)
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	minValid := opts.MinValid
	if minValid < 1 {
		minValid = 1
	}
	res.MinValid = minValid
	if opts.MaxAge > 0 {
		res.MaxAge = opts.MaxAge.String()
	}

	entries, err := os.ReadDir(opts.Dir)
	if err != nil {
		return res, fmt.Errorf("retention: read directory: %w", err)
	}
	var ring *security.KeyRing
	if opts.KeyRingPath != "" {
		ring, err = security.LoadKeyRing(opts.KeyRingPath)
		if err != nil {
			return res, fmt.Errorf("retention: load keyring: %w", err)
		}
	}

	var valid, invalid []SnapshotCandidate
	for _, e := range entries {
		if e.IsDir() || !isSnapshotFile(e.Name()) {
			continue
		}
		path := filepath.Join(opts.Dir, e.Name())
		cand := SnapshotCandidate{SnapshotPath: path, ManifestPath: ManifestPathFor(path)}
		if st, statErr := os.Stat(path); statErr == nil {
			cand.SizeBytes = st.Size()
			cand.ModifiedAt = st.ModTime().UTC()
		}
		if _, statErr := os.Stat(cand.ManifestPath); statErr != nil {
			cand.Reason = "manifest missing"
			invalid = append(invalid, cand)
			continue
		}
		var ver Verification
		var verErr error
		if strings.HasSuffix(path, ".enc") {
			if ring == nil {
				cand.Reason = "keyring required to verify an encrypted snapshot"
				invalid = append(invalid, cand)
				continue
			}
			ver, verErr = VerifyBackupWithKeyring(ctx, path, cand.ManifestPath, ring)
		} else {
			ver, verErr = VerifyBackup(ctx, path, cand.ManifestPath)
		}
		if verErr != nil {
			cand.Reason = verErr.Error()
			invalid = append(invalid, cand)
			continue
		}
		cand.Valid = true
		cand.SchemaVersion = ver.Manifest.SchemaVersion
		valid = append(valid, cand)
	}

	res.Scanned = len(valid) + len(invalid)
	res.Valid = len(valid)

	// Newest first: both limits are monotone in recency, so the keep set is
	// always a prefix of this order.
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].ModifiedAt.Equal(valid[j].ModifiedAt) {
			return valid[i].SnapshotPath > valid[j].SnapshotPath
		}
		return valid[i].ModifiedAt.After(valid[j].ModifiedAt)
	})

	// Both limits are monotone in recency, so the keep set is a prefix: keep the
	// first max(MaxCount, count-within-MaxAge) newest valid snapshots, never fewer
	// than MinValid.
	keepCount := len(valid)
	if opts.MaxCount > 0 || opts.MaxAge > 0 {
		keepCount = 0
		if opts.MaxCount > 0 {
			keepCount = opts.MaxCount
		}
		if opts.MaxAge > 0 {
			within := 0
			for _, c := range valid {
				if now.Sub(c.ModifiedAt) <= opts.MaxAge {
					within++
				}
			}
			if within > keepCount {
				keepCount = within
			}
		}
		if keepCount < minValid {
			keepCount = minValid
		}
		if keepCount > len(valid) {
			keepCount = len(valid)
		}
	}

	for i, c := range valid {
		if i < keepCount {
			c.Action = retentionKeep
			res.Kept++
			res.Candidates = append(res.Candidates, c)
			continue
		}
		c.Action = retentionDelete
		if opts.DryRun {
			c.Action = retentionDryRun
		}
		if err := deleteSnapshotPair(c.SnapshotPath, c.ManifestPath, opts.DryRun); err != nil {
			res.Failures = append(res.Failures, err.Error())
			c.Action = retentionKeep
			res.Kept++
			res.Candidates = append(res.Candidates, c)
			continue
		}
		if !opts.DryRun {
			res.Deleted++
		}
		res.Candidates = append(res.Candidates, c)
	}

	// Purging is deliberately constrained. An unverifiable snapshot is not a
	// worthless snapshot: a wrong or missing keyring makes perfectly good backups
	// fail verification, so a purge may never empty the directory and may never
	// take the recovery set below the verified count or the MinValid floor.
	purgeBlocked := ""
	if opts.PurgeInvalid {
		switch {
		case len(valid) == 0:
			purgeBlocked = "refusing to purge: no snapshot verifies with the supplied keyring"
		case len(valid) < minValid:
			purgeBlocked = fmt.Sprintf("refusing to purge: only %d verified snapshot(s) but -min-valid is %d", len(valid), minValid)
		}
	}
	remaining := len(valid) + len(invalid)
	for _, c := range invalid {
		c.Action = retentionInvalid
		if strings.Contains(c.Reason, "keyring") || strings.Contains(c.Reason, "manifest") {
			c.Action = retentionUnverifiable
		}
		switch {
		case !opts.PurgeInvalid, purgeBlocked != "":
			// keep: nothing may be purged in this state
		case remaining-1 < minValid || remaining-1 < len(valid):
			// keep: the floor (or the verified count) would be crossed
		default:
			if opts.DryRun {
				c.Action = retentionDryRun
			} else if err := deleteSnapshotPair(c.SnapshotPath, c.ManifestPath, false); err != nil {
				res.Failures = append(res.Failures, err.Error())
				c.Action = retentionInvalid
			} else {
				c.Action = retentionDelete
				res.Deleted++
				remaining--
			}
		}
		res.Candidates = append(res.Candidates, c)
	}
	if purgeBlocked != "" {
		res.Failures = append(res.Failures, purgeBlocked)
	}

	res.FinishedAt = now
	reportPath := opts.ReportPath
	if reportPath == "" {
		reportPath = filepath.Join(opts.Dir, fmt.Sprintf("retention-%s.json", now.UTC().Format("20060102T150405Z")))
	}
	if err := writeJSONAtomic(reportPath, res); err != nil {
		res.Failures = append(res.Failures, fmt.Sprintf("write report: %v", err))
	} else {
		res.ReportPath = reportPath
	}
	if len(res.Failures) > 0 {
		return res, fmt.Errorf("retention: %d failure(s): %s", len(res.Failures), strings.Join(res.Failures, "; "))
	}
	return res, nil
}

// isSnapshotFile reports whether name looks like a snapshot this package wrote.
func isSnapshotFile(name string) bool {
	if !strings.HasPrefix(name, "state-") {
		return false
	}
	if strings.HasSuffix(name, ".manifest.json") || strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
		return false
	}
	return strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite.enc")
}

// deleteSnapshotPair removes a snapshot and then its manifest. The snapshot goes
// first: if that fails, the pair stays consistent and the failure is reported.
func deleteSnapshotPair(snapshotPath, manifestPath string, dryRun bool) error {
	if dryRun {
		return nil
	}
	if err := os.Remove(snapshotPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", filepath.Base(snapshotPath), err)
	}
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", filepath.Base(manifestPath), err)
	}
	return nil
}

// writeJSONAtomic replaces path with the encoded value in one rename.
func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
