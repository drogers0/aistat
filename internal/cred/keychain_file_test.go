//go:build linux || windows

package cred

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/drogers0/aistat/v2/internal/testenv"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

func writeCred(t *testing.T, dir, body string) string {
	t.Helper()
	claudeDir := filepath.Join(dir, ".claude")
	testutil.WantNoErr(t, os.MkdirAll(claudeDir, 0o700))
	path := filepath.Join(claudeDir, ".credentials.json")
	testutil.WantNoErr(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestReadClaudeToken(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path", func(t *testing.T) {
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			writeCred(t, dir, `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-abc"}}`)
			got, err := ReadClaudeToken(context.Background())
			testutil.WantNoErr(t, err)
			if got != "sk-ant-oat01-abc" {
				t.Errorf("got %q, want %q", got, "sk-ant-oat01-abc")
			}
		}},
		{"missing file", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir())
			_, err := ReadClaudeToken(context.Background())
			if !errors.Is(err, ErrClaudeTokenNotFound) {
				t.Errorf("expected ErrClaudeTokenNotFound, got: %v", err)
			}
		}},
		{"malformed json", func(t *testing.T) {
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			writeCred(t, dir, "not json")
			_, err := ReadClaudeToken(context.Background())
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, ErrClaudeTokenNotFound) {
				t.Errorf("malformed JSON should not be classified as missing-token; got: %v", err)
			}
		}},
		{"empty token", func(t *testing.T) {
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			writeCred(t, dir, `{"claudeAiOauth":{"accessToken":""}}`)
			_, err := ReadClaudeToken(context.Background())
			if !errors.Is(err, ErrClaudeTokenNotFound) {
				t.Errorf("expected ErrClaudeTokenNotFound, got: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestWriteClaudeLiveBlob(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path", func(t *testing.T) {
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-write-test","refreshToken":"rt","expiresAt":9999}}`)
			testutil.WantNoErr(t, WriteClaudeLiveBlob(context.Background(), blob))
			c, err := ReadClaudeCredential(context.Background())
			testutil.WantNoErr(t, err)
			if !bytes.Equal(c.Raw, blob) {
				t.Errorf("read-back bytes differ\ngot:  %q\nwant: %q", c.Raw, blob)
			}
			if c.AccessToken != "sk-ant-write-test" {
				t.Errorf("AccessToken: got %q, want %q", c.AccessToken, "sk-ant-write-test")
			}
		}},
		{"file mode", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX file modes are advisory on Windows (NTFS ACLs)")
			}
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			blob := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
			testutil.WantNoErr(t, WriteClaudeLiveBlob(context.Background(), blob))
			path := filepath.Join(dir, ".claude", ".credentials.json")
			info, err := os.Stat(path)
			testutil.WantNoErr(t, err)
			if got := info.Mode().Perm(); got != 0o600 {
				t.Errorf("file mode: got %04o, want 0600", got)
			}
		}},
		{"creates parent dir", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX dir modes are advisory on Windows (NTFS ACLs)")
			}
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			// Do not pre-create ~/.claude; WriteClaudeLiveBlob must create it.
			blob := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
			testutil.WantNoErr(t, WriteClaudeLiveBlob(context.Background(), blob))
			claudeDir := filepath.Join(dir, ".claude")
			info, err := os.Stat(claudeDir)
			testutil.WantNoErr(t, err)
			if !info.IsDir() {
				t.Error(".claude is not a directory")
			}
			if got := info.Mode().Perm(); got != 0o700 {
				t.Errorf(".claude dir mode: got %04o, want 0700", got)
			}
		}},
		{"no tmp file after success", func(t *testing.T) {
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			claudeDir := filepath.Join(dir, ".claude")
			testutil.WantNoErr(t, os.MkdirAll(claudeDir, 0o700))

			blob := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
			testutil.WantNoErr(t, WriteClaudeLiveBlob(context.Background(), blob))
			entries, err := os.ReadDir(claudeDir)
			testutil.WantNoErr(t, err)
			for _, e := range entries {
				if e.Name() != ".credentials.json" {
					t.Errorf("unexpected file left in .claude dir: %q", e.Name())
				}
			}
		}},
		{"no tmp file after failure", func(t *testing.T) {
			dir := t.TempDir()
			testenv.RedirectHome(t, dir)
			claudeDir := filepath.Join(dir, ".claude")
			testutil.WantNoErr(t, os.MkdirAll(claudeDir, 0o700))

			// Place the destination path as a directory so os.Rename into it fails.
			// CreateTemp uses a different name pattern, so it succeeds; only Rename fails.
			dest := filepath.Join(claudeDir, ".credentials.json")
			testutil.WantNoErr(t, os.Mkdir(dest, 0o700))

			blob := []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`)
			err := WriteClaudeLiveBlob(context.Background(), blob)
			if err == nil {
				t.Fatal("expected error when rename fails")
			}

			// No temp files should remain.
			entries, err2 := os.ReadDir(claudeDir)
			testutil.WantNoErr(t, err2)
			for _, e := range entries {
				if e.Name() != ".credentials.json" {
					t.Errorf("tmp file not cleaned up: %q", e.Name())
				}
			}
		}},
		{"home unset", func(t *testing.T) {
			t.Setenv("HOME", "")
			if runtime.GOOS == "windows" {
				// os.UserHomeDir reads %USERPROFILE% on Windows, not $HOME.
				t.Setenv("USERPROFILE", "")
			}
			err := WriteClaudeLiveBlob(context.Background(), []byte(`{"claudeAiOauth":{"accessToken":"tok"}}`))
			if err == nil {
				t.Fatal("expected error when HOME is unset")
			}
			// credPath wraps ErrClaudeTokenNotFound for the home-dir failure; that
			// sentinel propagates through WriteClaudeLiveBlob's wrapping.
			if !errors.Is(err, ErrClaudeTokenNotFound) {
				t.Errorf("expected error to wrap ErrClaudeTokenNotFound, got: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
