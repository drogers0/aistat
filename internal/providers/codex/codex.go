package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/httpx"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/providers/multiaccount"
	"github.com/drogers0/aistat/v2/internal/providers/usagecache"
)

const (
	// Undocumented internal endpoint shared with the ChatGPT web app. Shape
	// may change without notice.
	endpoint    = "https://chatgpt.com/backend-api/wham/usage"
	credTimeout = 10 * time.Second

	// baseTimeout is the minimum time budget covering the live-credential read,
	// reconciliation, and at least one account's usage fetch.
	baseTimeout = 10 * time.Second

	// perAccountBudget is added per account returned by Reconcile. Sized to
	// permit one max-length Retry-After: 10 sleep plus the actual fetch on
	// attempts 1 + 2 before sleepWithCtx's deadline check short-circuits.
	perAccountBudget = 15 * time.Second

	// refreshSkew is the safety margin before ExpiresAt at which a stored
	// token is considered near-expiry and proactively refreshed.
	refreshSkew = 30 * time.Second

	// msgTokensRevoked and msgStaleRefresh are tightened, actionable phrases
	// for the two well-known upstream OAuth failure modes.
	msgTokensRevoked = "tokens revoked by upstream (likely a `codex login` for another account); run `codex login` to recover"
	msgStaleRefresh  = "stale refresh token (codex CLI rotated it); retry or run `codex login` to recover"
)

// isRevokedTokenErr reports whether err is an upstream "this token is dead,
// re-login to recover" signal from the chatgpt.com usage endpoint. OpenAI has
// returned two interchangeable codes for the same condition (a token invalidated
// because another account logged in on the same client): "token_revoked" and
// "token_invalidated". Both map to msgTokensRevoked. The ErrAuthDenied guard
// scopes this to the usage endpoint; the refresh endpoint's failures are handled
// separately in refreshErrorMessage.
func isRevokedTokenErr(err error) bool {
	if !errors.Is(err, providers.ErrAuthDenied) {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "token_revoked") || strings.Contains(s, "token_invalidated")
}

// Client fetches Codex usage limits, optionally for multiple stored accounts.
type Client struct {
	doer             *httpx.Doer
	endpoint         string
	refresh          *refreshClient
	store            accounts.Store
	readCredential   func(ctx context.Context) (cred.Credential, error)
	lookupID         func(idToken string) (sub, email string, err error) // nil → wraps cred.ParseCodexIDToken
	warn             io.Writer                                           // receives per-run warn lines; defaults to os.Stderr in New
	now              func() time.Time
	baseTimeout      time.Duration
	perAccountBudget time.Duration
	cache            *usagecache.Cache // always non-nil; disabled state degrades to no-op
	cacheBypass      bool              // skips the cache read path; writes still propagate
}

// Option mutates a Client at construction time.
type Option func(*Client)

// WithStore overrides the account store. The default is accounts.NewMemoryStore().
func WithStore(s accounts.Store) Option { return func(c *Client) { c.store = s } }

// WithNow overrides the clock used to compute ResetAfterSeconds and the
// token-expiry skew check. Defaults to time.Now. Intended for tests.
func WithNow(fn func() time.Time) Option { return func(c *Client) { c.now = fn } }

// WithCacheBypass bypasses the cache read path when true; writes still
// propagate so the next invocation without bypass benefits from the fresh result.
func WithCacheBypass(bypass bool) Option { return func(c *Client) { c.cacheBypass = bypass } }

// CacheBypassEnabled reports whether the cache read path is bypassed.
// Test-only seam; not part of the public API.
func (c *Client) CacheBypassEnabled() bool { return c.cacheBypass }

// New constructs a Client. debug receives [debug] lines when non-nil.
func New(debug io.Writer, userAgent string, opts ...Option) *Client {
	doer := httpx.NewDoer(
		&http.Client{CheckRedirect: httpx.RejectSchemeDowngrade},
		userAgent,
		"codex",
		nil,
		debug,
	)
	c := &Client{
		doer:             doer,
		endpoint:         endpoint,
		refresh:          newRefreshClient(doer),
		store:            accounts.NewMemoryStore(),
		readCredential:   cred.ReadCodexCredential,
		warn:             os.Stderr,
		now:              time.Now,
		baseTimeout:      baseTimeout,
		perAccountBudget: perAccountBudget,
	}
	for _, o := range opts {
		o(c)
	}
	// Initialize cache after options so WithNow's clock propagates into the cache.
	c.cache = usagecache.New("codex", c.now, func(s string) { c.warnf("%s\n", s) })
	return c
}

func (c *Client) ID() string { return "codex" }

// warnf writes a warn line to c.warn (always visible, unlike debug lines).
func (c *Client) warnf(format string, args ...any) {
	if c.warn != nil {
		fmt.Fprintf(c.warn, format, args...)
	}
}

// logCacheHit emits one [debug] line on a usage cache hit.
func (c *Client) logCacheHit(key string, age time.Duration) {
	if c.doer.Debug != nil {
		fmt.Fprintf(c.doer.Debug, "[debug] codex: usage cache hit for %s (age %ds)\n",
			key, int(age.Seconds()))
	}
}

// readLiveCredential reads the live Codex credential. A missing credential
// (ErrCodexTokenNotFound) is represented as (nil, nil) — absence is not an
// error. Any other error (e.g. JSON parse error) is surfaced.
func (c *Client) readLiveCredential(ctx context.Context) (*cred.Credential, error) {
	credCtx, cancel := context.WithTimeout(ctx, credTimeout)
	defer cancel()
	cr, err := c.readCredential(credCtx)
	if err != nil {
		if errors.Is(err, cred.ErrCodexTokenNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &cr, nil
}

// window is one entry in the usage API rate_limit response.
type window struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type response struct {
	RateLimit *struct {
		PrimaryWindow   *window `json:"primary_window"`
		SecondaryWindow *window `json:"secondary_window"`
	} `json:"rate_limit"`
	CodeReviewRateLimit *window `json:"code_review_rate_limit"`
}

// windowLabel maps a limit_window_seconds value to a human-readable label with
// ±5% tolerance. Known buckets: ~18000→"five_hour", ~604800→"seven_day",
// ~2592000→"thirty_day". Unknown durations fall through to "window_<N>s" so
// the data still surfaces. This fixes the slot-vs-duration bug: free accounts
// have no five-hour tier; their weekly cap appears in the primary window and
// was previously hard-coded to "five_hour" regardless of limit_window_seconds.
func windowLabel(limitWindowSeconds int64) string {
	type bucket struct {
		center int64
		label  string
	}
	buckets := []bucket{
		{18000, "five_hour"},
		{604800, "seven_day"},
		{2592000, "thirty_day"},
	}
	for _, b := range buckets {
		lo := b.center - b.center/20 // 5% below
		hi := b.center + b.center/20 // 5% above
		if limitWindowSeconds >= lo && limitWindowSeconds <= hi {
			return b.label
		}
	}
	return fmt.Sprintf("window_%ds", limitWindowSeconds)
}

// toLimit converts a window object to a providers.Limit. Returns ok=false
// when ResetAt is non-positive — observed when an inactive window object is
// returned with zero fields (e.g. a code-review window for a user who has
// never used the feature, or a freshly-created account with no usage in the
// window). Rendering epoch-0 as 1970-01-01T00:00:00Z with zero seconds
// remaining is worse than omitting the window entirely.
func (w window) toLimit(now time.Time) (providers.Limit, bool) {
	if w.ResetAt <= 0 {
		return providers.Limit{}, false
	}
	resets := time.Unix(w.ResetAt, 0).UTC().Truncate(time.Second)
	secs := int(resets.Sub(now).Seconds())
	if secs < 0 {
		secs = 0
	}
	return providers.Limit{
		UsedPercent:       w.UsedPercent,
		RemainingPercent:  100 - w.UsedPercent,
		ResetsAt:          resets,
		ResetAfterSeconds: secs,
	}, true
}

// rotateRawBlob returns a copy of rawBlob with tokens.access_token and
// tokens.refresh_token updated from tok. tokens.id_token is updated only when
// the refresh response returned a new one (tok.IDToken != ""); otherwise the
// existing id_token is left in place. The id_token is identity-only — every
// consumer (extractIDToken, findActive, ResolveActiveKey) reads sub/email, not
// exp, and sub is stable across a refresh, so a preserved (expired-but-same-sub)
// id_token cannot mis-resolve identity. Refresh expiry is read from the rotated
// access_token JWT by StoredExpiresAt, so a stale id_token no longer affects the
// gate. Unknown fields at all levels survive the round-trip.
func rotateRawBlob(rawBlob json.RawMessage, tok Token) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(rawBlob, &m); err != nil {
		return nil, fmt.Errorf("rotateRawBlob: unmarshal: %w", err)
	}
	tokens, ok := m["tokens"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("rotateRawBlob: tokens missing or wrong type")
	}
	tokens["access_token"] = tok.AccessToken
	tokens["refresh_token"] = tok.RefreshToken
	if tok.IDToken != "" {
		tokens["id_token"] = tok.IDToken
	}
	m["tokens"] = tokens
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("rotateRawBlob: marshal: %w", err)
	}
	return json.RawMessage(out), nil
}

// refreshErrorMessage returns a human-readable description of a refresh error.
// Uses "codex login" recovery hints (D7).
func refreshErrorMessage(err error) string {
	// Match the real upstream body, e.g. "Your refresh token has already been
	// used to generate a new access token." (invalid_request_error, not
	// invalid_grant). The literal "refresh_token already" never appears.
	if strings.Contains(err.Error(), "already been used") {
		return msgStaleRefresh
	}
	if errors.Is(err, ErrRefreshRejected) {
		return "account credential expired (run `codex login` to refresh)"
	}
	if errors.Is(err, ErrRefreshEndpointBroken) {
		return fmt.Sprintf(
			"aistat: codex: refresh endpoint rejected request (%s); this is likely an aistat refresh implementation issue, not your account. Run 'codex login' to work around it for this account and file an issue at %s",
			err, providers.IssueTrackerURL,
		)
	}
	return err.Error()
}

// fetchLimitsFresh calls the usage endpoint with accessToken and returns the
// parsed limits. Uses windowLabel to key limits by actual window duration
// rather than slot position, fixing the free-account mislabeling bug (D5).
func (c *Client) fetchLimitsFresh(ctx context.Context, accessToken string) (map[string]providers.Limit, error) {
	var raw response
	if err := c.doer.GetJSON(ctx, c.endpoint, accessToken, c.perAccountBudget, &raw, httpx.DefaultClassify); err != nil {
		return nil, err
	}
	if raw.RateLimit == nil {
		return nil, errors.New("codex usage response missing rate_limit object")
	}

	// Note: c.now() and the orchestrator's checked_at are separate clocks, so
	// reset_after_seconds + checked_at may differ from resets_at by up to one
	// second. Accepted as a known trade-off (same caveat as pre-v2.1 Fetch).
	now := c.now().UTC().Truncate(time.Second)
	limits := map[string]providers.Limit{}

	for _, w := range []*window{raw.RateLimit.PrimaryWindow, raw.RateLimit.SecondaryWindow} {
		if w == nil {
			continue
		}
		if lim, ok := w.toLimit(now); ok {
			limits[windowLabel(w.LimitWindowSeconds)] = lim
		}
	}
	if w := raw.CodeReviewRateLimit; w != nil {
		if lim, ok := w.toLimit(now); ok {
			limits["code_review_"+windowLabel(w.LimitWindowSeconds)] = lim
		}
	}

	return limits, nil
}

// fetchLimitsCached checks the usage cache first, falling through to
// fetchLimitsFresh on miss. On a successful fresh fetch, writes through to
// the cache even when cacheBypass is set. If key is empty (live-unstored
// fallback path), skips all cache interaction and calls fetchLimitsFresh directly.
func (c *Client) fetchLimitsCached(ctx context.Context, accessToken, key string) (map[string]providers.Limit, error) {
	if key == "" {
		return c.fetchLimitsFresh(ctx, accessToken)
	}
	if !c.cacheBypass {
		if cached, age, ok := c.cache.GetWithAge(key); ok {
			cached = multiaccount.RecomputeResetAfter(cached, c.now())
			c.logCacheHit(key, age)
			return cached, nil
		}
	}
	limits, err := c.fetchLimitsFresh(ctx, accessToken)
	if err == nil {
		c.cache.Put(key, limits)
	}
	return limits, err
}

// FetchUsage calls the usage endpoint for a known account key and returns the
// parsed limits via the same cached path as the reporting flow. Passing an
// empty key skips cache entirely and falls through to a fresh fetch.
func (c *Client) FetchUsage(ctx context.Context, accessToken, key string) (map[string]providers.Limit, error) {
	return c.fetchLimitsCached(ctx, accessToken, key)
}

// doReconcile is the shared write-capable reconcile path used by both Fetch
// and ReconcileAndPersist. It reads the live credential, lists the store
// (warn on list error), runs Reconcile, and persists any inserted or upserted
// slot before returning.
func (c *Client) doReconcile(ctx context.Context) (*cred.Credential, ReconcileOutput, error) {
	live, err := c.readLiveCredential(ctx)
	if err != nil {
		return nil, ReconcileOutput{}, err
	}

	stored, listErr := c.store.List(ctx)
	if listErr != nil {
		c.warnf("aistat: codex: could not read account store (%s); proceeding with live credential only\n", listErr)
		stored = nil
	}

	lookupFn := c.lookupID
	if lookupFn == nil {
		lookupFn = func(idToken string) (string, string, error) {
			sub, email, _, err := cred.ParseCodexIDToken(idToken)
			return sub, email, err
		}
	}

	out := Reconcile(ReconcileInput{
		LiveBlob: live,
		Stored:   stored,
		LookupID: lookupFn,
		Now:      c.now(),
	})

	// Persist before the usage fetches for crash robustness.
	if out.Inserted || out.Upserted {
		for _, acct := range out.Accounts {
			if acct.Key() == out.ActiveKey {
				if err := c.store.Upsert(ctx, acct); err != nil {
					c.warnf("aistat: codex: could not persist account %s (uuid %s): %s\n", acct.Email, acct.UUID, err)
				}
				break
			}
		}
	}

	return live, out, nil
}

// ReconcileAndPersist is the exported write-capable reconcile entry point
// called by cmd/aistat/switch.go after it has written a new blob to the live
// auth.json. Running it post-write naturally updates last_seen_at on the now-
// active account. Errors from store.List and store.Upsert are non-fatal (warn-only).
func (c *Client) ReconcileAndPersist(ctx context.Context) error {
	_, reconcileOut, err := c.doReconcile(ctx)
	if err != nil {
		return err
	}
	if reconcileOut.CaptureWarn != "" {
		c.warnf("%s\n", reconcileOut.CaptureWarn)
	}
	return nil
}

// Fetch implements providers.Provider. It reads the live credential, reconciles
// it against the multi-account store, optionally refreshes near-expiry tokens,
// fetches usage for each account sequentially, and returns a ProviderOutput
// with per-account detail in Accounts.
//
// ErrTransient is returned only when zero accounts succeeded AND at least one
// failure was transient (D8 retry rule).
func (c *Client) Fetch(ctx context.Context) (providers.ProviderOutput, error) {
	live, reconcileOut, err := c.doReconcile(ctx)
	if err != nil {
		return providers.ProviderOutput{}, err
	}

	// No live credential AND no stored accounts → auth missing.
	if live == nil && len(reconcileOut.Accounts) == 0 {
		return providers.ProviderOutput{}, fmt.Errorf("%w: %w", providers.ErrAuthMissing, cred.ErrCodexTokenNotFound)
	}

	// Emit CaptureWarn before the per-account loop.
	if reconcileOut.CaptureWarn != "" {
		c.warnf("%s\n", reconcileOut.CaptureWarn)
	}

	// Compute pool timeout budget (includes synthetic LiveUnstored row if any).
	totalAccounts := len(reconcileOut.Accounts)
	if reconcileOut.LiveUnstored != nil {
		totalAccounts++
	}
	budget := multiaccount.Budget(c.baseTimeout, c.perAccountBudget, totalAccounts)
	poolCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var accountResults []providers.AccountResult
	var transientCount, successCount int

	// If the live credential could not be reconciled to a stored slot,
	// prepend a synthetic in-memory row for the live account (D8).
	if reconcileOut.LiveUnstored != nil {
		limits, fetchErr := c.fetchLimitsFresh(poolCtx, reconcileOut.LiveUnstored.AccessToken)
		ar := providers.AccountResult{
			Email:  "(live Codex account)",
			UUID:   "",
			Active: true,
		}
		if ok, trans := multiaccount.RecordFetchOutcome(&ar, limits, fetchErr); ok {
			successCount++
		} else if trans {
			transientCount++
		}
		if isRevokedTokenErr(fetchErr) {
			ar.Error = fmt.Sprintf("aistat: codex: %s: %s", ar.Email, msgTokensRevoked)
		}
		accountResults = append(accountResults, ar)
	}

	sort.Slice(reconcileOut.Accounts, func(i, j int) bool { return reconcileOut.Accounts[i].Key() < reconcileOut.Accounts[j].Key() })
	// Per-account sequential fetch (with optional refresh).
	for _, acct := range reconcileOut.Accounts {
		ar := providers.AccountResult{
			Email:  acct.Email,
			UUID:   acct.UUID,
			Plan:   acct.RateLimitTier,
			Key:    acct.Key(),
			Active: acct.Key() == reconcileOut.ActiveKey,
		}

		// Refresh if the token is near expiry (ExpiresAt present and within skew).
		now := c.now()
		if StoredExpiresAt(acct) > 0 && StoredExpiresAt(acct) < now.Add(refreshSkew).UnixMilli() {
			tok, refreshErr := c.refresh.Exchange(poolCtx, StoredRefreshToken(acct))
			if refreshErr != nil {
				ar.Error = refreshErrorMessage(refreshErr)
				if errors.Is(refreshErr, providers.ErrTransient) {
					transientCount++
				}
				accountResults = append(accountResults, ar)
				continue
			}
			// Persist rotated tokens; preserves unknown fields via map[string]any round-trip.
			if newBlob, blobErr := rotateRawBlob(acct.RawBlob, tok); blobErr == nil {
				acct.RawBlob = newBlob
				_ = c.store.Upsert(poolCtx, acct)
			}
		}

		limits, fetchErr := c.fetchLimitsCached(poolCtx, StoredAccessToken(acct), acct.Key())
		if ok, trans := multiaccount.RecordFetchOutcome(&ar, limits, fetchErr); ok {
			successCount++
		} else if trans {
			transientCount++
		}
		if isRevokedTokenErr(fetchErr) {
			ar.Error = fmt.Sprintf("aistat: codex: %s: %s", acct.Email, msgTokensRevoked)
		}
		accountResults = append(accountResults, ar)
	}

	multiaccount.SortAccountResults(accountResults)

	out := providers.ProviderOutput{
		Accounts: accountResults,
	}

	// D8: ErrTransient only when zero succeeded AND at least one was transient.
	if successCount == 0 && transientCount > 0 {
		return out, fmt.Errorf("%w: all %d account fetch(es) failed", providers.ErrTransient, len(accountResults))
	}

	return out, nil
}

// FetchForSwitch is read-only against the live auth.json and the multi-account
// store. It performs no token refresh and no Upsert. Non-active accounts with
// ErrAuthDenied are excluded with a per-account warn; excluded accounts must
// not be auto-picked by aistat switch (they cannot be refreshed here without
// risking writing a stale pre-rotation token to the live auth.json).
func (c *Client) FetchForSwitch(ctx context.Context) ([]providers.AccountResult, error) {
	live, err := c.readLiveCredential(ctx)
	if err != nil {
		return nil, err
	}

	stored, err := c.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex: reading account store: %w", err)
	}

	// ResolveActiveKey is read-only: at most one JWT parse, no writes.
	lookupFn2 := c.lookupID
	if lookupFn2 == nil {
		lookupFn2 = func(idToken string) (string, string, error) {
			sub, email, _, err := cred.ParseCodexIDToken(idToken)
			return sub, email, err
		}
	}
	activeKey, _ := ResolveActiveKey(ReconcileInput{
		LiveBlob: live,
		Stored:   stored,
		LookupID: lookupFn2,
		Now:      c.now(),
	})

	var nonActive []accounts.Account
	for _, acct := range stored {
		if acct.Key() != activeKey {
			nonActive = append(nonActive, acct)
		}
	}

	budget := multiaccount.Budget(c.baseTimeout, c.perAccountBudget, len(nonActive))
	poolCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var results []providers.AccountResult
	sort.Slice(nonActive, func(i, j int) bool { return nonActive[i].Key() < nonActive[j].Key() })
	for _, acct := range nonActive {
		limits, fetchErr := c.fetchLimitsCached(poolCtx, StoredAccessToken(acct), acct.Key())
		if fetchErr != nil {
			if errors.Is(fetchErr, providers.ErrAuthDenied) {
				hint := "run `aistat usage` to refresh"
				if isRevokedTokenErr(fetchErr) {
					hint = "run `codex login` to recover"
				}
				c.warnf("aistat: codex: %s: stored credential rejected (%s); excluded from auto-pick\n", acct.Email, hint)
			} else {
				c.warnf("aistat: codex: %s: usage fetch failed (%s); excluded from auto-pick\n", acct.Email, fetchErr)
			}
			continue
		}
		results = append(results, providers.AccountResult{
			Email:  acct.Email,
			UUID:   acct.UUID,
			Key:    acct.Key(),
			Plan:   acct.RateLimitTier,
			Active: false,
			Limits: limits,
		})
	}

	multiaccount.SortAccountResults(results)
	return results, nil
}

// PostSwitchVerify calls the usage endpoint for target immediately after a
// switch write to check whether the just-activated tokens are usable.
// FetchUsage hits the 90 s cache in auto-pick (warm from FetchForSwitch);
// live HTTP only in the --to path where no FetchForSwitch cache write preceded it.
// The 5 s deadline bounds the courtesy check on the --to path: a slow/unreachable
// network produces a transient that the caller silently drops, same as no deadline,
// but without a ~45 s hang from three full perAccountBudget retry attempts.
func (c *Client) PostSwitchVerify(ctx context.Context, target accounts.Account) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.FetchUsage(ctx, StoredAccessToken(target), target.Key())
	if err == nil {
		return nil
	}
	if isRevokedTokenErr(err) {
		return fmt.Errorf("%s: %s: %w", target.Email, msgTokensRevoked, providers.ErrAuthDenied)
	}
	return err
}
