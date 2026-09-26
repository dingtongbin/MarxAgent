// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package storage

import (
	"fmt"
	"os"
	"syscall"
)

// platformHandle is the file descriptor of the lock on this platform.
type platformHandle = *os.File

// lockFile takes an exclusive advisory lock. The lock belongs to the open file
// description, so it is released automatically if the process dies, which is
// what stops a crashed instance from leaving a scope permanently unusable.
func lockFile(path string) (platformHandle, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s is held by another process", err, path)
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.WriteString(fmt.Sprint(os.Getpid())); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func unlockFile(handle platformHandle) error {
	if handle == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(handle.Fd()), syscall.LOCK_UN)
	closeErr := handle.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
