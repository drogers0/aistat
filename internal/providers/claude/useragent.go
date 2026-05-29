package claude

import (
	"fmt"
	"os"
)

// DefaultUserAgent returns the User-Agent that puts the Claude provider in the
// lenient `/oauth/usage` rate-limit bucket. Anthropic's edge throttles
// non-`claude-code/<version>` clients aggressively (multiple persistent-429
// reports in github.com/anthropics/claude-code and an independent
// reverse-engineering note in Maciek-roboblog/Claude-Code-Usage-Monitor#202).
//
// The Claude Client uses a single shared httpx.Doer for the usage, profile,
// and refresh endpoints, so this UA reaches all three. The override applies
// uniformly; surgical per-endpoint UAs are not supported.
//
// Working-tree and `go install` builds resolve version to "dev"; sending
// `claude-code/dev` would plausibly miss Anthropic's `claude-code/<semver>`
// matcher and drop us back into the punitive bucket. Substitute a SemVer
// pre-release identifier so dev builds still parse as `claude-code/<semver>`.
//
// The AISTAT_CLAUDE_USER_AGENT env var overrides verbatim — set it to
// "aistat/<version> (+url)" to opt back into the honest UA.
func DefaultUserAgent(version string) string {
	if v := os.Getenv("AISTAT_CLAUDE_USER_AGENT"); v != "" {
		return v
	}
	if version == "" || version == "dev" {
		version = "0.0.0-dev"
	}
	return fmt.Sprintf("claude-code/%s", version)
}
