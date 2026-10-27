// Command retention prunes a snapshot directory under an explicit policy.
//
// Usage:
//
//	go run ./internal/ops/cmd/retention -dir /app/backups //	    [-max-age 720h] [-max-count 10] [-min-valid 2] //	    [-keyring /run/secrets/onboarding_master_key] [-purge-invalid] [-dry-run]
//
// The newest valid snapshot and at least -min-valid recovery points always
// survive. The JSON report is written atomically into the snapshot directory and
// is the retention audit record.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/baicie/account-onboarding/internal/ops"
)

func main() {
	dir := flag.String("dir", "", "snapshot directory (required)")
	maxAge := flag.Duration("max-age", 0, "delete snapshots older than this (0 disables)")
	maxCount := flag.Int("max-count", 0, "keep at most this many valid snapshots (0 disables)")
	minValid := flag.Int("min-valid", 1, "minimum valid snapshots to preserve")
	keyring := flag.String("keyring", "", "keyring file, required for encrypted snapshots")
	purgeInvalid := flag.Bool("purge-invalid", false, "also delete snapshots that fail verification")
	dryRun := flag.Bool("dry-run", false, "report the plan without deleting")
	timeout := flag.Duration("timeout", 5*time.Minute, "retention timeout")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "retention: -dir is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := ops.ApplyRetention(ctx, ops.RetentionOptions{
		Dir:          *dir,
		MaxAge:       *maxAge,
		MaxCount:     *maxCount,
		MinValid:     *minValid,
		KeyRingPath:  *keyring,
		PurgeInvalid: *purgeInvalid,
		DryRun:       *dryRun,
	})
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
	if err != nil {
		fmt.Fprintln(os.Stderr, "retention:", err)
		os.Exit(1)
	}
}
