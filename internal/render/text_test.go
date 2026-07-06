package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

func mkLimit(used float64, secs int) providers.Limit {
	at, _ := time.Parse(time.RFC3339, "2026-05-26T20:00:00Z")
	return providers.Limit{
		UsedPercent:       used,
		RemainingPercent:  100 - used,
		ResetsAt:          at.Add(time.Duration(secs) * time.Second),
		ResetAfterSeconds: secs,
	}
}

func TestText(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"design sample", func(t *testing.T) {
			// Claude uses the new nested Accounts form; Codex/Copilot use the legacy
			// flat Limits form. This is the canonical end-to-end rendering demo.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {
						Limits: map[string]providers.Limit{
							"five_hour":        mkLimit(2, 4*3600+53*60),
							"seven_day":        mkLimit(21, 2*86400+5*3600),
							"seven_day_sonnet": mkLimit(0, 2*86400+5*3600),
						},
						Accounts: []providers.AccountResult{
							{
								Email:  "me@personal.com",
								Plan:   "default_claude_max_5x",
								Active: true,
								Limits: map[string]providers.Limit{
									"five_hour":        mkLimit(2, 4*3600+53*60),
									"seven_day":        mkLimit(21, 2*86400+5*3600),
									"seven_day_sonnet": mkLimit(0, 2*86400+5*3600),
								},
							},
							{
								Email:  "me@work.company.com",
								Plan:   "default_claude_max_20x",
								Active: false,
								Limits: map[string]providers.Limit{
									"five_hour": mkLimit(71, 5*60),
								},
							},
						},
					},
					"codex": {Limits: map[string]providers.Limit{
						"five_hour":             mkLimit(0, 3*3600+12*60),
						"seven_day":             mkLimit(11, 4*86400+1*3600),
						"code_review_seven_day": mkLimit(0, 4*86400+1*3600),
					}},
					"copilot": {Limits: map[string]providers.Limit{
						"month": mkLimit(4, 5*86400+7*3600),
					}},
				},
			}
			var buf bytes.Buffer
			testutil.WantNoErr(t, Text(&buf, r, []string{"claude", "codex", "copilot"}))
			want := string(testutil.LoadFixture(t, "text-design-sample.golden"))
			if buf.String() != want {
				t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
			}
		}},
		{"single provider", func(t *testing.T) {
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Limits: map[string]providers.Limit{
						"five_hour": mkLimit(2, 4*3600+53*60),
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- 5-hour: 2.0% (resets in 4h 53m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"requested but none in report", func(t *testing.T) {
			var buf bytes.Buffer
			testutil.WantNoErr(t, Text(&buf, providers.Report{}, []string{"claude"}))
			if buf.Len() != 0 {
				t.Errorf("expected empty output when no requested providers are in report, got %q", buf.String())
			}
		}},
		{"empty requested", func(t *testing.T) {
			var buf bytes.Buffer
			_ = Text(&buf, providers.Report{}, nil)
			if buf.Len() != 0 {
				t.Fatalf("expected empty, got %q", buf.String())
			}
		}},
		{"error only", func(t *testing.T) {
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Error: "Claude token not found"},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage: Claude token not found\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"mixed success and error", func(t *testing.T) {
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Limits: map[string]providers.Limit{"five_hour": mkLimit(2, 4*3600)}},
					"codex":  {Error: "Codex token not found"},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude", "codex"})
			want := "Claude usage\n- 5-hour: 2.0% (resets in 4h 0m)\n\nCodex usage: Codex token not found\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"unknown key after known", func(t *testing.T) {
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Limits: map[string]providers.Limit{
						"five_hour":   mkLimit(2, 3600),
						"new_window":  mkLimit(5, 7200),
						"alpha_extra": mkLimit(3, 1800),
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- 5-hour: 2.0% (resets in 1h 0m)\n- alpha_extra: 3.0% (resets in 30m)\n- new_window: 5.0% (resets in 2h 0m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"model-scoped fable window renders via generic humanizer", func(t *testing.T) {
			// seven_day_fable has no hard-coded label; it falls into the unknown-key
			// path and must render as "7-day fable" via humanizeWindowKey, not the
			// raw snake_case key.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Limits: map[string]providers.Limit{
						"five_hour":        mkLimit(2, 4*3600+53*60),
						"seven_day":        mkLimit(21, 2*86400+5*3600),
						"seven_day_sonnet": mkLimit(0, 2*86400+5*3600),
						"seven_day_fable":  mkLimit(44, 1*86400+10*3600),
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n" +
				"- 5-hour: 2.0% (resets in 4h 53m)\n" +
				"- 7-day: 21.0% (resets in 2d 5h)\n" +
				"- 7-day sonnet: 0.0% (resets in 2d 5h)\n" +
				"- 7-day fable: 44.0% (resets in 1d 10h)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"model rename degrades gracefully via humanizer", func(t *testing.T) {
			// If Anthropic renames "Fable" -> "Fable 5", the parser emits
			// seven_day_fable_5, which has no per-model label. It must still
			// render legibly as "7-day fable 5" rather than the raw key.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Limits: map[string]providers.Limit{
						"five_hour":         mkLimit(2, 3600),
						"seven_day_fable_5": mkLimit(15, 7200),
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- 5-hour: 2.0% (resets in 1h 0m)\n- 7-day fable 5: 15.0% (resets in 2h 0m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"genuinely unknown non-model key keeps raw label", func(t *testing.T) {
			// A key that doesn't have the seven_day_ model prefix must be left
			// untouched by the humanizer and still render with its raw key.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Limits: map[string]providers.Limit{
						"five_hour":    mkLimit(2, 3600),
						"window_1234s": mkLimit(9, 60),
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- 5-hour: 2.0% (resets in 1h 0m)\n- window_1234s: 9.0% (resets in 1m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"title case", func(t *testing.T) {
			// Sanity check the capitalization helper isn't broken for all three.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude":  {Limits: map[string]providers.Limit{"five_hour": mkLimit(1, 60)}},
					"codex":   {Limits: map[string]providers.Limit{"five_hour": mkLimit(1, 60)}},
					"copilot": {Limits: map[string]providers.Limit{"month": mkLimit(1, 60)}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude", "codex", "copilot"})
			s := buf.String()
			for _, want := range []string{"Claude usage", "Codex usage", "Copilot usage"} {
				if !strings.Contains(s, want) {
					t.Errorf("missing %q in:\n%s", want, s)
				}
			}
		}},
		{"claude accounts single", func(t *testing.T) {
			// Single active account always renders the nested form so the (active)
			// marker is visible.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Accounts: []providers.AccountResult{
						{
							Email:  "me@example.com",
							Plan:   "default_claude_pro",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(34, 4*3600+53*60),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- me@example.com (active) [Pro]\n  - 5-hour: 34.0% (resets in 4h 53m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"claude accounts two", func(t *testing.T) {
			// Two accounts: active first (renderer trusts caller ordering), inactive
			// second. Both plan labels resolved via rateLimitTierLabels.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Accounts: []providers.AccountResult{
						{
							Email:  "a@work.com",
							Plan:   "default_claude_max_20x",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(10, 3600),
								"seven_day": mkLimit(5, 2*86400+3*3600),
							},
						},
						{
							Email:  "b@personal.com",
							Plan:   "default_claude_max_5x",
							Active: false,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(90, 600),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := string(testutil.LoadFixture(t, "text-claude-accounts-two.golden"))
			if buf.String() != want {
				t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
			}
		}},
		{"claude accounts fallback live row", func(t *testing.T) {
			// Fallback live row: Email = "(live Claude account)", Plan = "", UUID = "".
			// The [Plan] suffix must be omitted entirely; (active) marker must appear.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Accounts: []providers.AccountResult{
						{
							Email:  "(live Claude account)",
							Plan:   "",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(50, 1800),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- (live Claude account) (active)\n  - 5-hour: 50.0% (resets in 30m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"claude accounts per account error", func(t *testing.T) {
			// Per-account error row: no nested limits, error appended after plan label.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Accounts: []providers.AccountResult{
						{
							Email:  "err@example.com",
							Plan:   "default_claude_pro",
							Active: true,
							Error:  "usage fetch timed out",
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- err@example.com (active) [Pro]: usage fetch timed out\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"claude accounts unknown tier", func(t *testing.T) {
			// Unknown rate_limit_tier renders as the raw value (drift-tolerant).
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Accounts: []providers.AccountResult{
						{
							Email:  "x@example.com",
							Plan:   "default_claude_enterprise",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(1, 60),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- x@example.com (active) [default_claude_enterprise]\n  - 5-hour: 1.0% (resets in 1m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"codex accounts single", func(t *testing.T) {
			// Codex has no rate_limit_tier; Plan="" renders without a [Plan] suffix.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"codex": {Accounts: []providers.AccountResult{
						{
							Email:  "me@example.com",
							Plan:   "",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(34, 4*3600+53*60),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"codex"})
			want := "Codex usage\n- me@example.com (active)\n  - 5-hour: 34.0% (resets in 4h 53m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"codex accounts two", func(t *testing.T) {
			// Two Codex accounts with mixed window sets, including the slot-vs-duration-
			// safe `seven_day` + `code_review_seven_day` keys T3 introduced.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"codex": {Accounts: []providers.AccountResult{
						{
							Email:  "a@work.com",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(10, 3600),
								"seven_day": mkLimit(5, 2*86400+3*3600),
							},
						},
						{
							Email:  "b@personal.com",
							Active: false,
							Limits: map[string]providers.Limit{
								"seven_day":             mkLimit(80, 86400),
								"code_review_seven_day": mkLimit(2, 4*86400+3600),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"codex"})
			want := string(testutil.LoadFixture(t, "text-codex-accounts-two.golden"))
			if buf.String() != want {
				t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
			}
		}},
		{"codex accounts fallback live row", func(t *testing.T) {
			// Fallback live row: Email = "(live Codex account)", Plan = "", UUID = "".
			// Mirrors claude accounts fallback live row with the Codex label.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"codex": {Accounts: []providers.AccountResult{
						{
							Email:  "(live Codex account)",
							Plan:   "",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(50, 1800),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"codex"})
			want := "Codex usage\n- (live Codex account) (active)\n  - 5-hour: 50.0% (resets in 30m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"claude accounts model-scoped fable window via humanizer", func(t *testing.T) {
			// Nested accounts form must also route seven_day_fable through the
			// generic humanizer (unknown-key fallback), not a hard-coded label.
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"claude": {Accounts: []providers.AccountResult{
						{
							Email:  "me@example.com",
							Plan:   "default_claude_max_5x",
							Active: true,
							Limits: map[string]providers.Limit{
								"five_hour":       mkLimit(2, 4*3600+53*60),
								"seven_day_fable": mkLimit(44, 1*86400+10*3600),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"})
			want := "Claude usage\n- me@example.com (active) [Max 5x]\n  - 5-hour: 2.0% (resets in 4h 53m)\n  - 7-day fable: 44.0% (resets in 1d 10h)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"codex accounts per account error", func(t *testing.T) {
			r := providers.Report{
				Providers: map[string]providers.ProviderResult{
					"codex": {Accounts: []providers.AccountResult{
						{
							Email:  "err@example.com",
							Active: true,
							Error:  "usage fetch timed out",
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"codex"})
			want := "Codex usage\n- err@example.com (active): usage fetch timed out\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestFormatResetDuration(t *testing.T) {
	tests := []struct {
		name string
		s    int
		want string
	}{
		{"days and hours", 5*86400 + 3*3600, "5d 3h"},
		{"hours and minutes", 4*3600 + 12*60, "4h 12m"},
		{"minutes only", 45 * 60, "45m"},
		{"zero", 0, "0m"},
		{"negative", -30, "0m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatResetDuration(tt.s)
			if got != tt.want {
				t.Errorf("formatResetDuration(%d) = %q, want %q", tt.s, got, tt.want)
			}
		})
	}
}
