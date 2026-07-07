//go:build !darwin && !linux && !windows

package cred

import (
	"context"
	"fmt"
)

// ReadClaudeCredential on platforms other than macOS/Linux returns an error
// wrapped in ErrClaudeTokenNotFound so the orchestrator classifies it as
// auth-missing (correct exit-code behavior). The wrapped sentinel's message
// recommends `claude /login`, which doesn't exist on these platforms —
// accepted as a cosmetic cost for the classification correctness.
func ReadClaudeCredential(ctx context.Context) (Credential, error) {
	return Credential{}, fmt.Errorf("%w: claude is not supported on this platform (requires macOS or Linux)", ErrClaudeTokenNotFound)
}

// ReadClaudeToken on platforms other than macOS/Linux returns an error wrapped
// in ErrClaudeTokenNotFound so the orchestrator classifies it as auth-missing
// (correct exit-code behavior). The wrapped sentinel's message recommends
// `claude /login`, which doesn't exist on these platforms — accepted as a
// cosmetic cost for the classification correctness.
func ReadClaudeToken(ctx context.Context) (string, error) {
	c, err := ReadClaudeCredential(ctx)
	return c.AccessToken, err
}

// WriteClaudeLiveBlob on platforms other than macOS/Linux returns
// ErrClaudeWriteUnsupported.
func WriteClaudeLiveBlob(ctx context.Context, rawBlob []byte) error {
	return fmt.Errorf("%w: claude is not supported on this platform (requires macOS or Linux)", ErrClaudeWriteUnsupported)
}
