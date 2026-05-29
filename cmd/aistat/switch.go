package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/orchestrate"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/providers/claude"
)

// switchClaudeClient is the minimal interface runSwitch needs from the Claude provider.
type switchClaudeClient interface {
	FetchForSwitch(ctx context.Context) ([]providers.AccountResult, error)
	ReconcileAndPersist(ctx context.Context) error
}

// Package-level injection seams — overridden by tests.
var (
	// writeClaudeLiveBlob writes rawBlob to the live Claude credential store.
	// Tests replace it to capture the bytes written without touching the keychain.
	writeClaudeLiveBlob = cred.WriteClaudeLiveBlob

	// newSwitchClient constructs the Claude client used by runSwitch.
	// Tests replace it to return a stub with canned FetchForSwitch results.
	newSwitchClient = func(debug io.Writer, ua string, store accounts.Store) switchClaudeClient {
		return claude.New(debug, ua, claude.WithStore(store))
	}

	// switchLookupActiveUUID resolves the currently-active account UUID from the
	// live credential. Returns ("", nil) when no live credential exists.
	// Tests replace it to return a canned UUID without reading the real keychain.
	switchLookupActiveUUID = realSwitchLookupActiveUUID

	// fetchLiveUsage fetches usage limits for the active account's access token.
	// Used by the D13 auto-pick "already on best account" comparison to determine
	// whether the active account has more headroom than the best non-active candidate.
	// Tests replace it to return canned data without making HTTP calls.
	//
	// Note: this mirrors claude.Client.fetchLimits. A dedicated method on claude.Client
	// would eliminate the duplication, but adding one would require touching that package.
	// The seam is kept here so the comparison stays read-only (no store mutation) and
	// tests can inject per-token canned limits.
	fetchLiveUsage = realFetchLiveUsage
)

// realFetchLiveUsage fetches usage limits for the given access token via the
// Claude provider's FetchUsage method. Called in auto-pick mode to read the
// active account's current headroom for the D13 "already on best" comparison.
// The uuid is passed through so the call shares the same cached path used by
// the reporting flow — a recent aistat usage call's cache entry for the active
// account is reused here, sparing the live endpoint a request and surviving a
// transient rate-limit on this account.
func realFetchLiveUsage(ctx context.Context, token, uuid, ua string, debug io.Writer) (map[string]providers.Limit, error) {
	return claude.New(debug, ua).FetchUsage(ctx, token, uuid)
}

// realSwitchLookupActiveUUID reads the live credential and resolves the
// currently-active account UUID. Returns ("", nil) when no live credential exists
// or when the active account cannot be determined.
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

// fiveHourBucket returns the D13 auto-pick sort bucket for a limits map.
// bucket = floor(five_hour.RemainingPercent / 5). Returns -1 when five_hour is absent.
func fiveHourBucket(limits map[string]providers.Limit) int {
	if limits == nil {
		return -1
	}
	win, ok := limits["five_hour"]
	if !ok {
		return -1
	}
	return int(math.Floor(win.RemainingPercent / 5))
}

// switchBetter reports whether candidate a is preferred over b for D13 auto-pick.
// Primary sort: fiveHourBucket descending. Tiebreak: lastSeen descending (more recent wins).
func switchBetter(a providers.AccountResult, aLastSeen time.Time, b providers.AccountResult, bLastSeen time.Time) bool {
	ba := fiveHourBucket(a.Limits)
	bb := fiveHourBucket(b.Limits)
	if ba != bb {
		return ba > bb
	}
	return aLastSeen.After(bLastSeen)
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

// runSwitch implements the `aistat switch` subcommand per D13. It rewrites
// the live Claude credential to a stored account's blob — no browser round-trip.
func runSwitch(args []string, stdout, stderr io.Writer, g globals) int {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var toArg string
	fs.StringVar(&toArg, "to", "", "")
	registerGlobalFlags(fs, &g)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(g, stdout); handled {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var debugW io.Writer
	if g.Debug {
		debugW = stderr
	}

	// Step 1: Open the account store (fail-closed per D13 ordering).
	store, err := openAccountStore(debugW)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not open account store: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	// Step 2: List stored accounts (needed for both modes before any mutation).
	stored, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not open account store: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	// Step 3: Identify the previously-active account (read-only, no store mutation).
	activeUUID, _ := switchLookupActiveUUID(ctx, stored, debugW)
	prevEmail := "none"
	if activeAcct := findAccountByUUID(stored, activeUUID); activeAcct != nil {
		prevEmail = activeAcct.Email
	}

	// Create the Claude client upfront — needed in both modes (FetchForSwitch
	// for auto-pick, ReconcileAndPersist post-write for both modes).
	client := newSwitchClient(debugW, claude.DefaultUserAgent(resolvedVersion()), store)

	// Step 4: Pick target account.
	var target accounts.Account

	if toArg != "" {
		// Explicit --to mode: resolve by email substring or UUID prefix (D13 matching rule).
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
		// Auto-pick mode: fetch usage for non-active accounts and rank by D13 comparator.
		if len(stored) == 0 {
			fmt.Fprintf(stderr, "aistat: claude: %s\n", cred.ErrClaudeTokenNotFound)
			return int(orchestrate.StatusUsageError)
		}
		if len(stored) == 1 && stored[0].UUID == activeUUID {
			fmt.Fprintln(stderr, "only one account stored; nothing to switch to (run `claude /login` to add another)")
			return int(orchestrate.StatusUsageError)
		}

		candidates, fetchErr := client.FetchForSwitch(ctx)
		if fetchErr != nil {
			fmt.Fprintf(stderr, "aistat: claude: auto-pick usage fetch failed: %s\n", fetchErr)
			return int(orchestrate.StatusUsageError)
		}

		if len(candidates) == 0 {
			fmt.Fprintln(stderr, "auto-pick failed: no accounts produced usable usage data; try `aistat switch --to <email>`")
			return int(orchestrate.StatusUsageError)
		}

		// Rank candidates by bucketed comparator (D13): bucket desc, LastSeenAt desc within bucket.
		best := candidates[0]
		bestAcct := findAccountByUUID(stored, best.UUID)
		for _, c := range candidates[1:] {
			cAcct := findAccountByUUID(stored, c.UUID)
			var cLastSeen, bestLastSeen time.Time
			if cAcct != nil {
				cLastSeen = cAcct.LastSeenAt
			}
			if bestAcct != nil {
				bestLastSeen = bestAcct.LastSeenAt
			}
			if switchBetter(c, cLastSeen, best, bestLastSeen) {
				best = c
				bestAcct = cAcct
			}
		}

		// Compare best candidate with active account to check "already on best" (D13).
		// fetchLiveUsage is read-only: no store mutation regardless of result.
		if activeAcct := findAccountByUUID(stored, activeUUID); activeAcct != nil {
			activeLimits, liveErr := fetchLiveUsage(ctx, activeAcct.AccessToken(), activeAcct.UUID, claude.DefaultUserAgent(resolvedVersion()), debugW)
			if liveErr == nil {
				activeAR := providers.AccountResult{Limits: activeLimits}
				var bestLastSeen time.Time
				if bestAcct != nil {
					bestLastSeen = bestAcct.LastSeenAt
				}
				// Active is "already best" when it ranks >= the best non-active candidate.
				if !switchBetter(best, bestLastSeen, activeAR, activeAcct.LastSeenAt) {
					fmt.Fprintf(stdout, "already on best account (%s)\n", prevEmail)
					return 0
				}
			}
		}

		if bestAcct == nil {
			// Should not occur: FetchForSwitch populates UUIDs from stored accounts.
			fmt.Fprintln(stderr, "auto-pick failed: no accounts produced usable usage data; try `aistat switch --to <email>`")
			return int(orchestrate.StatusUsageError)
		}
		target = *bestAcct
	}

	// Step 5: Write target's blob to the live keychain. This is the first mutation.
	// On failure the multi-account store is untouched.
	writeCtx, writeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer writeCancel()
	if err := writeClaudeLiveBlob(writeCtx, []byte(target.RawBlob)); err != nil {
		fmt.Fprintf(stderr, "aistat: claude: write to live credential failed: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	// Step 6: Post-write reconcile so the store's LastSeenAt reflects the new active.
	// A crash here leaves LastSeenAt slightly stale; the next `aistat usage` self-corrects.
	_ = client.ReconcileAndPersist(ctx)

	fmt.Fprintf(stdout, "switched to %s (uuid %s); was %s\n", target.Email, target.UUID, prevEmail)
	return 0
}
