package codex

import (
	"fmt"
	"os"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// DefaultUserAgent returns the User-Agent for Codex requests. Today this is
// the honest aistat UA — there is no evidence Codex's endpoint partitions
// rate-limits by UA. The seam exists so the default can be tuned if that
// changes (mirrors claude.DefaultUserAgent).
//
// The AISTAT_CODEX_USER_AGENT env var overrides verbatim.
func DefaultUserAgent(version string) string {
	if v := os.Getenv("AISTAT_CODEX_USER_AGENT"); v != "" {
		return v
	}
	return fmt.Sprintf("aistat/%s (+%s)", version, providers.ProjectURL)
}
