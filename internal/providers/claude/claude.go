package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	endpoint    = "https://api.anthropic.com/api/oauth/usage"
	betaHeader  = "oauth-2025-04-20"
	credTimeout = 10 * time.Second

	// baseTimeout is the minimum time budget covering the live-credential read,
	// reconciliation, and at least one account's usage fetch.
	baseTimeout = 10 * time.Second

	// perAccountBudget is added per account returned by Reconcile, covering
	// refresh + usage for each account. The total budget is applied as a single
	// context.WithTimeout over the whole per-account loop (pooled, not per-
	// account), so a slow first account can starve later ones — acceptable as
	// a v1 simplicity trade-off documented in D7. The 15 s ceiling permits one
	// max-length Retry-After: 10 sleep plus the actual fetch on attempts 1 + 2
	// before sleepWithCtx's deadline check short-circuits a second sleep.
	perAccountBudget = 15 * time.Second

	// refreshSkew is the safety margin before ExpiresAt at which a stored
	// token is considered near-expiry and proactively refreshed.
	refreshSkew = 30 * time.Second
)

// Client fetches Claude usage limits, optionally for multiple stored accounts.
type Client struct {
	doer             *httpx.Doer
	endpoint         string
	profile          *profileClient
	refresh          *refreshClient
	store            accounts.Store
	readCredential   func(ctx context.Context) (cred.Credential, error)
	warn             io.Writer // receives per-run warn lines; defaults to os.Stderr in New
	now              func() time.Time
	baseTimeout      time.Duration
	perAccountBudget time.Duration
	cache            *usagecache.Cache // always non-nil; disabled state degrades to no-op
	cacheBypass      bool        // skips the cache read path; writes still propagate (D8)
}

// Option mutates a Client at construction time.
type Option func(*Client)

// WithStore overrides the account store. The default is accounts.NewMemoryStore()
// so a New() call without WithStore still works (tests, fake_provider). Production
// realProviders pass a real platform store via this option (Step 11).
func WithStore(s accounts.Store) Option { return func(c *Client) { c.store = s } }

// WithNow overrides the clock used to compute ResetAfterSeconds and the
// token-expiry skew check. Defaults to time.Now. Intended for tests.
func WithNow(fn func() time.Time) Option { return func(c *Client) { c.now = fn } }

// WithCacheBypass bypasses the cache read path when true; writes still
// propagate so the next invocation without bypass benefits from the fresh
// result (D8 write-through contract). Has no effect when false (default).
func WithCacheBypass(bypass bool) Option { return func(c *Client) { c.cacheBypass = bypass } }

// CacheBypassEnabled reports whether the cache read path is bypassed.
// Test-only seam; not part of the public API.
func (c *Client) CacheBypassEnabled() bool { return c.cacheBypass }

// New constructs a Client. debug receives [debug] lines when non-nil.
// warn (distinct from debug) receives always-visible diagnostic lines; it
// defaults to os.Stderr and is not configurable via options — tests in this
// package set c.warn directly.
func New(debug io.Writer, userAgent string, opts ...Option) *Client {
	doer := httpx.NewDoer(
		&http.Client{CheckRedirect: httpx.RejectSchemeDowngrade},
		userAgent,
		"claude",
		map[string]string{"Anthropic-Beta": betaHeader},
		debug,
	)
	c := &Client{
		doer:             doer,
		endpoint:         endpoint,
		profile:          newProfileClient(doer),
		refresh:          newRefreshClient(doer),
		store:            accounts.NewMemoryStore(),
		readCredential:   cred.ReadClaudeCredential,
		warn:             os.Stderr,
		now:              time.Now,
		baseTimeout:      baseTimeout,
		perAccountBudget: perAccountBudget,
	}
	for _, o := range opts {
		o(c)
	}
	// Initialize cache after options so WithNow's clock propagates into the cache.
	c.cache = usagecache.New("claude", c.now, func(s string) { c.warnf("%s\n", s) })
	return c
}

func (c *Client) ID() string { return "claude" }

// GetProfile fetches the OAuth profile for the given access token.
// Used by cmd/aistat/accounts.go (D11 active-UUID resolution) to get a profile
// lookup callable without constructing a full Fetch call.
func (c *Client) GetProfile(ctx context.Context, token string) (Profile, error) {
	return c.profile.Get(ctx, token)
}

// window is one entry in the usage API's response map.
type window struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    *string `json:"resets_at"`
}

// scopedLimit is one entry in the "limits" array — Anthropic's migration
// target for model-scoped limits (see fetchLimitsFresh below).
type scopedLimit struct {
	Kind     string  `json:"kind"`
	Percent  float64 `json:"percent"`
	ResetsAt *string `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// parseReset normalizes a window's resets_at: UTC, second precision, and the
// seconds-until-reset clamped at zero. now must already be UTC-truncated.
func parseReset(s string, now time.Time) (resets time.Time, secs int, err error) {
	resets, err = time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, 0, err
	}
	resets = resets.UTC().Truncate(time.Second)
	secs = int(resets.Sub(now).Seconds())
	if secs < 0 {
		secs = 0
	}
	return resets, secs, nil
}

// scopedWindowKey derives the seven_day_<model> window key from a scoped
// limit's model display name. Defensive slug: lowercase, every rune outside
// [a-z0-9] becomes '_', consecutive '_' collapse, and leading/trailing '_'
// are trimmed. Returns "" when nothing survives (empty or punctuation-only
// names) so callers can skip the entry.
func scopedWindowKey(displayName string) string {
	var b strings.Builder
	pendingSep := false
	for _, r := range strings.ToLower(displayName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if pendingSep && b.Len() > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			pendingSep = false
		} else {
			pendingSep = true
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "seven_day_" + b.String()
}

// warnf writes a warn line to c.warn (always visible, unlike debug lines).
func (c *Client) warnf(format string, args ...any) {
	if c.warn != nil {
		fmt.Fprintf(c.warn, format, args...)
	}
}

// readLiveCredential reads the live Claude credential. A missing credential
// (ErrClaudeTokenNotFound) is represented as (nil, nil) — absence is not an
// error. Any other error (e.g. JSON parse error) is surfaced.
func (c *Client) readLiveCredential(ctx context.Context) (*cred.Credential, error) {
	credCtx, cancel := context.WithTimeout(ctx, credTimeout)
	defer cancel()
	cr, err := c.readCredential(credCtx)
	if err != nil {
		if errors.Is(err, cred.ErrClaudeTokenNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &cr, nil
}

// FetchUsage calls the usage endpoint for a known account UUID and returns the
// parsed limits. Goes through the same cached path as the reporting flow
// (fetchLimitsCached): cache hit if a recent entry exists; otherwise fetches
// fresh and writes through. Exposed so cmd/aistat/switch can read the
// currently-active account's headroom for D13's "already on best" comparison
// without re-implementing the window parser; FetchForSwitch returns only the
// non-active rows.
//
// Passing an empty uuid (e.g. for an unstored live credential) skips cache
// entirely and falls through to a fresh fetch — the cache has no key to
// associate the result with.
func (c *Client) FetchUsage(ctx context.Context, accessToken, uuid string) (map[string]providers.Limit, error) {
	return c.fetchLimitsCached(ctx, accessToken, uuid)
}

// fetchLimitsFresh calls the usage endpoint with accessToken and returns the
// parsed limits. ctx is expected to already carry the pool budget;
// perAccountBudget is used as the per-call ceiling within that budget.
// No cache interaction; callers that want caching use fetchLimitsCached.
func (c *Client) fetchLimitsFresh(ctx context.Context, accessToken string) (map[string]providers.Limit, error) {
	// Decode into RawMessage values, NOT map[string]*window: Anthropic adds
	// sibling fields over time (e.g. a per-app usage array) whose shapes we
	// don't model. Forcing every value to be a window made one array-valued
	// field fail the whole response — taking the five_hour / seven_day windows
	// we do surface down with it. We decode only the closed set below.
	var raw map[string]json.RawMessage
	if err := c.doer.GetJSON(ctx, c.endpoint, accessToken, c.perAccountBudget, &raw, httpx.DefaultClassify); err != nil {
		return nil, err
	}

	// Note: c.now() and the orchestrator's checked_at are separate clocks, so
	// reset_after_seconds + checked_at may differ from resets_at by up to one
	// second. Accepted as a known trade-off (same caveat as pre-v2.1 Fetch).
	now := c.now().UTC().Truncate(time.Second)
	limits := map[string]providers.Limit{}
	// Closed set of windows we surface; all others are intentionally filtered.
	for _, key := range []string{"five_hour", "seven_day", "seven_day_sonnet"} {
		rawWin, ok := raw[key]
		if !ok {
			continue
		}
		var win window
		if err := json.Unmarshal(rawWin, &win); err != nil {
			// A surfaced key carrying an unexpected shape is skipped rather than
			// failing the whole account — graceful degradation, same spirit as
			// the array-valued sibling fields above.
			continue
		}
		if win.ResetsAt == nil {
			continue
		}
		resets, secs, err := parseReset(*win.ResetsAt, now)
		if err != nil {
			return nil, fmt.Errorf("claude window %s has unparseable resets_at %q: %w", key, *win.ResetsAt, err)
		}
		limits[key] = providers.Limit{
			UsedPercent:       win.Utilization,
			RemainingPercent:  100 - win.Utilization,
			ResetsAt:          resets,
			ResetAfterSeconds: secs,
		}
	}

	// "limits" is Anthropic's migration target for model-scoped weekly limits
	// (e.g. a "Fable"-scoped weekly window alongside the aggregate seven_day).
	// Only weekly_scoped entries are surfaced, as seven_day_<model>; session and
	// weekly_all duplicate five_hour/seven_day above and are skipped, and
	// is_active is deliberately NOT a gating condition — scoped limits surface
	// regardless of which window the API marks active. The key derives from
	// scope.model.display_name because scope.model.id is null in today's API
	// responses; if Anthropic renames a model the derived key changes silently
	// and its render label in internal/render/text.go stops matching — accepted
	// (these windows are informational-only), revisit when model.id is populated.
	// Top-level keys win on collision since both can describe the
	// same window during the migration period. Unlike the whitelist loop above,
	// a malformed array or an unparseable resets_at is skipped rather than
	// failing the response: these entries are additive/informational-plus and
	// must never take five_hour / seven_day down with them.
	if rawLimits, ok := raw["limits"]; ok {
		var scoped []scopedLimit
		if err := json.Unmarshal(rawLimits, &scoped); err != nil {
			c.debugf("usage limits array unparseable; scoped windows skipped: %s", err)
		} else {
			for _, sl := range scoped {
				if sl.Kind != "weekly_scoped" {
					continue
				}
				if sl.Scope == nil || sl.Scope.Model == nil {
					continue
				}
				key := scopedWindowKey(sl.Scope.Model.DisplayName)
				if key == "" {
					continue
				}
				if sl.ResetsAt == nil {
					continue
				}
				resets, secs, err := parseReset(*sl.ResetsAt, now)
				if err != nil {
					c.debugf("scoped window %s skipped: unparseable resets_at %q", key, *sl.ResetsAt)
					continue
				}
				if _, exists := limits[key]; exists {
					continue
				}
				limits[key] = providers.Limit{
					UsedPercent:       sl.Percent,
					RemainingPercent:  100 - sl.Percent,
					ResetsAt:          resets,
					ResetAfterSeconds: secs,
				}
			}
		}
	}

	return limits, nil
}

// fetchLimitsCached checks the usage cache first, falling through to
// fetchLimitsFresh on miss. On a successful fresh fetch, writes through to
// the cache even when cacheBypass is set (D8 write-through contract).
// If uuid is empty (live-unstored fallback path), skips all cache interaction
// and calls fetchLimitsFresh directly.
func (c *Client) fetchLimitsCached(ctx context.Context, accessToken, uuid string) (map[string]providers.Limit, error) {
	if uuid == "" {
		return c.fetchLimitsFresh(ctx, accessToken)
	}
	if !c.cacheBypass {
		if cached, age, ok := c.cache.GetWithAge(uuid); ok {
			cached = multiaccount.RecomputeResetAfter(cached, c.now())
			c.logCacheHit(uuid, age)
			return cached, nil
		}
	}
	limits, err := c.fetchLimitsFresh(ctx, accessToken)
	if err == nil {
		c.cache.Put(uuid, limits)
	}
	return limits, err
}

// debugf writes one "[debug] claude: ..." line to the doer's debug writer.
func (c *Client) debugf(format string, args ...any) {
	if c.doer.Debug != nil {
		fmt.Fprintf(c.doer.Debug, "[debug] claude: "+format+"\n", args...)
	}
}

// logCacheHit emits one [debug] line on a usage cache hit.
func (c *Client) logCacheHit(uuid string, age time.Duration) {
	c.debugf("usage cache hit for %s (age %ds)", uuid, int(age.Seconds()))
}

// rotateRawBlob returns a copy of rawBlob with claudeAiOauth.accessToken,
// refreshToken, and expiresAt replaced by the values from tok. Unknown fields
// are preserved via map[string]any round-trip so future Anthropic additions
// survive rotation without loss.
func rotateRawBlob(rawBlob json.RawMessage, tok Token) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(rawBlob, &m); err != nil {
		return nil, fmt.Errorf("rotateRawBlob: unmarshal: %w", err)
	}
	oauth, ok := m["claudeAiOauth"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("rotateRawBlob: claudeAiOauth missing or wrong type")
	}
	oauth["accessToken"] = tok.AccessToken
	oauth["refreshToken"] = tok.RefreshToken
	oauth["expiresAt"] = tok.ExpiresAt
	m["claudeAiOauth"] = oauth
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("rotateRawBlob: marshal: %w", err)
	}
	return json.RawMessage(out), nil
}

func refreshErrorMessage(err error) string {
	if errors.Is(err, ErrRefreshRejected) {
		return "account credential expired (run `claude /login` to refresh)"
	}
	if errors.Is(err, ErrRefreshEndpointBroken) {
		return fmt.Sprintf(
			"aistat: claude: refresh endpoint rejected request (%s); this is likely an aistat refresh implementation issue, not your account. Run 'claude /login' to work around it for this account and file an issue at %s",
			err, providers.IssueTrackerURL,
		)
	}
	return err.Error()
}

// doReconcile is the shared write-capable reconcile path used by both Fetch
// and ReconcileAndPersist. It reads the live credential, lists the store
// (warn on list error), runs Reconcile, and persists any inserted or upserted
// slot before returning. A crash between the Upsert here and the usage fetches
// in Fetch leaves the store with fresh metadata — the next run self-corrects.
//
// Returns (nil, ReconcileOutput{}, err) only when reading the live credential
// fails with a non-missing error. List failures and Upsert failures are treated
// as non-fatal.
func (c *Client) doReconcile(ctx context.Context) (*cred.Credential, ReconcileOutput, error) {
	live, err := c.readLiveCredential(ctx)
	if err != nil {
		return nil, ReconcileOutput{}, err
	}

	stored, listErr := c.store.List(ctx)
	if listErr != nil {
		c.warnf("aistat: claude: could not read account store (%s); proceeding with live credential only\n", listErr)
		stored = nil
	}

	out := Reconcile(ReconcileInput{
		LiveBlob: live,
		Stored:   stored,
		LookupProfile: func(token string) (Profile, error) {
			return c.profile.Get(ctx, token)
		},
		Now: c.now(),
	})

	// Step 5: persist before the usage fetches for crash robustness.
	// Invariant: Reconcile sets Inserted/Upserted only for the active slot,
	// so the UUID filter below always writes exactly the account that changed.
	// Persistence stays best-effort (a write failure does not fail Fetch), but
	// we warn so a deterministic store-write failure does not silently recur.
	if out.Inserted || out.Upserted {
		for _, acct := range out.Accounts {
			if acct.UUID == out.ActiveUUID {
				if err := c.store.Upsert(ctx, acct); err != nil {
					c.warnf("aistat: claude: could not persist account %s (uuid %s): %s\n", acct.Email, acct.UUID, err)
				}
				break
			}
		}
	}

	return live, out, nil
}

// ReconcileAndPersist is the exported write-capable reconcile entry point
// called by cmd/aistat/switch.go after it has written a new blob to the live
// keychain. Running it post-write naturally updates last_seen_at on the now-
// active account and captures any metadata drift on previously-active slots.
// Errors from store.List and store.Upsert are non-fatal (warn-only).
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
// with per-account detail in Accounts and the active account's limits in Limits
// (active-account compatibility projection per D3).
//
// Timeout budget: baseTimeout + len(accounts) × perAccountBudget, applied as a
// single pool over the per-account loop. A slow first account can starve later
// accounts — accepted in v2.1.0. The parent context from the orchestrator
// still bounds total runtime; the orchestrator does not retry (backoff lives
// in httpx.Doer).
//
// ErrTransient is returned only when zero accounts succeeded AND at least one
// failure was transient (D8 retry rule). Per-account errors that don't wipe out
// every account are recorded in AccountResult.Error only.
func (c *Client) Fetch(ctx context.Context) (providers.ProviderOutput, error) {
	live, reconcileOut, err := c.doReconcile(ctx)
	if err != nil {
		return providers.ProviderOutput{}, err
	}

	// Step 10: no live credential AND no stored accounts → auth missing.
	if live == nil && len(reconcileOut.Accounts) == 0 {
		return providers.ProviderOutput{}, fmt.Errorf("%w: %w", providers.ErrAuthMissing, cred.ErrClaudeTokenNotFound)
	}

	// Emit CaptureWarn (D1 branch 4) before the per-account loop.
	if reconcileOut.CaptureWarn != "" {
		c.warnf("%s\n", reconcileOut.CaptureWarn)
	}

	// Step 4: compute the pool timeout budget. Count the LiveUnstored synthetic
	// row (if any) so its fetchLimitsFresh call has a slot in the budget.
	totalAccounts := len(reconcileOut.Accounts)
	if reconcileOut.LiveUnstored != nil {
		totalAccounts++
	}
	budget := multiaccount.Budget(c.baseTimeout, c.perAccountBudget, totalAccounts)
	poolCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var accountResults []providers.AccountResult
	var transientCount, successCount int

	// Step 6: if the live credential could not be reconciled to a stored slot
	// (fallback case), prepend a synthetic in-memory row for the live account.
	if reconcileOut.LiveUnstored != nil {
		limits, fetchErr := c.fetchLimitsFresh(poolCtx, reconcileOut.LiveUnstored.AccessToken)
		ar := providers.AccountResult{
			Email:  "(live Claude account)",
			UUID:   "",
			Plan:   "",
			Active: true,
		}
		if ok, trans := multiaccount.RecordFetchOutcome(&ar, limits, fetchErr); ok {
			successCount++
		} else if trans {
			transientCount++
		}
		accountResults = append(accountResults, ar)
	}

	// Step 7: per-account sequential fetch (with optional refresh).
	for _, acct := range reconcileOut.Accounts {
		ar := providers.AccountResult{
			Email:  acct.Email,
			UUID:   acct.UUID,
			Plan:   acct.RateLimitTier,
			Active: acct.UUID == reconcileOut.ActiveUUID,
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
			// Persist rotated tokens. rotateRawBlob preserves unknown fields via
			// map[string]any round-trip so future Anthropic additions survive rotation.
			if newBlob, blobErr := rotateRawBlob(acct.RawBlob, tok); blobErr == nil {
				acct.RawBlob = newBlob
				_ = c.store.Upsert(poolCtx, acct)
			}
		}

		limits, fetchErr := c.fetchLimitsCached(poolCtx, StoredAccessToken(acct), acct.UUID)
		if ok, trans := multiaccount.RecordFetchOutcome(&ar, limits, fetchErr); ok {
			successCount++
		} else if trans {
			transientCount++
		}
		accountResults = append(accountResults, ar)
	}

	// Sort: active first, then by Email ASCII ascending (D3, deterministic).
	// The provider-level Limits field is left nil — when Accounts is populated,
	// ProviderResult.MarshalJSON omits the top-level "limits" key entirely, and
	// the text renderer routes on len(Accounts) > 0 to its per-account view.
	multiaccount.SortAccountResults(accountResults)

	out := providers.ProviderOutput{
		Accounts: accountResults,
	}

	// Step 9 / D8: return ErrTransient only when zero accounts succeeded AND
	// at least one failure was transient (triggers orchestrator retry). A mixed
	// set (transient + auth-denied) still retries when nothing succeeded.
	if successCount == 0 && transientCount > 0 {
		return out, fmt.Errorf("%w: all %d account fetch(es) failed", providers.ErrTransient, len(accountResults))
	}

	return out, nil
}

// FetchForSwitch is read-only against the live keychain and the multi-account
// store. It performs no token refresh and no Upsert. Each non-active stored
// account's usage limits come from fetchLimitsCached — same code path as the
// reporting flow — so a recent aistat usage call's cached entries are reused
// here. This unifies the per-account read across reporting and switch decisions
// and means a 429 during the reporting path (which the cache absorbs) does not
// reappear during the very next switch invocation. Stale data up to the cache
// TTL is accepted as a trade-off (D7, revised): the failure mode of "exclude a
// rate-limited account from auto-pick" is strictly worse for the user than the
// failure mode of "use a 30-second-old number near a window boundary."
//
// If the call returns ErrAuthDenied (the stored access token has typically
// expired), the account is excluded from the returned slice and a per-account
// warn is emitted to stderr. Excluded accounts must NOT be auto-picked by
// aistat switch, because we cannot refresh them here without risking writing a
// stale pre-rotation token to the live keychain (D6 rationale).
//
// On store.List error the error is returned directly (the caller, switch.go,
// maps it to its exit code). On per-account fetch failure the account is
// excluded with a warn and is not in the returned slice. The store is never
// mutated. This guarantees aistat switch never publishes a stale refresh token.
func (c *Client) FetchForSwitch(ctx context.Context) ([]providers.AccountResult, error) {
	live, err := c.readLiveCredential(ctx)
	if err != nil {
		return nil, err
	}

	stored, err := c.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("claude: reading account store: %w", err)
	}

	// ResolveActiveUUID is read-only: at most one profile call, no writes.
	activeUUID, _ := ResolveActiveUUID(ReconcileInput{
		LiveBlob: live,
		Stored:   stored,
		LookupProfile: func(token string) (Profile, error) {
			return c.profile.Get(ctx, token)
		},
		Now: c.now(),
	})

	var nonActive []accounts.Account
	for _, acct := range stored {
		if acct.UUID != activeUUID {
			nonActive = append(nonActive, acct)
		}
	}

	budget := multiaccount.Budget(c.baseTimeout, c.perAccountBudget, len(nonActive))
	poolCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var results []providers.AccountResult
	for _, acct := range nonActive {
		limits, fetchErr := c.fetchLimitsCached(poolCtx, StoredAccessToken(acct), acct.UUID)
		if fetchErr != nil {
			if errors.Is(fetchErr, providers.ErrAuthDenied) {
				c.warnf("aistat: claude: %s: stored credential rejected (run `aistat usage` to refresh); excluded from auto-pick\n", acct.Email)
			} else {
				c.warnf("aistat: claude: %s: usage fetch failed (%s); excluded from auto-pick\n", acct.Email, fetchErr)
			}
			continue
		}
		results = append(results, providers.AccountResult{
			Email:  acct.Email,
			UUID:   acct.UUID,
			Plan:   acct.RateLimitTier,
			Active: false,
			Limits: limits,
		})
	}

	return results, nil
}

// PostSwitchVerify is a no-op for Claude — Claude has no post-switch failure modes that need a courtesy check.
func (c *Client) PostSwitchVerify(_ context.Context, _ accounts.Account) error { return nil }
