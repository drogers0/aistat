package render

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// textLabels holds the human-facing label and the display order for every
// known limit key per provider. Update this table whenever a provider adds
// a new known limit key.
var textLabels = map[string][]struct{ Key, Label string }{
	"claude":  {{"five_hour", "5-hour"}, {"seven_day", "7-day"}, {"seven_day_sonnet", "7-day sonnet"}},
	"codex":   {{"five_hour", "5-hour"}, {"seven_day", "7-day"}, {"code_review_seven_day", "Code review 7-day"}},
	"copilot": {{"month", "month"}},
}

// rateLimitTierLabels maps known Claude rate_limit_tier values to friendly
// display names. Unknown tiers fall through to the raw value (drift-tolerant).
var rateLimitTierLabels = map[string]string{
	"default_claude_max_5x":  "Max 5x",
	"default_claude_max_20x": "Max 20x",
	"default_claude_pro":     "Pro",
	"default_claude_free":    "Free",
}

// formatPlanLabel returns the friendly label for a known tier, the raw value
// for an unknown tier, or "" when tier is empty (so the fallback row's empty
// Plan renders without a [Plan] suffix).
func formatPlanLabel(tier string) string {
	if tier == "" {
		return ""
	}
	if label, ok := rateLimitTierLabels[tier]; ok {
		return label
	}
	return tier
}

// Text writes the human-readable rendering of report r for the providers in
// requested (in the given order).
func Text(w io.Writer, r providers.Report, requested []string) error {
	var sections []string
	for _, id := range requested {
		result, ok := r.Providers[id]
		if !ok {
			continue
		}
		title := providers.Title(id) + " usage"

		if len(result.Accounts) > 0 {
			sections = append(sections, renderAccountsSection(title, id, result.Accounts))
			continue
		}

		// Legacy flat form: Codex/Copilot, or Claude without Accounts populated.
		if result.Error != "" {
			sections = append(sections, title+": "+result.Error)
			continue
		}
		if len(result.Limits) == 0 {
			// Success-with-no-windows is distinguishable from failure in JSON
			// (`"limits": {}` vs `null`); in text mode we suppress the lone
			// header row rather than emit a section with only a title.
			continue
		}
		var lines []string
		lines = append(lines, title)
		known := textLabels[id]
		seen := map[string]bool{}
		for _, kl := range known {
			limit, ok := result.Limits[kl.Key]
			if !ok {
				continue
			}
			seen[kl.Key] = true
			lines = append(lines, formatLimitLine(kl.Label, limit))
		}
		var unknown []string
		for k := range result.Limits {
			if !seen[k] {
				unknown = append(unknown, k)
			}
		}
		sort.Strings(unknown)
		for _, k := range unknown {
			lines = append(lines, formatLimitLine(humanizeWindowKey(k), result.Limits[k]))
		}
		sections = append(sections, strings.Join(lines, "\n"))
	}
	if len(sections) == 0 {
		return nil
	}
	if _, err := io.WriteString(w, strings.Join(sections, "\n\n")+"\n"); err != nil {
		return err
	}
	return nil
}

// renderAccountsSection builds the nested text section for a provider whose
// ProviderResult has a non-empty Accounts slice (D4). The renderer trusts the
// caller's ordering — active-first, email-asc is the orchestrator's job.
func renderAccountsSection(title, providerID string, accts []providers.AccountResult) string {
	lines := []string{title}
	known := textLabels[providerID]
	for _, ar := range accts {
		// "- <email>[ (active)][ [Plan]]"
		header := "- " + ar.Email
		if ar.Active {
			header += " (active)"
		}
		if label := formatPlanLabel(ar.Plan); label != "" {
			header += " [" + label + "]"
		}
		if ar.Error != "" {
			lines = append(lines, header+": "+ar.Error)
			continue
		}
		lines = append(lines, header)
		// Inner limit bullets indented 2 spaces, reusing textLabels ordering.
		seen := map[string]bool{}
		for _, kl := range known {
			limit, ok := ar.Limits[kl.Key]
			if !ok {
				continue
			}
			seen[kl.Key] = true
			lines = append(lines, "  "+formatLimitLine(kl.Label, limit))
		}
		var unknownKeys []string
		for k := range ar.Limits {
			if !seen[k] {
				unknownKeys = append(unknownKeys, k)
			}
		}
		sort.Strings(unknownKeys)
		for _, k := range unknownKeys {
			lines = append(lines, "  "+formatLimitLine(humanizeWindowKey(k), ar.Limits[k]))
		}
	}
	return strings.Join(lines, "\n")
}

// humanizeWindowKey turns a model-scoped window key like "seven_day_fable" or
// "seven_day_fable_5" into a readable "7-day fable" / "7-day fable 5" derived
// straight from the key, so a model rename degrades gracefully instead of
// surfacing a raw snake_case key. Keys without the seven_day_ model prefix are
// returned unchanged (the unknown-key fallback keeps its raw label for them).
func humanizeWindowKey(key string) string {
	const p = "seven_day_"
	if rest := strings.TrimPrefix(key, p); rest != key && rest != "" {
		return "7-day " + strings.ReplaceAll(rest, "_", " ")
	}
	return key
}

func formatLimitLine(label string, l providers.Limit) string {
	return fmt.Sprintf("- %s: %.1f%% (resets in %s)", label, l.UsedPercent, formatResetDuration(l.ResetAfterSeconds))
}

func formatResetDuration(seconds int) string {
	if seconds <= 0 {
		return "0m"
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
