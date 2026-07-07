//go:build linux

package accounts

import (
	"fmt"
	"os"
	"syscall"
)

// withLock opens the sentinel lock file (creating it mode 0600 if absent),
// acquires a shared or exclusive flock, calls fn, then releases the lock. The
// lock sits on a separate sentinel file rather than the data file because
// atomicWrite replaces the data file via rename — a flock on the data file's
// open fd would travel with the orphaned inode after rename, letting a second
// writer race ahead with stale state. The sentinel never gets renamed so the
// lock anchors a stable serialization point.
func (s *fileStore) withLock(exclusive bool, fn func() error) error {
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("accounts: open lock file: %w", err)
	}
	defer f.Close()
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		return fmt.Errorf("accounts: acquire store lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}
