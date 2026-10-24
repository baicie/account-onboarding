// Command backup creates a consistent, checksummed SQLite snapshot of a live
// WAL database. It is the implementation behind scripts/backup.sh.
//
// Usage:
//
//	go run ./internal/ops/cmd/backup -db /app/data/state.sqlite [-out-dir DIR] \
//	    [-encrypt -keyring /run/secrets/onboarding_master_key]
//
// -encrypt seals the snapshot with AES-256-GCM under the keyring current
// version; the output filename ends in ".sqlite.enc" and the manifest records
// encryption/key_version/plaintext_sha256. The JSON result is written to stdout;
// a missing/unhealthy database or missing keyring exits 1.
package main

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/baicie/account-onboarding/internal/ops"
	"github.com/baicie/account-onboarding/internal/ops/cliutil"
)

func main() {
	db := flag.String("db", "", "path to the live SQLite database (required)")
	outDir := flag.String("out-dir", "", "snapshot directory (default: database directory)")
	encrypt := flag.Bool("encrypt", false, "encrypt the snapshot at rest with AES-256-GCM")
	keyring := flag.String("keyring", "", "keyring file used with -encrypt (raw/hex/base64 32-byte key or JSON envelope)")
	timeout := flag.Duration("timeout", 5*time.Minute, "backup timeout")
	flag.Parse()
	if *db == "" {
		cliutil.UsageError("backup: -db is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := ops.Backup(ctx, ops.BackupOptions{
		DBPath:      *db,
		OutDir:      *outDir,
		Encrypt:     *encrypt,
		KeyRingPath: *keyring,
	})
	if err != nil {
		cliutil.Fail("backup failed: %v", err)
	}
	cliutil.PrintJSON(res)
	if _, err := os.Stat(res.SnapshotPath); err != nil {
		cliutil.Fail("backup did not publish a snapshot: %v", err)
	}
}
