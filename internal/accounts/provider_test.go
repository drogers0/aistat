//go:build darwin || linux || windows

package accounts

import (
	"testing"

	"github.com/drogers0/aistat/v2/internal/testenv"
)

func TestProviderValidate(t *testing.T) {
	tests := []struct {
		name    string
		p       Provider
		wantErr bool
	}{
		{"claude is valid", ProviderClaude, false},
		{"codex is valid", ProviderCodex, false},
		{"empty string errors", Provider(""), true},
		{"path traversal errors", Provider("../attack"), true},
		{"unknown provider errors", Provider("unknown"), true},
		{"path separator errors", Provider("claude/foo"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.validate()
			if tt.wantErr && err == nil {
				t.Errorf("Provider(%q).validate(): want error, got nil", tt.p)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("Provider(%q).validate(): want nil, got %v", tt.p, err)
			}
		})
	}
}

// TestOpenStore_InvalidProvider asserts that OpenStore returns an error for
// invalid provider strings on every supported platform.
func TestOpenStore_InvalidProvider(t *testing.T) {
	tests := []struct {
		name string
		p    Provider
	}{
		{"empty string", Provider("")},
		{"path traversal", Provider("../attack")},
		{"unknown provider", Provider("unknown")},
		{"path separator", Provider("claude/foo")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a temp home so no real credential dirs are touched.
			testenv.RedirectHome(t, t.TempDir())
			_, err := OpenStore(tt.p)
			if err == nil {
				t.Errorf("OpenStore(%q): want error, got nil", tt.p)
			}
		})
	}
}
