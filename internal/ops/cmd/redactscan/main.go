// Command redactscan scans local log files and/or persisted event rows for
// leaked secrets (JWTs, bearer tokens, sensitive assignments, long token shapes,
// known secret values) and exits non-zero on findings.
//
// Usage:
//
//	go run ./internal/ops/cmd/redactscan -file /var/log/onboardd.log
//	go run ./internal/ops/cmd/redactscan -event-db /app/data/state.sqlite
//
// Exit codes: 0 clean, 2 findings, 1 scanner error. Findings never echo the
// matched value.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/baicie/account-onboarding/internal/ops"
	"github.com/baicie/account-onboarding/internal/ops/cliutil"
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func main() {
	var files multiFlag
	var known multiFlag
	flag.Var(&files, "file", "log/text file to scan (repeatable)")
	flag.Var(&known, "known-secret", "exact secret value that must not appear (repeatable; never printed)")
	eventDB := flag.String("event-db", "", "SQLite database whose events/steps/audit rows are scanned")
	secretsFile := flag.String("known-secrets-file", "", "file with one known secret value per line")
	maxFindings := flag.Int("max-findings", 200, "maximum findings to report")
	timeout := flag.Duration("timeout", 5*time.Minute, "scan timeout")
	flag.Parse()
	if len(files) == 0 && *eventDB == "" {
		cliutil.UsageError("redactscan: at least one -file or -event-db is required")
	}
	if *secretsFile != "" {
		raw, err := os.ReadFile(*secretsFile)
		if err != nil {
			cliutil.Fail("read known secrets file: %v", err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			known = append(known, line)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	opts := ops.ScanOptions{KnownSecrets: known, MaxFindings: *maxFindings}
	report := ops.ScanReport{FindingsByKind: map[string]int{}}
	for _, path := range files {
		rep, err := ops.ScanFile(path, opts)
		if err != nil {
			cliutil.Fail("scan %s: %v", path, err)
		}
		report.Merge(rep)
	}
	if *eventDB != "" {
		rep, err := ops.ScanDatabase(ctx, *eventDB, opts)
		if err != nil {
			cliutil.Fail("scan database: %v", err)
		}
		report.Merge(rep)
	}
	cliutil.PrintJSON(report)
	if !report.Clean() {
		fmt.Fprintf(os.Stderr, "ERROR: sensitive-data findings: %d (see JSON above; values are redacted)\n", len(report.Findings))
		os.Exit(2)
	}
}
