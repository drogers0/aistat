package codex

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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/httpx"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/providers/usagecache"
	"github.com/drogers0/aistat/v2/internal/testenv"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// ── test infrastructure ──────────────────────────────────────────────────────

// minUsageBody is a minimal valid usage API response for tests that don't
// validate limit values.
var minUsageBody = []byte(`{"rate_limit":{"primary_window":{"used_percent":50,"limit_window_seconds":18000,"reset_at":1779842256}}}`)

// routingUsageSrv returns an httptest.Server that returns different responses
// based on the Bearer token in the Authorization header. Fails the test for
// unknown tokens.
func routingUsageSrv(t *testing.T, routes map[string]struct {
	status int
	body   []byte
}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(auth, "Bearer ")
		resp, ok := routes[tok]
		if !ok {
			t.Errorf("routingUsageSrv: unexpected bearer token %q", tok)
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(resp.status)
		_, _ = w.Write(resp.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ── assertion helpers ────────────────────────────────────────────────────────

func wantAccounts(t *testing.T, out providers.ProviderOutput, n int) {
	t.Helper()
	if len(out.Accounts) != n {
		t.Fatalf("Accounts len = %d, want %d", len(out.Accounts), n)
	}
}

// ── fetch fixture ────────────────────────────────────────────────────────────

type fetchOpts struct {
	usage    *httptest.Server
	refresh  *httptest.Server
	live     *cred.Credential
	store    accounts.Store
	lookupID func(string) (string, string, error)
	warn     io.Writer
	now      func() time.Time
}

func runFetch(t *testing.T, o fetchOpts) (providers.ProviderOutput, error) {
	t.Helper()
	if o.usage == nil {
		o.usage = testutil.NewStubServer(t, minUsageBody, 200, nil)
	}
	if o.refresh == nil {
		o.refresh = testutil.RejectServer(t, "refresh")
	}
	return buildClient(t, o.usage, o.refresh, o.live, o.store, o.lookupID, o.warn, o.now).Fetch(context.Background())
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

// buildClient constructs a Client for testing. usageSrv and refreshSrv are the
// test servers; liveCred is nil to simulate missing credential; store is nil to
// default to empty MemoryStore; lookupID is nil to default to cred.ParseCodexIDToken;
// warnBuf is nil to discard warns; nowFn is nil to use testNow.
//
// Cache isolation: redirects the user home to a per-test TempDir.
func buildClient(
	t *testing.T,
	usageSrv *httptest.Server,
	refreshSrv *httptest.Server,
	liveCred *cred.Credential,
	store accounts.Store,
	lookupID func(string) (string, string, error),
	warnBuf io.Writer,
	nowFn func() time.Time,
) *Client {
	t.Helper()

	// Isolate cache I/O from the developer's real cache directory.
	testenv.RedirectHome(t, t.TempDir())

	usageDoer := httpx.NewDoer(usageSrv.Client(), "aistat-test/0", "codex", nil, nil)

	refDoer := httpx.NewDoer(refreshSrv.Client(), "aistat-test/0", "codex", nil, nil)
	rc := newRefreshClient(refDoer)
	rc.endpoint = refreshSrv.URL + "/oauth/token"
	rc.timeout = 5 * time.Second

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
			return cred.Credential{}, cred.ErrCodexTokenNotFound
		}
		return *liveCred, nil
	}

	warnFn := func(s string) { fmt.Fprintln(warnBuf, s) }

	return &Client{
		doer:             usageDoer,
		endpoint:         usageSrv.URL + "/backend-api/wham/usage",
		refresh:          rc,
		store:            store,
		readCredential:   readCred,
		lookupID:         lookupID,
		warn:             warnBuf,
		now:              nowFn,
		baseTimeout:      10 * time.Second,
		perAccountBudget: 3 * time.Second,
		cache:            usagecache.New("codex", nowFn, warnFn),
	}
}

// singleAccountStore returns a MemoryStore with one pre-populated account
// whose AT matches the given live credential's AT (enabling byte-match in
// reconcile without a LookupID call).
func singleAccountStore(t *testing.T, live *cred.Credential) *accounts.MemoryStore {
	t.Helper()
	store := accounts.NewMemoryStore()
	acct := makeCodexAccount("test-uuid", "test@example.com", live.AccessToken, "fake-rt", 0)
	if err := store.Upsert(context.Background(), acct); err != nil {
		t.Fatalf("store.Upsert: %v", err)
	}
	return store
}

// sortedKeys returns sorted keys of a limits map for deterministic assertions.
func keys(m map[string]providers.Limit) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// storeUUIDs returns sorted UUIDs from the store.
func storeUUIDs(t *testing.T, s accounts.Store) []string {
	t.Helper()
	accts, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	ids := make([]string, 0, len(accts))
	for _, a := range accts {
		ids = append(ids, a.UUID)
	}
	sort.Strings(ids)
	return ids
}

// ── rotateRawBlob tests ───────────────────────────────────────────────────────

func TestRotateRawBlob(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path updates tokens and preserves unknown fields", func(t *testing.T) {
			// Build a blob with all three token fields plus an unknown top-level field.
			original := []byte(`{"tokens":{"access_token":"old-at","refresh_token":"old-rt","id_token":"old-it","extra_field":"preserve"},"other_top":"preserved"}`)
			tok := Token{
				AccessToken:  "new-at",
				RefreshToken: "new-rt",
				IDToken:      "new-it",
			}
			out, err := rotateRawBlob(original, tok)
			if err != nil {
				t.Fatalf("rotateRawBlob: %v", err)
			}
			// Verify updated fields.
			if got := extractIDToken(out); got != "new-it" {
				t.Errorf("id_token = %q, want %q", got, "new-it")
			}
			at := parseStoredRaw(accounts.Account{RawBlob: out}).Tokens.AccessToken
			if at != "new-at" {
				t.Errorf("access_token = %q, want %q", at, "new-at")
			}
			rt := parseStoredRaw(accounts.Account{RawBlob: out}).Tokens.RefreshToken
			if rt != "new-rt" {
				t.Errorf("refresh_token = %q, want %q", rt, "new-rt")
			}
			// Unknown fields preserved.
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if m["other_top"] != "preserved" {
				t.Errorf("other_top = %v, want %q", m["other_top"], "preserved")
			}
			tokens, _ := m["tokens"].(map[string]any)
			if tokens["extra_field"] != "preserve" {
				t.Errorf("tokens.extra_field = %v, want %q", tokens["extra_field"], "preserve")
			}
		}},
		{"empty id_token preserves existing identity claim", func(t *testing.T) {
			// Token.IDToken == "" (refresh omitted id_token) → the existing id_token
			// is left in place. It is identity-only (sub stable across refresh);
			// expiry is read from the rotated access_token, not the id_token.
			original := []byte(`{"tokens":{"access_token":"old-at","refresh_token":"old-rt","id_token":"existing-jwt"}}`)
			tok := Token{
				AccessToken:  "new-at",
				RefreshToken: "new-rt",
				IDToken:      "",
			}
			out, err := rotateRawBlob(original, tok)
			if err != nil {
				t.Fatalf("rotateRawBlob: %v", err)
			}
			if got := extractIDToken(out); got != "existing-jwt" {
				t.Errorf("id_token = %q, want %q (existing id_token must be preserved)", got, "existing-jwt")
			}
		}},
		{"no id_token in original, none returned, none injected", func(t *testing.T) {
			// Opaque-token accounts (rawCodexBlob with exp 0) have no id_token key.
			// A refresh that omits id_token must not inject an empty one.
			original := []byte(`{"tokens":{"access_token":"old-at","refresh_token":"old-rt"}}`)
			out, err := rotateRawBlob(original, Token{AccessToken: "new-at", RefreshToken: "new-rt", IDToken: ""})
			if err != nil {
				t.Fatalf("rotateRawBlob: %v", err)
			}
			if got := extractIDToken(out); got != "" {
				t.Errorf("id_token = %q, want \"\" (none must be injected)", got)
			}
		}},
		{"malformed JSON errors", func(t *testing.T) {
			_, err := rotateRawBlob([]byte(`not json`), Token{AccessToken: "x"})
			if err == nil {
				t.Fatal("expected error for malformed JSON")
			}
		}},
		{"missing tokens object errors", func(t *testing.T) {
			_, err := rotateRawBlob([]byte(`{"other":"field"}`), Token{AccessToken: "x"})
			if err == nil {
				t.Fatal("expected error for missing tokens object")
			}
			if !strings.Contains(err.Error(), "tokens missing or wrong type") {
				t.Errorf("error = %q, want 'tokens missing or wrong type'", err.Error())
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── windowLabel tests ─────────────────────────────────────────────────────────

func TestWindowLabel(t *testing.T) {
	tests := []struct {
		name string
		secs int64
		want string
	}{
		{"five_hour exact", 18000, "five_hour"},
		{"seven_day exact", 604800, "seven_day"},
		{"thirty_day exact", 2592000, "thirty_day"},
		{"within tolerance", 17500, "five_hour"},         // 17500 is within 5% of 18000 (lo=17100, hi=18900).
		{"lower boundary inclusive", 17100, "five_hour"}, // exactly 5% below 18000 = 17100
		{"just below lower boundary", 17099, "window_17099s"},
		{"upper boundary inclusive", 18900, "five_hour"}, // exactly 5% above 18000 = 18900
		{"just above upper boundary", 18901, "window_18901s"},
		{"unknown duration", 86400, "window_86400s"},
		{"zero duration", 0, "window_0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := windowLabel(tt.secs); got != tt.want {
				t.Errorf("windowLabel(%d) = %q, want %q", tt.secs, got, tt.want)
			}
		})
	}
}

// ── Fetch golden / happy-path tests ──────────────────────────────────────────

func TestFetch_golden(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"reset after seconds truncated", func(t *testing.T) {
			// TestFetch_ResetAfterSecondsTruncated: sub-second component of c.now()
			// is stripped before computing ResetAfterSeconds.
			frozen := time.Date(2026, 5, 15, 12, 34, 56, 789_000_000, time.UTC)
			resetAt := frozen.Add(3 * time.Hour).Truncate(time.Second).Unix()
			body := []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":` +
				strconv.FormatInt(resetAt, 10) + `}}}`)

			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			want := 3 * 3600
			if got := out.Accounts[0].Limits["five_hour"].ResetAfterSeconds; got != want {
				t.Errorf("ResetAfterSeconds = %d, want %d (regression: removing .Truncate yields want-1)", got, want)
			}
		}},
		{"golden fixture two windows", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage.json"), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 1)
			limits := out.Accounts[0].Limits
			if len(limits) != 2 {
				t.Fatalf("expected 2 limits, got %d: %v", len(limits), limits)
			}
			for _, want := range []string{"five_hour", "seven_day"} {
				if _, ok := limits[want]; !ok {
					t.Errorf("missing %s", want)
				}
			}
			if _, ok := limits["code_review_seven_day"]; ok {
				t.Error("code_review_seven_day should be absent when API returns null")
			}
			fh := limits["five_hour"]
			if fh.UsedPercent != 2 {
				t.Errorf("five_hour used_percent = %v, want 2", fh.UsedPercent)
			}
			if fh.RemainingPercent != 98 {
				t.Errorf("five_hour remaining_percent = %v, want 98", fh.RemainingPercent)
			}
			wantTime := time.Unix(1779842256, 0).UTC()
			if !fh.ResetsAt.Equal(wantTime) {
				t.Errorf("resets_at = %v, want %v", fh.ResetsAt, wantTime)
			}
		}},
		{"code review included", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage_with_code_review.json"), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if _, ok := out.Accounts[0].Limits["code_review_seven_day"]; !ok {
				t.Fatalf("code_review_seven_day should be present, got %v", out.Accounts[0].Limits)
			}
			cr := out.Accounts[0].Limits["code_review_seven_day"]
			if cr.UsedPercent != 33 {
				t.Errorf("code_review used_percent = %v, want 33", cr.UsedPercent)
			}
		}},
		{"request shape", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			var got http.Request
			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage.json"), 200, &got)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			_, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if got.Method != "GET" {
				t.Errorf("method = %s", got.Method)
			}
			if got.URL.Path != "/backend-api/wham/usage" {
				t.Errorf("path = %s", got.URL.Path)
			}
			if h := got.Header.Get("Authorization"); h != "Bearer fake-jwt" {
				t.Errorf("Authorization = %q", h)
			}
			if h := got.Header.Get("User-Agent"); !strings.Contains(h, "aistat") {
				t.Errorf("User-Agent missing: %q", h)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch malformed-body / bad-response tests ────────────────────────────────

func TestFetch_malformed_body(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"missing rate_limit produces per-account error", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, []byte(`{}`), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			if err != nil {
				t.Fatalf("expected no provider-level error, got: %v", err)
			}
			wantAccounts(t, out, 1)
			if !strings.Contains(out.Accounts[0].Error, "missing rate_limit") {
				t.Errorf("Accounts[0].Error = %q, want 'missing rate_limit'", out.Accounts[0].Error)
			}
		}},
		{"null rate_limit produces per-account error", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, []byte(`{"rate_limit":null}`), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			if err != nil {
				t.Fatalf("expected no provider-level error, got: %v", err)
			}
			if !strings.Contains(out.Accounts[0].Error, "missing rate_limit") {
				t.Errorf("Accounts[0].Error = %q, want 'missing rate_limit'", out.Accounts[0].Error)
			}
		}},
		{"non-JSON 200 produces per-account error", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, []byte("<html>oops</html>"), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			if err != nil {
				t.Fatalf("provider-level error = %v, want nil", err)
			}
			if !strings.Contains(out.Accounts[0].Error, "non-JSON") {
				t.Errorf("Accounts[0].Error = %q, want 'non-JSON'", out.Accounts[0].Error)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch zero-reset_at window-skip tests ─────────────────────────────────────

func TestFetch_window_zero_reset(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"primary window reset_at zero skipped secondary present", func(t *testing.T) {
			body := []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":0},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1780429056}}}`)
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if _, ok := out.Accounts[0].Limits["five_hour"]; ok {
				t.Errorf("five_hour must be skipped when primary_window.reset_at == 0")
			}
			if _, ok := out.Accounts[0].Limits["seven_day"]; !ok {
				t.Errorf("seven_day should still be present alongside skipped five_hour")
			}
		}},
		{"secondary window reset_at zero skipped primary present", func(t *testing.T) {
			body := []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":1779842256},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":0}}}`)
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if _, ok := out.Accounts[0].Limits["seven_day"]; ok {
				t.Errorf("seven_day must be skipped when secondary_window.reset_at == 0")
			}
			if _, ok := out.Accounts[0].Limits["five_hour"]; !ok {
				t.Errorf("five_hour should still be present alongside skipped seven_day")
			}
		}},
		{"code review skipped on zero reset_at", func(t *testing.T) {
			body := []byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_at":1779842256},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_at":1780429056}},"code_review_rate_limit":{"used_percent":0,"limit_window_seconds":0,"reset_at":0}}`)
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if _, ok := out.Accounts[0].Limits["code_review_seven_day"]; ok {
				t.Errorf("code_review_seven_day must be skipped when reset_at == 0")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch HTTP-error / non-transient tests ────────────────────────────────────

func TestFetch_http_error(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"status 418 is bare per-account error not transient", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, []byte("teapot"), 418, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			if err != nil {
				// 418 is not transient; no provider-level error expected.
				t.Fatalf("provider-level error = %v, want nil (418 is bare per-account error)", err)
			}
			if !strings.Contains(out.Accounts[0].Error, "HTTP 418") {
				t.Errorf("Accounts[0].Error = %q, want mention of HTTP 418", out.Accounts[0].Error)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch auth-missing / credential-error tests ───────────────────────────────

func TestFetch_auth_missing(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"token missing is auth missing", func(t *testing.T) {
			usageSrv := testutil.NewStubServer(t, []byte(`{}`), 200, nil)
			// liveCred == nil → readCredential returns ErrCodexTokenNotFound → (nil, nil).
			// store == nil → empty MemoryStore → no accounts → ErrAuthMissing.
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, nil, noLookupCall(t), nil, nil)

			_, err := c.Fetch(context.Background())
			testutil.WantErrIs(t, err, providers.ErrAuthMissing)
			if !strings.Contains(err.Error(), cred.CodexTokenMissingMessage) {
				t.Errorf("expected exact message, got: %v", err)
			}
		}},
		{"token generic error propagated", func(t *testing.T) {
			sentinel := errors.New("some auth.json failure")
			usageSrv := testutil.NewStubServer(t, []byte(`{}`), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, nil, noLookupCall(t), nil, nil)
			// Override readCredential to return the sentinel error (not ErrCodexTokenNotFound).
			c.readCredential = func(context.Context) (cred.Credential, error) {
				return cred.Credential{}, sentinel
			}

			_, err := c.Fetch(context.Background())
			testutil.WantErrIs(t, err, sentinel)
		}},
		{"live absent zero stored is auth missing", func(t *testing.T) {
			_, err := runFetch(t, fetchOpts{lookupID: noLookupCall(t)})
			testutil.WantErrIs(t, err, providers.ErrAuthMissing)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch multi-account tests ─────────────────────────────────────────────────

func TestFetch_multi_account(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"two accounts both succeed active first", func(t *testing.T) {
			liveA := makeCodexCred("tok-a", "ref-a", 0)
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", 0)
			store := testutil.MemStore(t, acctA, acctB)

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), liveA, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if got := int(count.Load()); got != 2 {
				t.Errorf("usage calls = %d, want 2", got)
			}
			wantAccounts(t, out, 2)
			// Sorted: active first, then by email.
			if !out.Accounts[0].Active {
				t.Error("Accounts[0].Active = false, want true (active first)")
			}
			if out.Accounts[1].Active {
				t.Error("Accounts[1].Active = true, want false")
			}
		}},
		{"two accounts email ordering when none active", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "zed@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "alice@example.com", "tok-b", "ref-b", 0)
			store := testutil.MemStore(t, acctA, acctB)

			// liveCred nil → no active account, both fetched.
			out, err := runFetch(t, fetchOpts{store: store, lookupID: noLookupCall(t)})
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 2)
			// Sorted by email ascending (no active).
			emails := []string{out.Accounts[0].Email, out.Accounts[1].Email}
			if emails[0] != "alice@example.com" || emails[1] != "zed@example.com" {
				t.Errorf("email order = %v, want [alice, zed]", emails)
			}
		}},
		{"stored refresh rejected one account succeeds", func(t *testing.T) {
			// Account A: active, no refresh needed (ExpiresAt=0).
			// Account B: near-expiry → refresh is attempted → rejected.
			frozen := testNow
			nearExpirySec := frozen.Add(10 * time.Second).Unix() // within 30s refreshSkew

			liveA := makeCodexCred("tok-a", "ref-a", 0)
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", nearExpirySec)
			store := testutil.MemStore(t, acctA, acctB)

			invalidGrantBody := []byte(`{"error":"invalid_grant"}`)
			refreshSrv := testutil.NewStubServer(t, invalidGrantBody, 400, nil)
			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)

			c := buildClient(t, usageSrv, refreshSrv, liveA, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			out, err := c.Fetch(context.Background())
			if err != nil {
				t.Fatalf("provider-level error = %v, want nil (one account succeeded)", err)
			}
			if got := int(count.Load()); got != 1 {
				t.Errorf("usage calls = %d, want 1 (only account A)", got)
			}
			// Find account B in output (bob@example.com).
			var bobAR *providers.AccountResult
			for i := range out.Accounts {
				if out.Accounts[i].Email == "bob@example.com" {
					bobAR = &out.Accounts[i]
				}
			}
			if bobAR == nil {
				t.Fatal("bob@example.com not found in output")
			}
			if !strings.Contains(bobAR.Error, "codex login") {
				t.Errorf("bob.Error = %q, want 'codex login' hint", bobAR.Error)
			}
		}},
		{"all transient returns err transient", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", 0)
			store := testutil.MemStore(t, acctA, acctB)

			usageSrv := testutil.NewStubServer(t, []byte("service unavailable"), 503, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), nil, nil)

			_, err := c.Fetch(context.Background())
			testutil.WantErrIs(t, err, providers.ErrTransient)
		}},
		{"auth denied only nil provider error", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)

			usageSrv := testutil.NewStubServer(t, []byte("unauthorized"), 401, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			if err != nil {
				t.Errorf("expected nil provider-level error for auth-denied-only failures, got: %v", err)
			}
			if out.Accounts[0].Error == "" {
				t.Error("expected per-account error, got empty")
			}
		}},
		{"mixed transient and auth denied returns err transient", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", 0)
			store := testutil.MemStore(t, acctA, acctB)

			// Route: tok-a → 503 (transient), tok-b → 401 (auth denied).
			usageSrv := routingUsageSrv(t, map[string]struct {
				status int
				body   []byte
			}{
				"tok-a": {503, []byte("unavailable")},
				"tok-b": {401, []byte("unauthorized")},
			})
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), nil, nil)

			_, err := c.Fetch(context.Background())
			testutil.WantErrIs(t, err, providers.ErrTransient)
		}},
		{"refresh transient returns err transient", func(t *testing.T) {
			frozen := testNow
			nearExpirySec := frozen.Add(10 * time.Second).Unix()

			acct := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", nearExpirySec)
			store := testutil.MemStore(t, acct)

			refreshSrv := testutil.NewStubServer(t, []byte("unavailable"), 503, nil)
			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)

			c := buildClient(t, usageSrv, refreshSrv, nil, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			_, err := c.Fetch(context.Background())
			testutil.WantErrIs(t, err, providers.ErrTransient)
		}},
		{"two accounts cache hit on second fetch", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", 0)
			store := testutil.MemStore(t, acctA, acctB)

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), nil, nil)

			// First Fetch: populates cache for both accounts.
			if _, err := c.Fetch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 2 {
				t.Fatalf("expected 2 usage calls on first Fetch, got %d", count.Load())
			}

			// Second Fetch: both served from cache.
			if _, err := c.Fetch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 2 {
				t.Errorf("expected 2 total usage calls (cache hit on second Fetch), got %d", count.Load())
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch live-unstored path tests ───────────────────────────────────────────

func TestFetch_live_unstored(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"live present lookup fails falls back to live unstored", func(t *testing.T) {
			// Build a blob with an invalid (non-parseable) JWT.
			raw := []byte(`{"tokens":{"access_token":"tok-live","refresh_token":"ref-live","id_token":"not.a.jwt"}}`)
			live := &cred.Credential{
				AccessToken:  "tok-live",
				RefreshToken: "ref-live",
				Raw:          raw,
			}

			// Empty store → no byte-match → falls back to LookupID → fails → LiveUnstored.
			out, err := runFetch(t, fetchOpts{live: live})
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 1)
			if out.Accounts[0].Email != "(live Codex account)" {
				t.Errorf("Email = %q, want %q", out.Accounts[0].Email, "(live Codex account)")
			}
			if !out.Accounts[0].Active {
				t.Error("LiveUnstored row should be Active=true")
			}
		}},
		{"live absent stored present rows non-active", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)

			out, err := runFetch(t, fetchOpts{store: store, lookupID: noLookupCall(t)})
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 1)
			if out.Accounts[0].Active {
				t.Error("account should be non-active when live credential is absent")
			}
		}},
		{"live unstored bypasses cache", func(t *testing.T) {
			// Invalid JWT → lookup fails → LiveUnstored.
			raw := []byte(`{"tokens":{"access_token":"tok-live","id_token":"x.y.z"}}`)
			live := &cred.Credential{AccessToken: "tok-live", Raw: raw}

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, nil, nil, nil, nil)

			_, _ = c.Fetch(context.Background())
			// Must have made a fresh fetch (no UUID to cache on).
			if count.Load() < 1 {
				t.Error("expected at least one fresh usage fetch for LiveUnstored row")
			}
		}},
		{"live unstored usage fetch fails surfaces error row", func(t *testing.T) {
			raw := []byte(`{"tokens":{"access_token":"tok-live","id_token":"x.y.z"}}`)
			live := &cred.Credential{AccessToken: "tok-live", Raw: raw}

			usageSrv := testutil.NewStubServer(t, []byte("fail"), 503, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, nil, nil, nil, nil)

			out, err := c.Fetch(context.Background())
			// ErrTransient because zero succeeded, one transient failure.
			testutil.WantErrIs(t, err, providers.ErrTransient)
			wantAccounts(t, out, 1)
			if out.Accounts[0].Email != "(live Codex account)" {
				t.Errorf("Email = %q, want %q", out.Accounts[0].Email, "(live Codex account)")
			}
			if out.Accounts[0].Error == "" {
				t.Error("expected per-account error for failed fetch, got empty")
			}
		}},
		{"live unstored token invalidated tightens to msg tokens revoked", func(t *testing.T) {
			raw := []byte(`{"tokens":{"access_token":"tok-live","id_token":"x.y.z"}}`)
			live := &cred.Credential{AccessToken: "tok-live", Raw: raw}

			invalidatedBody := []byte(`{"error":{"code":"token_invalidated","message":"Your authentication token has been invalidated."}}`)
			usageSrv := testutil.NewStubServer(t, invalidatedBody, 401, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, nil, nil, nil, nil)

			out, _ := c.Fetch(context.Background())
			wantAccounts(t, out, 1)
			ar := out.Accounts[0]
			if ar.Email != "(live Codex account)" {
				t.Errorf("Email = %q, want %q", ar.Email, "(live Codex account)")
			}
			if !strings.Contains(ar.Error, msgTokensRevoked) {
				t.Errorf("ar.Error = %q, want substring %q", ar.Error, msgTokensRevoked)
			}
			if strings.Contains(ar.Error, "HTTP 401") {
				t.Errorf("ar.Error should not contain raw 'HTTP 401' prefix: %q", ar.Error)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch token-rotation / cache tests ───────────────────────────────────────

func TestFetch_token_rotation(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"rotated tokens persisted to store", func(t *testing.T) {
			frozen := testNow
			nearExpirySec := frozen.Add(10 * time.Second).Unix()
			farFutureSec := frozen.Add(time.Hour).Unix()

			// Live credential carries a near-expiry access_token; reconcile byte-matches
			// and propagates the live blob to the stored slot, so StoredExpiresAt returns
			// the near-expiry value (from the access_token JWT) and refresh is triggered.
			live := makeCodexCred("tok-a", "ref-a", nearExpirySec)
			acct := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", nearExpirySec)
			store := testutil.MemStore(t, acct)

			// Refresh returns a realistic far-future access_token JWT (the production
			// shape); refreshSuccessBody omits id_token, so rotateRawBlob preserves
			// the existing one (verified below).
			rotatedAT := accessJWT("tok-a-new", farFutureSec)
			refreshSrv := testutil.NewStubServer(t, refreshSuccessBody(rotatedAT, "ref-a-new"), 200, nil)
			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)

			c := buildClient(t, usageSrv, refreshSrv, live, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if len(out.Accounts) == 0 {
				t.Fatal("expected at least one account")
			}

			// Verify tokens were persisted to the store.
			storedAccts, _ := store.List(context.Background())
			if len(storedAccts) != 1 {
				t.Fatalf("store has %d accounts, want 1", len(storedAccts))
			}
			if got := StoredAccessToken(storedAccts[0]); got != rotatedAT {
				t.Errorf("stored access_token = %q, want %q", got, rotatedAT)
			}
			if got := StoredRefreshToken(storedAccts[0]); got != "ref-a-new" {
				t.Errorf("stored refresh_token = %q, want %q", got, "ref-a-new")
			}
			// The refresh response omitted id_token, so the pre-refresh identity
			// token must be preserved in the stored blob (rotateRawBlob behavior).
			if got := extractIDToken(storedAccts[0].RawBlob); got == "" {
				t.Error("id_token should be preserved in stored blob after a refresh that omits one")
			}
		}},
		{"reconcile upsert before usage fetches", func(t *testing.T) {
			// Live credential with a far-future expiry (1h) to avoid triggering the
			// near-expiry refresh path (30s skew). Purpose of this test is to verify
			// the upsert-before-usage-fetch ordering, not refresh behavior.
			sub := "uuid-new"
			farFutureSec := testNow.Unix() + 3600
			live := makeCodexCred("tok-new", "ref-new", farFutureSec)

			usageSrv := testutil.NewStubServer(t, []byte("fail"), 503, nil) // usage fails
			// Use fixedLookup so the LookupID step succeeds and inserts the slot.
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, nil, fixedLookup(sub, "new@example.com"), nil, nil)

			_, _ = c.Fetch(context.Background())

			// The slot must be in the store even though usage failed.
			if ids := storeUUIDs(t, c.store); len(ids) == 0 || ids[0] != sub {
				t.Errorf("store UUIDs = %v, want [%s]", ids, sub)
			}
		}},
		{"cache hit recomputes reset after seconds", func(t *testing.T) {
			live := makeCodexCred("tok-a", "ref-a", 0)
			acct := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acct)

			// The usage fixture has reset_at=1779842256 (~5h18m future from frozen).
			frozen := time.Unix(1779842256-19077, 0).UTC() // exactly 19077s before reset
			laterTime := frozen.Add(9000 * time.Second)    // 10077s before reset

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			// First fetch: populates cache.
			out1, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if count.Load() != 1 {
				t.Fatalf("expected 1 usage call after first Fetch, got %d", count.Load())
			}
			ras1 := out1.Accounts[0].Limits["five_hour"].ResetAfterSeconds

			// Advance time and fetch again (cache hit expected within 90s TTL).
			c.now = func() time.Time { return frozen.Add(5 * time.Second) }
			out2, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if count.Load() != 1 {
				t.Errorf("expected cache hit on second Fetch (no new HTTP call), got %d total calls", count.Load())
			}
			ras2 := out2.Accounts[0].Limits["five_hour"].ResetAfterSeconds
			// ResetAfterSeconds should be 5s less on the second call.
			if ras2 >= ras1 {
				t.Errorf("ResetAfterSeconds did not decrease: first=%d, second=%d", ras1, ras2)
			}
			_ = laterTime // suppress unused warning
		}},
		{"rotated token shares uuid-keyed cache entry", func(t *testing.T) {
			frozen := testNow
			nearExpirySec := frozen.Add(10 * time.Second).Unix()

			acct := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", nearExpirySec)
			store := testutil.MemStore(t, acct)

			// Refresh returns a far-future access_token JWT so the post-rotation
			// StoredExpiresAt is far future (> now+skew) — the real production path
			// for "freshly-refreshed token is not re-refreshed", not an opaque→0 fallback.
			farFutureSec := frozen.Add(time.Hour).Unix()
			refreshSrv, refreshCount := testutil.CountingServer(t, 200, refreshSuccessBody(accessJWT("tok-a-new", farFutureSec), "ref-a-new"))
			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)

			c := buildClient(t, usageSrv, refreshSrv, nil, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			// First Fetch: refresh fires (near expiry), usage fetched and cached under uuid-a.
			_, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if usageCount.Load() != 1 {
				t.Fatalf("expected 1 usage call on first Fetch, got %d", usageCount.Load())
			}
			if refreshCount.Load() != 1 {
				t.Fatalf("expected 1 refresh call on first Fetch, got %d", refreshCount.Load())
			}

			// Second Fetch: rotated access_token is a fresh far-future JWT →
			// StoredExpiresAt > now+skew → gate false → no refresh; UUID-keyed cache
			// still holds the entry → no usage HTTP call.
			_, err = c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if usageCount.Load() != 1 {
				t.Errorf("expected cache hit on second Fetch (UUID-keyed), got %d total usage calls", usageCount.Load())
			}
			if refreshCount.Load() != 1 {
				t.Errorf("expected no second refresh (fresh far-future access_token JWT), got %d total refresh calls", refreshCount.Load())
			}
		}},
		{"valid access token with expired id token does not refresh", func(t *testing.T) {
			// Regression for the reported bug: access_token valid for an hour but the
			// OIDC id_token expired an hour ago. The gate must read the access_token
			// exp (valid) and NOT refresh — the usage call uses the valid token.
			frozen := testNow
			farFutureSec := frozen.Add(time.Hour).Unix()
			pastSec := frozen.Add(-time.Hour).Unix()
			at := accessJWT("tok-a", farFutureSec)
			blob := json.RawMessage(`{"tokens":{"access_token":"` + at + `","refresh_token":"ref-a","id_token":"` +
				syntheticIDToken("sub-tok-a", "alice@example.com", pastSec) + `"}}`)
			acct := accounts.Account{UUID: "uuid-a", Email: "alice@example.com", RawBlob: blob}
			live := &cred.Credential{AccessToken: at, RefreshToken: "ref-a", Raw: []byte(blob)}
			store := testutil.MemStore(t, acct)

			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			// RejectServer fails the test if the refresh endpoint is hit at all.
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if usageCount.Load() != 1 {
				t.Errorf("expected exactly 1 usage call, got %d", usageCount.Load())
			}
			if len(out.Accounts) != 1 {
				t.Fatalf("expected 1 account, got %d", len(out.Accounts))
			}
			if out.Accounts[0].Error != "" {
				t.Errorf("account error = %q, want empty (no refresh, valid token)", out.Accounts[0].Error)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestNew_WithCacheBypass verifies that WithCacheBypass sets the cacheBypass field.
func TestNew_WithCacheBypass(t *testing.T) {
	testenv.RedirectHome(t, t.TempDir())
	c := New(nil, "test/0", WithCacheBypass(true))
	if !c.CacheBypassEnabled() {
		t.Error("CacheBypassEnabled() = false, want true after WithCacheBypass(true)")
	}
}

// ── FetchForSwitch tests ──────────────────────────────────────────────────────

func TestFetchForSwitch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path excludes active returns non-active", func(t *testing.T) {
			liveA := makeCodexCred("tok-a", "ref-a", 0)
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", 0)
			store := testutil.MemStore(t, acctA, acctB)

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), liveA, store, noLookupCall(t), nil, nil)

			results, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			// Only non-active account (B) should be returned.
			if len(results) != 1 {
				t.Fatalf("results len = %d, want 1", len(results))
			}
			if results[0].UUID != "uuid-b" {
				t.Errorf("result UUID = %q, want %q", results[0].UUID, "uuid-b")
			}
			if results[0].Active {
				t.Error("FetchForSwitch result.Active must be false")
			}
			if int(count.Load()) != 1 {
				t.Errorf("usage calls = %d, want 1 (only account B)", count.Load())
			}
		}},
		{"stored token rejected excluded from results with warn", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)

			usageSrv := testutil.NewStubServer(t, []byte("unauthorized"), 401, nil)
			var warnBuf bytes.Buffer
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), &warnBuf, nil)

			results, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			if len(results) != 0 {
				t.Errorf("expected 0 results (account excluded), got %d", len(results))
			}
			if !strings.Contains(warnBuf.String(), "excluded from auto-pick") {
				t.Errorf("warn = %q, want 'excluded from auto-pick'", warnBuf.String())
			}
		}},
		{"transient failure excludes account", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)

			usageSrv := testutil.NewStubServer(t, []byte("unavailable"), 503, nil)
			var warnBuf bytes.Buffer
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), &warnBuf, nil)

			results, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			if len(results) != 0 {
				t.Errorf("expected 0 results (transient → excluded), got %d", len(results))
			}
		}},
		{"never refreshes never mutates store", func(t *testing.T) {
			frozen := testNow
			nearExpirySec := frozen.Add(10 * time.Second).Unix()

			// Account with near-expiry token (would normally trigger refresh in Fetch).
			acct := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", nearExpirySec)
			store := testutil.MemStore(t, acct)

			// Snapshot store state before call.
			snapshot, _ := store.List(context.Background())

			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), nil,
				func() time.Time { return frozen })

			// RejectServer will fail the test if refresh is attempted.
			_, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)

			// Verify store is unchanged.
			after, _ := store.List(context.Background())
			if len(after) != len(snapshot) {
				t.Errorf("store len changed: %d → %d", len(snapshot), len(after))
			}
			if string(after[0].RawBlob) != string(snapshot[0].RawBlob) {
				t.Error("store RawBlob mutated by FetchForSwitch")
			}
		}},
		{"active uuid unresolvable all accounts treated as candidates", func(t *testing.T) {
			raw := []byte(`{"tokens":{"access_token":"tok-live","id_token":"not.a.jwt"}}`)
			live := &cred.Credential{AccessToken: "tok-live", Raw: raw}

			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", 0)
			store := testutil.MemStore(t, acctA, acctB)

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, nil, nil, nil)

			results, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			// Both accounts should be included (none identified as active).
			if len(results) != 2 {
				t.Fatalf("results len = %d, want 2", len(results))
			}
			for _, r := range results {
				if r.Active {
					t.Errorf("account %s.Active = true, all should be false", r.Email)
				}
			}
			if int(count.Load()) != 2 {
				t.Errorf("usage calls = %d, want 2", count.Load())
			}
		}},
		{"uses cache hit from prior fetch", func(t *testing.T) {
			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			acctB := makeCodexAccount("uuid-b", "bob@example.com", "tok-b", "ref-b", 0)
			liveA := makeCodexCred("tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA, acctB)

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), liveA, store, noLookupCall(t), nil, nil)

			// First call: Fetch populates cache for both accounts.
			if _, err := c.Fetch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 2 {
				t.Fatalf("Fetch: expected 2 usage calls, got %d", count.Load())
			}

			// FetchForSwitch: should hit cache for account B (non-active).
			if _, err := c.FetchForSwitch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 2 {
				t.Errorf("FetchForSwitch: expected cache hit (no new HTTP call), got %d total calls", count.Load())
			}
		}},
		{"token revoked warn hint contains codex login not aistat usage", func(t *testing.T) {
			revokedBody := []byte(`{"error":{"code":"token_revoked","message":"Encountered invalidated oauth token"}}`)

			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)

			usageSrv := testutil.NewStubServer(t, revokedBody, 401, nil)
			var warnBuf bytes.Buffer
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), &warnBuf, nil)

			results, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			if len(results) != 0 {
				t.Errorf("expected 0 results (account excluded), got %d", len(results))
			}
			warn := warnBuf.String()
			if !strings.Contains(warn, "`codex login`") {
				t.Errorf("warn should contain '`codex login`': %q", warn)
			}
			if !strings.Contains(warn, "stored credential rejected") {
				t.Errorf("warn should contain 'stored credential rejected': %q", warn)
			}
			if strings.Contains(warn, "`aistat usage`") {
				t.Errorf("token_revoked should not suggest `aistat usage`: %q", warn)
			}
		}},
		{"other auth denied keeps aistat usage hint", func(t *testing.T) {
			otherBody := []byte(`{"error":{"code":"unauthorized","message":"bad token"}}`)

			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)

			usageSrv := testutil.NewStubServer(t, otherBody, 401, nil)
			var warnBuf bytes.Buffer
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, store, noLookupCall(t), &warnBuf, nil)

			results, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			if len(results) != 0 {
				t.Errorf("expected 0 results, got %d", len(results))
			}
			warn := warnBuf.String()
			if !strings.Contains(warn, "`aistat usage`") {
				t.Errorf("non-token_revoked body should keep `aistat usage` hint: %q", warn)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── FetchUsage tests ──────────────────────────────────────────────────────────

func TestFetchUsage(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"uses cache hit", func(t *testing.T) {
			live := makeCodexCred("tok-a", "ref-a", 0)
			acct := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acct)

			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			// Fetch populates the cache.
			if _, err := c.Fetch(context.Background()); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 1 {
				t.Fatalf("expected 1 call after Fetch, got %d", count.Load())
			}

			// FetchUsage should hit cache.
			if _, err := c.FetchUsage(context.Background(), "tok-a", "uuid-a"); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 1 {
				t.Errorf("expected cache hit for FetchUsage, got %d total calls", count.Load())
			}
		}},
		{"empty uuid bypasses cache always fetches", func(t *testing.T) {
			usageSrv, count := testutil.CountingServer(t, 200, minUsageBody)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, nil, noLookupCall(t), nil, nil)

			for i := 0; i < 3; i++ {
				if _, err := c.FetchUsage(context.Background(), "tok-x", ""); err != nil {
					t.Fatal(err)
				}
			}
			if count.Load() != 3 {
				t.Errorf("expected 3 fresh fetches for empty UUID, got %d", count.Load())
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch slot-vs-duration label tests ───────────────────────────────────────

func TestFetch_slot_label(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"free account weekly in primary labeled seven_day", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage_free_weekly_primary.json"), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			limits := out.Accounts[0].Limits
			if _, ok := limits["five_hour"]; ok {
				t.Error("five_hour must not appear for a free account with weekly primary_window")
			}
			if _, ok := limits["seven_day"]; !ok {
				t.Errorf("seven_day must appear when primary_window.limit_window_seconds=604800, got: %v", keys(limits))
			}
		}},
		{"paid account both windows labeled correctly", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage_paid_both_windows.json"), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			limits := out.Accounts[0].Limits
			if _, ok := limits["five_hour"]; !ok {
				t.Errorf("five_hour must appear for primary_window.limit_window_seconds=18000, got: %v", keys(limits))
			}
			if _, ok := limits["seven_day"]; !ok {
				t.Errorf("seven_day must appear for secondary_window.limit_window_seconds=604800, got: %v", keys(limits))
			}
		}},
		{"unknown duration falls to window_86400s bucket", func(t *testing.T) {
			live := makeCodexCred("fake-jwt", "fake-rt", 0)
			store := singleAccountStore(t, live)

			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage_unknown_duration.json"), 200, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			limits := out.Accounts[0].Limits
			if _, ok := limits["window_86400s"]; !ok {
				t.Errorf("window_86400s must appear for unknown limit_window_seconds=86400, got: %v", keys(limits))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── Fetch token-revoked tightening tests ─────────────────────────────────────

func TestFetch_token_revoked(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"token revoked surfaces tight string not http 401 prefix", func(t *testing.T) {
			revokedBody := []byte(`{"error":{"code":"token_revoked","message":"Encountered invalidated oauth token"}}`)

			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)
			live := makeCodexCred("tok-a", "ref-a", 0)

			usageSrv := testutil.NewStubServer(t, revokedBody, 401, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, _ := c.Fetch(context.Background())
			wantAccounts(t, out, 1)
			ar := out.Accounts[0]
			if !strings.Contains(ar.Error, msgTokensRevoked) {
				t.Errorf("ar.Error = %q, want substring %q", ar.Error, msgTokensRevoked)
			}
			if strings.Contains(ar.Error, "HTTP 401") {
				t.Errorf("ar.Error should not contain raw 'HTTP 401' prefix: %q", ar.Error)
			}
		}},
		{"token invalidated surfaces tight string not http 401 prefix", func(t *testing.T) {
			invalidatedBody := []byte(`{"error":{"code":"token_invalidated","message":"Your authentication token has been invalidated."}}`)

			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)
			live := makeCodexCred("tok-a", "ref-a", 0)

			usageSrv := testutil.NewStubServer(t, invalidatedBody, 401, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, _ := c.Fetch(context.Background())
			wantAccounts(t, out, 1)
			ar := out.Accounts[0]
			if !strings.Contains(ar.Error, msgTokensRevoked) {
				t.Errorf("ar.Error = %q, want substring %q", ar.Error, msgTokensRevoked)
			}
			if strings.Contains(ar.Error, "HTTP 401") {
				t.Errorf("ar.Error should not contain raw 'HTTP 401' prefix: %q", ar.Error)
			}
		}},
		{"other auth denied body does not trigger tightening", func(t *testing.T) {
			otherBody := []byte(`{"error":{"code":"some_other_error","message":"unexpected"}}`)

			acctA := makeCodexAccount("uuid-a", "alice@example.com", "tok-a", "ref-a", 0)
			store := testutil.MemStore(t, acctA)
			live := makeCodexCred("tok-a", "ref-a", 0)

			usageSrv := testutil.NewStubServer(t, otherBody, 401, nil)
			c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), live, store, noLookupCall(t), nil, nil)

			out, _ := c.Fetch(context.Background())
			wantAccounts(t, out, 1)
			ar := out.Accounts[0]
			if ar.Error == "" {
				t.Error("ar.Error must be non-empty for auth-denied")
			}
			if strings.Contains(ar.Error, msgTokensRevoked) {
				t.Errorf("non-token_revoked body should not trigger tightening: %q", ar.Error)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── refreshErrorMessage tests ─────────────────────────────────────────────────

func TestRefreshErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"stale refresh token body returns msg stale refresh", func(t *testing.T) {
			err := fmt.Errorf("%w: HTTP 401 from https://auth.openai.com/oauth/token: Your refresh token has already been used to generate a new access token. Please try signing in again.", ErrRefreshRejected)
			got := refreshErrorMessage(err)
			if got != msgStaleRefresh {
				t.Errorf("refreshErrorMessage = %q, want %q", got, msgStaleRefresh)
			}
		}},
		{"passthrough keeps existing error strings", func(t *testing.T) {
			rejErr := fmt.Errorf("%w: HTTP 400 bad grant", ErrRefreshRejected)
			got := refreshErrorMessage(rejErr)
			if !strings.Contains(got, "account credential expired") {
				t.Errorf("ErrRefreshRejected: refreshErrorMessage = %q, want 'account credential expired'", got)
			}

			brokenErr := fmt.Errorf("%w: HTTP 404 from endpoint", ErrRefreshEndpointBroken)
			got2 := refreshErrorMessage(brokenErr)
			if !strings.Contains(got2, "refresh endpoint rejected request") {
				t.Errorf("ErrRefreshEndpointBroken: refreshErrorMessage = %q, want 'refresh endpoint rejected request'", got2)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestIsRevokedTokenErr locks the two real upstream "dead token" codes to the
// tightened message. The chatgpt.com usage endpoint has returned both
// "token_revoked" and "token_invalidated" interchangeably for the same
// condition; both must map to msgTokensRevoked.
func TestIsRevokedTokenErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"token_revoked usage body", fmt.Errorf("%w: HTTP 401: {\"error\":{\"code\":\"token_revoked\"}}", providers.ErrAuthDenied), true},
		{"token_invalidated usage body", fmt.Errorf("%w: HTTP 401 from https://chatgpt.com/backend-api/wham/usage: {\"error\":{\"code\":\"token_invalidated\",\"message\":\"Your authentication token has been invalidated.\"}}", providers.ErrAuthDenied), true},
		{"other auth-denied body", fmt.Errorf("%w: HTTP 401: some_other_error", providers.ErrAuthDenied), false},
		{"invalidated text but not auth-denied", errors.New("token_invalidated"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRevokedTokenErr(tt.err); got != tt.want {
				t.Errorf("isRevokedTokenErr = %v, want %v", got, tt.want)
			}
		})
	}
}

// ── PostSwitchVerify real-impl test (Step 5 / A3#1) ───────────────────────────

// TestCodexPostSwitchVerify_RealImplWrapsErrAuthDenied exercises the real Codex
// PostSwitchVerify against a live httptest.Server returning 401 token_revoked.
// This guards the load-bearing %w wrap and bare-phrase contracts; stub-based
// switch_test.go cases cannot catch a future implementer who omits %w.
func TestCodexPostSwitchVerify_RealImplWrapsErrAuthDenied(t *testing.T) {
	revokedBody := []byte(`{"error":{"code":"token_revoked","message":"Encountered invalidated oauth token for user"}}`)
	usageSrv := testutil.NewStubServer(t, revokedBody, 401, nil)

	target := makeCodexAccount("uuid-target", "target@example.com", "tok-target", "ref-target", 0)
	c := buildClient(t, usageSrv, testutil.RejectServer(t, "refresh"), nil, nil, noLookupCall(t), nil, nil)

	err := c.PostSwitchVerify(context.Background(), target)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, providers.ErrAuthDenied) {
		t.Errorf("expected errors.Is(err, ErrAuthDenied) = true; err = %v", err)
	}
	if !strings.Contains(err.Error(), msgTokensRevoked) {
		t.Errorf("err.Error() should contain msgTokensRevoked: %q", err.Error())
	}
	if strings.HasPrefix(err.Error(), "aistat: codex:") {
		t.Errorf("PostSwitchVerify must return bare phrase (no 'aistat: codex:' prefix): %q", err.Error())
	}
}
