// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FileLock is an exclusive lock over a data directory. The interface exists so
// the platform difference lives in one build tagged file instead of a runtime
// branch, and so a scope that cannot lock is closed rather than left to guess.
//
// Only one MarxAgent process may own a scope. The pure Go SQLite driver has no
// cross process file locking, so this lock is what keeps two processes from
// interleaving writes into the same ledger.
type FileLock struct {
	path string
	mu   sync.Mutex
	// platform state, set by lockFile
	locked bool
	handle platformHandle
}

// NewFileLock prepares a lock without taking it.
func NewFileLock(path string) (*FileLock, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("storage: lock path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("storage: create lock directory: %w", err)
	}
	return &FileLock{path: path}, nil
}

// LockFile prepares and takes a lock.
func LockFile(path string) (*FileLock, error) {
	lock, err := NewFileLock(path)
	if err != nil {
		return nil, err
	}
	if err := lock.Acquire(); err != nil {
		return nil, err
	}
	return lock, nil
}

// Path reports the lock file path.
func (l *FileLock) Path() string { return l.path }

// Acquire takes the lock, failing if another process holds it.
func (l *FileLock) Acquire() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locked {
		return nil
	}
	handle, err := lockFile(l.path)
	if err != nil {
		return fmt.Errorf("storage: acquire scope lock: %w", err)
	}
	l.handle = handle
	l.locked = true
	return nil
}

// Release drops the lock. Releasing an unheld lock is a no op, which keeps the
// shutdown path free of ordering concerns.
func (l *FileLock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.locked {
		return nil
	}
	err := unlockFile(l.handle)
	l.handle = nil
	l.locked = false
	if err != nil {
		return fmt.Errorf("storage: release scope lock: %w", err)
	}
	return nil
}

// Held reports whether this process holds the lock.
func (l *FileLock) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locked
}
