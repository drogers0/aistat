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
)

// noopResolver is a stub resolveActiveUUID that always returns "" (no active account).
func noopResolver(_ context.Context, _ []accounts.Account) (string, error) {
	return "", nil
}

// stubResolver returns a resolver that always reports the given UUID as active.
func stubResolver(activeUUID string) func(context.Context, []accounts.Account) (string, error) {
	return func(_ context.Context, _ []accounts.Account) (string, error) {
		return activeUUID, nil
	}
}

// runAccounts helper: run runAccountsInner with a MemoryStore and the given resolver.
func runAccountsTest(store *accounts.MemoryStore, resolver func(context.Context, []accounts.Account) (string, error), args ...string) runResult {
	var stdout, stderr bytes.Buffer
	code := runAccounts(args, &stdout, &stderr, globals{}, store, resolver)
	return runResult{stdout.String(), stderr.String(), code}
}

// seedAccount inserts an account into ms with the given fields.
func seedAccount(t *testing.T, ms *accounts.MemoryStore, uuid, email, plan string, lastSeen time.Time) {
	t.Helper()
	rawBlob, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "tok-" + uuid, "refreshToken": "rt-" + uuid},
	})
	a, err := accounts.NewAccount(rawBlob, uuid, email, email, plan, lastSeen)
	if err != nil {
		t.Fatalf("seedAccount: %v", err)
	}
	if err := ms.Upsert(context.Background(), a); err != nil {
		t.Fatalf("seedAccount Upsert: %v", err)
	}
}

// --- Unknown subcommand errors ---

func TestAccounts_EmptySubcmd(t *testing.T) {
	ms := accounts.NewMemoryStore()
	r := runAccountsTest(ms, noopResolver) // no args → empty sub
	if r.code != 2 {
		t.Fatalf("expected exit 2, got %d", r.code)
	}
	want := "unknown subcommand \"\" \u2014 want \"list\" or \"remove\""
	if !strings.Contains(r.stderr, want) {
		t.Fatalf("missing error %q; stderr: %s", want, r.stderr)
	}
}

func TestAccounts_UnknownSubcmd(t *testing.T) {
	ms := accounts.NewMemoryStore()
	r := runAccountsTest(ms, noopResolver, "foo")
	if r.code != 2 {
		t.Fatalf("expected exit 2, got %d", r.code)
	}
	want := "unknown subcommand \"foo\" \u2014 want \"list\" or \"remove\""
	if !strings.Contains(r.stderr, want) {
		t.Fatalf("missing error %q; stderr: %s", want, r.stderr)
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

// --- accounts list ---

func TestAccountsList_EmptyStore(t *testing.T) {
	ms := accounts.NewMemoryStore()
	r := runAccountsTest(ms, noopResolver, "list")
	if r.code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr %q)", r.code, r.stderr)
	}
	if r.stdout != "" {
		t.Fatalf("expected empty stdout, got %q", r.stdout)
	}
}

func TestAccountsList_NotStaleAt30Days(t *testing.T) {
	ms := accounts.NewMemoryStore()
	// 30 days minus 1 minute: unambiguously NOT stale regardless of test execution time.
	lastSeen := time.Now().Add(-30*24*time.Hour + time.Minute)
	seedAccount(t, ms, "aaaa-1111", "user@example.com", "default_claude_max_5x", lastSeen)

	r := runAccountsTest(ms, noopResolver, "list")
	if r.code != 0 {
		t.Fatalf("expected exit 0, got %d", r.code)
	}
	if strings.Contains(r.stdout, "(stale)") {
		t.Fatalf("account at <30 days should NOT be stale; stdout: %s", r.stdout)
	}
	if !strings.Contains(r.stdout, "user@example.com") {
		t.Fatalf("missing account in output; stdout: %s", r.stdout)
	}
}

func TestAccountsList_StaleAfter30DaysPlus1Minute(t *testing.T) {
	ms := accounts.NewMemoryStore()
	// 30 days + 1 minute: unambiguously stale regardless of test execution time.
	lastSeen := time.Now().Add(-30*24*time.Hour - time.Minute)
	seedAccount(t, ms, "bbbb-2222", "old@example.com", "default_claude_pro", lastSeen)

	r := runAccountsTest(ms, noopResolver, "list")
	if r.code != 0 {
		t.Fatalf("expected exit 0, got %d", r.code)
	}
	if !strings.Contains(r.stdout, "(stale)") {
		t.Fatalf("account >30d old should be marked stale; stdout: %s", r.stdout)
	}
}

func TestAccountsList_SortedByEmail(t *testing.T) {
	ms := accounts.NewMemoryStore()
	now := time.Now()
	seedAccount(t, ms, "uuid-z", "z@example.com", "plan", now)
	seedAccount(t, ms, "uuid-a", "a@example.com", "plan", now)
	seedAccount(t, ms, "uuid-m", "m@example.com", "plan", now)

	r := runAccountsTest(ms, noopResolver, "list")
	if r.code != 0 {
		t.Fatalf("expected exit 0, got %d", r.code)
	}
	idxA := strings.Index(r.stdout, "a@example.com")
	idxM := strings.Index(r.stdout, "m@example.com")
	idxZ := strings.Index(r.stdout, "z@example.com")
	if !(idxA < idxM && idxM < idxZ) {
		t.Fatalf("accounts not sorted by email; stdout:\n%s", r.stdout)
	}
}

// --- accounts remove: argument errors ---

func TestAccountsRemove_NoArg(t *testing.T) {
	ms := accounts.NewMemoryStore()
	r := runAccountsTest(ms, noopResolver, "remove")
	if r.code != 2 {
		t.Fatalf("expected exit 2, got %d", r.code)
	}
	if !strings.Contains(r.stderr, "accounts remove requires an email or uuid argument") {
		t.Fatalf("missing error; stderr: %s", r.stderr)
	}
}

func TestAccountsRemove_NoMatch(t *testing.T) {
	ms := accounts.NewMemoryStore()
	seedAccount(t, ms, "cccc-3333", "real@example.com", "plan", time.Now())

	r := runAccountsTest(ms, noopResolver, "remove", "nobody@example.com")
	if r.code != 2 {
		t.Fatalf("expected exit 2, got %d", r.code)
	}
	if !strings.Contains(r.stderr, `no stored account matches "nobody@example.com"`) {
		t.Fatalf("missing error; stderr: %s", r.stderr)
	}
}

func TestAccountsRemove_MultipleMatches(t *testing.T) {
	ms := accounts.NewMemoryStore()
	seedAccount(t, ms, "dddd-4444", "work@company.com", "plan", time.Now())
	seedAccount(t, ms, "eeee-5555", "work@other.com", "plan", time.Now())

	// "work" matches both emails via substring.
	r := runAccountsTest(ms, noopResolver, "remove", "work")
	if r.code != 2 {
		t.Fatalf("expected exit 2, got %d", r.code)
	}
	if !strings.Contains(r.stderr, `multiple stored accounts match "work", disambiguate by uuid`) {
		t.Fatalf("missing error; stderr: %s", r.stderr)
	}
}

// --- accounts remove: active-protection (D11) ---

func TestAccountsRemove_ActiveProtection(t *testing.T) {
	ms := accounts.NewMemoryStore()
	activeUUID := "ffff-6666"
	seedAccount(t, ms, activeUUID, "active@example.com", "plan", time.Now())

	// Resolver reports this account as active.
	r := runAccountsTest(ms, stubResolver(activeUUID), "remove", "active@example.com")
	if r.code != 2 {
		t.Fatalf("expected exit 2, got %d", r.code)
	}
	want := "cannot remove currently active account \u2014 use 'claude /logout' first"
	if !strings.Contains(r.stderr, want) {
		t.Fatalf("missing error %q; stderr: %s", want, r.stderr)
	}

	// Account must still be present in the store.
	listed, _ := ms.List(context.Background())
	if len(listed) != 1 {
		t.Fatalf("store should still have 1 account after blocked remove; got %d", len(listed))
	}
}

func TestAccountsRemove_ResolverErrorFailsClosed(t *testing.T) {
	ms := accounts.NewMemoryStore()
	uuid := "ffff-7777"
	seedAccount(t, ms, uuid, "target@example.com", "plan", time.Now())

	errResolver := func(_ context.Context, _ []accounts.Account) (string, error) {
		return "", errors.New("profile lookup timed out")
	}
	r := runAccountsTest(ms, errResolver, "remove", "target@example.com")
	if r.code != 2 {
		t.Fatalf("expected exit 2, got %d (stderr %q)", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, "could not verify active account") {
		t.Fatalf("missing fail-closed message; stderr: %s", r.stderr)
	}
	if !strings.Contains(r.stderr, "profile lookup timed out") {
		t.Fatalf("expected wrapped resolver error; stderr: %s", r.stderr)
	}
	// Account must still be present.
	listed, _ := ms.List(context.Background())
	if len(listed) != 1 {
		t.Fatalf("store should still have 1 account after blocked remove; got %d", len(listed))
	}
}

// --- accounts remove: happy path ---

func TestAccountsRemove_HappyPath(t *testing.T) {
	ms := accounts.NewMemoryStore()
	uuid := "gggg-7777"
	seedAccount(t, ms, uuid, "gone@example.com", "plan", time.Now())

	// A different UUID is active → remove is allowed.
	r := runAccountsTest(ms, stubResolver("other-uuid"), "remove", "gone@example.com")
	if r.code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr %q)", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "removed gone@example.com") {
		t.Fatalf("missing removal confirmation; stdout: %s", r.stdout)
	}
	if !strings.Contains(r.stdout, fmt.Sprintf("uuid %s", uuid)) {
		t.Fatalf("missing uuid in output; stdout: %s", r.stdout)
	}

	// Account must be gone.
	listed, _ := ms.List(context.Background())
	if len(listed) != 0 {
		t.Fatalf("store should be empty after remove; got %d", len(listed))
	}
}

// --- UUID-prefix matching ---

func TestAccountsRemove_UUIDPrefix(t *testing.T) {
	ms := accounts.NewMemoryStore()
	uuid := "abcdef01-1234-5678-9abc-def012345678"
	seedAccount(t, ms, uuid, "user@example.com", "plan", time.Now())

	// 8+ hex chars prefix matches.
	r := runAccountsTest(ms, noopResolver, "remove", "abcdef01")
	if r.code != 0 {
		t.Fatalf("expected exit 0 for UUID-prefix remove, got %d (stderr %q)", r.code, r.stderr)
	}
	listed, _ := ms.List(context.Background())
	if len(listed) != 0 {
		t.Fatalf("account should be removed; got %d accounts", len(listed))
	}
}

func TestAccountsRemove_EmailSubstring(t *testing.T) {
	ms := accounts.NewMemoryStore()
	seedAccount(t, ms, "hhhh-8888", "david@personal.com", "plan", time.Now())

	// "personal" matches as email substring.
	r := runAccountsTest(ms, noopResolver, "remove", "personal")
	if r.code != 0 {
		t.Fatalf("expected exit 0 for email-substring remove, got %d (stderr %q)", r.code, r.stderr)
	}
	listed, _ := ms.List(context.Background())
	if len(listed) != 0 {
		t.Fatalf("account should be removed; got %d accounts", len(listed))
	}
}
