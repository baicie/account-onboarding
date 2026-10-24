//go:build windows

package storage

import "fmt"

// instanceLock is not implemented for Windows; the supported targets are Linux
// and macOS (see README). Returning an error keeps the single-writer contract
// explicit instead of silently allowing concurrent writers.
type instanceLock struct{}

func acquireInstanceLock(string) (*instanceLock, error) {
	return nil, fmt.Errorf("storage: single-writer instance lock is not supported on windows")
}

func (l *instanceLock) release() {}
