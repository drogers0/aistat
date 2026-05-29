package copilot

import (
	"fmt"
	"os"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// DefaultUserAgent returns the User-Agent for Copilot requests. Today this is
// the honest aistat UA — there is no evidence the GitHub Copilot billing
// endpoint partitions rate-limits by UA. The seam exists so the default can be
// tuned if that changes (mirrors claude.DefaultUserAgent).
//
// The AISTAT_COPILOT_USER_AGENT env var overrides verbatim.
func DefaultUserAgent(version string) string {
	if v := os.Getenv("AISTAT_COPILOT_USER_AGENT"); v != "" {
		return v
	}
	return fmt.Sprintf("aistat/%s (+%s)", version, providers.ProjectURL)
}
