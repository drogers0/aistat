//go:build windows

package accounts

import (
	"fmt"
	"os"

	"github.com/drogers0/aistat/v2/internal/winlock"
)

// withLock is the Windows counterpart of the unix flock-based lock (see
// store_file_unix.go for the sentinel-file rationale). It takes a whole-file
// LockFileEx lock on the sentinel. The lock error is checked before the Unlock
// defer is registered and before fn runs, so fn never executes with the lock
// unheld.
func (s *fileStore) withLock(exclusive bool, fn func() error) error {
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("accounts: open lock file: %w", err)
	}
	defer f.Close()
	if err := winlock.Lock(f, exclusive); err != nil {
		return fmt.Errorf("accounts: acquire store lock: %w", err)
	}
	defer winlock.Unlock(f) //nolint:errcheck
	return fn()
}
