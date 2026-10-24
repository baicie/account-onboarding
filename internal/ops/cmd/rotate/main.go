// Command rotate re-encrypts every secret under the current key version in one
// transaction. It is the implementation behind scripts/rotate_key.sh.
//
// Usage:
//
//	go run ./internal/ops/cmd/rotate -db /app/data/state.sqlite \
//	    -keyring /run/secrets/onboarding_master_key [-dry-run]
//
// The keyring must contain the new current key AND every older version:
//
//	{"current":2,"keys":{"1":"<base64>","2":"<base64>"}}
//
// Stop the service before a real (non-dry-run) rotation.
package main

import (
	"context"
	"flag"
	"time"

	"github.com/baicie/account-onboarding/internal/ops"
	"github.com/baicie/account-onboarding/internal/ops/cliutil"
)

func main() {
	db := flag.String("db", "", "database path (required)")
	keyring := flag.String("keyring", "", "keyring file (required)")
	dryRun := flag.Bool("dry-run", false, "report what would change without writing")
	timeout := flag.Duration("timeout", 5*time.Minute, "rotation timeout")
	flag.Parse()
	if *db == "" || *keyring == "" {
		cliutil.UsageError("rotate: -db and -keyring are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := ops.RotateSecrets(ctx, ops.RotationOptions{
		DBPath:      *db,
		KeyRingPath: *keyring,
		DryRun:      *dryRun,
	})
	if err != nil {
		cliutil.Fail("rotation failed (no data changed on failure): %v", err)
	}
	cliutil.PrintJSON(res)
}
