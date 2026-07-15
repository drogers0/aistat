package providers

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

// Sentinel errors that providers wrap with %w so the orchestrator
// can classify retry behavior with a single errors.Is switch.
var (
	ErrAuthMissing = errors.New("auth missing")
	ErrAuthDenied  = errors.New("auth denied")
	ErrTransient   = errors.New("transient failure")
)

// iso8601Layout renders UTC as "+00:00" (not "Z"), matching the documented
// JSON output contract.
const iso8601Layout = "2006-01-02T15:04:05-07:00"

// KnownProviderIDs is the canonical list of provider identifiers in the order
// the CLI documents them. The renderer's default order, the orchestrator's
// requested-set when no provider is specified, and the CLI help text all
// derive from this slice. Treat as immutable.
var KnownProviderIDs = []string{"claude", "codex", "copilot"}

// Title returns the human-readable section label for a known provider ID.
// The caller must pass a non-empty ID from KnownProviderIDs.
func Title(id string) string {
	return strings.ToUpper(id[:1]) + id[1:]
}

// ProjectURL is the upstream repository for this binary. Used in the User-Agent
// (so endpoint owners can identify the client) and as the prefix for
// IssueTrackerURL.
const ProjectURL = "https://github.com/drogers0/aistat"

// IssueTrackerURL is the upstream issue tracker. Cited in provider error
// messages emitted when an upstream-API shape change is detected, so users
// can file a bug with the exact context already in the message.
const IssueTrackerURL = ProjectURL + "/issues"

// Provider is the contract every credential+endpoint backend implements.
type Provider interface {
	ID() string
	Fetch(ctx context.Context) (ProviderOutput, error)
}

// ProviderOutput is what a provider returns on success. The map key is the
// limit-window name (e.g. "five_hour", "seven_day", "month").
type ProviderOutput struct {
	Limits   map[string]Limit
	Accounts []AccountResult
}

// Limit is one usage window for one provider.
type Limit struct {
	UsedPercent       float64   `json:"used_percent"`
	RemainingPercent  float64   `json:"remaining_percent"`
	ResetsAt          time.Time `json:"resets_at"`
	ResetAfterSeconds int       `json:"reset_after_seconds"`
}

// MarshalJSON emits the four documented fields in the documented order, with
// ResetsAt formatted as "+00:00" instead of Go's default "Z" and percent
// fields rounded to 2 decimal places to suppress float artifacts (e.g. an
// internal value of 67.339999999 becomes 67.34 on the wire). The fields are
// listed manually rather than via the "type alias + embed" trick because
// Go's encoding/json does not deduplicate struct fields by JSON tag.
func (l Limit) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		UsedPercent       float64 `json:"used_percent"`
		RemainingPercent  float64 `json:"remaining_percent"`
		ResetsAt          string  `json:"resets_at"`
		ResetAfterSeconds int     `json:"reset_after_seconds"`
	}{
		UsedPercent:       roundPct(l.UsedPercent),
		RemainingPercent:  roundPct(l.RemainingPercent),
		ResetsAt:          l.ResetsAt.UTC().Format(iso8601Layout),
		ResetAfterSeconds: l.ResetAfterSeconds,
	})
}

func roundPct(f float64) float64 { return math.Round(f*100) / 100 }

// Report is the top-level JSON document. The orchestrator populates it;
// renderers consume it. Lives here, not in internal/render, so the
// orchestrator never imports the renderer.
type Report struct {
	CheckedAt time.Time                 `json:"checked_at"`
	Providers map[string]ProviderResult `json:"providers"`
}

func (r Report) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		CheckedAt string                    `json:"checked_at"`
		Providers map[string]ProviderResult `json:"providers"`
	}{
		CheckedAt: r.CheckedAt.UTC().Format(iso8601Layout),
		Providers: r.Providers,
	})
}

// AccountResult is one stored provider account's contribution to a ProviderResult.
// Active is intentionally not omitempty — false is meaningful (the account is
// stored but not currently live). Limits is intentionally NOT omitempty so a
// successful fetch with zero recognized windows still serializes as
// `"limits": {}` rather than vanishing — mirrors the Copilot top-level
// contract where `{}` means "asked, got nothing" and `null` means "fetch
// failed". The per-account fetch sets Limits to a non-nil (possibly empty)
// map on success and leaves it nil + sets Error on failure. UUID is hidden
// from JSON (json:"-") because email is the user-facing identifier for
// scripted consumers; UUIDs surface in `aistat accounts list` text output
// and in `aistat switch`'s confirmation line, both of which are the
// discovery surfaces for UUID-prefix matching.
type AccountResult struct {
	Email  string           `json:"email"`
	UUID   string           `json:"-"`
	Plan   string           `json:"plan"`
	Active bool             `json:"active"`
	Limits map[string]Limit `json:"limits"`
	Error  string           `json:"error,omitempty"`
}

// ProviderResult is one provider's contribution to the Report.
//
// Error uses omitempty so a successful provider serializes without an
// "error" key.
//
// Limits serialization depends on whether Accounts is populated:
//   - Accounts empty (Copilot path): Limits always serializes —
//     success-with-windows → `"limits": {...}`, zero-windows → `"limits": {}`,
//     failure → `"limits": null`. `{}` vs `null` lets callers distinguish
//     "asked, got nothing" from "failed".
//   - Accounts non-empty (Claude/Codex multi-account path): the `limits` key is
//     omitted entirely. The active account's limits live in
//     `accounts[i].limits` where `active == true`; a top-level mirror would
//     just duplicate that block.
type ProviderResult struct {
	Limits   map[string]Limit `json:"limits"`
	Accounts []AccountResult  `json:"accounts,omitempty"`
	Error    string           `json:"error,omitempty"`
}

func (r ProviderResult) MarshalJSON() ([]byte, error) {
	if len(r.Accounts) > 0 {
		// Multi-account path: `accounts` is canonical; omit the top-level
		// limits mirror.
		return json.Marshal(struct {
			Accounts []AccountResult `json:"accounts"`
			Error    string          `json:"error,omitempty"`
		}{Accounts: r.Accounts, Error: r.Error})
	}
	// Flat path (Copilot, or Claude/Codex with no stored accounts and an
	// immediate fetch error): always include limits.
	return json.Marshal(struct {
		Limits map[string]Limit `json:"limits"`
		Error  string           `json:"error,omitempty"`
	}{Limits: r.Limits, Error: r.Error})
}
