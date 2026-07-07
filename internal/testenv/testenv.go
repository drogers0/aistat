// Package testenv provides tiny, dependency-free test helpers for redirecting
// OS user-directory lookups. It imports nothing from the module, so any test
// package can use it without risking an import cycle (unlike internal/testutil,
// which imports internal/accounts). Production code must not import this package.
package testenv

import (
	"path/filepath"
	"runtime"
	"testing"
)

// RedirectHome points os.UserHomeDir and os.UserCacheDir at dir for the duration
// of the test, on every OS. Plain t.Setenv("HOME", …) only redirects the unix
// resolvers; on Windows UserHomeDir reads %USERPROFILE% and UserCacheDir reads
// %LocalAppData%. Setting all of them keeps a test isolated regardless of GOOS.
// (os.UserHomeDir/os.UserCacheDir read the environment at call time, so this
// takes effect for resolves that happen after RedirectHome returns.)
func RedirectHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)          // unix home; darwin/linux UserCacheDir base
	t.Setenv("XDG_CACHE_HOME", "") // linux: fall back to $HOME/.cache
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", dir)                                     // UserHomeDir
		t.Setenv("LocalAppData", filepath.Join(dir, "AppData", "Local")) // UserCacheDir
	}
}
