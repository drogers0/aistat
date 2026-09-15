package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
)

type runResult struct {
	stdout string
	stderr string
	code   int
}

// wantExit asserts r.code == want, else fails the test with Fatalf.
func wantExit(t *testing.T, r runResult, want int) {
	t.Helper()
	if r.code != want {
		t.Fatalf("expected exit %d, got %d\nstdout: %s\nstderr: %s", want, r.code, r.stdout, r.stderr)
	}
}

// wantOut asserts strings.Contains(r.stdout, sub), else fails with Errorf.
func wantOut(t *testing.T, r runResult, sub string) {
	t.Helper()
	if !strings.Contains(r.stdout, sub) {
		t.Errorf("stdout missing %q\nstdout: %s", sub, r.stdout)
	}
}

// wantErrOut asserts strings.Contains(r.stderr, sub), else fails with Errorf.
func wantErrOut(t *testing.T, r runResult, sub string) {
	t.Helper()
	if !strings.Contains(r.stderr, sub) {
		t.Errorf("stderr missing %q\nstderr: %s", sub, r.stderr)
	}
}

// runCLI invokes run() in-process so tests do not pay the per-test go-build
// cost a subprocess approach would. Tests that need --fake live in
// main_fake_test.go (build-tagged) so they only run under -tags=fake.
func runCLI(args ...string) runResult {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return runResult{stdout.String(), stderr.String(), code}
}

// withMemoryStore swaps openAccountStore for a MemoryStore during the test,
// restoring the original on cleanup.
func withMemoryStore(t *testing.T) *accounts.MemoryStore {
	t.Helper()
	ms := accounts.NewMemoryStore()
	old := openAccountStore
	openAccountStore = func(_ io.Writer) (accounts.Store, error) {
		return ms, nil
	}
	t.Cleanup(func() { openAccountStore = old })
	return ms
}

// --- Help / Version ---

func TestCLIHelpVersion(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"help flag", func(t *testing.T) {
			r := runCLI("--help")
			wantExit(t, r, 0)
			wantOut(t, r, "aistat")
			wantOut(t, r, "-h, --human")
			wantOut(t, r, "accounts")
			if r.stderr != "" {
				t.Fatalf("stderr should be empty on --help: %s", r.stderr)
			}
		}},
		{"version flag", func(t *testing.T) {
			r := runCLI("--version")
			wantExit(t, r, 0)
			if got := strings.TrimSpace(r.stdout); got == "" {
				t.Fatalf("expected non-empty version, got empty")
			}
		}},
		{"help lists all known providers", func(t *testing.T) {
			r := runCLI("--help")
			for _, id := range []string{"claude", "codex", "copilot"} {
				wantOut(t, r, id)
			}
		}},
		{"help documents address selectors", func(t *testing.T) {
			r := runCLI("--help")
			wantExit(t, r, 0)
			const flagLine = "  --to <id>          Switch to a specific stored account (address, organization slug, email substring, or UUID prefix)\n"
			const noteLine = "  --to <id> matches a full address, a unique organization slug, an email substring, or a UUID prefix. Without a provider arg,\n"
			if !strings.Contains(r.stdout, flagLine) {
				t.Fatalf("help missing exact flag line %q", flagLine)
			}
			if !strings.Contains(r.stdout, noteLine) {
				t.Fatalf("help missing exact note line %q", noteLine)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- Global flag placement ---

func TestCLIGlobalFlags(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"debug before subcommand accepted", func(t *testing.T) {
			// --debug before "usage" sets g.Debug so providers emit debug lines.
			// In a no-credentials test env the run will fail at the provider level
			// (exit 1), but the flag must be accepted without a usage error (exit 2).
			r := runCLI("--debug", "usage")
			if r.code == 2 {
				t.Fatalf("--debug before usage should not produce exit 2 (flag parse error); got stderr: %s", r.stderr)
			}
		}},
		{"equal form rejected", func(t *testing.T) {
			r := runCLI("--debug=true", "usage")
			wantExit(t, r, 2)
			wantErrOut(t, r, "--flag=value form not supported for global flags")
			wantErrOut(t, r, "--debug")
		}},
		{"human flag placement equivalence", func(t *testing.T) {
			// --human before subcommand and after subcommand are both accepted.
			// We can't compare output (depends on real credentials) but we can
			// ensure neither produces exit 2.
			r1 := runCLI("--human", "usage")
			r2 := runCLI("usage", "--human")
			for _, r := range []runResult{r1, r2} {
				if r.code == 2 {
					t.Fatalf("--human placement should not produce exit 2; stderr: %s", r.stderr)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- Bad input / unknown tokens ---

func TestCLIBadInput(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"unknown subcommand", func(t *testing.T) {
			r := runCLI("unknown-subcmd")
			wantExit(t, r, 2)
			if r.stdout != "" {
				t.Fatalf("stdout should be empty: %s", r.stdout)
			}
			wantErrOut(t, r, `unknown subcommand "unknown-subcmd"`)
		}},
		{"unknown flag", func(t *testing.T) {
			// Unknown flag left in rest by scanGlobals → passed to runUsage's FlagSet.
			r := runCLI("--unknown")
			wantExit(t, r, 2)
			if r.stdout != "" {
				t.Fatalf("stdout should be empty: %s", r.stdout)
			}
			wantErrOut(t, r, "flag provided but not defined")
		}},
		{"dropped json flag", func(t *testing.T) {
			r := runCLI("--json")
			wantExit(t, r, 2)
			if r.stdout != "" {
				t.Fatalf("stdout must be empty: %s", r.stdout)
			}
			wantErrOut(t, r, "flag provided but not defined")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- Usage subcommand ---

func TestCLIUsage(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"no arg equivalent to usage", func(t *testing.T) {
			withMemoryStore(t)
			// No-arg and explicit "usage" both dispatch to runUsage — neither should
			// produce a usage error (exit 2). Provider-level failures (exit 1) are
			// expected in environments without live credentials; that is not a routing
			// failure. Both must produce valid JSON output.
			for _, args := range [][]string{{}, {"usage"}} {
				r := runCLI(args...)
				if r.code == 2 {
					t.Errorf("args %v: should not produce exit 2 (usage/routing error); stderr: %s", args, r.stderr)
					continue
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(r.stdout), &m); err != nil {
					t.Errorf("args %v: invalid JSON: %v\noutput: %s", args, err, r.stdout)
					continue
				}
				if _, ok := m["checked_at"]; !ok {
					t.Errorf("args %v: missing checked_at in JSON output", args)
				}
			}
		}},
		{"unknown provider errors", func(t *testing.T) {
			r := runCLI("usage", "unknown-provider")
			wantExit(t, r, 2)
			wantErrOut(t, r, "usage unknown-provider: provider must be one of claude, codex, copilot")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- switch subcommand ---

func TestCLISwitch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"help flag", func(t *testing.T) {
			r := runCLI("switch", "--help")
			wantExit(t, r, 0)
			wantOut(t, r, "aistat")
		}},
		{"no stored accounts bulk exit 0", func(t *testing.T) {
			// Bulk switch with both stores empty → no eligible providers → exit 0.
			withMemoryStore(t)
			withCodexMemoryStore(t)

			r := runCLI("switch")
			wantExit(t, r, 0)
			wantErrOut(t, r, "no providers have multiple stored accounts")
		}},
		{"claude one account shows login hint", func(t *testing.T) {
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			seedAccount(t, ms, "uuid-only", "only@example.com", "plan", time.Now())
			withSwitchActiveKey(t, "uuid-only")

			r := runCLI("switch", "claude")
			wantExit(t, r, 2)
			wantErrOut(t, r, "claude /login")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- accounts subcommand routing ---

func TestCLIAccounts(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"no subcommand errors", func(t *testing.T) {
			withMemoryStore(t)
			r := runCLI("accounts")
			wantExit(t, r, 2)
			wantErrOut(t, r, "unknown subcommand \"\" — want \"list\" or \"remove\"")
		}},
		{"list empty stores json", func(t *testing.T) {
			withMemoryStore(t)
			withCodexMemoryStore(t)
			r := runCLI("accounts", "list")
			wantExit(t, r, 0)
			// JSON output for empty stores: both provider keys present.
			var result map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &result); err != nil {
				t.Fatalf("expected valid JSON, got %q: %v", r.stdout, err)
			}
			if _, ok := result["claude"]; !ok {
				t.Fatalf("missing claude key in JSON: %s", r.stdout)
			}
		}},
		{"list help flag", func(t *testing.T) {
			withMemoryStore(t)
			r := runCLI("accounts", "list", "--help")
			wantExit(t, r, 0)
			wantOut(t, r, "aistat")
		}},
		{"list version flag", func(t *testing.T) {
			withMemoryStore(t)
			r := runCLI("accounts", "list", "--version")
			wantExit(t, r, 0)
			if got := strings.TrimSpace(r.stdout); got == "" {
				t.Fatalf("expected non-empty version, got empty")
			}
		}},
		{"list claude provider arg", func(t *testing.T) {
			withMemoryStore(t)
			withCodexMemoryStore(t)
			r := runCLI("accounts", "list", "claude")
			wantExit(t, r, 0)
			var result map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &result); err != nil {
				t.Fatalf("expected valid JSON, got %q: %v", r.stdout, err)
			}
			if _, ok := result["claude"]; !ok {
				t.Fatalf("missing claude key in JSON: %s", r.stdout)
			}
			if _, ok := result["codex"]; ok {
				t.Fatalf("codex key should be absent for single-provider list: %s", r.stdout)
			}
		}},
		{"remove unknown provider errors", func(t *testing.T) {
			withMemoryStore(t)
			withCodexMemoryStore(t)
			r := runCLI("accounts", "remove", "some-id", "bogus")
			wantExit(t, r, 2)
			wantErrOut(t, r, "unknown provider")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestScanGlobalsColor covers the value-taking global added for --color. The
// two-token form is the reason scanGlobals has to consume the value at all:
// left to the subcommand FlagSet, `aistat --color never usage` would take
// "never" as the subcommand under the first-non-flag rule.
func TestScanGlobalsColor(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantColor string
		wantSub   string
		wantRest  []string
		wantErr   string
	}{
		{name: "equals form", args: []string{"--color=never", "usage"}, wantColor: "never", wantSub: "usage"},
		{name: "two-token form", args: []string{"--color", "never", "usage"}, wantColor: "never", wantSub: "usage"},
		{name: "two-token form does not eat the subcommand", args: []string{"--color", "always"}, wantColor: "always"},
		{name: "empty value is allowed and means auto", args: []string{"--color=", "usage"}, wantColor: "", wantSub: "usage"},
		{name: "value may look like a flag", args: []string{"--color", "-h"}, wantColor: "-h"},
		{name: "missing value errors", args: []string{"--color"}, wantErr: "flag needs an argument: --color"},
		{name: "boolean =value rejection is unchanged", args: []string{"--human=true"}, wantErr: "--flag=value form not supported for global flags; use `--human`"},
		{name: "color after the subcommand is left for the FlagSet", args: []string{"usage", "--color=never"}, wantSub: "usage", wantRest: []string{"--color=never"}},
		// A value-taking flag that is NOT a global still has to be recognized, or
		// its value is mistaken for the subcommand. --interval is the first such
		// flag reachable before a subcommand token (usage is the default verb).
		{name: "non-global value flag passes through with its value", args: []string{"-h", "--watch", "--interval", "15"},
			wantRest: []string{"--watch", "--interval", "15"}},
		{name: "non-global value flag in equals form passes through", args: []string{"-h", "--interval=15"},
			wantRest: []string{"--interval=15"}},
		{name: "switch value flags do not swallow their subcommand", args: []string{"--to", "alice", "switch"},
			wantSub: "switch", wantRest: []string{"--to", "alice"}},
		{name: "threshold flags take a value", args: []string{"--if-above-5h", "90", "switch"},
			wantSub: "switch", wantRest: []string{"--if-above-5h", "90"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, sub, rest, err := scanGlobals(tt.args)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if g.Color != tt.wantColor {
				t.Errorf("Color = %q, want %q", g.Color, tt.wantColor)
			}
			if sub != tt.wantSub {
				t.Errorf("sub = %q, want %q", sub, tt.wantSub)
			}
			if len(rest) != len(tt.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tt.wantRest)
			}
			for i := range rest {
				if rest[i] != tt.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, rest[i], tt.wantRest[i])
				}
			}
		})
	}
}
