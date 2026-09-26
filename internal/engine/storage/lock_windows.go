// SPDX-License-Identifier: Apache-2.0

//go:build windows

package storage

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// The constants come from the platform headers rather than the Windows SDK, so
// the build keeps working without cgo and without a toolchain installed.
const (
	lockFileExclusiveFlag = 0x00000002
	lockFileFailImmediate = 0x00000001
	allBytes              = ^uint32(0)
)

// overlapped is the structure LockFileEx requires. It is allocated on the heap
// and kept for as long as the lock is held, because the structure belongs to
// the lock rather than to the call: a stack local would leave the kernel
// holding an address whose lifetime it cannot check.
type overlapped struct {
	internal uintptr
	high     uint32
	low      uint32
	event    syscall.Handle
}

// platformHandle is the state of the lock on this platform.
type platformHandle = *windowsLock

// windowsLock keeps everything the platform call needs to undo itself.
type windowsLock struct {
	file       *os.File
	overlapped *overlapped
}

// lockFile takes an exclusive lock over the whole file. The lock is owned by
// the handle, so the operating system releases it when the process dies, which
// is what keeps a crashed instance from leaving a scope unusable.
func lockFile(path string) (*windowsLock, error) {
	// Share mode allows reads, so recovery and a query tool can inspect a scope
	// while it is locked, while a second writer is refused.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	state := &windowsLock{file: file, overlapped: &overlapped{}}
	result, _, _ := procLockFileEx.Call(
		file.Fd(),
		uintptr(lockFileExclusiveFlag|lockFileFailImmediate),
		0,
		uintptr(allBytes),
		uintptr(allBytes),
		uintptr(unsafe.Pointer(state.overlapped)),
	)
	if result == 0 {
		_ = file.Close()
		return nil, fmt.Errorf("the scope at %s is held by another process", path)
	}
	// The owner is recorded without truncating: the byte range is locked now, so
	// the write has to fit inside it rather than change its length.
	if _, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		_ = state.unlock()
		_ = file.Close()
		return nil, err
	}
	return state, nil
}

func unlockFile(handle *windowsLock) error {
	if handle == nil {
		return nil
	}
	return handle.unlock()
}

func (l *windowsLock) unlock() error {
	_, _, _ = procUnlockFileEx.Call(
		l.file.Fd(),
		0,
		uintptr(allBytes),
		uintptr(allBytes),
		uintptr(unsafe.Pointer(l.overlapped)),
	)
	return l.file.Close()
}
