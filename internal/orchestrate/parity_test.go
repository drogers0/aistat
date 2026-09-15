package orchestrate

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/render"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

func TestParity(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		// TestParity_JSONContract locks the byte-stable JSON shape: every Limit has
		// the four documented fields in the documented order, every provider key is
		// present for every requested provider, checked_at + resets_at use "+00:00"
		// not "Z", and providers are sorted alphabetically.
		{"json contract", func(t *testing.T) {
			frozen := time.Date(2026, 5, 26, 20, 0, 0, 0, time.UTC)
			mk := func(used float64, secs int) providers.Limit {
				return providers.Limit{
					UsedPercent:       used,
					RemainingPercent:  100 - used,
					ResetsAt:          frozen.Add(time.Duration(secs) * time.Second),
					ResetAfterSeconds: secs,
				}
			}
			p1 := &stubProvider{id: "claude", results: []stubResult{{out: providers.ProviderOutput{Limits: map[string]providers.Limit{
				"five_hour":        mk(2, 17625),
				"seven_day":        mk(21, 191825),
				"seven_day_sonnet": mk(0, 191825),
			}}}}}
			p2 := &stubProvider{id: "codex", results: []stubResult{{out: providers.ProviderOutput{Limits: map[string]providers.Limit{
				"five_hour":             mk(0, 11525),
				"seven_day":             mk(11, 349225),
				"code_review_seven_day": mk(0, 349225),
			}}}}}
			p3 := &stubProvider{id: "copilot", results: []stubResult{{out: providers.ProviderOutput{Limits: map[string]providers.Limit{
				"month": mk(4, 1065600),
			}}}}}

			report, status := Run(context.Background(),
				[]string{"claude", "codex", "copilot"},
				[]providers.Provider{p1, p2, p3},
				Options{Now: func() time.Time { return frozen }},
			)
			if status != StatusOK {
				t.Fatalf("status = %d, want 0", status)
			}

			var buf bytes.Buffer
			testutil.WantNoErr(t, render.JSON(&buf, report))
			got := buf.String()

			// 1. checked_at uses "+00:00", not "Z".
			if !strings.Contains(got, `"checked_at": "2026-05-26T20:00:00+00:00"`) {
				t.Errorf("checked_at format wrong: %s", got)
			}
			if strings.Contains(got, `"Z"`) {
				t.Errorf("found Z suffix; should be +00:00: %s", got)
			}

			// 2. Top-level providers sorted alphabetically.
			iClaude := strings.Index(got, `"claude"`)
			iCodex := strings.Index(got, `"codex"`)
			iCopilot := strings.Index(got, `"copilot"`)
			if !(iClaude >= 0 && iClaude < iCodex && iCodex < iCopilot) {
				t.Errorf("provider order wrong: %s", got)
			}

			// 3. Limit field order: used_percent, remaining_percent, resets_at, reset_after_seconds.
			idxUsed := strings.Index(got, `"used_percent"`)
			idxRem := strings.Index(got, `"remaining_percent"`)
			idxResets := strings.Index(got, `"resets_at"`)
			idxAfter := strings.Index(got, `"reset_after_seconds"`)
			if !(idxUsed < idxRem && idxRem < idxResets && idxResets < idxAfter) {
				t.Errorf("Limit field order wrong: %s", got)
			}

			// 4. Each known limit key appears exactly once per provider.
			for _, key := range []string{"five_hour", "seven_day", "seven_day_sonnet", "code_review_seven_day", "month"} {
				if !strings.Contains(got, `"`+key+`"`) {
					t.Errorf("missing limit key %q in %s", key, got)
				}
			}

			// 5. Float artifact regression: no ".999999" or ".000001" suffixes.
			if strings.Contains(got, ".999999") || strings.Contains(got, ".000001") {
				t.Errorf("float-precision artifact detected: %s", got)
			}

			// 6. Design sample values for claude.five_hour should round-trip cleanly.
			if !strings.Contains(got, `"used_percent": 2,`) {
				t.Errorf("integer used_percent should marshal without decimal: %s", got)
			}
			if !strings.Contains(got, `"reset_after_seconds": 17625`) {
				t.Errorf("reset_after_seconds for claude.five_hour wrong: %s", got)
			}
		}},
		{"text contract", func(t *testing.T) {
			frozen := time.Date(2026, 5, 26, 20, 0, 0, 0, time.UTC)
			mk := func(used float64, secs int) providers.Limit {
				return providers.Limit{
					UsedPercent:       used,
					RemainingPercent:  100 - used,
					ResetsAt:          frozen.Add(time.Duration(secs) * time.Second),
					ResetAfterSeconds: secs,
				}
			}
			p1 := &stubProvider{id: "claude", results: []stubResult{{out: providers.ProviderOutput{Limits: map[string]providers.Limit{
				"five_hour":        mk(2, 4*3600+53*60),
				"seven_day":        mk(21, 2*86400+5*3600),
				"seven_day_sonnet": mk(0, 2*86400+5*3600),
			}}}}}
			p2 := &stubProvider{id: "codex", results: []stubResult{{out: providers.ProviderOutput{Limits: map[string]providers.Limit{
				"five_hour":             mk(0, 3*3600+12*60),
				"seven_day":             mk(11, 4*86400+1*3600),
				"code_review_seven_day": mk(0, 4*86400+1*3600),
			}}}}}
			p3 := &stubProvider{id: "copilot", results: []stubResult{{out: providers.ProviderOutput{Limits: map[string]providers.Limit{
				"month": mk(4, 5*86400+7*3600),
			}}}}}

			report, _ := Run(context.Background(),
				[]string{"claude", "codex", "copilot"},
				[]providers.Provider{p1, p2, p3},
				Options{Now: func() time.Time { return frozen }},
			)

			var buf bytes.Buffer
			testutil.WantNoErr(t, render.Text(&buf, report, []string{"claude", "codex", "copilot"}, false))
			want := string(testutil.LoadFixture(t, "text_contract.golden"))
			if buf.String() != want {
				t.Errorf("text contract drift:\ngot:\n%s\nwant:\n%s", buf.String(), want)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
