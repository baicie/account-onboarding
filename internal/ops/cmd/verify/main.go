// Command verify proves a backup snapshot matches its manifest: sha256, size,
// schema version, PRAGMA integrity_check, foreign keys and row census.
//
// Usage:
//
//	go run ./internal/ops/cmd/verify -snapshot /backups/state-...Z.sqlite \
//	    [-manifest /backups/state-...Z.sqlite.manifest.json] \
//	    [-keyring /run/secrets/onboarding_master_key]
//
// The keyring is mandatory for an AES-256-GCM encrypted snapshot (.sqlite.enc):
// verification decrypts the payload, checks its sha256/size and runs
// integrity_check on the decrypted SQLite file. A wrong/missing key fails closed.
package main

import (
	"context"
	"flag"
	"time"

	"github.com/baicie/account-onboarding/internal/ops"
	"github.com/baicie/account-onboarding/internal/ops/cliutil"
	"github.com/baicie/account-onboarding/internal/security"
)

func main() {
	snapshot := flag.String("snapshot", "", "snapshot path (required)")
	manifest := flag.String("manifest", "", "manifest path (default: <snapshot>.manifest.json)")
	keyring := flag.String("keyring", "", "keyring file (required for encrypted snapshots)")
	timeout := flag.Duration("timeout", 5*time.Minute, "verification timeout")
	flag.Parse()
	if *snapshot == "" {
		cliutil.UsageError("verify: -snapshot is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var ring *security.KeyRing
	if *keyring != "" {
		loaded, err := security.LoadKeyRing(*keyring)
		if err != nil {
			cliutil.Fail("load keyring: %v", err)
		}
		ring = loaded
	}
	ver, err := ops.VerifyBackupWithKeyring(ctx, *snapshot, *manifest, ring)
	if err != nil {
		cliutil.Fail("verification failed: %v", err)
	}
	cliutil.PrintJSON(ver)
}
