//go:build unix

package storage

import (
	"fmt"
	"os"
	"syscall"
)

// instanceLock is an advisory exclusive lock on "<db>.lock". It enforces the
// documented single-writer deployment: two onboardd processes (or two test
// servers) cannot open the same database concurrently. The kernel releases the
// lock automatically when the process exits, including SIGKILL.
type instanceLock struct {
	file *os.File
}

func acquireInstanceLock(dbPath string) (*instanceLock, error) {
	lockPath := dbPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("storage: open instance lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("storage: another onboardd instance already owns %s (single-writer only): %w", dbPath, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	}
	return &instanceLock{file: f}, nil
}

func (l *instanceLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
