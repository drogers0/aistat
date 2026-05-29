package claude

import "testing"

func TestDefaultUserAgent_Default(t *testing.T) {
	t.Setenv("AISTAT_CLAUDE_USER_AGENT", "")
	got := DefaultUserAgent("2.1.0")
	want := "claude-code/2.1.0"
	if got != want {
		t.Errorf("DefaultUserAgent = %q, want %q", got, want)
	}
}

func TestDefaultUserAgent_EnvOverride(t *testing.T) {
	t.Setenv("AISTAT_CLAUDE_USER_AGENT", "custom-ua/9")
	got := DefaultUserAgent("2.1.0")
	want := "custom-ua/9"
	if got != want {
		t.Errorf("DefaultUserAgent with env override = %q, want %q", got, want)
	}
}

// TestDefaultUserAgent_DevBuildSubstitutesSemver: working-tree and
// `go install` builds resolve version to "dev" (or "" if buildinfo is absent).
// Substituting a SemVer pre-release identifier keeps the UA shape
// `claude-code/<semver>` so Anthropic's matcher still drops us in the lenient
// bucket on dev builds.
func TestDefaultUserAgent_DevBuildSubstitutesSemver(t *testing.T) {
	t.Setenv("AISTAT_CLAUDE_USER_AGENT", "")
	for _, in := range []string{"", "dev"} {
		got := DefaultUserAgent(in)
		want := "claude-code/0.0.0-dev"
		if got != want {
			t.Errorf("DefaultUserAgent(%q) = %q, want %q", in, got, want)
		}
	}
}
