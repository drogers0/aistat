//go:build !darwin && !linux && !windows

package cred

import (
	"context"
	"errors"
	"testing"
)

func TestReadClaudeToken_UnsupportedPlatformWrapsSentinel(t *testing.T) {
	_, err := ReadClaudeToken(context.Background())
	if err == nil {
		t.Fatal("expected error on unsupported platform")
	}
	if !errors.Is(err, ErrClaudeTokenNotFound) {
		t.Errorf("error should wrap ErrClaudeTokenNotFound for correct orchestrator classification; got: %v", err)
	}
}

func TestWriteClaudeLiveBlob_UnsupportedPlatform(t *testing.T) {
	err := WriteClaudeLiveBlob(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected error on unsupported platform")
	}
	if !errors.Is(err, ErrClaudeWriteUnsupported) {
		t.Errorf("error should wrap ErrClaudeWriteUnsupported; got: %v", err)
	}
}
