// Command restore verifies a snapshot and restores it into a target database
// path after proving integrity, row counts and secret decryption.
//
// Usage:
//
//	go run ./internal/ops/cmd/restore -snapshot FILE -target PATH \
//	    -keyring /run/secrets/onboarding_master_key [-live-db PATH] [-force]
//
// The target is only replaced when --force is given; an existing live database
// is always refused without it.
package main

import (
	"context"
	"flag"
	"time"

	"github.com/baicie/account-onboarding/internal/ops"
	"github.com/baicie/account-onboarding/internal/ops/cliutil"
)

func main() {
	snapshot := flag.String("snapshot", "", "snapshot path (required)")
	manifest := flag.String("manifest", "", "manifest path (default: <snapshot>.manifest.json)")
	target := flag.String("target", "", "target database path (required)")
	liveDB := flag.String("live-db", "", "service database path; overwriting it requires -force")
	keyring := flag.String("keyring", "", "keyring file used to prove secret decryption")
	force := flag.Bool("force", false, "replace an existing target database")
	keepOld := flag.Bool("keep-old", false, "keep the replaced database as <target>.pre-restore-<timestamp>")
	timeout := flag.Duration("timeout", 10*time.Minute, "restore timeout")
	flag.Parse()
	if *snapshot == "" || *target == "" {
		cliutil.UsageError("restore: -snapshot and -target are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := ops.Restore(ctx, ops.RestoreOptions{
		SnapshotPath: *snapshot,
		ManifestPath: *manifest,
		TargetPath:   *target,
		LiveDBPath:   *liveDB,
		KeyRingPath:  *keyring,
		Force:        *force,
		KeepOld:      *keepOld,
	})
	if err != nil {
		cliutil.Fail("restore failed: %v", err)
	}
	cliutil.PrintJSON(res)
}
