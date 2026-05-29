package codex

import "testing"

func TestDefaultUserAgent_Default(t *testing.T) {
	t.Setenv("AISTAT_CODEX_USER_AGENT", "")
	got := DefaultUserAgent("2.1.0")
	want := "aistat/2.1.0 (+https://github.com/drogers0/aistat)"
	if got != want {
		t.Errorf("DefaultUserAgent = %q, want %q", got, want)
	}
}

func TestDefaultUserAgent_EnvOverride(t *testing.T) {
	t.Setenv("AISTAT_CODEX_USER_AGENT", "custom-ua/9")
	got := DefaultUserAgent("2.1.0")
	want := "custom-ua/9"
	if got != want {
		t.Errorf("DefaultUserAgent with env override = %q, want %q", got, want)
	}
}
