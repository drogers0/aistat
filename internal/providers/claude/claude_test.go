package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/httpx"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// ── test infrastructure ──────────────────────────────────────────────────────

// minUsageBody is a minimal valid usage API response used in tests that don't
// validate limit values.
var minUsageBody = []byte(`{"five_hour":{"utilization":50.0,"resets_at":"2027-01-01T00:00:00+00:00"}}`)

// stubStaticServer returns an httptest.Server that always responds with the
// given status and body. The server is closed on t.Cleanup.
func stubStaticServer(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stubCountingServer returns an httptest.Server that always responds with the
// given status and body, and an atomic counter that increments on each request.
func stubCountingServer(t *testing.T, status int, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &count
}

// stubSequentialServer returns an httptest.Server that serves responses[0] for
// the first request, responses[1] for the second, and so on. The last entry is
// repeated for any additional requests.
func stubSequentialServer(t *testing.T, responses []struct {
	status int
	body   []byte
}) *httptest.Server {
	t.Helper()
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(count.Add(1)) - 1
		if i >= len(responses) {
			i = len(responses) - 1
		}
		w.WriteHeader(responses[i].status)
		_, _ = w.Write(responses[i].body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// profileBody builds a minimal valid /api/oauth/profile response JSON.
func profileBody(uuid, email, rateLimitTier string) []byte {
	b, _ := json.Marshal(map[string]any{
		"account":      map[string]any{"uuid": uuid, "email": email, "display_name": email},
		"organization": map[string]any{"rate_limit_tier": rateLimitTier},
	})
	return b
}

// refreshSuccessBody builds a minimal valid OAuth token response JSON.
func refreshSuccessBody(accessToken, refreshToken string) []byte {
	b, _ := json.Marshal(map[string]any{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"expires_in":    3600,
	})
	return b
}

// buildClient constructs a Client for testing. Each component (usage, profile,
// refresh) uses its own doer so each can point at a separate test server.
// If warnBuf is nil, warns are discarded. If nowFn is nil, testNow is used.
//
// Cache isolation: sets HOME (and clears XDG_CACHE_HOME) to a per-test
// TempDir so no test touches the developer's real cache directory.
func buildClient(
	t *testing.T,
	usageSrv *httptest.Server,
	profileSrv *httptest.Server,
	refreshSrv *httptest.Server,
	liveCred *cred.Credential,
	store accounts.Store,
	warnBuf io.Writer,
	nowFn func() time.Time,
) *Client {
	t.Helper()

	// Isolate cache I/O from the developer's real cache directory.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")

	usageDoer := httpx.NewDoer(usageSrv.Client(), "aistat-test/0", "claude",
		map[string]string{"Anthropic-Beta": betaHeader}, nil)

	profDoer := httpx.NewDoer(profileSrv.Client(), "aistat-test/0", "claude",
		map[string]string{"Anthropic-Beta": betaHeader}, nil)
	pc := newProfileClient(profDoer)
	pc.endpoint = profileSrv.URL + "/api/oauth/profile"

	refDoer := httpx.NewDoer(refreshSrv.Client(), "aistat-test/0", "claude", nil, nil)
	rc := newRefreshClient(refDoer)
	rc.endpoint = refreshSrv.URL + "/v1/oauth/token"

	if store == nil {
		store = accounts.NewMemoryStore()
	}
	if warnBuf == nil {
		warnBuf = io.Discard
	}
	if nowFn == nil {
		nowFn = func() time.Time { return testNow }
	}
	rc.now = nowFn

	readCred := func(context.Context) (cred.Credential, error) {
		if liveCred == nil {
			return cred.Credential{}, cred.ErrClaudeTokenNotFound
		}
		return *liveCred, nil
	}

	warnFn := func(s string) { fmt.Fprintln(warnBuf, s) }

	return &Client{
		doer:             usageDoer,
		endpoint:         usageSrv.URL + "/api/oauth/usage",
		profile:          pc,
		refresh:          rc,
		store:            store,
		readCredential:   readCred,
		warn:             warnBuf,
		now:              nowFn,
		baseTimeout:      10 * time.Second,
		perAccountBudget: 3 * time.Second,
		cache:            newUsageCache(nowFn, warnFn),
	}
}

// noRefreshServer returns a stub refresh server that fails the test if hit.
// The server is closed on t.Cleanup.
func noRefreshServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("refresh server must not be called, but received %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// noProfileServer returns a stub profile server that fails the test if hit.
// The server is closed on t.Cleanup.
func noProfileServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("profile server must not be called, but received %s %s", r.Method, r.URL.Path)
		w.WriteHeader(500)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// storeUUIDSet returns the set of UUIDs currently in the store.
func storeUUIDSet(t *testing.T, s *accounts.MemoryStore) map[string]bool {
	t.Helper()
	accts, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	set := map[string]bool{}
	for _, a := range accts {
		set[a.UUID] = true
	}
	return set
}

// sortedKeys returns the sorted keys of a map for deterministic assertions.
func keys(m map[string]providers.Limit) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── original single-account tests (rewritten for new API) ───────────────────

func TestFetch_ResetAfterSecondsTruncated(t *testing.T) {
	frozen := time.Date(2026, 5, 15, 12, 34, 56, 789_000_000, time.UTC)
	resetsAt := frozen.Add(3 * time.Hour).Truncate(time.Second)
	body := []byte(`{"five_hour":{"utilization":50,"resets_at":"` + resetsAt.Format(time.RFC3339Nano) + `"}}`)

	live := makeCred("tok-live", "ref-live", 0)
	storedAcct := makeAccount("uuid-1", "user@example.com", "tok-live", "ref-live", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), storedAcct)

	usageSrv := stubStaticServer(t, 200, body)
	profileSrv := noProfileServer(t) // byte-match → no profile call
	refreshSrv := noRefreshServer(t) // no refresh needed (ExpiresAt == 0)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil,
		func() time.Time { return frozen })

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := int(resetsAt.Sub(frozen.Truncate(time.Second)).Seconds())
	if got := out.Accounts[0].Limits["five_hour"].ResetAfterSeconds; got != want {
		t.Errorf("ResetAfterSeconds = %d, want %d", got, want)
	}
}

func TestFetch_GoldenFixture(t *testing.T) {
	live := makeCred("tok-live", "ref-live", 0)
	storedAcct := makeAccount("uuid-1", "user@example.com", "tok-live", "ref-live", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), storedAcct)

	usageSrv := stubStaticServer(t, 200, testutil.LoadFixture(t, "usage.json"))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)
	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Single-account success path — limits live on the per-account row;
	// out.Limits is intentionally nil under the multi-account contract.
	if len(out.Accounts) != 1 {
		t.Fatalf("expected 1 account row, got %d", len(out.Accounts))
	}
	acctLimits := out.Accounts[0].Limits
	if len(acctLimits) != 3 {
		t.Fatalf("expected 3 limits, got %d: %v", len(acctLimits), keys(acctLimits))
	}
	for _, want := range []string{"five_hour", "seven_day", "seven_day_sonnet"} {
		if _, ok := acctLimits[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	for _, unwanted := range []string{"seven_day_omelette", "seven_day_opus", "tangelo", "iguana_necktie"} {
		if _, ok := acctLimits[unwanted]; ok {
			t.Errorf("bonus window %s should be filtered out", unwanted)
		}
	}
	fh := acctLimits["five_hour"]
	if fh.UsedPercent != 47.0 {
		t.Errorf("five_hour used_percent = %v, want 47.0", fh.UsedPercent)
	}
	if fh.RemainingPercent != 53.0 {
		t.Errorf("five_hour remaining_percent = %v, want 53.0", fh.RemainingPercent)
	}
	if fh.ResetsAt.Nanosecond() != 0 {
		t.Errorf("resets_at not truncated: %v", fh.ResetsAt)
	}
	wantTime, _ := time.Parse(time.RFC3339, "2026-05-26T22:00:00Z")
	if !fh.ResetsAt.Equal(wantTime) {
		t.Errorf("resets_at = %v, want %v", fh.ResetsAt, wantTime)
	}
	if !out.Accounts[0].Active {
		t.Error("single account row should be active")
	}
}

func TestFetch_NullResetsAtIsSkipped(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2026-05-26T22:00:00+00:00"},"seven_day_omelette":{"utilization":50.0,"resets_at":null}}`)
	live := makeCred("tok-live", "ref", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

	usageSrv := stubStaticServer(t, 200, body)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, _ := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if _, ok := out.Accounts[0].Limits["seven_day_omelette"]; ok {
		t.Error("seven_day_omelette should be excluded when resets_at is null")
	}
}

func TestFetch_RequestShape(t *testing.T) {
	var gotReq http.Request
	live := makeCred("sk-ant-oat01-fake", "ref", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("u1", "a@b.com", "sk-ant-oat01-fake", "ref", 0))

	usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage.json"), 200, &gotReq)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Method != "GET" {
		t.Errorf("method = %s, want GET", gotReq.Method)
	}
	if gotReq.URL.Path != "/api/oauth/usage" {
		t.Errorf("path = %s", gotReq.URL.Path)
	}
	if h := gotReq.Header.Get("Authorization"); h != "Bearer sk-ant-oat01-fake" {
		t.Errorf("Authorization = %q", h)
	}
	if h := gotReq.Header.Get("Anthropic-Beta"); h != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q", h)
	}
	if h := gotReq.Header.Get("User-Agent"); !strings.Contains(h, "aistat") {
		t.Errorf("User-Agent missing: %q", h)
	}
}

// TestFetch_Status418IsBareError verifies that a 418 from the usage endpoint
// becomes a per-account error (not a classified transient/auth-denied) and the
// provider does NOT surface a provider-level ErrTransient for a single-account
// non-transient failure.
func TestFetch_Status418IsBareError(t *testing.T) {
	live := makeCred("tok-live", "ref", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

	usageSrv := stubStaticServer(t, 418, []byte("teapot"))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	// 418 is not transient → no provider-level ErrTransient.
	if errors.Is(err, providers.ErrTransient) || errors.Is(err, providers.ErrAuthDenied) {
		t.Errorf("418 should not produce transient/auth-denied provider error, got: %v", err)
	}
	// The per-account error must mention HTTP 418.
	if len(out.Accounts) == 0 {
		t.Fatal("expected account rows even on per-account error")
	}
	if !strings.Contains(out.Accounts[0].Error, "HTTP 418") {
		t.Errorf("per-account error should mention HTTP 418: %q", out.Accounts[0].Error)
	}
}

func TestFetch_NonJSON200(t *testing.T) {
	live := makeCred("tok-live", "ref", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

	usageSrv := stubStaticServer(t, 200, []byte("<html>oops</html>"))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, _ := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if len(out.Accounts) == 0 {
		t.Fatal("expected account rows even on per-account error")
	}
	if !strings.Contains(out.Accounts[0].Error, "non-JSON") {
		t.Errorf("per-account error should mention non-JSON: %q", out.Accounts[0].Error)
	}
}

func TestFetch_BadResetsAt(t *testing.T) {
	body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"yesterday"}}`)
	live := makeCred("tok-live", "ref", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

	usageSrv := stubStaticServer(t, 200, body)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, _ := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if len(out.Accounts) == 0 {
		t.Fatal("expected account rows even on per-account error")
	}
	if !strings.Contains(out.Accounts[0].Error, "unparseable resets_at") {
		t.Errorf("per-account error should mention unparseable resets_at: %q", out.Accounts[0].Error)
	}
	if !strings.Contains(out.Accounts[0].Error, "five_hour") {
		t.Errorf("per-account error should name window: %q", out.Accounts[0].Error)
	}
}

func TestFetch_TokenMissingIsAuthMissing(t *testing.T) {
	// nil liveCred + empty store → ErrAuthMissing
	usageSrv := stubStaticServer(t, 200, []byte(`{}`))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil).Fetch(context.Background())
	if !errors.Is(err, providers.ErrAuthMissing) {
		t.Errorf("expected ErrAuthMissing, got: %v", err)
	}
	if !strings.Contains(err.Error(), cred.ClaudeTokenMissingMessage) {
		t.Errorf("expected token-missing message, got: %v", err)
	}
}

func TestFetch_TokenGenericErrorPropagated(t *testing.T) {
	sentinel := errors.New("some keychain failure")
	usageSrv := stubStaticServer(t, 200, []byte(`{}`))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil)
	c.readCredential = func(context.Context) (cred.Credential, error) {
		return cred.Credential{}, sentinel
	}

	_, err := c.Fetch(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("generic error should propagate, got: %v", err)
	}
	if errors.Is(err, providers.ErrAuthMissing) {
		t.Errorf("generic err should not be classified ErrAuthMissing")
	}
}

// ── new multi-account tests ──────────────────────────────────────────────────

// TestFetch_TwoAccount_BothSucceed: active byte-match + one non-active stored.
// out.Accounts has 2 rows with active first.
func TestFetch_TwoAccount_BothSucceed(t *testing.T) {
	acctA := makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0)
	acctB := makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acctA)
	_ = store.Upsert(context.Background(), acctB)

	live := makeCred("tok-a", "ref-a", 0) // byte-matches acctA → acctA is active

	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t) // byte-match → no profile call
	refreshSrv := noRefreshServer(t) // no refresh (ExpiresAt == 0)

	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("Accounts len = %d, want 2", len(out.Accounts))
	}
	// Active first.
	if !out.Accounts[0].Active {
		t.Errorf("Accounts[0] should be active (was %q)", out.Accounts[0].Email)
	}
	if out.Accounts[0].UUID != "uuid-a" {
		t.Errorf("active account UUID = %q, want uuid-a", out.Accounts[0].UUID)
	}
	if out.Accounts[1].Active {
		t.Errorf("Accounts[1] should not be active")
	}
	if out.Accounts[1].UUID != "uuid-b" {
		t.Errorf("second account UUID = %q, want uuid-b", out.Accounts[1].UUID)
	}
	// Provider-level Limits is intentionally nil under the multi-account
	// contract — accounts[i].limits is canonical.
	if out.Limits != nil {
		t.Errorf("provider-level Limits should be nil under multi-account contract; got %v", out.Limits)
	}
}

// TestFetch_TwoAccount_EmailOrdering: active first, then ASCII ascending by email.
func TestFetch_TwoAccount_EmailOrdering(t *testing.T) {
	// Two non-active stored accounts; live absent so neither is active.
	acctZ := makeAccount("uuid-z", "zeta@example.com", "tok-z", "ref-z", 0)
	acctA := makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acctZ)
	_ = store.Upsert(context.Background(), acctA)

	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, store, nil, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("Accounts len = %d, want 2", len(out.Accounts))
	}
	if out.Accounts[0].Email >= out.Accounts[1].Email {
		t.Errorf("accounts not sorted by email: %q >= %q", out.Accounts[0].Email, out.Accounts[1].Email)
	}
}

// TestFetch_StoredRefreshRejected: account B's refresh token is invalid →
// per-account error, account A succeeds → provider returns success (not ErrTransient).
func TestFetch_StoredRefreshRejected(t *testing.T) {
	// ExpiresAt within refreshSkew triggers refresh.
	nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
	farExpiry := testNow.Add(1 * time.Hour).UnixMilli()

	acctA := makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", farExpiry)
	acctB := makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", nearExpiry)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acctA)
	_ = store.Upsert(context.Background(), acctB)

	live := makeCred("tok-a", "ref-a", farExpiry) // byte-matches acctA

	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t) // byte-match path
	// Refresh server returns invalid_grant for acctB's refresh attempt.
	refreshSrv := stubStaticServer(t, 400, []byte(`{"error":"invalid_grant"}`))

	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil,
		func() time.Time { return testNow }).Fetch(context.Background())
	if err != nil {
		t.Fatalf("provider should succeed (acctA ok), got: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("Accounts len = %d, want 2", len(out.Accounts))
	}
	// Provider-level Limits is intentionally nil — multi-account contract.
	if out.Limits != nil {
		t.Errorf("provider-level Limits should be nil; got %v", out.Limits)
	}
	// Find acctA and acctB in results and check their state.
	var sawA, sawB bool
	for _, ar := range out.Accounts {
		switch ar.UUID {
		case "uuid-b":
			sawB = true
			if ar.Error == "" {
				t.Error("acctB should have per-account error")
			}
			if !strings.Contains(ar.Error, "account credential expired") {
				t.Errorf("acctB error should mention expired credential, got: %q", ar.Error)
			}
		case "uuid-a":
			sawA = true
			if !ar.Active {
				t.Error("acctA (byte-matches live) should be Active=true")
			}
			if ar.Error != "" {
				t.Errorf("acctA should succeed, got error: %q", ar.Error)
			}
			if ar.Limits == nil {
				t.Error("acctA should have limits")
			}
		}
	}
	if !sawA {
		t.Error("uuid-a not found in results")
	}
	if !sawB {
		t.Error("uuid-b not found in results")
	}
}

// TestFetch_AllTransient: both accounts' usage fetch returns 503 → ErrTransient.
func TestFetch_AllTransient(t *testing.T) {
	acctA := makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0)
	acctB := makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acctA)
	_ = store.Upsert(context.Background(), acctB)

	usageSrv := stubStaticServer(t, 503, []byte(`{"error":"service unavailable"}`))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	// live absent → both accounts non-active
	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, store, nil, nil).Fetch(context.Background())
	if !errors.Is(err, providers.ErrTransient) {
		t.Errorf("expected ErrTransient, got: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("partial accounts should still be returned, got %d", len(out.Accounts))
	}
	for _, ar := range out.Accounts {
		if ar.Error == "" {
			t.Errorf("account %q should have per-account error", ar.Email)
		}
	}
}

// TestFetch_MixedTransientAndAuthDenied: one transient + one auth-denied,
// zero succeeded → provider still returns ErrTransient (D8 retry rule).
func TestFetch_MixedTransientAndAuthDenied(t *testing.T) {
	acctA := makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0)
	acctB := makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acctA)
	_ = store.Upsert(context.Background(), acctB)

	// The MemoryStore List returns accounts in map-iteration order (non-deterministic).
	// The httpx retry layer retries transient errors up to maxAttempts=3 times, so
	// the transient account (whichever is fetched first) consumes 3 sequential 503s
	// before the httpx loop gives up and returns ErrTransient to Fetch. The auth-denied
	// account then receives the repeated 401 last entry. Update the response count if
	// httpx.maxAttempts changes.
	usageSrv := stubSequentialServer(t, []struct {
		status int
		body   []byte
	}{
		{503, []byte(`{"error":"unavailable"}`)},
		{503, []byte(`{"error":"unavailable"}`)},
		{503, []byte(`{"error":"unavailable"}`)}, // 3 = httpx.maxAttempts
		{401, []byte(`{"error":"unauthorized"}`)},
	})
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, store, nil, nil).Fetch(context.Background())
	if !errors.Is(err, providers.ErrTransient) {
		t.Errorf("expected ErrTransient (mixed transient+auth-denied, zero succeeded), got: %v", err)
	}
}

// TestFetch_LivePresentProfileFails: profile call fails → fallback row with
// "(live Claude account)", CaptureWarn emitted.
func TestFetch_LivePresentProfileFails(t *testing.T) {
	live := makeCred("tok-live", "ref-live", 0)
	// empty store → no byte-match → profile call needed
	store := accounts.NewMemoryStore()

	profileSrv := stubStaticServer(t, 503, []byte(`{"error":"unavailable"}`))
	usageSrv := stubStaticServer(t, 200, minUsageBody)
	refreshSrv := noRefreshServer(t)

	var warnBuf bytes.Buffer
	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, &warnBuf, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(out.Accounts) != 1 {
		t.Fatalf("Accounts len = %d, want 1 fallback row", len(out.Accounts))
	}
	ar := out.Accounts[0]
	if ar.Email != "(live Claude account)" {
		t.Errorf("Email = %q, want %q", ar.Email, "(live Claude account)")
	}
	if ar.UUID != "" {
		t.Errorf("UUID = %q, want empty", ar.UUID)
	}
	if ar.Plan != "" {
		t.Errorf("Plan = %q, want empty", ar.Plan)
	}
	if !ar.Active {
		t.Error("fallback row should be Active = true")
	}
	if ar.Limits == nil {
		t.Error("fallback row should have limits from usage fetch")
	}

	warn := warnBuf.String()
	if !strings.Contains(warn, "could not capture live account profile") {
		t.Errorf("CaptureWarn missing generic fallback message, got: %q", warn)
	}
	if !strings.Contains(warn, "claude /login") {
		t.Errorf("CaptureWarn missing recovery hint, got: %q", warn)
	}
}

// TestFetch_ProfileMissingFields: profile 200 with missing account.uuid →
// stricter diagnostic warn.
func TestFetch_ProfileMissingFields(t *testing.T) {
	live := makeCred("tok-live", "ref-live", 0)
	store := accounts.NewMemoryStore()

	// 200 response missing uuid.
	profileSrv := stubStaticServer(t, 200, []byte(`{"account":{"uuid":"","email":"user@example.com"}}`))
	usageSrv := stubStaticServer(t, 200, minUsageBody)
	refreshSrv := noRefreshServer(t)

	var warnBuf bytes.Buffer
	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, &warnBuf, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(out.Accounts) != 1 {
		t.Fatalf("Accounts len = %d, want 1 fallback row", len(out.Accounts))
	}
	if out.Accounts[0].Email != "(live Claude account)" {
		t.Errorf("Email = %q, want fallback email", out.Accounts[0].Email)
	}

	warn := warnBuf.String()
	if !strings.Contains(warn, "missing required fields") {
		t.Errorf("CaptureWarn should use stricter diagnostic, got: %q", warn)
	}
	if strings.Contains(warn, "claude /login") {
		t.Errorf("stricter diagnostic must not contain 'claude /login', got: %q", warn)
	}
}

// TestFetch_LiveAbsentZeroStored: live absent + no stored accounts → ErrAuthMissing.
func TestFetch_LiveAbsentZeroStored(t *testing.T) {
	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil).Fetch(context.Background())
	if !errors.Is(err, providers.ErrAuthMissing) {
		t.Errorf("expected ErrAuthMissing, got: %v", err)
	}
}

// TestFetch_LiveAbsentStoredPresent: no live credential, stored accounts present
// → all rows non-active, no ErrAuthMissing.
func TestFetch_LiveAbsentStoredPresent(t *testing.T) {
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("uuid-a", "a@b.com", "tok-a", "ref-a", 0))
	_ = store.Upsert(context.Background(), makeAccount("uuid-b", "b@c.com", "tok-b", "ref-b", 0))

	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, store, nil, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("Accounts len = %d, want 2", len(out.Accounts))
	}
	for _, ar := range out.Accounts {
		if ar.Active {
			t.Errorf("account %q should not be active (no live credential)", ar.Email)
		}
	}
}

// ── FetchForSwitch tests ─────────────────────────────────────────────────────

// TestFetchForSwitch_Happy: stored access token valid, usage 200, AccountResult
// populated, store unmodified, refresh server receives zero requests.
func TestFetchForSwitch_Happy(t *testing.T) {
	activeAcct := makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0)
	nonActiveAcct := makeAccount("uuid-other", "other@example.com", "tok-other", "ref-other", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), activeAcct)
	_ = store.Upsert(context.Background(), nonActiveAcct)

	live := makeCred("tok-active", "ref-active", 0) // byte-matches activeAcct

	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t) // byte-match → no profile call
	refreshSrv, refreshCount := stubCountingServer(t, 200, refreshSuccessBody("at2", "rt2"))

	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).FetchForSwitch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("FetchForSwitch should return 1 non-active account, got %d", len(out))
	}
	if out[0].UUID != "uuid-other" {
		t.Errorf("result UUID = %q, want uuid-other", out[0].UUID)
	}
	if out[0].Active {
		t.Error("FetchForSwitch result should have Active = false")
	}
	if out[0].Limits == nil {
		t.Error("result should have limits")
	}

	// Store must be unchanged (no Upsert calls).
	uuids := storeUUIDSet(t, store)
	if !uuids["uuid-active"] || !uuids["uuid-other"] {
		t.Errorf("store changed after FetchForSwitch: %v", uuids)
	}

	if n := refreshCount.Load(); n != 0 {
		t.Errorf("refresh server received %d requests, expected 0", n)
	}
}

// TestFetchForSwitch_StoredTokenRejected: usage 401 → account excluded from
// returned slice, per-account warn emitted, refresh never called, store unchanged.
func TestFetchForSwitch_StoredTokenRejected(t *testing.T) {
	activeAcct := makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0)
	badAcct := makeAccount("uuid-bad", "bad@example.com", "tok-bad", "ref-bad", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), activeAcct)
	_ = store.Upsert(context.Background(), badAcct)

	live := makeCred("tok-active", "ref-active", 0)

	usageSrv := stubStaticServer(t, 401, []byte(`{"error":"unauthorized"}`))
	profileSrv := noProfileServer(t)
	refreshSrv, refreshCount := stubCountingServer(t, 200, refreshSuccessBody("at2", "rt2"))

	var warnBuf bytes.Buffer
	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, &warnBuf, nil).FetchForSwitch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(out) != 0 {
		t.Errorf("rejected account should be excluded, got %d results", len(out))
	}

	warn := warnBuf.String()
	if !strings.Contains(warn, "stored credential rejected") {
		t.Errorf("warn should mention rejected credential, got: %q", warn)
	}
	if !strings.Contains(warn, "excluded from auto-pick") {
		t.Errorf("warn should mention excluded from auto-pick, got: %q", warn)
	}

	// Store unchanged.
	uuids := storeUUIDSet(t, store)
	if !uuids["uuid-active"] || !uuids["uuid-bad"] {
		t.Errorf("store changed after FetchForSwitch: %v", uuids)
	}

	if n := refreshCount.Load(); n != 0 {
		t.Errorf("refresh server received %d requests, expected 0", n)
	}
}

// TestFetchForSwitch_TransientExclusion: usage 503 → account excluded with
// "usage fetch failed" warn.
func TestFetchForSwitch_TransientExclusion(t *testing.T) {
	// No live credential: the stored account is non-active, gets a usage fetch,
	// returns 503 → excluded from the result set with a warn.
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

	usageSrv := stubStaticServer(t, 503, []byte(`{"error":"unavailable"}`))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	var warnBuf bytes.Buffer
	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, store, &warnBuf, nil).FetchForSwitch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("transient account should be excluded, got %d results", len(out))
	}
	warn := warnBuf.String()
	if !strings.Contains(warn, "usage fetch failed") {
		t.Errorf("warn should mention usage fetch failed, got: %q", warn)
	}
	if !strings.Contains(warn, "excluded from auto-pick") {
		t.Errorf("warn should mention excluded from auto-pick, got: %q", warn)
	}
}

// ── shared reconcile-persist tests ───────────────────────────────────────────

// TestFetch_RotatedTokensPersisted: an account with a near-expiry token is
// refreshed, and the store ends up with the rotated AT2/RT2 blob.
func TestFetch_RotatedTokensPersisted(t *testing.T) {
	nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)

	live := makeCred("tok-a", "ref-a", nearExpiry) // byte-matches

	refreshSrv := stubStaticServer(t, 200, refreshSuccessBody("tok-a2", "ref-a2"))
	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
		nil, func() time.Time { return testNow }).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the store now has AT2/RT2 in the blob.
	stored, _ := store.List(context.Background())
	if len(stored) != 1 {
		t.Fatalf("store should have 1 account, got %d", len(stored))
	}
	if got := stored[0].AccessToken(); got != "tok-a2" {
		t.Errorf("stored AccessToken = %q, want tok-a2", got)
	}
	if got := stored[0].RefreshToken(); got != "ref-a2" {
		t.Errorf("stored RefreshToken = %q, want ref-a2", got)
	}
}

// TestFetchForSwitch_NeverRefreshesNeverMutatesStore: FetchForSwitch does not
// touch the refresh server and does not mutate the store even when the account
// has a near-expiry token.
func TestFetchForSwitch_NeverRefreshesNeverMutatesStore(t *testing.T) {
	nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
	activeAcct := makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0)
	otherAcct := makeAccount("uuid-other", "other@example.com", "tok-other", "ref-other", nearExpiry)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), activeAcct)
	_ = store.Upsert(context.Background(), otherAcct)

	live := makeCred("tok-active", "ref-active", 0) // byte-matches active

	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv, refreshCount := stubCountingServer(t, 200, refreshSuccessBody("tok-other2", "ref-other2"))

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
		nil, func() time.Time { return testNow }).FetchForSwitch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Refresh must not have been called.
	if n := refreshCount.Load(); n != 0 {
		t.Errorf("refresh called %d times, expected 0", n)
	}

	// Store must still have the original RT (not ref-other2).
	stored, _ := store.List(context.Background())
	for _, s := range stored {
		if s.UUID == "uuid-other" {
			if got := s.RefreshToken(); got != "ref-other" {
				t.Errorf("store RefreshToken = %q, want ref-other (store must be unchanged)", got)
			}
		}
	}
}

// TestFetch_ReconcileUpsertBeforeUsageFetches: verifies the store is updated
// (step 5 persist) before any usage fetch by checking the store state on the
// first usage request using an intercepting handler.
func TestFetch_ReconcileUpsertBeforeUsageFetches(t *testing.T) {
	// New account (no byte-match) → profile inserts → upsert before usage fetch.
	live := makeCred("tok-live", "ref-live", 0)
	store := accounts.NewMemoryStore()

	profBody := profileBody("uuid-new", "new@example.com", "default_claude_max_5x")
	profileSrv := stubStaticServer(t, 200, profBody)

	var upsertedBeforeUsage bool
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// When the first usage fetch arrives, check whether the account was upserted.
		accts, _ := store.List(context.Background())
		for _, a := range accts {
			if a.UUID == "uuid-new" {
				upsertedBeforeUsage = true
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write(minUsageBody)
	}))
	t.Cleanup(usageSrv.Close)

	refreshSrv := noRefreshServer(t)
	t.Cleanup(refreshSrv.Close)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !upsertedBeforeUsage {
		t.Error("account should be upserted (step 5) before the first usage fetch")
	}
}

// TestFetch_ProviderLimitsFromActiveAccount: provider-level Limits is the
// active account's limits (or nil when active account errored).
func TestFetch_ProviderLimitsFromActiveAccount(t *testing.T) {
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)

	live := makeCred("tok-a", "ref-a", 0) // active

	// Usage returns an error → active account has no limits → provider Limits = nil.
	usageSrv := stubStaticServer(t, 503, []byte(`{"error":"down"}`))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, _ := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if out.Limits != nil {
		t.Errorf("Limits should be nil when active account errored, got %v", out.Limits)
	}
}

// TestFetch_ThreeAccount_MixedForErrTransientRule: 3 accounts, 2 transient + 1
// auth-denied, zero succeeded → ErrTransient. Pins the D8 retry trigger.
func TestFetch_ThreeAccount_MixedForErrTransientRule(t *testing.T) {
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))
	_ = store.Upsert(context.Background(), makeAccount("uuid-b", "b@example.com", "tok-b", "ref-b", 0))
	_ = store.Upsert(context.Background(), makeAccount("uuid-c", "c@example.com", "tok-c", "ref-c", 0))

	// 2 transient accounts × maxAttempts=3 each = 6 consecutive 503s; the 3rd
	// account (whichever it is in non-deterministic map order) receives the repeated
	// 401 last entry. Update the response count if httpx.maxAttempts changes.
	usageSrv := stubSequentialServer(t, []struct {
		status int
		body   []byte
	}{
		{503, []byte(`{"error":"down"}`)},
		{503, []byte(`{"error":"down"}`)},
		{503, []byte(`{"error":"down"}`)},
		{503, []byte(`{"error":"down"}`)},
		{503, []byte(`{"error":"down"}`)},
		{503, []byte(`{"error":"down"}`)}, // 6 = 2 accounts × httpx.maxAttempts(3)
		{401, []byte(`{"error":"unauthorized"}`)},
	})
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, store, nil, nil).Fetch(context.Background())
	if !errors.Is(err, providers.ErrTransient) {
		t.Errorf("expected ErrTransient (2 transient + 1 auth-denied, zero succeeded), got: %v", err)
	}
}

// TestFetch_RefreshTransient_ErrTransient: single account with near-expiry
// token, refresh server returns 503 → refresh fails transient → successCount==0,
// transientCount>0 → provider returns ErrTransient.
func TestFetch_RefreshTransient_ErrTransient(t *testing.T) {
	nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)

	live := makeCred("tok-a", "ref-a", nearExpiry) // byte-matches

	refreshSrv := stubStaticServer(t, 503, []byte(`{"error":"service unavailable"}`))
	usageSrv := noRefreshServer(t) // usage must not be reached after refresh fails
	profileSrv := noProfileServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
		nil, func() time.Time { return testNow }).Fetch(context.Background())
	if !errors.Is(err, providers.ErrTransient) {
		t.Errorf("expected ErrTransient when refresh fails transient, got: %v", err)
	}
}

// TestFetch_LiveUnstored_UsageFetchFails: live credential present, empty store,
// profile call returns 503 → LiveUnstored set, then usage fetch also returns 503
// → transientCount > 0, successCount == 0 → provider returns ErrTransient.
func TestFetch_LiveUnstored_UsageFetchFails(t *testing.T) {
	live := makeCred("tok-live", "ref-live", 0)

	profileSrv := stubStaticServer(t, 503, []byte(`{"error":"unavailable"}`))
	usageSrv := stubStaticServer(t, 503, []byte(`{"error":"unavailable"}`))
	refreshSrv := noRefreshServer(t)

	var warnBuf bytes.Buffer
	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, nil, &warnBuf, nil).Fetch(context.Background())
	if !errors.Is(err, providers.ErrTransient) {
		t.Errorf("expected ErrTransient (live unstored, usage transient), got: %v", err)
	}
	if !strings.Contains(warnBuf.String(), "live row without storing") {
		t.Errorf("warn should mention live row without storing, got: %q", warnBuf.String())
	}
}

// TestFetch_AuthDeniedOnly_NilError: single stored account, usage returns 401
// (auth-denied), no transient failures → successCount==0, transientCount==0
// → provider returns nil error with per-account error populated. Pins the D8
// contract that ErrAuthDenied is never returned at the provider level from Fetch.
func TestFetch_AuthDeniedOnly_NilError(t *testing.T) {
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)

	live := makeCred("tok-a", "ref-a", 0)

	usageSrv := stubStaticServer(t, 401, []byte(`{"error":"unauthorized"}`))
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
	if err != nil {
		t.Errorf("Fetch must return nil error for auth-denied only (D8); got: %v", err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("expected 1 account result, got %d", len(out.Accounts))
	}
	if out.Accounts[0].Error == "" {
		t.Error("per-account error must be set for auth-denied account")
	}
}

// TestFetch_RotateExpiresAtZero: when the refresh response omits expires_in,
// rotateRawBlob must write expiresAt=0 to the blob — not preserve the stale
// pre-rotation timestamp — so the skew guard does not re-trigger on the next run.
func TestFetch_RotateExpiresAtZero(t *testing.T) {
	nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)

	live := makeCred("tok-a", "ref-a", nearExpiry)

	// Refresh response deliberately omits expires_in.
	noExpiryBody, _ := json.Marshal(map[string]any{
		"access_token":  "tok-a2",
		"refresh_token": "ref-a2",
	})
	refreshSrv := stubStaticServer(t, 200, noExpiryBody)
	usageSrv := stubStaticServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)

	_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
		nil, func() time.Time { return testNow }).Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stored, _ := store.List(context.Background())
	if len(stored) != 1 {
		t.Fatalf("store should have 1 account, got %d", len(stored))
	}
	if got := stored[0].AccessToken(); got != "tok-a2" {
		t.Errorf("stored AccessToken = %q, want tok-a2", got)
	}
	// expiresAt must be 0, not the old nearExpiry value.
	if got := stored[0].ExpiresAt(); got != 0 {
		t.Errorf("stored ExpiresAt = %d, want 0 (no expiry from server)", got)
	}
}

// ── cache integration tests ──────────────────────────────────────────────────

// TestFetch_TwoAccount_CacheHit: pre-populate cache for acct A; run Fetch;
// assert the usage server saw a request only for acct B.
func TestFetch_TwoAccount_CacheHit(t *testing.T) {
	acctA := makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0)
	acctB := makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acctA)
	_ = store.Upsert(context.Background(), acctB)

	live := makeCred("tok-a", "ref-a", 0) // byte-matches acctA

	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)

	// Pre-populate cache for acctA — only acctB should fire a fresh request.
	c.cache.Put("uuid-a", map[string]providers.Limit{
		"five_hour": {UsedPercent: 30, RemainingPercent: 70, ResetsAt: testNow.Add(time.Hour), ResetAfterSeconds: 3600},
	})

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := usageCount.Load(); n != 1 {
		t.Errorf("usage server requests = %d, want 1 (only acctB should fire)", n)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("Accounts len = %d, want 2", len(out.Accounts))
	}
	// Active account (acctA) should have limits from the cache (no error).
	for _, ar := range out.Accounts {
		if ar.UUID == "uuid-a" && ar.Error != "" {
			t.Errorf("acctA (cache hit) should have no error, got: %q", ar.Error)
		}
	}
}

// TestFetch_TwoAccount_CacheExpiredBothFire: pre-populate cache with entries
// older than TTL; assert the usage server saw requests for both accounts.
func TestFetch_TwoAccount_CacheExpiredBothFire(t *testing.T) {
	t.Setenv("AISTAT_USAGE_CACHE_TTL", "1s")

	acctA := makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0)
	acctB := makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acctA)
	_ = store.Upsert(context.Background(), acctB)

	now := testNow
	nowFn := func() time.Time { return now }
	live := makeCred("tok-a", "ref-a", 0)

	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nowFn)

	c.cache.Put("uuid-a", map[string]providers.Limit{"five_hour": {ResetsAt: now.Add(time.Hour)}})
	c.cache.Put("uuid-b", map[string]providers.Limit{"five_hour": {ResetsAt: now.Add(time.Hour)}})

	// Advance clock past the 1 s TTL.
	now = now.Add(2 * time.Second)

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := usageCount.Load(); n != 2 {
		t.Errorf("usage server requests = %d, want 2 (both entries expired)", n)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("Accounts len = %d, want 2", len(out.Accounts))
	}
}

// TestFetch_CacheHitRecomputesResetAfter: cached Limit.ResetsAt is absolute;
// ResetAfterSeconds is recomputed from the current clock on every cache hit.
func TestFetch_CacheHitRecomputesResetAfter(t *testing.T) {
	// Use a 90 s TTL so a 40 s clock advance does not expire the entry.
	t.Setenv("AISTAT_USAGE_CACHE_TTL", "90s")

	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)
	live := makeCred("tok-a", "ref-a", 0)

	now := testNow
	nowFn := func() time.Time { return now }

	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nowFn)

	// Put at T with ResetsAt = T + 60s; ResetAfterSeconds stored as 60.
	resetsAt := now.Add(60 * time.Second)
	c.cache.Put("uuid-a", map[string]providers.Limit{
		"five_hour": {UsedPercent: 50, RemainingPercent: 50, ResetsAt: resetsAt, ResetAfterSeconds: 60},
	})

	// Advance clock to T + 40s; expect ResetAfterSeconds = 20 on cache hit.
	now = now.Add(40 * time.Second)

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usageCount.Load() != 0 {
		t.Errorf("usage server should not be hit on cache hit, got %d requests", usageCount.Load())
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("Accounts len = %d, want 1", len(out.Accounts))
	}
	fh, ok := out.Accounts[0].Limits["five_hour"]
	if !ok {
		t.Fatal("five_hour limit missing")
	}
	if fh.ResetAfterSeconds != 20 {
		t.Errorf("ResetAfterSeconds = %d, want 20 (recomputed from cached ResetsAt)", fh.ResetAfterSeconds)
	}
	// ResetsAt must be preserved unchanged.
	if !fh.ResetsAt.Equal(resetsAt) {
		t.Errorf("ResetsAt = %v, want %v", fh.ResetsAt, resetsAt)
	}
}

// TestFetch_LiveUnstoredBypassesCache: the live-unstored fallback path (no UUID)
// always calls fetchLimitsFresh regardless of cache state.
func TestFetch_LiveUnstoredBypassesCache(t *testing.T) {
	live := makeCred("tok-live", "ref-live", 0)
	// Empty store → no byte-match → profile call needed.
	store := accounts.NewMemoryStore()

	// Profile returns 503 → LiveUnstored branch (no UUID available).
	profileSrv := stubStaticServer(t, 503, []byte(`{"error":"unavailable"}`))
	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	refreshSrv := noRefreshServer(t)

	var warnBuf bytes.Buffer
	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, &warnBuf, nil)

	// Pre-populate cache for some UUID — fallback path has no UUID and should
	// bypass the cache entirely.
	c.cache.Put("some-uuid", map[string]providers.Limit{"five_hour": {ResetAfterSeconds: 999}})

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usageCount.Load() != 1 {
		t.Errorf("usage server requests = %d, want 1 (fresh fetch required, no UUID)", usageCount.Load())
	}
	if len(out.Accounts) != 1 || out.Accounts[0].Email != "(live Claude account)" {
		t.Errorf("expected fallback row, got: %+v", out.Accounts)
	}
}

// TestFetch_RotatedTokenSharesCacheEntry: after a token rotation, fetchLimitsCached
// still keys on UUID (unchanged), so a pre-populated cache entry produces a hit
// without any usage server requests.
func TestFetch_RotatedTokenSharesCacheEntry(t *testing.T) {
	nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)
	live := makeCred("tok-a", "ref-a", nearExpiry) // byte-matches

	refreshSrv := stubStaticServer(t, 200, refreshSuccessBody("tok-a2", "ref-a2"))
	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
		nil, func() time.Time { return testNow })

	// Pre-populate cache for uuid-a with original limits.
	originalResetsAt := testNow.Add(time.Hour)
	c.cache.Put("uuid-a", map[string]providers.Limit{
		"five_hour": {UsedPercent: 20, RemainingPercent: 80, ResetsAt: originalResetsAt, ResetAfterSeconds: 3600},
	})

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Token was rotated (refresh fired) but UUID is unchanged → cache hit.
	if n := usageCount.Load(); n != 0 {
		t.Errorf("usage server requests = %d, want 0 (cache hit despite token rotation)", n)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("Accounts len = %d, want 1", len(out.Accounts))
	}
	if out.Accounts[0].Error != "" {
		t.Errorf("account should have no error on cache hit, got: %q", out.Accounts[0].Error)
	}
}

// TestFetch_CacheMiss429HonorsRetryAfterWithinBudget: a cache miss triggers a
// live fetch; the first response is 429 Retry-After: 1; the second succeeds.
// The account should still produce limits within the 3 s per-account budget.
func TestFetch_CacheMiss429HonorsRetryAfterWithinBudget(t *testing.T) {
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)
	live := makeCred("tok-a", "ref-a", 0)

	var callCount atomic.Int32
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(callCount.Add(1))
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write(minUsageBody)
	}))
	t.Cleanup(usageSrv.Close)

	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if n := callCount.Load(); n != 2 {
		t.Errorf("usage server calls = %d, want 2 (retry after 429)", n)
	}
	if len(out.Accounts) == 0 || out.Accounts[0].Error != "" {
		t.Errorf("account should succeed after retry; error = %q", out.Accounts[0].Error)
	}
}

// TestFetch_CacheMissRepeatedRetryAfterExceedsBudget: when the server returns
// 429 Retry-After: 2 on every attempt, and the pool budget (baseTimeout=0,
// perAccountBudget=3 s) means the second sleep would exceed the deadline,
// sleepWithCtx skips the second sleep → exactly 2 attempts → ErrTransient.
func TestFetch_CacheMissRepeatedRetryAfterExceedsBudget(t *testing.T) {
	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)
	live := makeCred("tok-a", "ref-a", 0)

	// Redirect cache to an isolated dir before constructing the cache.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")

	nowFn := func() time.Time { return testNow }

	var callCount atomic.Int32
	usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	t.Cleanup(usageSrv.Close)

	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	usageDoer := httpx.NewDoer(usageSrv.Client(), "aistat-test/0", "claude",
		map[string]string{"Anthropic-Beta": betaHeader}, nil)
	profDoer := httpx.NewDoer(profileSrv.Client(), "aistat-test/0", "claude",
		map[string]string{"Anthropic-Beta": betaHeader}, nil)
	pc := newProfileClient(profDoer)
	pc.endpoint = profileSrv.URL + "/api/oauth/profile"
	refDoer := httpx.NewDoer(refreshSrv.Client(), "aistat-test/0", "claude", nil, nil)
	rc := newRefreshClient(refDoer)
	rc.endpoint = refreshSrv.URL + "/v1/oauth/token"
	rc.now = nowFn

	c := &Client{
		doer:             usageDoer,
		endpoint:         usageSrv.URL + "/api/oauth/usage",
		profile:          pc,
		refresh:          rc,
		store:            store,
		readCredential:   func(context.Context) (cred.Credential, error) { return *live, nil },
		warn:             io.Discard,
		now:              nowFn,
		baseTimeout:      0,              // no base buffer: pool = just perAccountBudget
		perAccountBudget: 3 * time.Second, // 0+3=3 s pool; 2nd Retry-After:2 sleep (0+2+2=4 s) exceeds it
		cache:            newUsageCache(nowFn, func(string) {}),
	}

	start := time.Now()
	out, err := c.Fetch(context.Background())
	elapsed := time.Since(start)

	if !errors.Is(err, providers.ErrTransient) {
		t.Errorf("expected ErrTransient, got: %v", err)
	}
	if n := callCount.Load(); n != 2 {
		t.Errorf("usage server calls = %d, want 2 (second sleep skipped by budget check)", n)
	}
	if elapsed > 4*time.Second {
		t.Errorf("test took %v, want < 4s (should not sleep a second 2s delay)", elapsed)
	}
	if len(out.Accounts) == 0 || out.Accounts[0].Error == "" {
		t.Error("account should carry per-account ErrTransient error")
	}
}

// TestFetchForSwitch_UsesCacheHit: pre-populate cache for a non-active
// account; FetchForSwitch reads from the cache, not the live endpoint. This
// pins the unified-code-path contract (D7, revised): switch and reporting
// share fetchLimitsCached so a recent aistat usage call's entries are
// reused here.
func TestFetchForSwitch_UsesCacheHit(t *testing.T) {
	activeAcct := makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0)
	otherAcct := makeAccount("uuid-other", "other@example.com", "tok-other", "ref-other", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), activeAcct)
	_ = store.Upsert(context.Background(), otherAcct)

	live := makeCred("tok-active", "ref-active", 0)

	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)

	// Pre-populate cache for the non-active account.
	c.cache.Put("uuid-other", map[string]providers.Limit{
		"five_hour": {UsedPercent: 10, RemainingPercent: 90, ResetsAt: testNow.Add(time.Hour)},
	})

	results, err := c.FetchForSwitch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("FetchForSwitch returned %d results, want 1", len(results))
	}
	if n := usageCount.Load(); n != 0 {
		t.Errorf("usage server requests = %d, want 0 (FetchForSwitch should serve from cache)", n)
	}
	if got := results[0].Limits["five_hour"].UsedPercent; got != 10 {
		t.Errorf("five_hour.UsedPercent = %v, want 10 (from cached entry)", got)
	}
}

// TestFetchUsage_UsesCacheHit: pre-populate cache for a UUID; FetchUsage with
// that UUID reads from cache, not the live endpoint. Unified-code-path
// contract for the switch active-account read.
func TestFetchUsage_UsesCacheHit(t *testing.T) {
	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil)

	// Pre-populate cache for some UUID.
	c.cache.Put("uuid-x", map[string]providers.Limit{
		"five_hour": {UsedPercent: 5, RemainingPercent: 95, ResetsAt: testNow.Add(time.Hour)},
	})

	limits, err := c.FetchUsage(context.Background(), "tok-test", "uuid-x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := usageCount.Load(); n != 0 {
		t.Errorf("usage server requests = %d, want 0 (FetchUsage should serve from cache)", n)
	}
	if got := limits["five_hour"].UsedPercent; got != 5 {
		t.Errorf("five_hour.UsedPercent = %v, want 5 (from cached entry)", got)
	}
}

// TestFetchUsage_EmptyUUIDBypassesCache: when uuid is empty (e.g. an unstored
// live credential), FetchUsage falls through to a fresh fetch with no cache
// interaction.
func TestFetchUsage_EmptyUUIDBypassesCache(t *testing.T) {
	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil)

	_, err := c.FetchUsage(context.Background(), "tok-test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := usageCount.Load(); n != 1 {
		t.Errorf("usage server requests = %d, want 1 (empty uuid forces fresh fetch)", n)
	}
}

// TestNew_WithCacheBypass: WithCacheBypass(true) skips the read path (usage
// server IS hit) but still writes through on success (D8 contract).
func TestNew_WithCacheBypass(t *testing.T) {
	// Verify WithCacheBypass actually wires the field through New.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	optClient := New(nil, "aistat-test/0", WithCacheBypass(true))
	if !optClient.cacheBypass {
		t.Fatal("WithCacheBypass(true) did not set cacheBypass on Client")
	}

	acct := makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0)
	store := accounts.NewMemoryStore()
	_ = store.Upsert(context.Background(), acct)
	live := makeCred("tok-a", "ref-a", 0)

	usageSrv, usageCount := stubCountingServer(t, 200, minUsageBody)
	profileSrv := noProfileServer(t)
	refreshSrv := noRefreshServer(t)

	c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)

	// Pre-populate cache for uuid-a.
	c.cache.Put("uuid-a", map[string]providers.Limit{
		"five_hour": {UsedPercent: 99, RemainingPercent: 1, ResetsAt: testNow.Add(time.Hour)},
	})

	// Enable bypass: read path should be skipped even though cache has an entry.
	c.cacheBypass = true

	out, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Bypass skips read → usage server must be hit.
	if n := usageCount.Load(); n != 1 {
		t.Errorf("usage server requests = %d, want 1 (bypass skips cache read)", n)
	}
	// Write-through: cache entry should be updated with the fresh result.
	if _, ok := c.cache.Get("uuid-a"); !ok {
		t.Error("cache should have entry for uuid-a after write-through on bypass")
	}
	// Account should reflect the fresh server data (minUsageBody has 50% utilization),
	// not the stale 99% cached entry.
	if len(out.Accounts) == 0 || out.Accounts[0].Error != "" {
		t.Errorf("account should succeed; error = %q", out.Accounts[0].Error)
	}
	if len(out.Accounts) > 0 {
		fh := out.Accounts[0].Limits["five_hour"]
		if fh.UsedPercent == 99 {
			t.Error("UsedPercent should be from fresh server data, not stale cache")
		}
	}
}

// TestNew_PassesUserAgentToDoer guards against a regression where claude.New
// drops or alters its userAgent arg. Combined with httpx_test.go's wire-level
// checks on Doer.UserAgent, this proves DefaultUserAgent reaches the wire.
func TestNew_PassesUserAgentToDoer(t *testing.T) {
	c := New(nil, "claude-code/1.2.3")
	if c.doer.UserAgent != "claude-code/1.2.3" {
		t.Errorf("doer.UserAgent = %q, want %q", c.doer.UserAgent, "claude-code/1.2.3")
	}
}
