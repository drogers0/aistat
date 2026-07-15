package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/autoswitch"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/orchestrate"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/providers/claude"
	codex "github.com/drogers0/aistat/v2/internal/providers/codex"
)

// switchable is the minimal interface that both claude.Client and codex.Client satisfy.
type switchable interface {
	FetchForSwitch(ctx context.Context) ([]providers.AccountResult, error)
	ReconcileAndPersist(ctx context.Context) error
	PostSwitchVerify(ctx context.Context, target accounts.Account) error
}

// Package-level injection seams — overridden by tests.
var (
	// writeClaudeLiveBlob writes rawBlob to the live Claude credential store.
	writeClaudeLiveBlob = cred.WriteClaudeLiveBlob

	// newSwitchClient constructs the Claude client used by runSwitch.
	newSwitchClient = func(debug io.Writer, ua string, store accounts.Store) switchable {
		return claude.New(debug, ua, claude.WithStore(store))
	}

	// switchLookupActiveUUID resolves the currently-active account UUID from the
	// live Claude credential.
	switchLookupActiveUUID = realSwitchLookupActiveUUID

	// fetchLiveUsage fetches usage limits for the active Claude account's access token.
	fetchLiveUsage = realFetchLiveUsage

	// writeCodexLiveBlob writes rawBlob to the live Codex credential store.
	writeCodexLiveBlob = cred.WriteCodexLiveBlob

	// newCodexSwitchClient constructs the Codex client used by runSwitch.
	newCodexSwitchClient = func(debug io.Writer, ua string, store accounts.Store) switchable {
		return codex.New(debug, ua, codex.WithStore(store))
	}

	// switchLookupCodexActiveUUID resolves the currently-active Codex account UUID.
	switchLookupCodexActiveUUID = realSwitchLookupCodexActiveUUID

	// fetchCodexLiveUsage fetches usage limits for the active Codex account.
	fetchCodexLiveUsage = realFetchCodexLiveUsage

	// watchSleepFn is the interruptible inter-tick wait for `switch --watch`;
	// overridden by tests to bound the loop.
	watchSleepFn = sleepWithCtx
)

// resolveCodexActiveUUID reads the live Codex credential and resolves the
// currently-active account UUID. Returns ("", nil) when unknowable (no live
// blob, parse error). Called by both realSwitchLookupCodexActiveUUID and
// makeRealCodexActiveUUIDResolver.
func resolveCodexActiveUUID(ctx context.Context, stored []accounts.Account) (string, error) {
	credCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cr, err := cred.ReadCodexCredential(credCtx)
	if err != nil {
		return "", nil // ErrCodexTokenNotFound or read failure → "no active account"
	}
	return codex.ResolveActiveUUID(codex.ReconcileInput{
		LiveBlob: &cr,
		Stored:   stored,
		LookupID: func(idToken string) (string, string, error) {
			sub, email, _, err := cred.ParseCodexIDToken(idToken)
			return sub, email, err
		},
		Now: time.Now(),
	})
}

func realSwitchLookupCodexActiveUUID(ctx context.Context, stored []accounts.Account, _ io.Writer) (string, error) {
	return resolveCodexActiveUUID(ctx, stored)
}

func realFetchCodexLiveUsage(ctx context.Context, token, uuid, ua string, debug io.Writer) (map[string]providers.Limit, error) {
	return codex.New(debug, ua).FetchUsage(ctx, token, uuid)
}

func realFetchLiveUsage(ctx context.Context, token, uuid, ua string, debug io.Writer) (map[string]providers.Limit, error) {
	return claude.New(debug, ua).FetchUsage(ctx, token, uuid)
}

// realSwitchLookupActiveUUID reads the live credential and resolves the
// currently-active Claude account UUID.
func realSwitchLookupActiveUUID(ctx context.Context, stored []accounts.Account, debug io.Writer) (string, error) {
	credCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cr, err := cred.ReadClaudeCredential(credCtx)
	if err != nil {
		if errors.Is(err, cred.ErrClaudeTokenNotFound) {
			return "", nil
		}
		return "", nil // treat any read failure as "no active account"
	}
	lookupClient := claude.New(debug, claude.DefaultUserAgent(resolvedVersion()))
	return claude.ResolveActiveUUID(claude.ReconcileInput{
		LiveBlob: &cr,
		Stored:   stored,
		LookupProfile: func(token string) (claude.Profile, error) {
			return lookupClient.GetProfile(ctx, token)
		},
		Now: time.Now(),
	})
}

// switchHandle bundles all provider-specific operations for the switch dispatcher.
// Adding a new switchable provider means adding one entry to buildSwitchHandles.
type switchHandle struct {
	id             string
	store          accounts.Store
	ua             string // per-provider User-Agent; set by buildSwitchHandles
	loginHint      string // surfaced in the one-account error: e.g. "run `claude /login` to add another"
	client         switchable
	lookupActive   func(ctx context.Context, stored []accounts.Account, debug io.Writer) (string, error)
	writeLiveBlob  func(ctx context.Context, raw []byte) error
	fetchLiveUsage func(ctx context.Context, token, uuid, ua string, debug io.Writer) (map[string]providers.Limit, error)
	storedAccess   func(a accounts.Account) string // extract access token from RawBlob
}

// buildSwitchHandles opens the per-provider stores and assembles one handle
// per switchable provider. An error opening a store is fatal-closed. Both
// stores are opened unconditionally even for single-provider invocations —
// the cost is one extra keychain/file read which is acceptable given store
// opens are cheap.
func buildSwitchHandles(debugW io.Writer, version string) ([]switchHandle, error) {
	claudeStore, err := openAccountStore(debugW)
	if err != nil {
		return nil, fmt.Errorf("claude: could not open account store: %w", err)
	}
	codexStore, err := openCodexAccountStore(debugW)
	if err != nil {
		return nil, fmt.Errorf("codex: could not open account store: %w", err)
	}
	// Each provider uses its own DefaultUserAgent — do not share a single UA string.
	claudeUA := claude.DefaultUserAgent(version)
	codexUA := codex.DefaultUserAgent(version)
	return []switchHandle{
		{
			id:             "claude",
			store:          claudeStore,
			ua:             claudeUA,
			loginHint:      "run `claude /login` to add another",
			client:         newSwitchClient(debugW, claudeUA, claudeStore),
			lookupActive:   switchLookupActiveUUID,
			writeLiveBlob:  writeClaudeLiveBlob,
			fetchLiveUsage: fetchLiveUsage,
			storedAccess:   func(a accounts.Account) string { return claude.StoredAccessToken(a) },
		},
		{
			id:             "codex",
			store:          codexStore,
			ua:             codexUA,
			loginHint:      "add another ChatGPT account and run `aistat usage` to register it",
			client:         newCodexSwitchClient(debugW, codexUA, codexStore),
			lookupActive:   switchLookupCodexActiveUUID,
			writeLiveBlob:  writeCodexLiveBlob,
			fetchLiveUsage: fetchCodexLiveUsage,
			storedAccess:   func(a accounts.Account) string { return codex.StoredAccessToken(a) },
		},
	}, nil
}

func knownSwitchProvider(p string) bool {
	return p == "claude" || p == "codex"
}

func handleByID(handles []switchHandle, id string) switchHandle {
	for _, h := range handles {
		if h.id == id {
			return h
		}
	}
	panic("handleByID: unknown provider " + id) // caller already validated
}

// Auto-pick ranking constants. The 5% bucket gives hysteresis (no flapping
// between two close accounts); exhaustedPct gates accounts spent on a sustained
// window. No reset-time or relief tuning knobs — that policy is deferred to #7.
const (
	bucketPct    = 5.0
	exhaustedPct = 1.0
)

const shortKey = "five_hour"

// Dead-end messages for a provider that cannot be switched (Fprintf formats with
// h.loginHint). Shared by runSwitchSingle's one-shot checks and routeConditional's
// watch-tick short-circuit so the wording stays a single source of truth.
const (
	msgNoAccountsStored = "no accounts stored; %s\n"
	msgOnlyOneAccount   = "only one account stored; nothing to switch to (%s)\n"
)

// longKeys are the true account-wide weekly ceilings used by the exhaustion
// gate, the sustained-headroom tiebreak, and the conditional weekly trigger.
// Model-scoped windows (e.g. seven_day_sonnet) are deliberately EXCLUDED and
// stay informational only: a spent model budget blocks that one model, not
// the account, so it must not flag the account as exhausted and rank it below
// an account whose 5-hour session is already full. Unknown windows
// (window_<N>s, code_review_*) are likewise informational. longKeys is a
// superset: absent keys are skipped.
var longKeys = []string{"seven_day", "thirty_day"}

// longRemaining is the binding (minimum) RemainingPercent across present long
// windows, or 100 when none are present (no sustained constraint).
func longRemaining(l map[string]providers.Limit) float64 {
	if _, w, ok := bindingLongWindow(l); ok {
		return w.RemainingPercent
	}
	return 100.0
}

// score is the lexicographic auto-pick rank for one account. Windows are ordered
// by operational role, never blended: five_hour is the immediate throttle, the
// long windows are the sustained ceiling. An account with no windows at all
// (a successful fetch on a fully-fresh account) scores as full headroom.
type score struct {
	exhausted bool // a present long window is at ~0% remaining
	immediate int  // floor(five_hour remaining / bucketPct); absent five_hour ⇒ full
	sustained int  // floor(longRemaining / bucketPct)
	lastSeen  time.Time
}

// scoreAccount computes the auto-pick rank for an account's limits. int(x/bucketPct)
// is floor over the non-negative percent domain (no math.Floor needed).
func scoreAccount(l map[string]providers.Limit, lastSeen time.Time) score {
	long := longRemaining(l)
	r := 100.0 // absent five_hour ⇒ untouched / not-applicable ⇒ full immediate headroom
	if w, ok := l[shortKey]; ok {
		r = w.RemainingPercent
	}
	return score{
		exhausted: long < exhaustedPct,
		immediate: int(r / bucketPct),
		sustained: int(long / bucketPct),
		lastSeen:  lastSeen,
	}
}

// better reports whether score a is preferred over b:
// non-exhausted ▸ more 5h headroom ▸ more weekly runway ▸ most recently active.
func (a score) better(b score) bool {
	if a.exhausted != b.exhausted {
		return !a.exhausted
	}
	if a.immediate != b.immediate {
		return a.immediate > b.immediate
	}
	if a.sustained != b.sustained {
		return a.sustained > b.sustained
	}
	return a.lastSeen.After(b.lastSeen)
}

// lastSeenOf returns a.LastSeenAt, or the zero time when a is nil.
func lastSeenOf(a *accounts.Account) time.Time {
	if a == nil {
		return time.Time{}
	}
	return a.LastSeenAt
}

// findAccountByUUID returns a pointer to the first account in stored whose UUID
// equals uuid, or nil if not found.
func findAccountByUUID(stored []accounts.Account, uuid string) *accounts.Account {
	for i := range stored {
		if stored[i].UUID == uuid {
			return &stored[i]
		}
	}
	return nil
}

// runSwitch implements the `aistat switch` subcommand.
func runSwitch(args []string, stdout, stderr io.Writer, g globals) int {
	// 1. Flag setup + two-pass parse (mirrors usage.go).
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var toArg string
	fs.StringVar(&toArg, "to", "", "")
	var notifyFlag, watch bool
	fs.BoolVar(&notifyFlag, "notify", false, "")
	fs.BoolVar(&watch, "watch", false, "")
	fs.BoolVar(&watch, "w", false, "")
	var if5h, ifWeekly string
	fs.StringVar(&if5h, "if-above-5h", "", "")
	fs.StringVar(&ifWeekly, "if-above-weekly", "", "")
	var interval int
	fs.IntVar(&interval, "interval", 300, "")
	registerGlobalFlags(fs, &g)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(g, stdout); handled {
		return code
	}
	// Extract optional <provider> positional.
	var providerArg string
	tail := fs.Args()
	if len(tail) > 0 {
		providerArg = tail[0]
		tail = tail[1:]
	}
	// Second parse so fs.NArg() reflects only truly unconsumed positionals.
	if err := fs.Parse(tail); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(g, stdout); handled {
		return code
	}
	// Reject leftover positionals.
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument: %s\n", fs.Arg(0))
		return int(orchestrate.StatusUsageError)
	}

	// 2. Validate provider arg if given.
	if providerArg != "" && !knownSwitchProvider(providerArg) {
		fmt.Fprintf(stderr, "unknown provider %q — want claude or codex\n", providerArg)
		return int(orchestrate.StatusUsageError)
	}

	// Conditional mode (gate the switch on the active account's usage) is entered
	// by any threshold flag or --watch. A watch loop is inherently conditional —
	// an unconditional loop would flap every tick.
	conditional := if5h != "" || ifWeekly != "" || watch

	// --to targets one explicit account, incompatible with the
	// gate-then-auto-pick conditional flow.
	if toArg != "" && (if5h != "" || ifWeekly != "") {
		fmt.Fprintln(stderr, "threshold flags cannot be combined with --to")
		return int(orchestrate.StatusUsageError)
	}
	if toArg != "" && watch {
		fmt.Fprintln(stderr, "--watch cannot be combined with --to")
		return int(orchestrate.StatusUsageError)
	}
	// Detect an explicit --interval by presence, not a value sentinel, so a
	// nonsensical --interval -1 still hits the <60 floor (fs.Visit accumulates
	// across both fs.Parse calls of the two-pass parse).
	intervalSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "interval" {
			intervalSet = true
		}
	})
	if intervalSet && !watch {
		fmt.Fprintln(stderr, "--interval requires --watch")
		return int(orchestrate.StatusUsageError)
	}
	if watch && interval < 60 {
		fmt.Fprintln(stderr, "--interval must be at least 60 seconds")
		return int(orchestrate.StatusUsageError)
	}

	// Resolve thresholds once, before any provider work (fail fast on bad config).
	// Per window: explicit flag > non-empty env > built-in default.
	opts := switchOpts{conditional: conditional, notify: notifyFlag || watch}
	if conditional {
		// Threshold errors are already source-qualified ("--if-above-5h: …" for a
		// flag, "AISTAT_IF_ABOVE_5H: … (source: environment)" for env), so they
		// print bare — no "aistat:" prefix that would double up.
		fiveHourTh, err := resolveThreshold("if-above-5h", if5h, autoswitch.EnvFiveHour, autoswitch.DefaultFiveHour)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return int(orchestrate.StatusUsageError)
		}
		weeklyTh, err := resolveThreshold("if-above-weekly", ifWeekly, autoswitch.EnvWeekly, autoswitch.DefaultWeekly)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return int(orchestrate.StatusUsageError)
		}
		opts.th = autoswitch.Thresholds{FiveHour: fiveHourTh, Weekly: weeklyTh}
	}

	// 3. Setup: context, debug writer.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var debugW io.Writer
	if g.Debug {
		debugW = stderr
	}

	// 4. Build handles (fail-closed on store-open error).
	handles, err := buildSwitchHandles(debugW, resolvedVersion())
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	// 5. Watch mode: run the conditional round on a timer in the foreground,
	// kept alive by the OS service manager (launchd KeepAlive / systemd
	// Restart=always). Notifications dedup across ticks so a persistent "no
	// better account" state warns once, not every tick.
	if watch {
		opts.notifier = newDedupNotifier(sendNotification, time.Hour, time.Now)
		providerDesc := "all providers"
		if providerArg != "" {
			providerDesc = providerArg
		}
		fmt.Fprintf(stdout, "watching %s every %ds (5h %s, weekly %s)\n",
			providerDesc, interval, watchThresholdDisplay(opts.th.FiveHour), watchThresholdDisplay(opts.th.Weekly))
		watchLoop(ctx, time.Duration(interval)*time.Second, func() {
			_ = routeConditional(ctx, handles, providerArg, opts, stdout, stderr, debugW)
		}, watchSleepFn)
		return 0
	}

	// 6. One-shot route.
	if providerArg != "" {
		h := handleByID(handles, providerArg)
		return runSwitchSingle(ctx, h, toArg, opts, stdout, stderr, debugW)
	} else if toArg != "" {
		return runSwitchInferProvider(ctx, handles, toArg, opts, stdout, stderr, debugW)
	}
	return runSwitchBulk(ctx, handles, opts, stdout, stderr, debugW)
}

// resolveThreshold picks one window's trigger level: an explicit flag value
// (parsed here so the error names the flag) shadows env entirely; otherwise
// non-empty env > built-in default.
func resolveThreshold(flagName, flagVal, envKey string, def float64) (autoswitch.Threshold, error) {
	if flagVal != "" {
		t, err := autoswitch.ParseThreshold(flagVal)
		if err != nil {
			return t, fmt.Errorf("--%s: %w", flagName, err)
		}
		return t, nil
	}
	return autoswitch.ResolveOne(envKey, def, os.Getenv)
}

// routeConditional dispatches a single conditional round to the right helper:
// a provider arg targets one provider, otherwise fan out across every provider
// with ≥2 stored accounts. --to is rejected earlier in conditional mode, so the
// infer path is never reachable here. Used by the `--watch` tick so a looped
// switch behaves exactly like the one-shot. A scoped provider with <2 stored
// accounts is short-circuited (mirroring runSwitchBulk's ≥2 filter) so watch
// does not run a per-tick reconcile + usage fetch it can never act on.
func routeConditional(ctx context.Context, handles []switchHandle, providerArg string, opts switchOpts, stdout, stderr, debugW io.Writer) int {
	if providerArg == "" {
		return runSwitchBulk(ctx, handles, opts, stdout, stderr, debugW)
	}
	h := handleByID(handles, providerArg)
	stored, err := h.store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s: could not list accounts: %s\n", h.id, err)
		return int(orchestrate.StatusAnyFailed)
	}
	if len(stored) < 2 {
		// Same dead ends as runSwitchSingle — the guard just reaches them without
		// the per-tick reconcile + usage fetch.
		if len(stored) == 0 {
			fmt.Fprintf(stderr, msgNoAccountsStored, h.loginHint)
		} else {
			fmt.Fprintf(stderr, msgOnlyOneAccount, h.loginHint)
		}
		return 0
	}
	return runSwitchSingle(ctx, h, "", opts, stdout, stderr, debugW)
}

// conditionalResult carries the outcome of the conditional threshold gate back
// to runSwitchSingle. When proceed is false the caller returns exitCode
// verbatim; when true, reason/limits feed the downstream "already on best"
// comparison. stored is always the post-reconcile store re-read so the caller
// adopts the refreshed slice (a switchable's List returns a fresh header each
// call).
type conditionalResult struct {
	stored   []accounts.Account
	reason   string
	limits   map[string]providers.Limit
	proceed  bool
	exitCode int
}

// evaluateConditional runs the threshold gate for the active account: reconcile
// the live credential back into the store, re-read it, fetch the active
// account's usage, and decide whether a switch should proceed. It performs no
// switch and mutates no live credential itself.
func (h switchHandle) evaluateConditional(ctx context.Context, stored []accounts.Account, activeUUID string, opts switchOpts, stdout, stderr, debugW io.Writer) conditionalResult {
	// The active account's stored blob goes stale between polls — the upstream
	// CLI refreshes the live credential in place and only `usage`'s reconcile
	// syncs it back. Reconcile here (best effort) and re-read the store so
	// storedAccess below carries the current token instead of 401-looping until
	// someone runs `aistat usage`.
	_ = h.client.ReconcileAndPersist(ctx)
	if refreshed, err := h.store.List(ctx); err == nil {
		stored = refreshed
	}
	activeAcct := findAccountByUUID(stored, activeUUID)
	if activeAcct == nil {
		fmt.Fprintf(stderr, "aistat: %s: cannot determine the active account; skipping conditional switch\n", h.id)
		return conditionalResult{stored: stored, exitCode: int(orchestrate.StatusAnyFailed)}
	}
	limits, err := h.fetchLiveUsage(ctx, h.storedAccess(*activeAcct), activeAcct.UUID, h.ua, debugW)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s: usage fetch for active account failed: %s\n", h.id, err)
		return conditionalResult{stored: stored, exitCode: int(orchestrate.StatusAnyFailed)}
	}
	reason, triggered := triggerReason(limits, opts.th)
	if !triggered {
		fmt.Fprintf(stdout, "no switch needed (%s)\n", usageSummary(limits))
		return conditionalResult{stored: stored, exitCode: 0}
	}
	return conditionalResult{stored: stored, reason: reason, limits: limits, proceed: true}
}

// runSwitchSingle performs a switch for a single provider handle.
// It contains the existing pick-target → write → reconcile logic.
func runSwitchSingle(ctx context.Context, h switchHandle, toArg string, opts switchOpts, stdout, stderr, debugW io.Writer) int {
	stored, err := h.store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s: could not list accounts: %s\n", h.id, err)
		return int(orchestrate.StatusUsageError)
	}

	activeUUID, _ := h.lookupActive(ctx, stored, debugW)
	prevEmail := "none"
	if activeAcct := findAccountByUUID(stored, activeUUID); activeAcct != nil {
		prevEmail = activeAcct.Email
	}

	// condReason is non-empty iff the conditional gate evaluated and triggered;
	// condActiveLimits caches the active account's usage from the gate so the
	// already-on-best comparison does not refetch.
	var condReason string
	var condActiveLimits map[string]providers.Limit

	// notifyNoBetter fires the conditional-switch warning at the two "threshold
	// hit but nothing better" dead ends so the text cannot drift between them.
	notifyNoBetter := func() {
		if opts.notify && condReason != "" {
			notifyBestEffort(ctx, opts.notifier, h.id, condReason+", no better account available", stderr)
		}
	}

	// In conditional mode the invocation is well-formed by the time provider
	// work starts, so fetch/write failures are runtime failures (exit 1), not
	// usage errors (exit 2) — the documented contract for timer-driven polls.
	failCode := int(orchestrate.StatusUsageError)
	if opts.conditional {
		failCode = int(orchestrate.StatusAnyFailed)
	}

	var target accounts.Account

	if toArg != "" {
		// Explicit --to mode: resolve by email substring or UUID prefix.
		matches := matchAccounts(toArg, stored)
		switch len(matches) {
		case 0:
			fmt.Fprintf(stderr, "no stored account matches %q\n", toArg)
			return int(orchestrate.StatusUsageError)
		case 1:
			// fall through
		default:
			fmt.Fprintf(stderr, "multiple stored accounts match %q, disambiguate by uuid\n", toArg)
			return int(orchestrate.StatusUsageError)
		}
		target = matches[0]
		if target.UUID == activeUUID {
			fmt.Fprintf(stdout, "already on %s\n", target.Email)
			return 0
		}
	} else {
		// Auto-pick mode: fetch usage for non-active accounts.
		if len(stored) == 0 {
			fmt.Fprintf(stderr, msgNoAccountsStored, h.loginHint)
			if opts.conditional {
				// A timer-driven poll with nothing stored is "nothing to do", not misuse.
				return 0
			}
			return int(orchestrate.StatusUsageError)
		}

		if opts.conditional {
			r := h.evaluateConditional(ctx, stored, activeUUID, opts, stdout, stderr, debugW)
			if !r.proceed {
				return r.exitCode
			}
			stored, condReason, condActiveLimits = r.stored, r.reason, r.limits
		}

		if len(stored) == 1 && stored[0].UUID == activeUUID {
			notifyNoBetter()
			fmt.Fprintf(stderr, msgOnlyOneAccount, h.loginHint)
			if opts.conditional {
				// A timer-driven poll with no alternative is "nothing to do", not misuse.
				return 0
			}
			return int(orchestrate.StatusUsageError)
		}

		candidates, fetchErr := h.client.FetchForSwitch(ctx)
		if fetchErr != nil {
			fmt.Fprintf(stderr, "aistat: %s: auto-pick usage fetch failed: %s\n", h.id, fetchErr)
			return failCode
		}

		if len(candidates) == 0 {
			fmt.Fprintln(stderr, "auto-pick failed: no accounts produced usable usage data; try `aistat switch --to <email>`")
			return failCode
		}

		// Rank candidates: non-exhausted ▸ more 5h headroom ▸ more weekly runway ▸ most recent.
		best := candidates[0]
		bestAcct := findAccountByUUID(stored, best.UUID)
		bestScore := scoreAccount(best.Limits, lastSeenOf(bestAcct))
		for _, c := range candidates[1:] {
			cAcct := findAccountByUUID(stored, c.UUID)
			cScore := scoreAccount(c.Limits, lastSeenOf(cAcct))
			if cScore.better(bestScore) {
				best, bestAcct, bestScore = c, cAcct, cScore
			}
		}

		// Compare best candidate with active account ("already on best" check).
		// fetchLiveUsage is read-only: no store mutation. The conditional gate
		// above already fetched this when it triggered — reuse it instead of
		// fetching twice.
		if activeAcct := findAccountByUUID(stored, activeUUID); activeAcct != nil {
			activeLimits, liveErr := condActiveLimits, error(nil)
			if activeLimits == nil {
				activeLimits, liveErr = h.fetchLiveUsage(ctx, h.storedAccess(*activeAcct), activeAcct.UUID, h.ua, debugW)
			}
			if liveErr == nil {
				if !bestScore.better(scoreAccount(activeLimits, activeAcct.LastSeenAt)) {
					notifyNoBetter()
					fmt.Fprintf(stdout, "already on best account (%s)\n", prevEmail)
					return 0
				}
			}
		}

		if bestAcct == nil {
			fmt.Fprintln(stderr, "auto-pick failed: no accounts produced usable usage data; try `aistat switch --to <email>`")
			return failCode
		}
		target = *bestAcct
	}

	// Write target's blob to the live credential. This is the first mutation.
	writeCtx, writeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer writeCancel()
	if err := h.writeLiveBlob(writeCtx, []byte(target.RawBlob)); err != nil {
		fmt.Fprintf(stderr, "aistat: %s: write to live credential failed: %s\n", h.id, err)
		return failCode
	}

	// Post-write reconcile so the store's LastSeenAt reflects the new active.
	_ = h.client.ReconcileAndPersist(ctx)

	fmt.Fprintf(stdout, "switched to %s (uuid %s); was %s\n", target.Email, target.UUID, prevEmail)
	if opts.notify {
		msg := "switched to " + target.Email
		if condReason != "" {
			msg += " (" + condReason + ")"
		}
		notifyBestEffort(ctx, opts.notifier, h.id, msg, stderr)
	}
	if err := h.client.PostSwitchVerify(ctx, target); err != nil {
		if errors.Is(err, providers.ErrAuthDenied) {
			fmt.Fprintf(stderr, "aistat: %s: switched-to account's tokens are not usable: %s\n", h.id, err)
		}
		// Other errors (network/etc.) are silently ignored — the switch succeeded; verify is courtesy.
	}
	return 0
}

// runSwitchBulk fans out switch across all providers with ≥2 stored accounts.
func runSwitchBulk(ctx context.Context, handles []switchHandle, opts switchOpts, stdout, stderr, debugW io.Writer) int {
	var eligible []switchHandle
	for _, h := range handles {
		stored, err := h.store.List(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "aistat: %s: could not list accounts: %s\n", h.id, err)
			continue
		}
		if len(stored) >= 2 {
			eligible = append(eligible, h)
		}
	}
	if len(eligible) == 0 {
		fmt.Fprintln(stderr, "no providers have multiple stored accounts; nothing to switch")
		return 0
	}
	exitCode := 0
	for _, h := range eligible {
		fmt.Fprintf(stdout, "[%s]\n", h.id)
		code := runSwitchSingle(ctx, h, "", opts, stdout, stderr, debugW)
		if code != 0 {
			exitCode = code
		}
	}
	return exitCode
}

// runSwitchInferProvider handles `aistat switch --to <id>` with no provider.
// It searches all stores for <id> and dispatches to runSwitchSingle when
// exactly one provider matches. Ambiguous cross-provider matches exit 2.
func runSwitchInferProvider(ctx context.Context, handles []switchHandle, toArg string, opts switchOpts, stdout, stderr, debugW io.Writer) int {
	type match struct {
		h    switchHandle
		acct accounts.Account
	}
	var matches []match
	var listErrs []string
	for _, h := range handles {
		stored, err := h.store.List(ctx)
		if err != nil {
			listErrs = append(listErrs, fmt.Sprintf("aistat: %s: could not list accounts: %s", h.id, err))
			continue
		}
		for _, m := range matchAccounts(toArg, stored) {
			matches = append(matches, match{h, m})
		}
	}
	if len(matches) == 0 {
		for _, e := range listErrs {
			fmt.Fprintln(stderr, e)
		}
		fmt.Fprintf(stderr, "no stored account matches %q\n", toArg)
		return int(orchestrate.StatusUsageError)
	}
	if len(matches) == 1 {
		return runSwitchSingle(ctx, matches[0].h, toArg, opts, stdout, stderr, debugW)
	}
	// More than one match — check if they're from different providers.
	providerSet := map[string]bool{}
	for _, m := range matches {
		providerSet[m.h.id] = true
	}
	if len(providerSet) > 1 {
		fmt.Fprintf(stderr, "multiple providers match %q; specify provider: aistat switch <provider> --to %s\n", toArg, toArg)
		return int(orchestrate.StatusUsageError)
	}
	// All matches in the same provider — single-provider disambiguation.
	fmt.Fprintf(stderr, "multiple stored accounts match %q, disambiguate by uuid\n", toArg)
	return int(orchestrate.StatusUsageError)
}
