package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/httpx"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/providers/claude"
	"github.com/drogers0/aistat/v2/internal/testenv"
	"github.com/drogers0/aistat/v2/internal/watchstate"
)

func TestUsageRefresh(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"flag accepted before and after provider", func(t *testing.T) {
			// TestUsage_RefreshFlagAcceptedBeforeAndAfterProvider pins the two-pass
			// FlagSet parsing: --refresh must be accepted both before and after the
			// optional provider positional without producing a usage error (exit 2).
			// Provider-level failures (exit 1) are expected in environments without
			// live credentials; what matters is that the flag is recognized.
			withMemoryStore(t)
			for _, args := range [][]string{
				{"usage", "--refresh", "claude"},
				{"usage", "claude", "--refresh"},
				{"usage", "--refresh"},
				{"--refresh"}, // bare invocation (no subcommand token) routes to runUsage
			} {
				r := runCLI(args...)
				if r.code == 2 {
					t.Errorf("args %v: --refresh produced exit 2 (flag parse error); stderr: %s", args, r.stderr)
				}
				if strings.Contains(r.stderr, "flag provided but not defined") {
					t.Errorf("args %v: --refresh not recognized: %s", args, r.stderr)
				}
			}
		}},
		{"passes through to claude option", func(t *testing.T) {
			// TestUsage_RefreshPassesThroughToClaudeOption verifies that --refresh wires
			// WithCacheBypass(true) all the way through buildProviders → realProviders →
			// claude.New. Uses the real provider-construction path (not fake mode) since
			// fake providers bypass claude.New.
			serialStderr := httpx.NewConcurrencySafeWriter(io.Discard)

			// cacheBypass=true: the Claude provider must have CacheBypassEnabled()=true.
			chosen, _ := buildProviders(serialStderr, false, true, nil)
			if len(chosen) == 0 {
				t.Fatal("buildProviders returned no providers")
			}
			claudeClient, ok := chosen[0].(*claude.Client)
			if !ok {
				t.Fatalf("expected first provider to be *claude.Client, got %T", chosen[0])
			}
			if !claudeClient.CacheBypassEnabled() {
				t.Fatal("WithCacheBypass(true) was not threaded through to the claude.Client")
			}

			// cacheBypass=false (default): CacheBypassEnabled() must be false.
			chosen2, _ := buildProviders(serialStderr, false, false, nil)
			if len(chosen2) == 0 {
				t.Fatal("buildProviders returned no providers")
			}
			claudeClient2, ok := chosen2[0].(*claude.Client)
			if !ok {
				t.Fatalf("expected first provider to be *claude.Client, got %T", chosen2[0])
			}
			if claudeClient2.CacheBypassEnabled() {
				t.Fatal("default buildProviders should have CacheBypassEnabled()=false")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestAttachWatchers(t *testing.T) {
	tick := time.Now() // List expires old heartbeats relative to wall time
	publish := func(t *testing.T, pid int, scope ...string) {
		t.Helper()
		if err := watchstate.Publish(watchstate.Heartbeat{PID: pid, Providers: scope, IntervalSecs: 300, LastTick: tick}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	newReport := func() providers.Report {
		return providers.Report{Providers: map[string]providers.ProviderResult{
			"claude":  {Limits: map[string]providers.Limit{}},
			"codex":   {Limits: map[string]providers.Limit{}},
			"copilot": {Limits: map[string]providers.Limit{}},
		}}
	}
	pids := func(r providers.Report, id string) []int {
		var out []int
		for _, w := range r.Providers[id].Watchers {
			out = append(out, w.PID)
		}
		return out
	}
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"bulk and scoped watchers attach to each covered provider", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir())
			publish(t, 1, "claude", "codex")
			publish(t, 2, "codex")
			r := newReport()
			attachWatchers(&r, []string{"claude", "codex", "copilot"}, io.Discard, false)
			if got := pids(r, "claude"); len(got) != 1 || got[0] != 1 {
				t.Errorf("claude watchers = %v, want [1]", got)
			}
			if got := pids(r, "codex"); len(got) != 2 {
				t.Errorf("codex watchers = %v, want two", got)
			}
			if got := pids(r, "copilot"); got != nil {
				t.Errorf("copilot watchers = %v, want none", got)
			}
		}},
		{"unrequested provider stays untouched", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir())
			publish(t, 1, "codex")
			r := newReport()
			attachWatchers(&r, []string{"claude"}, io.Discard, false)
			if got := pids(r, "codex"); got != nil {
				t.Errorf("codex watchers = %v, want none when only claude requested", got)
			}
		}},
		{"no watchers leaves the report unchanged", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir())
			r := newReport()
			var stderr bytes.Buffer
			attachWatchers(&r, []string{"claude", "codex"}, &stderr, true)
			if pids(r, "claude") != nil || pids(r, "codex") != nil || stderr.Len() != 0 {
				t.Errorf("unexpected watchers or stderr %q", stderr.String())
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
