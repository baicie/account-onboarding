// Command keyring manages the JSON master-key envelope used by the service and
// by rotate/restore tooling. It never prints key material.
//
// Usage:
//
//	go run ./internal/ops/cmd/keyring init   -out /run/secrets/onboarding_master_key
//	go run ./internal/ops/cmd/keyring rotate -in OLD -out NEW
//	go run ./internal/ops/cmd/keyring show   -in KEYRING
//
// "rotate" generates a new random current key (version = max+1) and keeps every
// older version in the new envelope, which is exactly what an online
// re-encryption pass needs. The operator replaces the keyring file only after
// scripts/rotate_key.sh has verified the re-encryption.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/baicie/account-onboarding/internal/ops/cliutil"
)

type envelope struct {
	Current int               `json:"current"`
	Keys    map[string]string `json:"keys"`
}

func main() {
	if len(os.Args) < 2 {
		cliutil.UsageError("keyring <init|rotate|show> [flags]")
	}
	switch os.Args[1] {
	case "init":
		runInit(os.Args[2:])
	case "rotate":
		runRotate(os.Args[2:])
	case "show":
		runShow(os.Args[2:])
	default:
		cliutil.UsageError("unknown keyring subcommand %q (want init|rotate|show)", os.Args[1])
	}
}

func runInit(args []string) {
	fs := flag.NewFlagSet("keyring init", flag.ExitOnError)
	out := fs.String("out", "", "output keyring file (required)")
	force := fs.Bool("force", false, "overwrite an existing file")
	_ = fs.Parse(args)
	if *out == "" {
		cliutil.UsageError("keyring init: -out is required")
	}
	key, err := newKey()
	if err != nil {
		cliutil.Fail("generate key: %v", err)
	}
	if err := writeEnvelope(*out, 1, map[int][]byte{1: key}, *force); err != nil {
		cliutil.Fail("%v", err)
	}
	cliutil.PrintJSON(map[string]any{"written": *out, "current": 1, "versions": []int{1}})
}

func runRotate(args []string) {
	fs := flag.NewFlagSet("keyring rotate", flag.ExitOnError)
	in := fs.String("in", "", "existing keyring file (required)")
	out := fs.String("out", "", "new keyring file (required)")
	force := fs.Bool("force", false, "overwrite an existing output file")
	_ = fs.Parse(args)
	if *in == "" || *out == "" {
		cliutil.UsageError("keyring rotate: -in and -out are required")
	}
	current, keys, err := readEnvelope(*in)
	if err != nil {
		cliutil.Fail("%v", err)
	}
	if current >= 1<<30 {
		cliutil.Fail("keyring rotate: version counter exhausted")
	}
	newKey, err := newKey()
	if err != nil {
		cliutil.Fail("generate key: %v", err)
	}
	next := current + 1
	keys[next] = newKey
	if err := writeEnvelope(*out, next, keys, *force); err != nil {
		cliutil.Fail("%v", err)
	}
	versions := make([]int, 0, len(keys))
	for v := range keys {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	cliutil.PrintJSON(map[string]any{"written": *out, "current": next, "versions": versions})
}

func runShow(args []string) {
	fs := flag.NewFlagSet("keyring show", flag.ExitOnError)
	in := fs.String("in", "", "keyring file (required)")
	_ = fs.Parse(args)
	if *in == "" {
		cliutil.UsageError("keyring show: -in is required")
	}
	current, keys, err := readEnvelope(*in)
	if err != nil {
		cliutil.Fail("%v", err)
	}
	versions := make([]int, 0, len(keys))
	for v := range keys {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	cliutil.PrintJSON(map[string]any{"current": current, "versions": versions})
}

func newKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

func writeEnvelope(path string, current int, keys map[int][]byte, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing keyring %q (use -force)", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create keyring dir: %w", err)
	}
	env := envelope{Current: current, Keys: map[string]string{}}
	for v, key := range keys {
		if len(key) != 32 {
			return fmt.Errorf("version %d key is not 32 bytes", v)
		}
		env.Keys[fmt.Sprintf("%d", v)] = base64.StdEncoding.EncodeToString(key)
	}
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Write to a temp file in the same directory, fsync, chmod 0600, rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(raw); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// readEnvelope accepts a JSON envelope or a single raw/hex/base64 32-byte key
// (treated as version 1). It never logs key material.
func readEnvelope(path string) (int, map[int][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, fmt.Errorf("read keyring: %w", err)
	}
	if len(raw) == 32 {
		return 1, map[int][]byte{1: append([]byte(nil), raw...)}, nil
	}
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, "{") {
		var env envelope
		if err := json.Unmarshal([]byte(text), &env); err != nil {
			return 0, nil, fmt.Errorf("decode keyring envelope: %w", err)
		}
		if env.Current < 1 {
			env.Current = 1
		}
		keys := map[int][]byte{}
		for vs, encoded := range env.Keys {
			var version int
			if _, err := fmt.Sscanf(vs, "%d", &version); err != nil || version < 1 {
				return 0, nil, fmt.Errorf("keyring version %q is not a positive integer", vs)
			}
			key, err := decodeKey(encoded)
			if err != nil {
				return 0, nil, fmt.Errorf("keyring version %d: %w", version, err)
			}
			keys[version] = key
		}
		if _, ok := keys[env.Current]; !ok {
			return 0, nil, fmt.Errorf("keyring current version %d is missing", env.Current)
		}
		return env.Current, keys, nil
	}
	key, err := decodeKey(text)
	if err != nil {
		return 0, nil, err
	}
	return 1, map[int][]byte{1: key}, nil
}

func decodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 32 {
		return []byte(s), nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("key must be 32 bytes encoded as raw/hex/base64")
}
