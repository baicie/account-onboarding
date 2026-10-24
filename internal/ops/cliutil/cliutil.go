// Package cliutil holds the small output/exit-code helpers shared by the
// internal/ops/cmd/* operator tools.
package cliutil

import (
	"encoding/json"
	"fmt"
	"os"
)

// PrintJSON writes indented JSON to stdout.
func PrintJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// Fail prints a redacted error summary to stderr and exits with code 1.
// Callers must never pass secret material.
func Fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

// UsageError prints a usage error and exits with code 2.
func UsageError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "USAGE: "+format+"\n", args...)
	os.Exit(2)
}
