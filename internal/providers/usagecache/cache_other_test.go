//go:build !darwin && !linux && !windows

package usagecache

import (
	"testing"

	"github.com/drogers0/aistat/v2/internal/providers"
)

func TestCacheOther_OpaqueKeyWarnsOnceAndNoOps(t *testing.T) {
	const key = "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"
	var warns []string
	c := New("claude", nil, func(s string) { warns = append(warns, s) })

	if got, ok := c.Get(key); got != nil || ok {
		t.Errorf("Get: want (nil, false), got (%v, %v)", got, ok)
	}
	c.Put(key, map[string]providers.Limit{"x": {}})
	if got, ok := c.Get(key); got != nil || ok {
		t.Errorf("Get after Put: want (nil, false), got (%v, %v)", got, ok)
	}

	if len(warns) != 1 {
		t.Fatalf("warn count: want 1, got %d", len(warns))
	}
	const wantWarning = "aistat: claude: usage cache disabled (platform not supported)"
	if warns[0] != wantWarning {
		t.Errorf("warning: got %q, want %q", warns[0], wantWarning)
	}
}
