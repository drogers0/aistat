package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/autoswitch"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/providers/claude"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

func th(fiveHour, weekly float64, fiveOff, weeklyOff bool) autoswitch.Thresholds {
	return autoswitch.Thresholds{
		FiveHour: autoswitch.Threshold{Pct: fiveHour, Off: fiveOff},
		Weekly:   autoswitch.Threshold{Pct: weekly, Off: weeklyOff},
	}
}

func TestTriggerReason(t *testing.T) {
	tests := []struct {
		name       string
		remaining  map[string]float64 // makeLimitsFull input: RemainingPercent per window
		th         autoswitch.Thresholds
		wantReason string
		wantHit    bool
	}{
		{"below both thresholds", map[string]float64{"five_hour": 58, "seven_day": 50},
			th(85, 95, false, false), "", false},
		{"five_hour at threshold triggers", map[string]float64{"five_hour": 15, "seven_day": 50},
			th(85, 95, false, false), "five_hour at 85%", true},
		{"five_hour above threshold triggers", map[string]float64{"five_hour": 13},
			th(85, 95, false, false), "five_hour at 87%", true},
		{"weekly triggers when 5h is fine", map[string]float64{"five_hour": 60, "seven_day": 4},
			th(85, 95, false, false), "seven_day at 96%", true},
		{"weekly at exact threshold triggers", map[string]float64{"five_hour": 60, "seven_day": 5},
			th(85, 95, false, false), "seven_day at 95%", true},
		{"thirty_day is the binding long window", map[string]float64{"five_hour": 60, "seven_day": 50, "thirty_day": 3},
			th(85, 95, false, false), "thirty_day at 97%", true},
		{"fable weekly is the binding long window", map[string]float64{"five_hour": 60, "seven_day": 50, "seven_day_fable": 4},
			th(85, 95, false, false), "seven_day_fable at 96%", true},
		{"5h checked before weekly", map[string]float64{"five_hour": 10, "seven_day": 2},
			th(85, 95, false, false), "five_hour at 90%", true},
		{"missing five_hour window cannot trigger 5h", map[string]float64{"seven_day": 50},
			th(85, 95, false, false), "", false},
		{"no windows at all never triggers", map[string]float64{},
			th(85, 95, false, false), "", false},
		{"off disables five_hour", map[string]float64{"five_hour": 1, "seven_day": 50},
			th(0, 95, true, false), "", false},
		{"off disables weekly", map[string]float64{"five_hour": 60, "seven_day": 1},
			th(85, 0, false, true), "", false},
		{"both off never triggers", map[string]float64{"five_hour": 1, "seven_day": 1},
			th(0, 0, true, true), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, hit := triggerReason(makeLimitsFull(tt.remaining), tt.th)
			if reason != tt.wantReason || hit != tt.wantHit {
				t.Errorf("triggerReason = (%q, %v), want (%q, %v)", reason, hit, tt.wantReason, tt.wantHit)
			}
		})
	}
}

func TestUsageSummary(t *testing.T) {
	tests := []struct {
		name      string
		remaining map[string]float64
		want      string
	}{
		{"prefers five_hour", map[string]float64{"five_hour": 58, "seven_day": 10}, "five_hour at 42%"},
		{"falls back to binding long window", map[string]float64{"seven_day": 70, "thirty_day": 20}, "thirty_day at 80%"},
		{"tie resolves to first long key", map[string]float64{"seven_day": 50, "thirty_day": 50}, "seven_day at 50%"},
		{"fable can be the binding fallback", map[string]float64{"seven_day": 70, "seven_day_fable": 20}, "seven_day_fable at 80%"},
		{"no windows", map[string]float64{}, "no usage windows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usageSummary(makeLimitsFull(tt.remaining)); got != tt.want {
				t.Errorf("usageSummary = %q, want %q", got, tt.want)
			}
		})
	}
}

// withEnvFilePath points the threshold env-file resolution at path.
func withEnvFilePath(t *testing.T, path string) {
	t.Helper()
	old := autoswitchEnvPath
	autoswitchEnvPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { autoswitchEnvPath = old })
}

// withNotifyCapture records notifications as "Title: message" strings.
func withNotifyCapture(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := sendNotification
	sendNotification = func(_ context.Context, title, message string) error {
		got = append(got, title+": "+message)
		return nil
	}
	t.Cleanup(func() { sendNotification = old })
	return &got
}

// clearThresholdEnv guards the test against the developer's real environment.
func clearThresholdEnv(t *testing.T) {
	t.Helper()
	t.Setenv(autoswitch.EnvFiveHour, "")
	t.Setenv(autoswitch.EnvWeekly, "")
}

// seedTwoAccounts stores work (active) + personal and stubs the active UUID.
func seedTwoAccounts(t *testing.T) *accounts.MemoryStore {
	t.Helper()
	ms := withMemoryStore(t)
	now := time.Now()
	seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))
	seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
	withSwitchActiveUUID(t, "uuid-work")
	return ms
}

func TestSwitchIfNeeded(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"below threshold prints no switch needed", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			// The gate reconciles unconditionally before the fetch below; stub the
			// client so this doesn't touch the real Claude Keychain credential.
			withSwitchClient(t, &stubSwitchClient{})
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 58, "seven_day": 50}), nil
			})
			written, _ := withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--if-needed", "--notify")
			wantExit(t, r, 0)
			wantOut(t, r, "no switch needed (five_hour at 42%)")
			if len(*written) != 0 {
				t.Errorf("live blob written despite no trigger: %s", *written)
			}
			if len(*notes) != 0 {
				t.Errorf("unexpected notifications: %v", *notes)
			}
		}},
		{"gate reconciles before reading the store", func(t *testing.T) {
			// The active account's stored token goes stale between polls (Claude
			// Code refreshes the live credential in place; only `aistat usage`'s
			// reconcile syncs the store copy back). The --if-needed gate must
			// reconcile before it fetches the active account's usage, or a stale
			// stored token 401-loops forever. Order proof: the fetch closure below
			// asserts reconcileCalled is already true by the time it runs.
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			stub := &stubSwitchClient{}
			withSwitchClient(t, stub)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				if !stub.reconcileCalled {
					t.Error("gate fetched active-account usage before reconciling — stale stored token would 401-loop")
				}
				return makeLimitsFull(map[string]float64{"five_hour": 58, "seven_day": 50}), nil
			})
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 0)
			wantOut(t, r, "no switch needed")
		}},
		{"gate sees a store blob updated by reconcile", func(t *testing.T) {
			// Reconcile alone is not enough — the gate must also re-read the store
			// afterward, otherwise it keeps using the pre-reconcile in-memory
			// `stored` slice. The stub's ReconcileAndPersist mutates the memory
			// store's uuid-work account with a distinguishable RawBlob; the fetch
			// closure asserts it saw the post-reconcile token, not the seeded one.
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			ms := withMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			withSwitchActiveUUID(t, "uuid-work")

			preToken := claude.StoredAccessToken(getAccountFromStore(t, ms, "uuid-work"))

			stub := &stubSwitchClient{
				reconcileFn: func(ctx context.Context) error {
					orig := getAccountFromStore(t, ms, "uuid-work")
					rawBlob, _ := json.Marshal(map[string]any{
						"claudeAiOauth": map[string]any{"accessToken": "fresh-token"},
					})
					updated, err := accounts.NewAccount(rawBlob, orig.UUID, orig.Email, orig.DisplayName, orig.RateLimitTier, orig.LastSeenAt)
					if err != nil {
						return err
					}
					return ms.Upsert(ctx, updated)
				},
			}
			withSwitchClient(t, stub)

			var seenToken string
			withFetchLiveUsageFn(t, func(token string) (map[string]providers.Limit, error) {
				seenToken = token
				return makeLimitsFull(map[string]float64{"five_hour": 58, "seven_day": 50}), nil
			})

			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 0)
			wantOut(t, r, "no switch needed")
			if seenToken != "fresh-token" {
				t.Errorf("gate fetched with token %q (pre-reconcile was %q), want post-reconcile token %q — store was not re-read after reconcile",
					seenToken, preToken, "fresh-token")
			}
		}},
		{"5h over threshold switches and notifies", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchResults: []providers.AccountResult{
				{UUID: "uuid-work", Email: "work@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50})},
				{UUID: "uuid-personal", Email: "personal@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 90, "seven_day": 80})},
			}})
			written, _ := withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--if-needed", "--notify")
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com")
			if len(*written) == 0 {
				t.Fatal("expected live blob write")
			}
			if len(*notes) != 1 || (*notes)[0] != "Claude: switched to personal@example.com (five_hour at 87%)" {
				t.Errorf("notifications = %v", *notes)
			}
		}},
		{"weekly over threshold switches", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 4}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchResults: []providers.AccountResult{
				{UUID: "uuid-work", Email: "work@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 4})},
				{UUID: "uuid-personal", Email: "personal@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 90, "seven_day": 80})},
			}})
			_, _ = withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--if-needed", "--notify")
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com")
			if len(*notes) != 1 || (*notes)[0] != "Claude: switched to personal@example.com (seven_day at 96%)" {
				t.Errorf("notifications = %v", *notes)
			}
		}},
		{"triggered but already on best warns via notification", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchResults: []providers.AccountResult{
				{UUID: "uuid-work", Email: "work@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50})},
				{UUID: "uuid-personal", Email: "personal@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 5, "seven_day": 50})},
			}})
			written, _ := withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--if-needed", "--notify")
			wantExit(t, r, 0)
			wantOut(t, r, "already on best account (work@example.com)")
			if len(*written) != 0 {
				t.Errorf("live blob written despite already-on-best: %s", *written)
			}
			if len(*notes) != 1 || (*notes)[0] != "Claude: five_hour at 87%, no better account available" {
				t.Errorf("notifications = %v", *notes)
			}
		}},
		{"unknown active account fails closed", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			ms := withMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", now)
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", now)
			withSwitchActiveUUID(t, "")
			// Stub so the unconditional reconcile doesn't touch the real Claude client.
			withSwitchClient(t, &stubSwitchClient{})
			written, _ := withWriteBlob(t)
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 1)
			wantErrOut(t, r, "cannot determine the active account; skipping conditional switch")
			if len(*written) != 0 {
				t.Errorf("live blob written despite fail-closed: %s", *written)
			}
		}},
		{"active usage fetch error fails closed", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			// Stub so the unconditional reconcile doesn't touch the real Claude client.
			withSwitchClient(t, &stubSwitchClient{})
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return nil, errors.New("boom")
			})
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 1)
			wantErrOut(t, r, "usage fetch for active account failed")
		}},
		{"post-trigger candidate fetch error exits one", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchErr: errors.New("network blip")})
			written, _ := withWriteBlob(t)
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 1)
			wantErrOut(t, r, "auto-pick usage fetch failed")
			if len(*written) != 0 {
				t.Errorf("live blob written despite fetch error: %s", *written)
			}
		}},
		{"if-needed with to is a usage error", func(t *testing.T) {
			r := runSwitchTest("claude", "--if-needed", "--to", "work")
			wantExit(t, r, 2)
			wantErrOut(t, r, "--if-needed cannot be combined with --to")
		}},
		{"invalid env threshold is a usage error", func(t *testing.T) {
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			t.Setenv(autoswitch.EnvFiveHour, "banana")
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 2)
			wantErrOut(t, r, "(source: environment)")
		}},
		{"env file threshold is honored", func(t *testing.T) {
			clearThresholdEnv(t)
			path := filepath.Join(t.TempDir(), "autoswitch.env")
			testutil.WantNoErr(t, autoswitch.WriteEnvFile(path, "50", "95"))
			withEnvFilePath(t, path)
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 45, "seven_day": 80}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchResults: []providers.AccountResult{
				{UUID: "uuid-work", Email: "work@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 45, "seven_day": 80})},
				{UUID: "uuid-personal", Email: "personal@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 90, "seven_day": 80})},
			}})
			_, _ = withWriteBlob(t)
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com")
		}},
		{"off in env disables the 5h trigger", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			t.Setenv(autoswitch.EnvFiveHour, "off")
			seedTwoAccounts(t)
			// Stub so the unconditional reconcile doesn't touch the real Claude client.
			withSwitchClient(t, &stubSwitchClient{})
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 1, "seven_day": 50}), nil
			})
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 0)
			wantOut(t, r, "no switch needed (five_hour at 99%)")
		}},
		{"bulk without provider applies the gate per provider", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withCodexMemoryStore(t)
			// Stub so the unconditional reconcile doesn't touch the real Claude client.
			withSwitchClient(t, &stubSwitchClient{})
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 58, "seven_day": 50}), nil
			})
			written, _ := withWriteBlob(t)
			r := runSwitchTest("--if-needed")
			wantExit(t, r, 0)
			wantOut(t, r, "[claude]")
			wantOut(t, r, "no switch needed (five_hour at 42%)")
			if strings.Contains(r.stdout, "[codex]") {
				t.Errorf("codex must be skipped with <2 stored accounts:\n%s", r.stdout)
			}
			if len(*written) != 0 {
				t.Errorf("live blob written despite no trigger: %s", *written)
			}
		}},
		{"triggered with single stored account exits zero and warns", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			ms := withMemoryStore(t)
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", time.Now())
			withSwitchActiveUUID(t, "uuid-work")
			// The gate now reconciles unconditionally before resolving the active
			// account, so this must be stubbed like its siblings — otherwise it
			// would exercise the real Claude client (real keychain read) and, on a
			// machine with a real Claude Code login, could grow `stored` past one
			// account and change this test's outcome.
			withSwitchClient(t, &stubSwitchClient{})
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50}), nil
			})
			written, _ := withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--if-needed", "--notify")
			wantExit(t, r, 0)
			wantErrOut(t, r, "only one account stored; nothing to switch to")
			if len(*notes) != 1 || (*notes)[0] != "Claude: five_hour at 87%, no better account available" {
				t.Errorf("notifications = %v", *notes)
			}
			if len(*written) != 0 {
				t.Errorf("live blob written despite single-account dead end: %s", *written)
			}
		}},
		{"to with notify sends plain notification", func(t *testing.T) {
			seedTwoAccounts(t)
			withSwitchClient(t, &stubSwitchClient{})
			_, _ = withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--to", "personal", "--notify")
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com")
			if len(*notes) != 1 || (*notes)[0] != "Claude: switched to personal@example.com" {
				t.Errorf("notifications = %v", *notes)
			}
		}},
		{"notification failure never changes the exit code", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchResults: []providers.AccountResult{
				{UUID: "uuid-work", Email: "work@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50})},
				{UUID: "uuid-personal", Email: "personal@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 90, "seven_day": 80})},
			}})
			_, _ = withWriteBlob(t)
			old := sendNotification
			sendNotification = func(_ context.Context, _, _ string) error { return errors.New("osascript exploded") }
			t.Cleanup(func() { sendNotification = old })
			r := runSwitchTest("claude", "--if-needed", "--notify")
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com")
			wantErrOut(t, r, "notification failed")
		}},
		{"switch without notify flag stays silent", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchResults: []providers.AccountResult{
				{UUID: "uuid-work", Email: "work@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 13, "seven_day": 50})},
				{UUID: "uuid-personal", Email: "personal@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 90, "seven_day": 80})},
			}})
			_, _ = withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--if-needed")
			wantExit(t, r, 0)
			if len(*notes) != 0 {
				t.Errorf("unexpected notifications without --notify: %v", *notes)
			}
		}},
		{"fable weekly over threshold switches", func(t *testing.T) {
			clearThresholdEnv(t)
			withEnvFilePath(t, filepath.Join(t.TempDir(), "absent.env"))
			seedTwoAccounts(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 50, "seven_day_fable": 3}), nil
			})
			withSwitchClient(t, &stubSwitchClient{fetchResults: []providers.AccountResult{
				{UUID: "uuid-work", Email: "work@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 50, "seven_day_fable": 3})},
				{UUID: "uuid-personal", Email: "personal@example.com", Limits: makeLimitsFull(map[string]float64{"five_hour": 90, "seven_day": 80, "seven_day_fable": 85})},
			}})
			_, _ = withWriteBlob(t)
			notes := withNotifyCapture(t)
			r := runSwitchTest("claude", "--if-needed", "--notify")
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com")
			if len(*notes) != 1 || (*notes)[0] != "Claude: switched to personal@example.com (seven_day_fable at 97%)" {
				t.Errorf("notifications = %v", *notes)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
