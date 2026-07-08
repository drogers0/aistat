//go:build windows

// Package winlock provides whole-file advisory locking on Windows via
// kernel32!LockFileEx / UnlockFileEx, the analog of syscall.Flock(LOCK_EX/LOCK_SH)
// on unix. It is loaded lazily through the standard-library syscall package so
// aistat gains Windows file locking without a third-party dependency.
//
// Locks cover the whole file ([0, 0xFFFFFFFFFFFFFFFF) at offset 0) and block
// until acquired (no LOCKFILE_FAIL_IMMEDIATELY), matching flock's blocking
// LOCK_SH/LOCK_EX. Windows byte-range locks are per-handle and mandatory, so two
// handles to the same file — even within one process — serialize against each
// other, giving the same cross-process and cross-goroutine guarantee callers
// rely on from flock.
package winlock

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const lockfileExclusiveLock = 0x2

// Whole-file byte range: lock the maximum region starting at offset 0. Locking
// beyond EOF is permitted on Windows, so this works on an empty sentinel file.
const (
	allBytesLow  = 0xFFFFFFFF
	allBytesHigh = 0xFFFFFFFF
)

// Lock acquires a blocking advisory lock on the whole file. exclusive selects a
// write (exclusive) lock; false selects a shared (read) lock.
func Lock(f *os.File, exclusive bool) error {
	var flags uint32
	if exclusive {
		flags = lockfileExclusiveLock
	}
	// Stack-local Overlapped: it cannot be GC-moved during the call and avoids a
	// per-lock heap allocation. LockFileEx requires a non-nil OVERLAPPED; a zeroed
	// one locks starting at offset 0.
	var ol syscall.Overlapped
	r1, _, e := procLockFileEx.Call(
		f.Fd(),
		uintptr(flags),
		0, // reserved, must be 0
		uintptr(allBytesLow),
		uintptr(allBytesHigh),
		uintptr(unsafe.Pointer(&ol)),
	)
	// Keep f alive across the blocking call: f.Fd() yields a bare uintptr HANDLE,
	// so without this the GC could finalize f and close the HANDLE mid-lock. Same
	// guard the os stdlib uses around raw-handle syscalls.
	runtime.KeepAlive(f)
	// .Call's error return is a syscall.Errno that is non-nil even on success
	// (Errno(0)); gate on the BOOL result r1 instead.
	if r1 == 0 {
		return fmt.Errorf("winlock: LockFileEx: %w", e)
	}
	return nil
}

// Unlock releases the whole-file lock held on f.
func Unlock(f *os.File) error {
	var ol syscall.Overlapped
	r1, _, e := procUnlockFileEx.Call(
		f.Fd(),
		0, // reserved, must be 0
		uintptr(allBytesLow),
		uintptr(allBytesHigh),
		uintptr(unsafe.Pointer(&ol)),
	)
	runtime.KeepAlive(f) // see Lock: keep the HANDLE alive across the call
	if r1 == 0 {
		return fmt.Errorf("winlock: UnlockFileEx: %w", e)
	}
	return nil
}
