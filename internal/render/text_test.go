package render

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/testutil"
	"github.com/drogers0/aistat/v2/internal/watchstate"
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
			// Claude uses the nested Accounts form; this fixture drives Codex and
			// Copilot through the flat Limits form to exercise both render paths
			// in one demo (in production Codex also emits Accounts).
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
								Email:            "me@personal.com",
								Plan:             "default_claude_max_5x",
								Active:           true,
								OrganizationType: "claude_max",
								Limits: map[string]providers.Limit{
									"five_hour":        mkLimit(2, 4*3600+53*60),
									"seven_day":        mkLimit(21, 2*86400+5*3600),
									"seven_day_sonnet": mkLimit(0, 2*86400+5*3600),
								},
							},
							{
								Email:            "me@work.company.com",
								Plan:             "default_claude_max_20x",
								Active:           false,
								OrganizationName: "Work Company",
								OrganizationType: "claude_team",
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
			testutil.WantNoErr(t, Text(&buf, r, []string{"claude", "codex", "copilot"}, false))
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- 5-hour: 2% (resets in 4h 53m)\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"requested but none in report", func(t *testing.T) {
			var buf bytes.Buffer
			testutil.WantNoErr(t, Text(&buf, providers.Report{}, []string{"claude"}, false))
			if buf.Len() != 0 {
				t.Errorf("expected empty output when no requested providers are in report, got %q", buf.String())
			}
		}},
		{"empty requested", func(t *testing.T) {
			var buf bytes.Buffer
			_ = Text(&buf, providers.Report{}, nil, false)
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
			_ = Text(&buf, r, []string{"claude"}, false)
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
			_ = Text(&buf, r, []string{"claude", "codex"}, false)
			want := "Claude usage\n- 5-hour: 2% (resets in 4h 0m)\n\nCodex usage: Codex token not found\n"
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- 5-hour: 2% (resets in 1h 0m)\n- alpha_extra: 3% (resets in 30m)\n- new_window: 5% (resets in 2h 0m)\n"
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n" +
				"- 5-hour: 2% (resets in 4h 53m)\n" +
				"- 7-day: 21% (resets in 2d 5h)\n" +
				"- 7-day sonnet: 0% (resets in 2d 5h)\n" +
				"- 7-day fable: 44% (resets in 1d 10h)\n"
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- 5-hour: 2% (resets in 1h 0m)\n- 7-day fable 5: 15% (resets in 2h 0m)\n"
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- 5-hour: 2% (resets in 1h 0m)\n- window_1234s: 9% (resets in 1m)\n"
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
			_ = Text(&buf, r, []string{"claude", "codex", "copilot"}, false)
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- me@example.com (active) [Pro]\n  - 5-hour: 34% (resets in 4h 53m)\n"
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
							Email:            "a@work.com",
							Plan:             "default_claude_max_20x",
							Active:           true,
							OrganizationName: "Work",
							OrganizationType: "claude_team",
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(10, 3600),
								"seven_day": mkLimit(5, 2*86400+3*3600),
							},
						},
						{
							Email:            "b@personal.com",
							Plan:             "default_claude_max_5x",
							Active:           false,
							OrganizationType: "claude_max",
							Limits: map[string]providers.Limit{
								"five_hour": mkLimit(90, 600),
							},
						},
					}},
				},
			}
			var buf bytes.Buffer
			_ = Text(&buf, r, []string{"claude"}, false)
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- (live Claude account) (active)\n  - 5-hour: 50% (resets in 30m)\n"
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- err@example.com (active) [Pro]: usage fetch timed out\n"
			if buf.String() != want {
				t.Fatalf("got %q want %q", buf.String(), want)
			}
		}},
		{"claude organization contexts and raven label", func(t *testing.T) {
			r := providers.Report{Providers: map[string]providers.ProviderResult{
				"claude": {Accounts: []providers.AccountResult{
					{Email: "team@example.com", Plan: "default_raven", Active: true, OrganizationName: "Acme", OrganizationType: "claude_team", Limits: map[string]providers.Limit{}},
					{Email: "me@example.com", Plan: "default_claude_max_5x", Active: true, OrganizationName: "me@example.com's Organization", OrganizationType: "claude_max", Limits: map[string]providers.Limit{}},
					{Email: "unknown@example.com", Plan: "default_claude_pro", OrganizationName: "Raven Lab", OrganizationType: "raven", Limits: map[string]providers.Limit{}},
					{Email: "unnamed@example.com", Plan: "default_claude_pro", OrganizationType: "", Limits: map[string]providers.Limit{}},
					{Email: "empty-team@example.com", Plan: "default_claude_pro", OrganizationType: "claude_team", Limits: map[string]providers.Limit{}},
					{Email: "sentinel@example.com", Plan: "default_claude_pro", Address: "sentinel@example.com/personal-aaaaaaaa", Limits: map[string]providers.Limit{}},
				}},
			}}
			var buf bytes.Buffer
			testutil.WantNoErr(t, Text(&buf, r, []string{"claude"}, false))
			const want = "Claude usage\n" +
				"- team@example.com (active) [Raven] (Acme, team)\n" +
				"- me@example.com (active) [Max 5x] (personal)\n" +
				"- unknown@example.com [Pro] (Raven Lab)\n" +
				"- unnamed@example.com [Pro]\n" +
				"- empty-team@example.com [Pro]\n" +
				"- sentinel@example.com [Pro] (personal)\n"
			if buf.String() != want {
				t.Fatalf("context rows = %q, want %q", buf.String(), want)
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- x@example.com (active) [default_claude_enterprise]\n  - 5-hour: 1% (resets in 1m)\n"
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
			_ = Text(&buf, r, []string{"codex"}, false)
			want := "Codex usage\n- me@example.com (active)\n  - 5-hour: 34% (resets in 4h 53m)\n"
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
			_ = Text(&buf, r, []string{"codex"}, false)
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
			_ = Text(&buf, r, []string{"codex"}, false)
			want := "Codex usage\n- (live Codex account) (active)\n  - 5-hour: 50% (resets in 30m)\n"
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
			_ = Text(&buf, r, []string{"claude"}, false)
			want := "Claude usage\n- me@example.com (active) [Max 5x]\n  - 5-hour: 2% (resets in 4h 53m)\n  - 7-day fable: 44% (resets in 1d 10h)\n"
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
			_ = Text(&buf, r, []string{"codex"}, false)
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

func TestTextWatchers(t *testing.T) {
	checked := time.Date(2026, 9, 10, 21, 5, 27, 0, time.UTC)
	five, weekly := 85.0, 95.0
	fresh := providers.WatcherView{
		PID: 111, IntervalSecs: 300, LastTick: checked.Add(-7 * time.Second), ReadAt: checked,
		Thresholds: watchstate.Thresholds{FiveHour: &five, Weekly: &weekly},
	}
	stale := providers.WatcherView{
		PID: 222, IntervalSecs: 300, LastTick: checked.Add(-14 * time.Minute), ReadAt: checked,
		Thresholds: watchstate.Thresholds{FiveHour: &five},
	}
	offWeekly := fresh
	offWeekly.Thresholds.Weekly = nil
	tests := []struct {
		name   string
		result providers.ProviderResult
		want   string
	}{
		{"fresh and stale under accounts header",
			providers.ProviderResult{
				Accounts: []providers.AccountResult{{Email: "me@example.com", Active: true, Limits: map[string]providers.Limit{"five_hour": mkLimit(47, 1800)}}},
				Watchers: []providers.WatcherView{fresh, stale},
			},
			"Claude usage\n" +
				"  auto-switch: 5h \u2265 85%, weekly \u2265 95% (checked 7s ago)\n" +
				"  auto-switch: STALE, last checked 14m ago (pid 222)\n" +
				"- me@example.com (active)\n  - 5-hour: 47% (resets in 30m)\n"},
		{"off threshold under flat header",
			providers.ProviderResult{
				Limits:   map[string]providers.Limit{"five_hour": mkLimit(47, 1800)},
				Watchers: []providers.WatcherView{offWeekly},
			},
			"Claude usage\n  auto-switch: 5h \u2265 85%, weekly off (checked 7s ago)\n- 5-hour: 47% (resets in 30m)\n"},
		{"flat fetch error keeps watcher",
			providers.ProviderResult{Error: "boom", Watchers: []providers.WatcherView{fresh}},
			"Claude usage: boom\n  auto-switch: 5h \u2265 85%, weekly \u2265 95% (checked 7s ago)\n"},
		{"no windows keeps watcher",
			providers.ProviderResult{Limits: map[string]providers.Limit{}, Watchers: []providers.WatcherView{fresh}},
			"Claude usage\n  auto-switch: 5h \u2265 85%, weekly \u2265 95% (checked 7s ago)\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// CheckedAt predates the read by an hour: ages must come from ReadAt.
			r := providers.Report{CheckedAt: checked.Add(-time.Hour), Providers: map[string]providers.ProviderResult{"claude": tt.result}}
			var buf bytes.Buffer
			testutil.WantNoErr(t, Text(&buf, r, []string{"claude"}, false))
			if buf.String() != tt.want {
				t.Fatalf("got %q want %q", buf.String(), tt.want)
			}
		})
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

func TestFormatPercent(t *testing.T) {
	tests := []struct {
		name string
		p    float64
		want string
	}{
		{"zero", 0, "0"},
		{"whole", 92, "92"},
		{"hundred", 100, "100"},
		{"one decimal", 92.4, "92.4"},
		{"sub-one", 0.5, "0.5"},
		{"boundary rounds to whole", 99.95, "100"}, // 99.95 -> "100.0" -> "100"
		{"fractional copilot", 73.447, "73.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatPercent(tt.p)
			if got != tt.want {
				t.Errorf("formatPercent(%v) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}

// TestFormatLimitLineFractional guards the display path that Copilot exercises
// in production: percent_remaining is a float, so used percentages can be
// genuinely fractional and the decimal must survive rendering (issue #29).
func TestFormatLimitLineFractional(t *testing.T) {
	t.Run("copilot fraction preserved", func(t *testing.T) {
		got := formatLimitLine("month", mkLimit(73.4, 5*86400), false)
		want := "- month: 73.4% (resets in 5d 0h)"
		if got != want {
			t.Errorf("formatLimitLine = %q, want %q", got, want)
		}
	})
}

// TestTextColor covers the whole point of in-renderer color: usage percentages
// are wrapped, and the watcher threshold line is not. A post-hoc regex over
// rendered output paints both, which is the bug this replaces.
func TestTextColor(t *testing.T) {
	checked := time.Date(2026, 9, 10, 21, 5, 27, 0, time.UTC)
	five, weekly := 85.0, 95.0
	// A dedicated report rather than an existing fixture: the shared ones top out
	// at 90% and would not reach the dark-red branch, and editing one would churn
	// a golden whose job is to prove color-off output never moved.
	r := providers.Report{
		CheckedAt: checked,
		Providers: map[string]providers.ProviderResult{"claude": {
			Accounts: []providers.AccountResult{
				{Email: "calm@example.com", Active: true, Limits: map[string]providers.Limit{
					"five_hour": mkLimit(10, 3600), "seven_day": mkLimit(72, 5*86400),
				}},
				{Email: "hot@example.com", Limits: map[string]providers.Limit{
					"five_hour": mkLimit(90, 600), "seven_day": mkLimit(100, 2*86400),
				}},
			},
			Watchers: []providers.WatcherView{{
				PID: 111, IntervalSecs: 300, LastTick: checked.Add(-7 * time.Second), ReadAt: checked,
				Thresholds: watchstate.Thresholds{FiveHour: &five, Weekly: &weekly},
			}},
		}},
	}

	t.Run("color on wraps only usage percentages", func(t *testing.T) {
		var buf bytes.Buffer
		testutil.WantNoErr(t, Text(&buf, r, []string{"claude"}, true))
		want := string(testutil.LoadFixture(t, "text-color-sample.golden"))
		if buf.String() != want {
			t.Fatalf("got %q want %q", buf.String(), want)
		}
	})

	t.Run("watcher thresholds are never colored", func(t *testing.T) {
		var buf bytes.Buffer
		testutil.WantNoErr(t, Text(&buf, r, []string{"claude"}, true))
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "auto-switch:") && strings.Contains(line, "\x1b") {
				t.Errorf("watcher line carries an escape: %q", line)
			}
		}
	})

	t.Run("color off differs from color on by escapes alone", func(t *testing.T) {
		var buf bytes.Buffer
		testutil.WantNoErr(t, Text(&buf, r, []string{"claude"}, false))
		// Stripping every escape from the colored golden must reproduce the
		// uncolored render exactly. A weaker "contains no \x1b" check would pass
		// even if color mode changed spacing, ordering or rounding.
		colored := string(testutil.LoadFixture(t, "text-color-sample.golden"))
		stripped := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(colored, "")
		if buf.String() != stripped {
			t.Fatalf("color-off render = %q, want %q", buf.String(), stripped)
		}
	})
}
