// Package filelock provides a small advisory whole-file lock used to serialize
// read-modify-write cycles on shared JSON state under NM_HOME across
// processes. It is the single owner of the OS locking primitive so callers do
// not each carry their own platform split.
//
// This is not the daemon singleton lock: that one is held for a process
// lifetime and is what prevents two daemons owning one root (see
// internal/daemon/lock.go). These locks are short-lived and released as soon
// as the state file has been rewritten.
package filelock

import (
	"fmt"
	"os"
)

// Lock is a held advisory lock on a lock file.
type Lock struct {
	file *os.File
}

// Acquire blocks until the lock at path is held. The lock file is created if
// it does not exist and is never removed, so an unlink can never drop a lock a
// concurrent holder still owns.
func Acquire(path string) (*Lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock file: %w", err)
	}
	return &Lock{file: file}, nil
}

// Release unlocks and closes the lock file. It is safe on a nil Lock so
// callers can defer it unconditionally.
func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockFile(l.file)
	_ = l.file.Close()
}
