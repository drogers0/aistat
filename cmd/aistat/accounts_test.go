package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// noopResolver is a stub resolveActiveKey that always returns "" (no active account).
func noopResolver(_ context.Context, _ []accounts.Account) (string, error) {
	return "", nil
}

// stubResolver returns a resolver that always reports the given key as active.
func stubResolver(activeKey string) func(context.Context, []accounts.Account) (string, error) {
	return func(_ context.Context, _ []accounts.Account) (string, error) {
		return activeKey, nil
	}
}

// runAccountsTest calls runAccounts with a single-provider (Claude) providerStore
// built from the given store and resolver. g controls rendering (Human: true for text).
func runAccountsTest(store *accounts.MemoryStore, resolver func(context.Context, []accounts.Account) (string, error), g globals, args ...string) runResult {
	var stdout, stderr bytes.Buffer
	ps := []providerStore{{
		id:             "claude",
		store:          store,
		activeResolver: resolver,
		logoutHint:     "use 'claude /logout' first",
	}}
	code := runAccounts(args, &stdout, &stderr, g, ps)
	return runResult{stdout.String(), stderr.String(), code}
}

// seedAccount inserts an account into ms with the given fields.
func seedAccount(t *testing.T, ms *accounts.MemoryStore, uuid, email, plan string, lastSeen time.Time) {
	t.Helper()
	rawBlob, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "tok-" + uuid, "refreshToken": "rt-" + uuid},
	})
	a, err := accounts.NewAccount(rawBlob, uuid, email, email, plan, "", "", "", lastSeen)
	testutil.WantNoErr(t, err)
	if err := ms.Upsert(context.Background(), a); err != nil {
		t.Fatalf("seedAccount Upsert: %v", err)
	}
}

func seedClaudeContext(t *testing.T, ms *accounts.MemoryStore, uuid, email, plan, organizationUUID, organizationName, organizationType string, lastSeen time.Time) accounts.Account {
	t.Helper()
	rawBlob, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "tok-" + uuid + organizationUUID, "refreshToken": "rt-" + uuid + organizationUUID},
	})
	testutil.WantNoErr(t, err)
	a, err := accounts.NewAccount(rawBlob, uuid, email, email, plan, organizationUUID, organizationName, organizationType, lastSeen)
	testutil.WantNoErr(t, err)
	testutil.WantNoErr(t, ms.Upsert(context.Background(), a))
	return a
}

// --- Unknown subcommand errors ---

func TestAccounts(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"empty subcommand", func(t *testing.T) {
			ms := testutil.MemStore(t)
			r := runAccountsTest(ms, noopResolver, globals{}) // no args → empty sub
			wantExit(t, r, 2)
			wantErrOut(t, r, "unknown subcommand \"\" — want \"list\" or \"remove\"")
		}},
		{"unknown subcommand", func(t *testing.T) {
			ms := testutil.MemStore(t)
			r := runAccountsTest(ms, noopResolver, globals{}, "foo")
			wantExit(t, r, 2)
			wantErrOut(t, r, "unknown subcommand \"foo\" — want \"list\" or \"remove\"")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- Store-open failure (tested via runAccountsSubcommand) ---

func TestAccountsSubcmd_StoreOpenFailure(t *testing.T) {
	old := openAccountStore
	injectErr := errors.New("permission denied")
	openAccountStore = func(_ io.Writer) (accounts.Store, error) {
		return nil, injectErr
	}
	t.Cleanup(func() { openAccountStore = old })

	var stdout, stderr bytes.Buffer
	code := runAccountsSubcommand([]string{"list"}, &stdout, &stderr, globals{})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
	want := "aistat: claude: could not open account store: permission denied"
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("missing error %q; stderr: %s", want, stderr.String())
	}
}

// --- accounts list (single-provider tests) ---

func TestAccountsList(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		// ---- single-provider (text mode) ----
		{"empty store", func(t *testing.T) {
			ms := testutil.MemStore(t)
			r := runAccountsTest(ms, noopResolver, globals{Human: true}, "list")
			wantExit(t, r, 0)
			if r.stdout != "" {
				t.Fatalf("expected empty stdout, got %q", r.stdout)
			}
		}},
		{"not stale at 30 days", func(t *testing.T) {
			ms := testutil.MemStore(t)
			// 30 days minus 1 minute: unambiguously NOT stale regardless of test execution time.
			lastSeen := time.Now().Add(-30*24*time.Hour + time.Minute)
			seedAccount(t, ms, "aaaa-1111", "user@example.com", "default_claude_max_5x", lastSeen)

			r := runAccountsTest(ms, noopResolver, globals{Human: true}, "list")
			wantExit(t, r, 0)
			if strings.Contains(r.stdout, "(stale)") {
				t.Fatalf("account at <30 days should NOT be stale; stdout: %s", r.stdout)
			}
			wantOut(t, r, "user@example.com")
		}},
		{"stale after 30 days plus 1 minute", func(t *testing.T) {
			ms := testutil.MemStore(t)
			// 30 days + 1 minute: unambiguously stale regardless of test execution time.
			lastSeen := time.Now().Add(-30*24*time.Hour - time.Minute)
			seedAccount(t, ms, "bbbb-2222", "old@example.com", "default_claude_pro", lastSeen)

			r := runAccountsTest(ms, noopResolver, globals{Human: true}, "list")
			wantExit(t, r, 0)
			wantOut(t, r, "(stale)")
		}},
		{"sorted by email", func(t *testing.T) {
			ms := testutil.MemStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-z", "z@example.com", "plan", now)
			seedAccount(t, ms, "uuid-a", "a@example.com", "plan", now)
			seedAccount(t, ms, "uuid-m", "m@example.com", "plan", now)

			r := runAccountsTest(ms, noopResolver, globals{Human: true}, "list")
			wantExit(t, r, 0)
			idxA := strings.Index(r.stdout, "a@example.com")
			idxM := strings.Index(r.stdout, "m@example.com")
			idxZ := strings.Index(r.stdout, "z@example.com")
			if !(idxA < idxM && idxM < idxZ) {
				t.Fatalf("accounts not sorted by email; stdout:\n%s", r.stdout)
			}
		}},
		{"canonical claude list uses address and organization metadata", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seen := time.Now()
			seedClaudeContext(t, ms,
				"9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c",
				"me@example.com",
				"default_claude_max_5x",
				"7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042",
				"me@example.com's Organization",
				"claude_max", seen)

			human := runAccountsTest(ms, noopResolver, globals{Human: true}, "list")
			wantExit(t, human, 0)
			if human.stdout != "me@example.com/personal-9f2a41c7  9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c  default_claude_max_5x\n" {
				t.Fatalf("human list = %q", human.stdout)
			}

			jsonResult := runAccountsTest(ms, noopResolver, globals{}, "list")
			wantExit(t, jsonResult, 0)
			const want = "{\"claude\":[{\"email\":\"me@example.com\",\"uuid\":\"9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c\",\"plan\":\"default_claude_max_5x\",\"stale\":false,\"address\":\"me@example.com/personal-9f2a41c7\",\"organization_name\":\"me@example.com's Organization\",\"organization_type\":\"claude_max\"}]}\n"
			if jsonResult.stdout != want {
				t.Fatalf("JSON list = %q, want %q", jsonResult.stdout, want)
			}
		}},
		{"equal email rows sort by key in text and JSON", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seen := time.Now()
			seedClaudeContext(t, ms, "bbbbbbbb-1111-4111-8111-111111111111", "same@example.com", "plan-b", "22222222-2222-4222-8222-222222222222", "Second", "claude_team", seen)
			seedClaudeContext(t, ms, "aaaaaaaa-1111-4111-8111-111111111111", "same@example.com", "plan-a", "11111111-1111-4111-8111-111111111111", "First", "claude_team", seen)

			human := runAccountsTest(ms, noopResolver, globals{Human: true}, "list")
			wantExit(t, human, 0)
			const wantHuman = "same@example.com/first-11111111  aaaaaaaa-1111-4111-8111-111111111111  plan-a\n" +
				"same@example.com/second-22222222  bbbbbbbb-1111-4111-8111-111111111111  plan-b\n"
			if human.stdout != wantHuman {
				t.Fatalf("human list = %q, want %q", human.stdout, wantHuman)
			}

			jsonResult := runAccountsTest(ms, noopResolver, globals{}, "list")
			wantExit(t, jsonResult, 0)
			const wantJSON = "{\"claude\":[{\"email\":\"same@example.com\",\"uuid\":\"aaaaaaaa-1111-4111-8111-111111111111\",\"plan\":\"plan-a\",\"stale\":false,\"address\":\"same@example.com/first-11111111\",\"organization_name\":\"First\",\"organization_type\":\"claude_team\"},{\"email\":\"same@example.com\",\"uuid\":\"bbbbbbbb-1111-4111-8111-111111111111\",\"plan\":\"plan-b\",\"stale\":false,\"address\":\"same@example.com/second-22222222\",\"organization_name\":\"Second\",\"organization_type\":\"claude_team\"}]}\n"
			if jsonResult.stdout != wantJSON {
				t.Fatalf("JSON list = %q, want %q", jsonResult.stdout, wantJSON)
			}
		}},
		// ---- multi-provider JSON tests ----
		{"bulk json two providers", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			now := time.Now()
			seedAccount(t, claudeMS, "c-uuid-1", "alice@example.com", "plan-a", now)
			seedAccount(t, claudeMS, "c-uuid-2", "bob@example.com", "plan-b", now)

			codexMS := testutil.MemStore(t)
			seedCodexAccount(t, codexMS, "d-uuid-1", "user@chatgpt.com", "codex-plan", now)

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "list")
			wantExit(t, r, 0)
			var result map[string][]accountSummary
			if err := json.Unmarshal([]byte(r.stdout), &result); err != nil {
				t.Fatalf("invalid JSON: %v\noutput: %s", err, r.stdout)
			}
			if len(result["claude"]) != 2 {
				t.Errorf("expected 2 Claude accounts, got %d", len(result["claude"]))
			}
			if len(result["codex"]) != 1 {
				t.Errorf("expected 1 Codex account, got %d", len(result["codex"]))
			}
			// Verify field shape on first entry.
			if result["claude"][0].Email == "" {
				t.Error("claude[0].email is empty")
			}
			if result["claude"][0].UUID == "" {
				t.Error("claude[0].uuid is empty")
			}
		}},
		{"json shape pins top-level keys and per-account fields", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			now := time.Now()
			seedAccount(t, claudeMS, "shape-uuid", "shape@example.com", "shape-plan", now)

			codexMS := testutil.MemStore(t)
			seedCodexAccount(t, codexMS, "shape-duuid", "shape@chatgpt.com", "codex-shape", now)

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "list")
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}

			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(r.stdout), &raw); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			for _, key := range []string{"claude", "codex"} {
				if _, ok := raw[key]; !ok {
					t.Errorf("missing top-level key %q", key)
				}
			}

			// Verify per-account field set for Claude.
			var claudeAccts []map[string]json.RawMessage
			if err := json.Unmarshal(raw["claude"], &claudeAccts); err != nil {
				t.Fatalf("claude array: %v", err)
			}
			if len(claudeAccts) != 1 {
				t.Fatalf("expected 1 claude account, got %d", len(claudeAccts))
			}
			for _, field := range []string{"email", "uuid", "plan", "stale"} {
				if _, ok := claudeAccts[0][field]; !ok {
					t.Errorf("claude account missing field %q", field)
				}
			}
			const want = "{\"claude\":[{\"email\":\"shape@example.com\",\"uuid\":\"shape-uuid\",\"plan\":\"shape-plan\",\"stale\":false}],\"codex\":[{\"email\":\"shape@chatgpt.com\",\"uuid\":\"shape-duuid\",\"plan\":\"codex-shape\",\"stale\":false}]}\n"
			if r.stdout != want {
				t.Fatalf("JSON list = %q, want %q", r.stdout, want)
			}
		}},
		{"bulk text section headers", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			seedAccount(t, claudeMS, "c-uuid-h", "claude@example.com", "plan", time.Now())

			codexMS := testutil.MemStore(t)
			seedCodexAccount(t, codexMS, "d-uuid-h", "codex@chatgpt.com", "plan", time.Now())

			r := runAccountsTwoStores(claudeMS, codexMS, globals{Human: true}, "list")
			wantExit(t, r, 0)
			wantOut(t, r, "=== claude ===")
			wantOut(t, r, "=== codex ===")
		}},
		{"single provider claude json", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			seedAccount(t, claudeMS, "c-uuid-sp", "sp@example.com", "plan", time.Now())

			codexMS := testutil.MemStore(t)

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "list", "claude")
			wantExit(t, r, 0)
			var result map[string][]accountSummary
			if err := json.Unmarshal([]byte(r.stdout), &result); err != nil {
				t.Fatalf("invalid JSON: %v", err)
			}
			if _, ok := result["claude"]; !ok {
				t.Error("missing claude key")
			}
			if _, ok := result["codex"]; ok {
				t.Error("codex key should be absent for single-provider list")
			}
		}},
		{"single provider claude text no section header", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			seedAccount(t, claudeMS, "c-uuid-txt", "txt@example.com", "plan", time.Now())

			codexMS := testutil.MemStore(t)

			r := runAccountsTwoStores(claudeMS, codexMS, globals{Human: true}, "list", "claude")
			wantExit(t, r, 0)
			// Single-provider text mode: no header (backward compat).
			if strings.Contains(r.stdout, "===") {
				t.Errorf("single-provider text should have no section header; stdout: %q", r.stdout)
			}
			wantOut(t, r, "txt@example.com")
		}},
		{"unknown provider errors", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			codexMS := testutil.MemStore(t)

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "list", "bogus")
			wantExit(t, r, 2)
			wantErrOut(t, r, "unknown provider")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// --- accounts remove ---

func TestAccountsRemove(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		// ---- argument errors (single-provider) ----
		{"no arg", func(t *testing.T) {
			ms := testutil.MemStore(t)
			r := runAccountsTest(ms, noopResolver, globals{}, "remove")
			wantExit(t, r, 2)
			if r.stderr != "accounts remove requires an address, organization slug, email, or UUID prefix argument\n" {
				t.Fatalf("stderr = %q", r.stderr)
			}
		}},
		{"no match", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seedAccount(t, ms, "cccc-3333", "real@example.com", "plan", time.Now())

			r := runAccountsTest(ms, noopResolver, globals{}, "remove", "nobody@example.com")
			wantExit(t, r, 2)
			wantErrOut(t, r, `no stored account matches "nobody@example.com"`)
		}},
		{"multiple matches", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seedAccount(t, ms, "dddd-4444", "work@company.com", "plan", time.Now())
			seedAccount(t, ms, "eeee-5555", "work@other.com", "plan", time.Now())

			// "work" matches both emails via substring.
			r := runAccountsTest(ms, noopResolver, globals{}, "remove", "work")
			wantExit(t, r, 2)
			if r.stderr != "multiple stored accounts match \"work\"; use one of: work@company.com (uuid dddd-4444), work@other.com (uuid eeee-5555)\n" {
				t.Fatalf("stderr = %q", r.stderr)
			}
		}},
		// ---- active-protection (D11) ----
		{"active protection blocks remove", func(t *testing.T) {
			ms := testutil.MemStore(t)
			activeKey := "ffff-6666"
			seedAccount(t, ms, activeKey, "active@example.com", "plan", time.Now())

			// Resolver reports this account as active.
			r := runAccountsTest(ms, stubResolver(activeKey), globals{}, "remove", "active@example.com")
			wantExit(t, r, 2)
			if r.stderr != "cannot remove currently active account (active@example.com (uuid ffff-6666)) — use 'claude /logout' first\n" {
				t.Fatalf("stderr = %q", r.stderr)
			}

			// Account must still be present in the store.
			listed, _ := ms.List(context.Background())
			if len(listed) != 1 {
				t.Fatalf("store should still have 1 account after blocked remove; got %d", len(listed))
			}
		}},
		{"active protection compares composite stored key", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seen := time.Now()
			seedClaudeContext(t, ms,
				"aaaaaaaa-1111-4111-8111-111111111111", "team@example.com", "default_claude_max_5x",
				"4b8e12d0-2222-4222-8222-222222222222", "Acme", "claude_team", seen)

			r := runAccountsTest(ms, stubResolver("aaaaaaaa-1111-4111-8111-111111111111_4b8e12d0-2222-4222-8222-222222222222"), globals{}, "remove", "team@example.com/acme-4b8e12d0")
			wantExit(t, r, 2)
			const want = "cannot remove currently active account (team@example.com/acme-4b8e12d0) — use 'claude /logout' first\n"
			if r.stderr != want {
				t.Fatalf("active-removal guard = %q, want %q", r.stderr, want)
			}
			listed, err := ms.List(context.Background())
			testutil.WantNoErr(t, err)
			if len(listed) != 1 {
				t.Fatalf("stored accounts after blocked removal = %d, want 1", len(listed))
			}
		}},
		{"canonical address wins matcher precedence", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seen := time.Now()
			seedClaudeContext(t, ms,
				"aaaaaaaa-1111-4111-8111-111111111111", "team@example.com", "default_claude_max_5x",
				"4b8e12d0-2222-4222-8222-222222222222", "Acme", "claude_team", seen)
			seedClaudeContext(t, ms,
				"bbbbbbbb-1111-4111-8111-111111111111", "team@example.com", "default_claude_max_5x",
				"88888888-2222-4222-8222-222222222222", "Elsewhere", "claude_team", seen)

			r := runAccountsTest(ms, noopResolver, globals{}, "remove", "team@example.com/acme-4b8e12d0")
			wantExit(t, r, 0)
			if r.stdout != "removed team@example.com/acme-4b8e12d0\n" {
				t.Fatalf("remove confirmation = %q", r.stdout)
			}
			listed, err := ms.List(context.Background())
			testutil.WantNoErr(t, err)
			if len(listed) != 1 || listed[0].Key() != "bbbbbbbb-1111-4111-8111-111111111111_88888888-2222-4222-8222-222222222222" {
				t.Fatalf("remaining key = %#v", listed)
			}
		}},
		{"ambiguous labels are canonical and sorted", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seen := time.Now()
			seedClaudeContext(t, ms,
				"aaaaaaaa-1111-4111-8111-111111111111", "z@example.com", "plan",
				"4b8e12d0-2222-4222-8222-222222222222", "Acme", "claude_team", seen)
			seedClaudeContext(t, ms,
				"bbbbbbbb-1111-4111-8111-111111111111", "a@example.com", "plan",
				"4b8e12d1-2222-4222-8222-222222222222", "Acme", "claude_team", seen)
			r := runAccountsTest(ms, noopResolver, globals{}, "remove", "acme")
			wantExit(t, r, 2)
			const want = "multiple stored accounts match \"acme\"; use one of: a@example.com/acme-4b8e12d1, z@example.com/acme-4b8e12d0\n"
			if r.stderr != want {
				t.Fatalf("ambiguity = %q, want %q", r.stderr, want)
			}
		}},
		{"resolver error fails closed", func(t *testing.T) {
			ms := testutil.MemStore(t)
			uuid := "ffff-7777"
			seedAccount(t, ms, uuid, "target@example.com", "plan", time.Now())

			errResolver := func(_ context.Context, _ []accounts.Account) (string, error) {
				return "", errors.New("profile lookup timed out")
			}
			r := runAccountsTest(ms, errResolver, globals{}, "remove", "target@example.com")
			wantExit(t, r, 2)
			wantErrOut(t, r, "could not verify active account")
			wantErrOut(t, r, "profile lookup timed out")
			// Account must still be present.
			listed, _ := ms.List(context.Background())
			if len(listed) != 1 {
				t.Fatalf("store should still have 1 account after blocked remove; got %d", len(listed))
			}
		}},
		// ---- happy path ----
		{"happy path removes account", func(t *testing.T) {
			ms := testutil.MemStore(t)
			uuid := "gggg-7777"
			seedAccount(t, ms, uuid, "gone@example.com", "plan", time.Now())

			// A different UUID is active → remove is allowed.
			r := runAccountsTest(ms, stubResolver("other-uuid"), globals{}, "remove", "gone@example.com")
			wantExit(t, r, 0)
			wantOut(t, r, "removed gone@example.com")
			wantOut(t, r, fmt.Sprintf("uuid %s", uuid))

			// Account must be gone.
			listed, _ := ms.List(context.Background())
			if len(listed) != 0 {
				t.Fatalf("store should be empty after remove; got %d", len(listed))
			}
		}},
		// ---- UUID-prefix matching ----
		{"uuid prefix matching", func(t *testing.T) {
			ms := testutil.MemStore(t)
			uuid := "abcdef01-1234-5678-9abc-def012345678"
			seedAccount(t, ms, uuid, "user@example.com", "plan", time.Now())

			// 8+ hex chars prefix matches.
			r := runAccountsTest(ms, noopResolver, globals{}, "remove", "abcdef01")
			wantExit(t, r, 0)
			listed, _ := ms.List(context.Background())
			if len(listed) != 0 {
				t.Fatalf("account should be removed; got %d accounts", len(listed))
			}
		}},
		{"email substring matching", func(t *testing.T) {
			ms := testutil.MemStore(t)
			seedAccount(t, ms, "hhhh-8888", "david@personal.com", "plan", time.Now())

			// "personal" matches as email substring.
			r := runAccountsTest(ms, noopResolver, globals{}, "remove", "david")
			wantExit(t, r, 0)
			listed, _ := ms.List(context.Background())
			if len(listed) != 0 {
				t.Fatalf("account should be removed; got %d accounts", len(listed))
			}
		}},
		// ---- multi-provider remove tests ----
		{"infer by id unique codex", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)

			codexMS := testutil.MemStore(t)
			seedCodexAccount(t, codexMS, "uuid-dtarget", "target@codex.com", "plan", time.Now())

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "remove", "target@codex.com")
			wantExit(t, r, 0)
			wantOut(t, r, "removed target@codex.com")
			listed, _ := codexMS.List(context.Background())
			if len(listed) != 0 {
				t.Fatalf("account should be removed from Codex store; got %d", len(listed))
			}
		}},
		{"infer by id ambiguous across providers", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			seedAccount(t, claudeMS, "uuid-cs", "shared@example.com", "plan", time.Now())

			codexMS := testutil.MemStore(t)
			seedCodexAccount(t, codexMS, "uuid-ds", "shared@chatgpt.com", "plan", time.Now())

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "remove", "shared")
			wantExit(t, r, 2)
			wantErrOut(t, r, "multiple providers")
		}},
		{"explicit provider codex removes by uuid prefix", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)

			codexMS := testutil.MemStore(t)
			// Use a hex-format UUID so UUID-prefix matching works.
			seedCodexAccount(t, codexMS, "abcdef01-1111-2222-3333-444444444444", "remove@chatgpt.com", "plan", time.Now())

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "remove", "abcdef01", "codex")
			wantExit(t, r, 0)
			listed, _ := codexMS.List(context.Background())
			if len(listed) != 0 {
				t.Fatalf("account should be removed; got %d accounts", len(listed))
			}
		}},
		{"too many args errors", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			codexMS := testutil.MemStore(t)

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "remove", "some-id", "claude", "extra")
			wantExit(t, r, 2)
			wantErrOut(t, r, "unexpected argument")
		}},
		{"explicit provider unknown errors", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			codexMS := testutil.MemStore(t)

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "remove", "some-id", "bogus")
			wantExit(t, r, 2)
			wantErrOut(t, r, "unknown provider")
		}},
		{"infer same provider multi match shows single-provider message", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)
			seedAccount(t, claudeMS, "uuid-cm1", "shared@work.com", "plan", time.Now())
			seedAccount(t, claudeMS, "uuid-cm2", "shared@personal.com", "plan", time.Now())

			codexMS := testutil.MemStore(t) // empty → no Codex match

			r := runAccountsTwoStores(claudeMS, codexMS, globals{}, "remove", "shared")
			wantExit(t, r, 2)
			wantErrOut(t, r, "multiple stored accounts match")
			if strings.Contains(r.stderr, "multiple providers") {
				t.Errorf("should NOT show cross-provider ambiguity; stderr: %q", r.stderr)
			}
		}},
		{"active protection codex uses provider logout hint", func(t *testing.T) {
			claudeMS := testutil.MemStore(t)

			codexMS := testutil.MemStore(t)
			seedCodexAccount(t, codexMS, "uuid-codex-active", "active@chatgpt.com", "plan", time.Now())

			stores := []providerStore{
				{
					id:             "claude",
					store:          claudeMS,
					activeResolver: noopResolver,
					logoutHint:     "use 'claude /logout' first",
				},
				{
					id:             "codex",
					store:          codexMS,
					activeResolver: stubResolver("uuid-codex-active"),
					logoutHint:     "log out of the Codex app first",
				},
			}

			var bOut, bErr writerBuf
			code := runAccounts([]string{"remove", "active@chatgpt.com"}, &bOut, &bErr, globals{}, stores)
			r := runResult{bOut.String(), bErr.String(), code}

			wantExit(t, r, 2)
			if r.stderr != "cannot remove currently active account (active@chatgpt.com (uuid uuid-codex-active)) — log out of the Codex app first\n" {
				t.Fatalf("stderr = %q", r.stderr)
			}
			listed, _ := codexMS.List(context.Background())
			if len(listed) != 1 {
				t.Fatalf("account should still be present; got %d", len(listed))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestAccountsRemoveAddressSelectors(t *testing.T) {
	tests := []struct {
		name     string
		argument string
		wantOut  string
		seed     func(t *testing.T, ms *accounts.MemoryStore)
	}{
		{
			name:     "unique clean slash slug",
			argument: "team@example.com/acme",
			wantOut:  "removed team@example.com/acme-4b8e12d0\n",
			seed: func(t *testing.T, ms *accounts.MemoryStore) {
				seedClaudeContext(t, ms, "aaaaaaaa-1111-4111-8111-111111111111", "team@example.com", "plan", "4b8e12d0-2222-4222-8222-222222222222", "Acme", "claude_team", time.Now())
			},
		},
		{
			name:     "unique bare slug",
			argument: "acme",
			wantOut:  "removed team@example.com/acme-4b8e12d0\n",
			seed: func(t *testing.T, ms *accounts.MemoryStore) {
				seedClaudeContext(t, ms, "aaaaaaaa-1111-4111-8111-111111111111", "team@example.com", "plan", "4b8e12d0-2222-4222-8222-222222222222", "Acme", "claude_team", time.Now())
			},
		},
		{
			name:     "observed max personal address",
			argument: "me@example.com/personal-9f2a41c7",
			wantOut:  "removed me@example.com/personal-9f2a41c7\n",
			seed: func(t *testing.T, ms *accounts.MemoryStore) {
				seedClaudeContext(t, ms, "9f2a41c7-3b5d-4e7f-9a1c-2d4e6f8a0b1c", "me@example.com", "default_claude_max_5x", "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042", "me@example.com's Organization", "claude_max", time.Now())
			},
		},
		{
			name:     "reserved personal team organization",
			argument: "team@example.com/organization-4b8e12d0",
			wantOut:  "removed team@example.com/organization-4b8e12d0\n",
			seed: func(t *testing.T, ms *accounts.MemoryStore) {
				seedClaudeContext(t, ms, "aaaaaaaa-1111-4111-8111-111111111111", "team@example.com", "plan", "4b8e12d0-2222-4222-8222-222222222222", "Personal", "claude_team", time.Now())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := testutil.MemStore(t)
			tt.seed(t, ms)
			r := runAccountsTest(ms, noopResolver, globals{}, "remove", tt.argument)
			wantExit(t, r, 0)
			if r.stdout != tt.wantOut {
				t.Fatalf("remove confirmation = %q, want %q", r.stdout, tt.wantOut)
			}
			listed, err := ms.List(context.Background())
			testutil.WantNoErr(t, err)
			if len(listed) != 0 {
				t.Fatalf("stored accounts after removal = %d, want 0", len(listed))
			}
		})
	}
}
