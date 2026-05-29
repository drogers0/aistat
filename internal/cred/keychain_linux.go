//go:build linux

package cred

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// credPath returns the path to ~/.claude/.credentials.json.
func credPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// %v (not %w) for the inner os error: we expose ErrClaudeTokenNotFound
		// as the sentinel so callers classify this as auth-missing, not as a
		// wrapped os error. Wrapping with %w would make errors.Is match against
		// internal os/syscall errors, which callers should not depend on.
		return "", fmt.Errorf("%w: cannot resolve home directory ($HOME unset): %v", ErrClaudeTokenNotFound, err)
	}
	return filepath.Join(home, ".claude", ".credentials.json"), nil
}

// ReadClaudeCredential returns the full credential blob from
// ~/.claude/.credentials.json (the file `claude /login` writes on Linux).
// ctx is accepted for signature parity; not used by the current implementation.
//
// File permissions are the Claude Code CLI's responsibility (it created the
// file); aistat is a read-only observer and does not police them.
func ReadClaudeCredential(ctx context.Context) (Credential, error) {
	path, err := credPath()
	if err != nil {
		return Credential{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Credential{}, ErrClaudeTokenNotFound
		}
		return Credential{}, fmt.Errorf("reading claude credentials: %w", err)
	}
	return parseClaudeCredFull(data)
}

// ReadClaudeToken returns the OAuth access token from
// ~/.claude/.credentials.json (the file `claude /login` writes on Linux).
// ctx is accepted for signature parity; not used by the current implementation.
//
// File permissions are the Claude Code CLI's responsibility (it created the
// file); aistat is a read-only observer and does not police them.
func ReadClaudeToken(ctx context.Context) (string, error) {
	c, err := ReadClaudeCredential(ctx)
	return c.AccessToken, err
}

// WriteClaudeLiveBlob atomically overwrites ~/.claude/.credentials.json with
// the given raw credential blob. The parent directory (~/.claude) is created
// with mode 0700 if absent. The file is written with mode 0600 via a
// temporary file in the same directory followed by os.Rename, so a partial
// write is never observable by ReadClaudeCredential.
func WriteClaudeLiveBlob(ctx context.Context, rawBlob []byte) error {
	path, err := credPath()
	if err != nil {
		return fmt.Errorf("cannot resolve credential path: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating credential directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".credentials-*.json")
	if err != nil {
		return fmt.Errorf("creating temporary credential file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting credential file mode: %w", err)
	}
	if _, err := tmp.Write(rawBlob); err != nil {
		tmp.Close()
		return fmt.Errorf("writing credential file: %w", err)
	}
	// Sync before close so a crash between rename and the next fsync cannot leave
	// the live credential half-written. Matches the atomic-write pattern in
	// internal/accounts/store_linux.go and internal/providers/claude/usagecache.go.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing credential file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("installing credential file: %w", err)
	}
	committed = true
	return nil
}
