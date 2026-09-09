//go:build fake

package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// --- Fake JSON output ---

func TestCLIFakeJSON(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"all providers present", func(t *testing.T) {
			r := runCLI("--fake")
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
				t.Fatalf("invalid JSON: %v\noutput: %s", err, r.stdout)
			}
			if _, ok := got["checked_at"]; !ok {
				t.Fatal("missing checked_at")
			}
			provs, _ := got["providers"].(map[string]any)
			for _, id := range []string{"claude", "codex", "copilot"} {
				if _, ok := provs[id]; !ok {
					t.Errorf("missing provider %s", id)
				}
			}
			claude, _ := provs["claude"].(map[string]any)
			rows, _ := claude["accounts"].([]any)
			if len(rows) != 2 {
				t.Fatalf("Claude fake account count = %d, want 2", len(rows))
			}
			personal, _ := rows[0].(map[string]any)
			team, _ := rows[1].(map[string]any)
			if personal["email"] != "fake@example.com" || team["email"] != "fake@example.com" {
				t.Fatalf("Claude fake emails = %v and %v, want same email", personal["email"], team["email"])
			}
			// Address *formatting* is pinned in internal/address; fake mode only smoke-tests
			// the multi-context shape — every row addressed, and the two rows distinguishable.
			personalAddr, _ := personal["address"].(string)
			teamAddr, _ := team["address"].(string)
			if personalAddr == "" || teamAddr == "" || personalAddr == teamAddr {
				t.Errorf("addresses = %q and %q, want two distinct non-empty addresses", personalAddr, teamAddr)
			}
			if personal["organization_type"] != "claude_max" || team["organization_type"] != "claude_team" {
				t.Errorf("organization types = %v and %v", personal["organization_type"], team["organization_type"])
			}
			if personal["organization_name"] != "fake@example.com's Organization" || team["organization_name"] != "Engineering" {
				t.Errorf("organization names = %v and %v", personal["organization_name"], team["organization_name"])
			}
			if personal["active"] != true || team["active"] != false {
				t.Errorf("active flags = %v and %v, want true and false", personal["active"], team["active"])
			}
			personalLimits, _ := personal["limits"].(map[string]any)
			teamLimits, _ := team["limits"].(map[string]any)
			personalFiveHour, _ := personalLimits["five_hour"].(map[string]any)
			teamFiveHour, _ := teamLimits["five_hour"].(map[string]any)
			if personalFiveHour["used_percent"] == teamFiveHour["used_percent"] {
				t.Errorf("fake Claude five-hour limits unexpectedly equal: %v", personalLimits["five_hour"])
			}
			codex, _ := provs["codex"].(map[string]any)
			codexRows, _ := codex["accounts"].([]any)
			if len(codexRows) != 1 {
				t.Fatalf("Codex fake account count = %d, want 1", len(codexRows))
			}
			codexRow, _ := codexRows[0].(map[string]any)
			for _, field := range []string{"address", "organization_name", "organization_type"} {
				if _, ok := codexRow[field]; ok {
					t.Errorf("Codex fake row unexpectedly contains %q", field)
				}
			}
		}},
		{"single provider excludes others", func(t *testing.T) {
			r := runCLI("usage", "claude", "--fake")
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			var got map[string]any
			_ = json.Unmarshal([]byte(r.stdout), &got)
			provs, _ := got["providers"].(map[string]any)
			if _, ok := provs["claude"]; !ok {
				t.Error("missing claude")
			}
			if _, ok := provs["codex"]; ok {
				t.Error("codex should be absent when single provider requested")
			}
			if _, ok := got["checked_at"]; !ok {
				t.Error("checked_at must always be present")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- Fake text output ---

func TestCLIFakeText(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"design sample shape", func(t *testing.T) {
			r := runCLI("--fake", "-h")
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			// Provider order: Claude → Codex → Copilot.
			iC := strings.Index(r.stdout, "Claude usage")
			iCx := strings.Index(r.stdout, "Codex usage")
			iCp := strings.Index(r.stdout, "Copilot usage")
			if !(iC >= 0 && iC < iCx && iCx < iCp) {
				t.Fatalf("wrong section order:\n%s", r.stdout)
			}
			// Sanity-check one line shape with the design's format.
			if !regexp.MustCompile(`- 5-hour: \d+(\.\d)?% \(resets in [^\)]+\)`).MatchString(r.stdout) {
				t.Fatalf("5-hour line missing or malformed:\n%s", r.stdout)
			}
		}},
		{"provider via usage subcommand", func(t *testing.T) {
			// Provider specified under the "usage" subcommand.
			r := runCLI("usage", "claude", "--fake", "-h")
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if !strings.Contains(r.stdout, "Claude usage") {
				t.Fatalf("missing Claude section: %s", r.stdout)
			}
			if strings.Contains(r.stdout, "Codex usage") {
				t.Fatalf("Codex should be absent: %s", r.stdout)
			}
		}},
		{"human long form matches short form", func(t *testing.T) {
			a := runCLI("--fake", "-h").stdout
			b := runCLI("--fake", "--human").stdout
			if a != b {
				t.Fatalf("-h and --human should match:\n%s\nvs\n%s", a, b)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- Fake error cases ---

func TestCLIFakeErrors(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"unknown subcommand with fake flag", func(t *testing.T) {
			// A bare token that isn't a known subcommand → exit 2 with unknown-subcommand error.
			r := runCLI("--fake", "-h", "bogussubcmd")
			if r.code != 2 {
				t.Fatalf("expected exit 2, got %d (stdout %q)", r.code, r.stdout)
			}
			if !strings.Contains(r.stderr, `unknown subcommand "bogussubcmd"`) {
				t.Fatalf("missing unknown-subcommand error: %s", r.stderr)
			}
		}},
		{"trailing positional under usage rejected", func(t *testing.T) {
			// Two positionals under "usage" → second is rejected.
			r := runCLI("usage", "claude", "codex", "--fake")
			if r.code != 2 {
				t.Fatalf("expected exit 2, got %d", r.code)
			}
			if !strings.Contains(r.stderr, "unexpected positional argument: codex") {
				t.Fatalf("missing trailing-positional error: %s", r.stderr)
			}
		}},
		{"provider failure exits 1", func(t *testing.T) {
			// Round-6 contract: exit 1 for runtime failures, exit 2 reserved for usage
			// / contract errors. The --fake-fail=claude knob injects a hard error in
			// the claude provider's Fetch; the other two still succeed, so the JSON
			// must contain claude.error AND codex.accounts (per-account shape).
			r := runCLI("--fake", "--fake-fail=claude")
			if r.code != 1 {
				t.Fatalf("expected exit 1 for runtime failure, got %d (stderr %q)", r.code, r.stderr)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
				t.Fatalf("invalid JSON: %v\noutput: %s", err, r.stdout)
			}
			provs, _ := got["providers"].(map[string]any)
			claude, _ := provs["claude"].(map[string]any)
			if claude["error"] == nil || claude["error"] == "" {
				t.Errorf("expected claude.error to be populated, got %v", claude)
			}
			if claude["limits"] != nil {
				t.Errorf("claude.limits should be null on failure, got %v", claude["limits"])
			}
			codex, _ := provs["codex"].(map[string]any)
			if codex["accounts"] == nil {
				t.Errorf("expected codex.accounts to still be present (a failing sibling does not block successes), got %v", codex)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
