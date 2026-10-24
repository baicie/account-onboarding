// Command loadsmoke runs the offline R4 load/concurrency smoke: N concurrent
// runs plus M sequential runs through the real HTTP API, real SQLite and the
// real workflow engine, with in-memory fake providers. It reports p50/p95/p99
// latency, error count and duplicate identity/idempotency checks.
//
// Usage:
//
//	go run ./internal/ops/cmd/loadsmoke [-concurrency 5] [-sequential 30] \
//	    [-work-dir DIR] [-log-file FILE]
//
// Exit codes: 0 pass, 1 fail (errors, incomplete runs or duplicates).
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
	concurrency := flag.Int("concurrency", 5, "concurrent runs")
	sequential := flag.Int("sequential", 30, "sequential runs")
	timeout := flag.Duration("timeout", 3*time.Minute, "whole smoke timeout")
	workDir := flag.String("work-dir", "", "keep the SQLite database in this directory (for evidence)")
	logFile := flag.String("log-file", "", "write redacted JSON service logs to this file")
	keyringOut := flag.String("keyring-out", "", "write the test master key (0600) for restore/rotation evidence")
	flag.Parse()
	if *concurrency < 1 || *sequential < 1 {
		cliutil.UsageError("loadsmoke: -concurrency and -sequential must be >= 1")
	}
	if *workDir != "" {
		if err := os.MkdirAll(*workDir, 0o700); err != nil {
			cliutil.Fail("create work dir: %v", err)
		}
	}
	var logWriter *os.File
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			cliutil.Fail("open log file: %v", err)
		}
		logWriter = f
		defer f.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	rep, err := ops.RunLoadSmoke(ctx, ops.LoadOptions{
		Concurrency:    *concurrency,
		Sequential:     *sequential,
		Timeout:        *timeout,
		Dir:            *workDir,
		LogWriter:      logWriter,
		KeyringOutPath: *keyringOut,
	})
	cliutil.PrintJSON(rep)
	if err != nil {
		cliutil.Fail("load smoke error: %v", err)
	}
	if !rep.Pass {
		cliutil.Fail("load smoke failed: %d errors, %d/%d completed", rep.ErrorCount, rep.CompletedRuns, rep.ExpectedRuns)
	}
}
