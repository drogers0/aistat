//go:build windows

package usagecache

import (
	"fmt"
	"os"

	"github.com/drogers0/aistat/v2/internal/winlock"
)

// withLock is the Windows counterpart of the unix flock-based lock (see
// cache_unix.go for the sentinel-file rationale). It uses a whole-file
// LockFileEx lock on the sentinel. The lock error is checked before the Unlock
// defer is registered and before fn runs, so fn never executes with the lock
// unheld.
func (c *Cache) withLock(exclusive bool, fn func() error) error {
	f, err := os.OpenFile(c.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("usage cache: open lock file: %w", err)
	}
	defer f.Close()
	if err := winlock.Lock(f, exclusive); err != nil {
		return fmt.Errorf("usage cache: acquire lock: %w", err)
	}
	defer winlock.Unlock(f) //nolint:errcheck
	return fn()
}
