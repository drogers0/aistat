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
	"github.com/drogers0/aistat/v2/internal/providers/usagecache"
	"github.com/drogers0/aistat/v2/internal/testenv"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// ── test infrastructure ──────────────────────────────────────────────────────

// minUsageBody is a minimal valid usage API response used in tests that don't
// validate limit values.
var minUsageBody = []byte(`{"five_hour":{"utilization":50.0,"resets_at":"2027-01-01T00:00:00+00:00"}}`)

// ── assertion helpers ────────────────────────────────────────────────────────

func wantAccounts(t *testing.T, out providers.ProviderOutput, n int) {
	t.Helper()
	if len(out.Accounts) != n {
		t.Fatalf("Accounts len = %d, want %d", len(out.Accounts), n)
	}
}

func wantWarn(t *testing.T, buf fmt.Stringer, sub string) {
	t.Helper()
	if !strings.Contains(buf.String(), sub) {
		t.Fatalf("warn = %q, want contains %q", buf.String(), sub)
	}
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
// Cache isolation: redirects the user home to a per-test
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
	testenv.RedirectHome(t, t.TempDir())

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
		cache:            usagecache.New("claude", nowFn, warnFn),
	}
}

// ── fetch fixture ────────────────────────────────────────────────────────────

type fetchOpts struct {
	usage, profile, refresh *httptest.Server
	live                    *cred.Credential
	store                   accounts.Store
	warn                    io.Writer
	now                     func() time.Time
}

func runFetch(t *testing.T, o fetchOpts) (providers.ProviderOutput, error) {
	t.Helper()
	if o.usage == nil {
		o.usage = testutil.NewStubServer(t, minUsageBody, 200, nil)
	}
	if o.profile == nil {
		o.profile = testutil.RejectServer(t, "profile")
	}
	if o.refresh == nil {
		o.refresh = testutil.RejectServer(t, "refresh")
	}
	return buildClient(t, o.usage, o.profile, o.refresh, o.live, o.store, o.warn, o.now).Fetch(context.Background())
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

func TestFetch_golden(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"reset after seconds truncated", func(t *testing.T) {
			frozen := time.Date(2026, 5, 15, 12, 34, 56, 789_000_000, time.UTC)
			resetsAt := frozen.Add(3 * time.Hour).Truncate(time.Second)
			body := []byte(`{"five_hour":{"utilization":50,"resets_at":"` + resetsAt.Format(time.RFC3339Nano) + `"}}`)

			live := makeCred("tok-live", "ref-live", 0)
			store := testutil.MemStore(t, makeAccount("uuid-1", "user@example.com", "tok-live", "ref-live", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile") // byte-match → no profile call
			refreshSrv := testutil.RejectServer(t, "refresh") // no refresh needed (ExpiresAt == 0)

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
		}},
		{"full fixture", func(t *testing.T) {
			live := makeCred("tok-live", "ref-live", 0)
			store := testutil.MemStore(t, makeAccount("uuid-1", "user@example.com", "tok-live", "ref-live", 0))

			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage.json"), 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

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
			if len(acctLimits) != 4 {
				t.Fatalf("expected 4 limits, got %d: %v", len(acctLimits), keys(acctLimits))
			}
			for _, want := range []string{"five_hour", "seven_day", "seven_day_sonnet", "seven_day_fable"} {
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
			fable := acctLimits["seven_day_fable"]
			if fable.UsedPercent != 44.0 {
				t.Errorf("seven_day_fable used_percent = %v, want 44.0", fable.UsedPercent)
			}
			if fable.RemainingPercent != 56.0 {
				t.Errorf("seven_day_fable remaining_percent = %v, want 56.0", fable.RemainingPercent)
			}
			wantFableTime, _ := time.Parse(time.RFC3339, "2026-07-04T10:00:00Z")
			if !fable.ResetsAt.Equal(wantFableTime) {
				t.Errorf("seven_day_fable resets_at = %v, want %v", fable.ResetsAt, wantFableTime)
			}
			if !out.Accounts[0].Active {
				t.Error("single account row should be active")
			}
		}},
		{"request shape", func(t *testing.T) {
			var gotReq http.Request
			live := makeCred("sk-ant-oat01-fake", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "sk-ant-oat01-fake", "ref", 0))

			usageSrv := testutil.NewStubServer(t, testutil.LoadFixture(t, "usage.json"), 200, &gotReq)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

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
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestFetch_parse(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"null resets_at skipped", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2026-05-26T22:00:00+00:00"},"seven_day_omelette":{"utilization":50.0,"resets_at":null}}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, _ := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if _, ok := out.Accounts[0].Limits["seven_day_omelette"]; ok {
				t.Error("seven_day_omelette should be excluded when resets_at is null")
			}
		}},
		{"array-valued sibling field tolerated", func(t *testing.T) {
			// Anthropic added a top-level field whose value is a JSON array (a
			// per-app usage breakdown). Decoding the whole body into
			// map[string]*window used to fail the entire response with
			// "cannot unmarshal array into ...window", hiding the five_hour /
			// seven_day windows we actually surface. Only the closed set of keys
			// must be decoded; siblings of any shape are ignored.
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T10:00:00+00:00"},"seven_day":{"utilization":60.0,"resets_at":"2027-01-02T06:00:00+00:00"},"seven_day_oauth_apps":null,"oauth_app_usage":[{"name":"app","utilization":5.0}]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("array-valued sibling field must not fail the fetch: %v", err)
			}
			if len(out.Accounts) == 0 {
				t.Fatal("expected an account row")
			}
			if e := out.Accounts[0].Error; e != "" {
				t.Fatalf("array-valued sibling field must not produce a per-account error: %q", e)
			}
			limits := out.Accounts[0].Limits
			if _, ok := limits["five_hour"]; !ok {
				t.Error("five_hour must still be parsed despite the array-valued sibling field")
			}
			if _, ok := limits["seven_day"]; !ok {
				t.Error("seven_day must still be parsed despite the array-valued sibling field")
			}
			if got := limits["five_hour"].UsedPercent; got != 10.0 {
				t.Errorf("five_hour used_percent = %v, want 10.0", got)
			}
		}},
		{"limits array: session and weekly_all skipped, only weekly_scoped surfaces", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":[` +
				`{"kind":"session","percent":62,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":{"display_name":"SessionModel"}}},` +
				`{"kind":"weekly_all","percent":33,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":{"display_name":"WeeklyAllModel"}}},` +
				`{"kind":"weekly_scoped","percent":44,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":{"display_name":"Testmodel"}}}` +
				`]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			limits := out.Accounts[0].Limits
			if got := limits["seven_day_testmodel"].UsedPercent; got != 44.0 {
				t.Errorf("seven_day_testmodel used_percent = %v, want 44.0", got)
			}
			if _, ok := limits["seven_day_sessionmodel"]; ok {
				t.Error("session kind must not surface a window")
			}
			if _, ok := limits["seven_day_weeklyallmodel"]; ok {
				t.Error("weekly_all kind must not surface a window")
			}
		}},
		{"limits array: display name with space becomes underscore-joined key", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":[` +
				`{"kind":"weekly_scoped","percent":20,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":{"display_name":"Fable Turbo"}}}` +
				`]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := out.Accounts[0].Limits["seven_day_fable_turbo"]; !ok {
				t.Error("expected seven_day_fable_turbo window from display_name \"Fable Turbo\"")
			}
		}},
		{"limits array: punctuation-heavy display name slugged", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":[` +
				`{"kind":"weekly_scoped","percent":25,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":{"display_name":"--Fable 2.0 (Preview)--"}}}` +
				`]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			limits := out.Accounts[0].Limits
			if _, ok := limits["seven_day_fable_2_0_preview"]; !ok {
				t.Errorf("expected seven_day_fable_2_0_preview from display_name \"--Fable 2.0 (Preview)--\", got: %v", keys(limits))
			}
		}},
		{"limits array: same-loop duplicate slugs keep the first entry", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":[` +
				`{"kind":"weekly_scoped","percent":40,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":{"display_name":"Fable"}}},` +
				`{"kind":"weekly_scoped","percent":80,"resets_at":"2027-01-02T00:00:00+00:00","scope":{"model":{"display_name":"fable"}}}` +
				`]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if e := out.Accounts[0].Error; e != "" {
				t.Fatalf("same-loop slug collision must not produce a per-account error: %q", e)
			}
			limits := out.Accounts[0].Limits
			if len(limits) != 2 {
				t.Fatalf("expected exactly five_hour + one seven_day_fable, got %d: %v", len(limits), keys(limits))
			}
			if got := limits["seven_day_fable"].UsedPercent; got != 40.0 {
				t.Errorf("seven_day_fable used_percent = %v, want 40.0 (first entry wins)", got)
			}
		}},
		{"limits array: weekly_scoped with null resets_at skipped", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":[` +
				`{"kind":"weekly_scoped","percent":20,"resets_at":null,"scope":{"model":{"display_name":"NullReset"}}}` +
				`]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := out.Accounts[0].Limits["seven_day_nullreset"]; ok {
				t.Error("weekly_scoped with null resets_at should be skipped")
			}
		}},
		{"limits array: weekly_scoped with null scope, null model, or empty display_name skipped", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":[` +
				`{"kind":"weekly_scoped","percent":20,"resets_at":"2027-01-01T00:00:00+00:00","scope":null},` +
				`{"kind":"weekly_scoped","percent":21,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":null}},` +
				`{"kind":"weekly_scoped","percent":22,"resets_at":"2027-01-01T00:00:00+00:00","scope":{"model":{"display_name":""}}}` +
				`]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			limits := out.Accounts[0].Limits
			if len(limits) != 1 {
				t.Fatalf("expected only five_hour to survive, got %d: %v", len(limits), keys(limits))
			}
			if _, ok := limits["five_hour"]; !ok {
				t.Error("five_hour must still be present")
			}
		}},
		{"limits array: top-level seven_day_sonnet wins over weekly_scoped collision", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},` +
				`"seven_day_sonnet":{"utilization":15.0,"resets_at":"2027-01-02T00:00:00+00:00"},` +
				`"limits":[{"kind":"weekly_scoped","percent":90,"resets_at":"2027-01-03T00:00:00+00:00","scope":{"model":{"display_name":"Sonnet"}}}]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := out.Accounts[0].Limits["seven_day_sonnet"].UsedPercent; got != 15.0 {
				t.Errorf("seven_day_sonnet used_percent = %v, want 15.0 (top-level value must win)", got)
			}
		}},
		{"limits array: malformed (object instead of array) tolerated", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":{"foo":"bar"}}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("malformed limits array must not fail the fetch: %v", err)
			}
			if e := out.Accounts[0].Error; e != "" {
				t.Fatalf("malformed limits array must not produce a per-account error: %q", e)
			}
			if _, ok := out.Accounts[0].Limits["five_hour"]; !ok {
				t.Error("five_hour must still be parsed despite the malformed limits array")
			}
		}},
		{"limits array: unparseable resets_at in one weekly_scoped entry skips only that entry", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"2027-01-01T00:00:00+00:00"},"limits":[` +
				`{"kind":"weekly_scoped","percent":20,"resets_at":"not-a-timestamp","scope":{"model":{"display_name":"Bad"}}},` +
				`{"kind":"weekly_scoped","percent":30,"resets_at":"2027-01-05T00:00:00+00:00","scope":{"model":{"display_name":"Good"}}}` +
				`]}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if err != nil {
				t.Fatalf("unparseable resets_at in a scoped entry must not fail the fetch: %v", err)
			}
			if e := out.Accounts[0].Error; e != "" {
				t.Fatalf("unparseable resets_at in a scoped entry must not produce a per-account error: %q", e)
			}
			limits := out.Accounts[0].Limits
			if _, ok := limits["seven_day_bad"]; ok {
				t.Error("entry with unparseable resets_at must be skipped")
			}
			if _, ok := limits["seven_day_good"]; !ok {
				t.Error("other valid entries must still surface")
			}
			if _, ok := limits["five_hour"]; !ok {
				t.Error("top-level windows must be unaffected")
			}
		}},
		{"bad resets_at", func(t *testing.T) {
			body := []byte(`{"five_hour":{"utilization":10.0,"resets_at":"yesterday"}}`)
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, body, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

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
		}},
		{"non-JSON 200", func(t *testing.T) {
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, []byte("<html>oops</html>"), 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			out, _ := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			if len(out.Accounts) == 0 {
				t.Fatal("expected account rows even on per-account error")
			}
			if !strings.Contains(out.Accounts[0].Error, "non-JSON") {
				t.Errorf("per-account error should mention non-JSON: %q", out.Accounts[0].Error)
			}
		}},
		{"418 bare error", func(t *testing.T) {
			// A 418 from the usage endpoint becomes a per-account error (not a
			// classified transient/auth-denied) and the provider does NOT surface a
			// provider-level ErrTransient for a single-account non-transient failure.
			live := makeCred("tok-live", "ref", 0)
			store := testutil.MemStore(t, makeAccount("u1", "a@b.com", "tok-live", "ref", 0))

			usageSrv := testutil.NewStubServer(t, []byte("teapot"), 418, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

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
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestFetch_auth(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"token missing", func(t *testing.T) {
			// nil liveCred + empty store → ErrAuthMissing
			usageSrv := testutil.NewStubServer(t, []byte(`{}`), 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil).Fetch(context.Background())
			testutil.WantErrIs(t, err, providers.ErrAuthMissing)
			if !strings.Contains(err.Error(), cred.ClaudeTokenMissingMessage) {
				t.Errorf("expected token-missing message, got: %v", err)
			}
		}},
		{"token generic error propagated", func(t *testing.T) {
			sentinel := errors.New("some keychain failure")
			usageSrv := testutil.NewStubServer(t, []byte(`{}`), 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil)
			c.readCredential = func(context.Context) (cred.Credential, error) {
				return cred.Credential{}, sentinel
			}

			_, err := c.Fetch(context.Background())
			testutil.WantErrIs(t, err, sentinel)
			if errors.Is(err, providers.ErrAuthMissing) {
				t.Errorf("generic err should not be classified ErrAuthMissing")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── new multi-account tests ──────────────────────────────────────────────────

func TestFetch_multi_account(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"two accounts both succeed", func(t *testing.T) {
			// active byte-match + one non-active stored. out.Accounts has 2 rows with active first.
			live := makeCred("tok-a", "ref-a", 0) // byte-matches acctA → acctA is active
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0),
				makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0),
			)
			out, err := runFetch(t, fetchOpts{live: live, store: store})
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 2)
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
		}},
		{"two accounts email ordering", func(t *testing.T) {
			// active first, then ASCII ascending by email.
			// Two non-active stored accounts; live absent so neither is active.
			store := testutil.MemStore(t,
				makeAccount("uuid-z", "zeta@example.com", "tok-z", "ref-z", 0),
				makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0),
			)
			out, err := runFetch(t, fetchOpts{store: store})
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 2)
			if out.Accounts[0].Email >= out.Accounts[1].Email {
				t.Errorf("accounts not sorted by email: %q >= %q", out.Accounts[0].Email, out.Accounts[1].Email)
			}
		}},
		{"stored refresh rejected", func(t *testing.T) {
			// Account B's refresh token is invalid →
			// per-account error, account A succeeds → provider returns success (not ErrTransient).
			// ExpiresAt within refreshSkew triggers refresh.
			nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
			farExpiry := testNow.Add(1 * time.Hour).UnixMilli()

			live := makeCred("tok-a", "ref-a", farExpiry) // byte-matches acctA
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", farExpiry),
				makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", nearExpiry),
			)

			// Refresh server returns invalid_grant for acctB's refresh attempt.
			refreshSrv := testutil.NewStubServer(t, []byte(`{"error":"invalid_grant"}`), 400, nil)

			out, err := runFetch(t, fetchOpts{
				refresh: refreshSrv,
				live:    live,
				store:   store,
				now:     func() time.Time { return testNow },
			})
			if err != nil {
				t.Fatalf("provider should succeed (acctA ok), got: %v", err)
			}
			wantAccounts(t, out, 2)
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
		}},
		{"all transient", func(t *testing.T) {
			// Both accounts' usage fetch returns 503 → ErrTransient.
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0),
				makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0),
			)
			usageSrv := testutil.NewStubServer(t, []byte(`{"error":"service unavailable"}`), 503, nil)

			// live absent → both accounts non-active
			out, err := runFetch(t, fetchOpts{usage: usageSrv, store: store})
			testutil.WantErrIs(t, err, providers.ErrTransient)
			if len(out.Accounts) != 2 {
				t.Fatalf("partial accounts should still be returned, got %d", len(out.Accounts))
			}
			for _, ar := range out.Accounts {
				if ar.Error == "" {
					t.Errorf("account %q should have per-account error", ar.Email)
				}
			}
		}},
		{"mixed transient and auth denied", func(t *testing.T) {
			// One transient + one auth-denied, zero succeeded →
			// provider still returns ErrTransient (D8 retry rule).
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0),
				makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0),
			)
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

			_, err := runFetch(t, fetchOpts{usage: usageSrv, store: store})
			testutil.WantErrIs(t, err, providers.ErrTransient)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestFetch_live(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"live present profile fails", func(t *testing.T) {
			// Profile call fails → fallback row with "(live Claude account)", CaptureWarn emitted.
			live := makeCred("tok-live", "ref-live", 0)
			// empty store → no byte-match → profile call needed

			profileSrv := testutil.NewStubServer(t, []byte(`{"error":"unavailable"}`), 503, nil)
			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)
			refreshSrv := testutil.RejectServer(t, "refresh")

			var warnBuf bytes.Buffer
			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, nil, &warnBuf, nil).Fetch(context.Background())
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 1)
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

			wantWarn(t, &warnBuf, "could not capture live account profile")
			wantWarn(t, &warnBuf, "claude /login")
		}},
		{"profile missing fields", func(t *testing.T) {
			// Profile 200 with missing account.uuid → stricter diagnostic warn.
			live := makeCred("tok-live", "ref-live", 0)

			// 200 response missing uuid.
			profileSrv := testutil.NewStubServer(t, []byte(`{"account":{"uuid":"","email":"user@example.com"}}`), 200, nil)
			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)
			refreshSrv := testutil.RejectServer(t, "refresh")

			var warnBuf bytes.Buffer
			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, nil, &warnBuf, nil).Fetch(context.Background())
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 1)
			if out.Accounts[0].Email != "(live Claude account)" {
				t.Errorf("Email = %q, want fallback email", out.Accounts[0].Email)
			}

			wantWarn(t, &warnBuf, "missing required fields")
			if strings.Contains(warnBuf.String(), "claude /login") {
				t.Errorf("stricter diagnostic must not contain 'claude /login', got: %q", warnBuf.String())
			}
		}},
		{"live absent zero stored", func(t *testing.T) {
			// Live absent + no stored accounts → ErrAuthMissing.
			_, err := runFetch(t, fetchOpts{})
			testutil.WantErrIs(t, err, providers.ErrAuthMissing)
		}},
		{"live absent stored present", func(t *testing.T) {
			// No live credential, stored accounts present
			// → all rows non-active, no ErrAuthMissing.
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "a@b.com", "tok-a", "ref-a", 0),
				makeAccount("uuid-b", "b@c.com", "tok-b", "ref-b", 0),
			)
			out, err := runFetch(t, fetchOpts{store: store})
			testutil.WantNoErr(t, err)
			wantAccounts(t, out, 2)
			for _, ar := range out.Accounts {
				if ar.Active {
					t.Errorf("account %q should not be active (no live credential)", ar.Email)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── FetchForSwitch tests ─────────────────────────────────────────────────────

func TestFetchForSwitch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"happy path", func(t *testing.T) {
			// Stored access token valid, usage 200, AccountResult populated,
			// store unmodified, refresh server receives zero requests.
			live := makeCred("tok-active", "ref-active", 0) // byte-matches activeAcct
			store := testutil.MemStore(t,
				makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0),
				makeAccount("uuid-other", "other@example.com", "tok-other", "ref-other", 0),
			)

			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile") // byte-match → no profile call
			refreshSrv, refreshCount := testutil.CountingServer(t, 200, refreshSuccessBody("at2", "rt2"))

			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
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
		}},
		{"stored token rejected", func(t *testing.T) {
			// Usage 401 → account excluded from returned slice, per-account warn
			// emitted, refresh never called, store unchanged.
			live := makeCred("tok-active", "ref-active", 0)
			store := testutil.MemStore(t,
				makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0),
				makeAccount("uuid-bad", "bad@example.com", "tok-bad", "ref-bad", 0),
			)

			usageSrv := testutil.NewStubServer(t, []byte(`{"error":"unauthorized"}`), 401, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv, refreshCount := testutil.CountingServer(t, 200, refreshSuccessBody("at2", "rt2"))

			var warnBuf bytes.Buffer
			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, &warnBuf, nil).FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)

			if len(out) != 0 {
				t.Errorf("rejected account should be excluded, got %d results", len(out))
			}

			wantWarn(t, &warnBuf, "stored credential rejected")
			wantWarn(t, &warnBuf, "excluded from auto-pick")

			// Store unchanged.
			uuids := storeUUIDSet(t, store)
			if !uuids["uuid-active"] || !uuids["uuid-bad"] {
				t.Errorf("store changed after FetchForSwitch: %v", uuids)
			}

			if n := refreshCount.Load(); n != 0 {
				t.Errorf("refresh server received %d requests, expected 0", n)
			}
		}},
		{"transient exclusion", func(t *testing.T) {
			// Usage 503 → account excluded with "usage fetch failed" warn.
			// No live credential: the stored account is non-active, gets a usage fetch,
			// returns 503 → excluded from the result set with a warn.
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

			usageSrv := testutil.NewStubServer(t, []byte(`{"error":"unavailable"}`), 503, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			var warnBuf bytes.Buffer
			out, err := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, store, &warnBuf, nil).FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			if len(out) != 0 {
				t.Errorf("transient account should be excluded, got %d results", len(out))
			}
			wantWarn(t, &warnBuf, "usage fetch failed")
			wantWarn(t, &warnBuf, "excluded from auto-pick")
		}},
		{"never refreshes never mutates store", func(t *testing.T) {
			// FetchForSwitch does not touch the refresh server and does not mutate
			// the store even when the account has a near-expiry token.
			nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
			live := makeCred("tok-active", "ref-active", 0) // byte-matches active
			store := testutil.MemStore(t,
				makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0),
				makeAccount("uuid-other", "other@example.com", "tok-other", "ref-other", nearExpiry),
			)

			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv, refreshCount := testutil.CountingServer(t, 200, refreshSuccessBody("tok-other2", "ref-other2"))

			_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
				nil, func() time.Time { return testNow }).FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)

			// Refresh must not have been called.
			if n := refreshCount.Load(); n != 0 {
				t.Errorf("refresh called %d times, expected 0", n)
			}

			// Store must still have the original RT (not ref-other2).
			stored, _ := store.List(context.Background())
			for _, s := range stored {
				if s.UUID == "uuid-other" {
					if got := StoredRefreshToken(s); got != "ref-other" {
						t.Errorf("store RefreshToken = %q, want ref-other (store must be unchanged)", got)
					}
				}
			}
		}},
		{"uses cache hit", func(t *testing.T) {
			// Pre-populate cache for a non-active account; FetchForSwitch reads from
			// the cache, not the live endpoint. This pins the unified-code-path
			// contract (D7, revised): switch and reporting share fetchLimitsCached so
			// a recent aistat usage call's entries are reused here.
			live := makeCred("tok-active", "ref-active", 0)
			store := testutil.MemStore(t,
				makeAccount("uuid-active", "active@example.com", "tok-active", "ref-active", 0),
				makeAccount("uuid-other", "other@example.com", "tok-other", "ref-other", 0),
			)

			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)

			// Pre-populate cache for the non-active account.
			c.cache.Put("uuid-other", map[string]providers.Limit{
				"five_hour": {UsedPercent: 10, RemainingPercent: 90, ResetsAt: testNow.Add(time.Hour)},
			})

			results, err := c.FetchForSwitch(context.Background())
			testutil.WantNoErr(t, err)
			if len(results) != 1 {
				t.Fatalf("FetchForSwitch returned %d results, want 1", len(results))
			}
			if n := usageCount.Load(); n != 0 {
				t.Errorf("usage server requests = %d, want 0 (FetchForSwitch should serve from cache)", n)
			}
			if got := results[0].Limits["five_hour"].UsedPercent; got != 10 {
				t.Errorf("five_hour.UsedPercent = %v, want 10 (from cached entry)", got)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── shared reconcile-persist tests ───────────────────────────────────────────

func TestFetch_persist(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"rotated tokens persisted", func(t *testing.T) {
			// An account with a near-expiry token is refreshed, and the store ends
			// up with the rotated AT2/RT2 blob.
			nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
			live := makeCred("tok-a", "ref-a", nearExpiry) // byte-matches
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry))

			refreshSrv := testutil.NewStubServer(t, refreshSuccessBody("tok-a2", "ref-a2"), 200, nil)
			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")

			_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
				nil, func() time.Time { return testNow }).Fetch(context.Background())
			testutil.WantNoErr(t, err)

			// Verify the store now has AT2/RT2 in the blob.
			stored, _ := store.List(context.Background())
			if len(stored) != 1 {
				t.Fatalf("store should have 1 account, got %d", len(stored))
			}
			if got := StoredAccessToken(stored[0]); got != "tok-a2" {
				t.Errorf("stored AccessToken = %q, want tok-a2", got)
			}
			if got := StoredRefreshToken(stored[0]); got != "ref-a2" {
				t.Errorf("stored RefreshToken = %q, want ref-a2", got)
			}
		}},
		{"rotate expires_at zero", func(t *testing.T) {
			// When the refresh response omits expires_in, rotateRawBlob must write
			// expiresAt=0 to the blob — not preserve the stale pre-rotation
			// timestamp — so the skew guard does not re-trigger on the next run.
			nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
			live := makeCred("tok-a", "ref-a", nearExpiry)
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry))

			// Refresh response deliberately omits expires_in.
			noExpiryBody, _ := json.Marshal(map[string]any{
				"access_token":  "tok-a2",
				"refresh_token": "ref-a2",
			})
			refreshSrv := testutil.NewStubServer(t, noExpiryBody, 200, nil)
			usageSrv := testutil.NewStubServer(t, minUsageBody, 200, nil)
			profileSrv := testutil.RejectServer(t, "profile")

			_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
				nil, func() time.Time { return testNow }).Fetch(context.Background())
			testutil.WantNoErr(t, err)

			stored, _ := store.List(context.Background())
			if len(stored) != 1 {
				t.Fatalf("store should have 1 account, got %d", len(stored))
			}
			if got := StoredAccessToken(stored[0]); got != "tok-a2" {
				t.Errorf("stored AccessToken = %q, want tok-a2", got)
			}
			// expiresAt must be 0, not the old nearExpiry value.
			if got := StoredExpiresAt(stored[0]); got != 0 {
				t.Errorf("stored ExpiresAt = %d, want 0 (no expiry from server)", got)
			}
		}},
		{"reconcile upsert before usage fetches", func(t *testing.T) {
			// Verifies the store is updated (step 5 persist) before any usage fetch
			// by checking the store state on the first usage request using an
			// intercepting handler.
			// New account (no byte-match) → profile inserts → upsert before usage fetch.
			live := makeCred("tok-live", "ref-live", 0)
			store := accounts.NewMemoryStore()

			profBody := profileBody("uuid-new", "new@example.com", "default_claude_max_5x")
			profileSrv := testutil.NewStubServer(t, profBody, 200, nil)

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

			refreshSrv := testutil.RejectServer(t, "refresh")

			_, err := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil).Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if !upsertedBeforeUsage {
				t.Error("account should be upserted (step 5) before the first usage fetch")
			}
		}},
		{"provider limits from active account", func(t *testing.T) {
			// Provider-level Limits is the active account's limits (or nil when
			// active account errored).
			live := makeCred("tok-a", "ref-a", 0) // active
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

			// Usage returns an error → active account has no limits → provider Limits = nil.
			usageSrv := testutil.NewStubServer(t, []byte(`{"error":"down"}`), 503, nil)

			out, _ := runFetch(t, fetchOpts{usage: usageSrv, live: live, store: store})
			if out.Limits != nil {
				t.Errorf("Limits should be nil when active account errored, got %v", out.Limits)
			}
		}},
		{"three accounts mixed for ErrTransient rule", func(t *testing.T) {
			// 3 accounts, 2 transient + 1 auth-denied, zero succeeded → ErrTransient.
			// Pins the D8 retry trigger.
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0),
				makeAccount("uuid-b", "b@example.com", "tok-b", "ref-b", 0),
				makeAccount("uuid-c", "c@example.com", "tok-c", "ref-c", 0),
			)
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

			_, err := runFetch(t, fetchOpts{usage: usageSrv, store: store})
			testutil.WantErrIs(t, err, providers.ErrTransient)
		}},
		{"refresh transient ErrTransient", func(t *testing.T) {
			// Single account with near-expiry token, refresh server returns 503 →
			// refresh fails transient → successCount==0, transientCount>0 →
			// provider returns ErrTransient.
			nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
			live := makeCred("tok-a", "ref-a", nearExpiry) // byte-matches
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry))

			refreshSrv := testutil.NewStubServer(t, []byte(`{"error":"service unavailable"}`), 503, nil)
			usageSrv := testutil.RejectServer(t, "usage") // usage must not be reached after refresh fails

			_, err := runFetch(t, fetchOpts{
				usage:   usageSrv,
				refresh: refreshSrv,
				live:    live,
				store:   store,
				now:     func() time.Time { return testNow },
			})
			testutil.WantErrIs(t, err, providers.ErrTransient)
		}},
		{"live unstored usage fetch fails", func(t *testing.T) {
			// Live credential present, empty store, profile call returns 503 →
			// LiveUnstored set, then usage fetch also returns 503 →
			// transientCount > 0, successCount == 0 → provider returns ErrTransient.
			live := makeCred("tok-live", "ref-live", 0)

			profileSrv := testutil.NewStubServer(t, []byte(`{"error":"unavailable"}`), 503, nil)
			usageSrv := testutil.NewStubServer(t, []byte(`{"error":"unavailable"}`), 503, nil)

			var warnBuf bytes.Buffer
			_, err := runFetch(t, fetchOpts{
				usage:   usageSrv,
				profile: profileSrv,
				live:    live,
				warn:    &warnBuf,
			})
			testutil.WantErrIs(t, err, providers.ErrTransient)
			wantWarn(t, &warnBuf, "live row without storing")
		}},
		{"auth denied only nil error", func(t *testing.T) {
			// Single stored account, usage returns 401 (auth-denied), no transient
			// failures → successCount==0, transientCount==0 → provider returns nil
			// error with per-account error populated. Pins the D8 contract that
			// ErrAuthDenied is never returned at the provider level from Fetch.
			live := makeCred("tok-a", "ref-a", 0)
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

			usageSrv := testutil.NewStubServer(t, []byte(`{"error":"unauthorized"}`), 401, nil)

			out, err := runFetch(t, fetchOpts{usage: usageSrv, live: live, store: store})
			if err != nil {
				t.Errorf("Fetch must return nil error for auth-denied only (D8); got: %v", err)
			}
			wantAccounts(t, out, 1)
			if out.Accounts[0].Error == "" {
				t.Error("per-account error must be set for auth-denied account")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── cache integration tests ──────────────────────────────────────────────────

func TestFetch_cache(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"two accounts cache hit", func(t *testing.T) {
			// Pre-populate cache for acct A; run Fetch; assert the usage server
			// saw a request only for acct B.
			live := makeCred("tok-a", "ref-a", 0) // byte-matches acctA
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0),
				makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0),
			)

			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)

			// Pre-populate cache for acctA — only acctB should fire a fresh request.
			c.cache.Put("uuid-a", map[string]providers.Limit{
				"five_hour": {UsedPercent: 30, RemainingPercent: 70, ResetsAt: testNow.Add(time.Hour), ResetAfterSeconds: 3600},
			})

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if n := usageCount.Load(); n != 1 {
				t.Errorf("usage server requests = %d, want 1 (only acctB should fire)", n)
			}
			wantAccounts(t, out, 2)
			// Active account (acctA) should have limits from the cache (no error).
			for _, ar := range out.Accounts {
				if ar.UUID == "uuid-a" && ar.Error != "" {
					t.Errorf("acctA (cache hit) should have no error, got: %q", ar.Error)
				}
			}
		}},
		{"two accounts cache expired both fire", func(t *testing.T) {
			// Pre-populate cache with entries older than TTL; assert the usage
			// server saw requests for both accounts.
			t.Setenv("AISTAT_USAGE_CACHE_TTL", "1s")

			now := testNow
			nowFn := func() time.Time { return now }
			live := makeCred("tok-a", "ref-a", 0)
			store := testutil.MemStore(t,
				makeAccount("uuid-a", "alpha@example.com", "tok-a", "ref-a", 0),
				makeAccount("uuid-b", "beta@example.com", "tok-b", "ref-b", 0),
			)

			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nowFn)

			c.cache.Put("uuid-a", map[string]providers.Limit{"five_hour": {ResetsAt: now.Add(time.Hour)}})
			c.cache.Put("uuid-b", map[string]providers.Limit{"five_hour": {ResetsAt: now.Add(time.Hour)}})

			// Advance clock past the 1 s TTL.
			now = now.Add(2 * time.Second)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if n := usageCount.Load(); n != 2 {
				t.Errorf("usage server requests = %d, want 2 (both entries expired)", n)
			}
			wantAccounts(t, out, 2)
		}},
		{"cache hit recomputes reset after", func(t *testing.T) {
			// Cached Limit.ResetsAt is absolute; ResetAfterSeconds is recomputed
			// from the current clock on every cache hit.
			// Use a 90 s TTL so a 40 s clock advance does not expire the entry.
			t.Setenv("AISTAT_USAGE_CACHE_TTL", "90s")

			now := testNow
			nowFn := func() time.Time { return now }
			live := makeCred("tok-a", "ref-a", 0)
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nowFn)

			// Put at T with ResetsAt = T + 60s; ResetAfterSeconds stored as 60.
			resetsAt := now.Add(60 * time.Second)
			c.cache.Put("uuid-a", map[string]providers.Limit{
				"five_hour": {UsedPercent: 50, RemainingPercent: 50, ResetsAt: resetsAt, ResetAfterSeconds: 60},
			})

			// Advance clock to T + 40s; expect ResetAfterSeconds = 20 on cache hit.
			now = now.Add(40 * time.Second)

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if usageCount.Load() != 0 {
				t.Errorf("usage server should not be hit on cache hit, got %d requests", usageCount.Load())
			}
			wantAccounts(t, out, 1)
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
		}},
		{"live unstored bypasses cache", func(t *testing.T) {
			// The live-unstored fallback path (no UUID) always calls
			// fetchLimitsFresh regardless of cache state.
			live := makeCred("tok-live", "ref-live", 0)
			// Empty store → no byte-match → profile call needed.

			// Profile returns 503 → LiveUnstored branch (no UUID available).
			profileSrv := testutil.NewStubServer(t, []byte(`{"error":"unavailable"}`), 503, nil)
			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			refreshSrv := testutil.RejectServer(t, "refresh")

			var warnBuf bytes.Buffer
			c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, nil, &warnBuf, nil)

			// Pre-populate cache for some UUID — fallback path has no UUID and should
			// bypass the cache entirely.
			c.cache.Put("some-uuid", map[string]providers.Limit{"five_hour": {ResetAfterSeconds: 999}})

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			if usageCount.Load() != 1 {
				t.Errorf("usage server requests = %d, want 1 (fresh fetch required, no UUID)", usageCount.Load())
			}
			if len(out.Accounts) != 1 || out.Accounts[0].Email != "(live Claude account)" {
				t.Errorf("expected fallback row, got: %+v", out.Accounts)
			}
		}},
		{"rotated token shares cache entry", func(t *testing.T) {
			// After a token rotation, fetchLimitsCached still keys on UUID
			// (unchanged), so a pre-populated cache entry produces a hit without
			// any usage server requests.
			nearExpiry := testNow.Add(5 * time.Second).UnixMilli()
			live := makeCred("tok-a", "ref-a", nearExpiry) // byte-matches
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", nearExpiry))

			refreshSrv := testutil.NewStubServer(t, refreshSuccessBody("tok-a2", "ref-a2"), 200, nil)
			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store,
				nil, func() time.Time { return testNow })

			// Pre-populate cache for uuid-a with original limits.
			originalResetsAt := testNow.Add(time.Hour)
			c.cache.Put("uuid-a", map[string]providers.Limit{
				"five_hour": {UsedPercent: 20, RemainingPercent: 80, ResetsAt: originalResetsAt, ResetAfterSeconds: 3600},
			})

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
			// Token was rotated (refresh fired) but UUID is unchanged → cache hit.
			if n := usageCount.Load(); n != 0 {
				t.Errorf("usage server requests = %d, want 0 (cache hit despite token rotation)", n)
			}
			wantAccounts(t, out, 1)
			if out.Accounts[0].Error != "" {
				t.Errorf("account should have no error on cache hit, got: %q", out.Accounts[0].Error)
			}
		}},
		{"cache miss 429 honors retry-after within budget", func(t *testing.T) {
			// A cache miss triggers a live fetch; the first response is 429
			// Retry-After: 1; the second succeeds. The account should still
			// produce limits within the 3 s per-account budget.
			live := makeCred("tok-a", "ref-a", 0)
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

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

			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

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
		}},
		{"cache miss repeated retry-after exceeds budget", func(t *testing.T) {
			// When the server returns 429 Retry-After: 2 on every attempt, and the
			// pool budget (baseTimeout=0, perAccountBudget=3 s) means the second
			// sleep would exceed the deadline, sleepWithCtx skips the second sleep
			// → exactly 2 attempts → ErrTransient.
			live := makeCred("tok-a", "ref-a", 0)
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

			// Redirect cache to an isolated dir before constructing the cache.
			testenv.RedirectHome(t, t.TempDir())

			nowFn := func() time.Time { return testNow }

			var callCount atomic.Int32
			usageSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				callCount.Add(1)
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(429)
				_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			}))
			t.Cleanup(usageSrv.Close)

			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

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
				baseTimeout:      0,               // no base buffer: pool = just perAccountBudget
				perAccountBudget: 3 * time.Second, // 0+3=3 s pool; 2nd Retry-After:2 sleep (0+2+2=4 s) exceeds it
				cache:            usagecache.New("claude", nowFn, func(string) {}),
			}

			start := time.Now()
			out, err := c.Fetch(context.Background())
			elapsed := time.Since(start)

			testutil.WantErrIs(t, err, providers.ErrTransient)
			if n := callCount.Load(); n != 2 {
				t.Errorf("usage server calls = %d, want 2 (second sleep skipped by budget check)", n)
			}
			if elapsed > 4*time.Second {
				t.Errorf("test took %v, want < 4s (should not sleep a second 2s delay)", elapsed)
			}
			if len(out.Accounts) == 0 || out.Accounts[0].Error == "" {
				t.Error("account should carry per-account ErrTransient error")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestFetchUsage_cache(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"uses cache hit", func(t *testing.T) {
			// Pre-populate cache for a UUID; FetchUsage with that UUID reads from
			// cache, not the live endpoint. Unified-code-path contract for the
			// switch active-account read.
			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil)

			// Pre-populate cache for some UUID.
			c.cache.Put("uuid-x", map[string]providers.Limit{
				"five_hour": {UsedPercent: 5, RemainingPercent: 95, ResetsAt: testNow.Add(time.Hour)},
			})

			limits, err := c.FetchUsage(context.Background(), "tok-test", "uuid-x")
			testutil.WantNoErr(t, err)
			if n := usageCount.Load(); n != 0 {
				t.Errorf("usage server requests = %d, want 0 (FetchUsage should serve from cache)", n)
			}
			if got := limits["five_hour"].UsedPercent; got != 5 {
				t.Errorf("five_hour.UsedPercent = %v, want 5 (from cached entry)", got)
			}
		}},
		{"empty UUID bypasses cache", func(t *testing.T) {
			// When uuid is empty (e.g. an unstored live credential), FetchUsage
			// falls through to a fresh fetch with no cache interaction.
			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, nil, nil, nil, nil)

			_, err := c.FetchUsage(context.Background(), "tok-test", "")
			testutil.WantNoErr(t, err)
			if n := usageCount.Load(); n != 1 {
				t.Errorf("usage server requests = %d, want 1 (empty uuid forces fresh fetch)", n)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"with cache bypass", func(t *testing.T) {
			// WithCacheBypass(true) skips the read path (usage server IS hit) but
			// still writes through on success (D8 contract).
			// Verify WithCacheBypass actually wires the field through New.
			testenv.RedirectHome(t, t.TempDir())
			optClient := New(nil, "aistat-test/0", WithCacheBypass(true))
			if !optClient.cacheBypass {
				t.Fatal("WithCacheBypass(true) did not set cacheBypass on Client")
			}

			live := makeCred("tok-a", "ref-a", 0)
			store := testutil.MemStore(t, makeAccount("uuid-a", "a@example.com", "tok-a", "ref-a", 0))

			usageSrv, usageCount := testutil.CountingServer(t, 200, minUsageBody)
			profileSrv := testutil.RejectServer(t, "profile")
			refreshSrv := testutil.RejectServer(t, "refresh")

			c := buildClient(t, usageSrv, profileSrv, refreshSrv, live, store, nil, nil)

			// Pre-populate cache for uuid-a.
			c.cache.Put("uuid-a", map[string]providers.Limit{
				"five_hour": {UsedPercent: 99, RemainingPercent: 1, ResetsAt: testNow.Add(time.Hour)},
			})

			// Enable bypass: read path should be skipped even though cache has an entry.
			c.cacheBypass = true

			out, err := c.Fetch(context.Background())
			testutil.WantNoErr(t, err)
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
		}},
		{"passes user agent to doer", func(t *testing.T) {
			// Guards against a regression where claude.New drops or alters its
			// userAgent arg. Combined with httpx_test.go's wire-level checks on
			// Doer.UserAgent, this proves DefaultUserAgent reaches the wire.
			c := New(nil, "claude-code/1.2.3")
			if c.doer.UserAgent != "claude-code/1.2.3" {
				t.Errorf("doer.UserAgent = %q, want %q", c.doer.UserAgent, "claude-code/1.2.3")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
