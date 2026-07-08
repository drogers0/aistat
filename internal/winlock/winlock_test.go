//go:build windows

package winlock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWinlock(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"lock unlock round trip", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sentinel.lock")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()
			if err := Lock(f, true); err != nil {
				t.Fatalf("Lock: %v", err)
			}
			if err := Unlock(f); err != nil {
				t.Fatalf("Unlock: %v", err)
			}
		}},
		{"shared lock on empty file", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "empty.lock")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()
			// Locking beyond EOF on a zero-length file must succeed.
			if err := Lock(f, false); err != nil {
				t.Fatalf("shared Lock on empty file: %v", err)
			}
			if err := Unlock(f); err != nil {
				t.Fatalf("Unlock: %v", err)
			}
		}},
		{"second exclusive lock blocks until release", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "contended.lock")
			open := func() *os.File {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatalf("open: %v", err)
				}
				return f
			}
			f1 := open()
			defer f1.Close()
			if err := Lock(f1, true); err != nil {
				t.Fatalf("Lock f1: %v", err)
			}

			acquired := make(chan struct{})
			go func() {
				f2 := open()
				defer f2.Close()
				if err := Lock(f2, true); err != nil {
					// An unexpected error (not contention) must not signal acquisition,
					// or the outer select would misread it as "acquired while held".
					// t.Errorf fails the test; return without closing acquired.
					t.Errorf("Lock f2: %v", err)
					return
				}
				_ = Unlock(f2)
				close(acquired)
			}()

			// The second handle must NOT acquire while f1 holds the lock.
			select {
			case <-acquired:
				t.Fatal("second exclusive lock acquired while first was held")
			case <-time.After(150 * time.Millisecond):
			}

			// Releasing f1 lets the goroutine proceed.
			if err := Unlock(f1); err != nil {
				t.Fatalf("Unlock f1: %v", err)
			}
			select {
			case <-acquired:
			case <-time.After(2 * time.Second):
				t.Fatal("second exclusive lock did not acquire after release")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
