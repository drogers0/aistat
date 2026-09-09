package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/address"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/orchestrate"
	"github.com/drogers0/aistat/v2/internal/providers/claude"
)

// staleThreshold is the duration after which a stored account is considered stale
// (last_seen_at < now - staleThreshold). Exactly 30 days is NOT stale.
const staleThreshold = 30 * 24 * time.Hour

// openAccountStore is the function used by runAccountsSubcommand to open the
// Claude platform account store. Replaced in tests to inject open failures.
var openAccountStore = func(debug io.Writer) (accounts.Store, error) {
	return accounts.OpenStore(accounts.ProviderClaude, accounts.WithDebug(debug))
}

// openCodexAccountStore opens the Codex platform account store.
// Replaced in tests to inject a MemoryStore.
var openCodexAccountStore = func(debug io.Writer) (accounts.Store, error) {
	return accounts.OpenStore(accounts.ProviderCodex, accounts.WithDebug(debug))
}

// providerStore groups per-provider store access and active-account resolution
// for the accounts subcommand dispatcher.
type providerStore struct {
	id             string
	store          accounts.Store
	activeResolver func(ctx context.Context, stored []accounts.Account) (string, error)
	logoutHint     string // e.g. "use 'claude /logout' first"
}

// accountSummary is the per-account JSON schema for accounts list.
type accountSummary struct {
	Email            string `json:"email"`
	UUID             string `json:"uuid"`
	Plan             string `json:"plan"`
	Stale            bool   `json:"stale"`
	Address          string `json:"address,omitempty"`
	OrganizationName string `json:"organization_name,omitempty"`
	OrganizationType string `json:"organization_type,omitempty"`
}

// runAccountsSubcommand is the entry point called from main's dispatch table.
// It opens both provider stores (fail-closed on error) and delegates to runAccounts.
func runAccountsSubcommand(args []string, stdout, stderr io.Writer, g globals) int {
	var debugW io.Writer
	if g.Debug {
		debugW = stderr
	}
	claudeStore, err := openAccountStore(debugW)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not open account store: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}
	codexStore, err := openCodexAccountStore(debugW)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: codex: could not open account store: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	stores := []providerStore{
		{
			id:             "claude",
			store:          claudeStore,
			activeResolver: makeRealActiveKeyResolver(g, stderr),
			logoutHint:     "use 'claude /logout' first",
		},
		{
			id:             "codex",
			store:          codexStore,
			activeResolver: makeRealCodexActiveKeyResolver(g, stderr),
			logoutHint:     "log out of the Codex app first",
		},
	}
	return runAccounts(args, stdout, stderr, g, stores)
}

// runAccounts is the testable inner implementation of the accounts subcommand.
// Tests inject MemoryStore-backed providerStores directly.
func runAccounts(
	args []string, stdout, stderr io.Writer, g globals,
	providerStores []providerStore,
) int {
	// Parse any global flags that appear before "list"/"remove".
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerGlobalFlags(fs, &g)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(g, stdout); handled {
		return code
	}

	remaining := fs.Args()
	if len(remaining) == 0 {
		fmt.Fprintf(stderr, "unknown subcommand \"\" \u2014 want \"list\" or \"remove\"\n")
		return int(orchestrate.StatusUsageError)
	}

	sub, subArgs := remaining[0], remaining[1:]
	switch sub {
	case "list":
		return runAccountsList(subArgs, stdout, stderr, &g, providerStores)
	case "remove":
		return runAccountsRemove(subArgs, stdout, stderr, &g, providerStores)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q \u2014 want \"list\" or \"remove\"\n", sub)
		return int(orchestrate.StatusUsageError)
	}
}

// runAccountsList lists stored accounts, sorted by email.
// Accepts an optional <provider> positional to scope to one provider.
// Default output is JSON; text with -h/--human.
func runAccountsList(
	subArgs []string, stdout, stderr io.Writer, g *globals,
	stores []providerStore,
) int {
	lfs := flag.NewFlagSet("accounts list", flag.ContinueOnError)
	lfs.SetOutput(io.Discard)
	registerGlobalFlags(lfs, g)
	if err := lfs.Parse(subArgs); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(*g, stdout); handled {
		return code
	}

	// Extract optional <provider> positional.
	var providerArg string
	tail := lfs.Args()
	if len(tail) > 0 {
		providerArg = tail[0]
		tail = tail[1:]
	}
	// Re-parse tail so lfs.NArg() reflects only truly unconsumed args.
	if err := lfs.Parse(tail); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(*g, stdout); handled {
		return code
	}
	if lfs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument: %s\n", lfs.Arg(0))
		return int(orchestrate.StatusUsageError)
	}

	// Scope to requested provider, or use all.
	var selectedStores []providerStore
	if providerArg != "" {
		found := false
		for _, ps := range stores {
			if ps.id == providerArg {
				selectedStores = []providerStore{ps}
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(stderr, "unknown provider %q\n", providerArg)
			return int(orchestrate.StatusUsageError)
		}
	} else {
		selectedStores = stores
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	now := time.Now()
	bulk := len(selectedStores) > 1

	if g.Human {
		for i, ps := range selectedStores {
			if bulk {
				fmt.Fprintf(stdout, "=== %s ===\n", ps.id)
			}
			accts, err := ps.store.List(ctx)
			if err != nil {
				fmt.Fprintf(stderr, "aistat: %s: could not list accounts: %s\n", ps.id, err)
				continue
			}
			sortAccountsForList(accts)
			for _, a := range accts {
				label := address.AddressFor(accts, a)
				if label == "" {
					label = a.Email
				}
				line := fmt.Sprintf("%s  %s  %s", label, a.UUID, a.RateLimitTier)
				if a.LastSeenAt.Before(now.Add(-staleThreshold)) {
					line += "  (stale)"
				}
				fmt.Fprintln(stdout, line)
			}
			if bulk && i < len(selectedStores)-1 {
				fmt.Fprintln(stdout) // blank line between sections
			}
		}
		return 0
	}

	// JSON mode (default).
	result := make(map[string][]accountSummary, len(selectedStores))
	for _, ps := range selectedStores {
		accts, err := ps.store.List(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "aistat: %s: could not list accounts: %s\n", ps.id, err)
			result[ps.id] = []accountSummary{}
			continue
		}
		sortAccountsForList(accts)
		summaries := make([]accountSummary, 0, len(accts))
		for _, a := range accts {
			summaries = append(summaries, accountSummary{
				Email:            a.Email,
				UUID:             a.UUID,
				Plan:             a.RateLimitTier,
				Stale:            a.LastSeenAt.Before(now.Add(-staleThreshold)),
				Address:          address.AddressFor(accts, a),
				OrganizationName: a.OrganizationName,
				OrganizationType: a.OrganizationType,
			})
		}
		result[ps.id] = summaries
	}
	data, _ := json.Marshal(result)
	fmt.Fprintln(stdout, string(data))
	return 0
}

// runAccountsRemove removes a stored account identified by the shared address
// matcher, with active-account protection.
// An optional second positional specifies the provider; without it, the
// provider is inferred by searching all stores (D7: unique cross-provider
// match proceeds; ambiguous exits 2).
func runAccountsRemove(
	subArgs []string, stdout, stderr io.Writer, g *globals,
	stores []providerStore,
) int {
	rfs := flag.NewFlagSet("accounts remove", flag.ContinueOnError)
	rfs.SetOutput(io.Discard)
	registerGlobalFlags(rfs, g)
	if err := rfs.Parse(subArgs); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(*g, stdout); handled {
		return code
	}

	ids := rfs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "accounts remove requires an address, organization slug, email, or UUID prefix argument")
		return int(orchestrate.StatusUsageError)
	}
	arg := ids[0]

	// Optional <provider> as second positional.
	var providerName string
	if len(ids) >= 2 {
		providerName = ids[1]
	}
	if len(ids) > 2 {
		fmt.Fprintf(stderr, "unexpected argument: %s\n", ids[2])
		return int(orchestrate.StatusUsageError)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Resolve the provider store to remove from.
	ps, target, ok := resolveRemoveTarget(ctx, stores, arg, providerName, stderr)
	if !ok {
		return int(orchestrate.StatusUsageError)
	}

	// Active-protection: resolve read-only, no store mutation.
	storedAll, err := ps.store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s: could not list accounts: %s\n", ps.id, err)
		return int(orchestrate.StatusUsageError)
	}
	activeKey, resolveErr := ps.activeResolver(ctx, storedAll)
	if resolveErr != nil {
		fmt.Fprintf(stderr, "aistat: %s: could not verify active account: %s (retry, or %s if you want to remove the active account)\n",
			ps.id, resolveErr, ps.logoutHint)
		return int(orchestrate.StatusUsageError)
	}
	if activeKey != "" && activeKey == target.Key() {
		fmt.Fprintf(stderr, "cannot remove currently active account (%s) \u2014 %s\n", address.AccountLabel(storedAll, target), ps.logoutHint)
		return int(orchestrate.StatusUsageError)
	}

	if err := ps.store.Delete(ctx, target.Key()); err != nil {
		fmt.Fprintf(stderr, "aistat: %s: could not remove account: %s\n", ps.id, err)
		return int(orchestrate.StatusUsageError)
	}

	fmt.Fprintf(stdout, "removed %s\n", address.AccountLabel(storedAll, target))
	return 0
}

// resolveRemoveTarget finds the providerStore and Account to remove.
// When providerName is given, it scopes to that provider only.
// When omitted, it searches all stores (D7 inference).
// Returns (ps, target, true) on success or emits an error and returns false.
func resolveRemoveTarget(ctx context.Context, stores []providerStore, arg, providerName string, stderr io.Writer) (providerStore, accounts.Account, bool) {
	if providerName != "" {
		// Explicit provider: find matching store.
		for _, ps := range stores {
			if ps.id == providerName {
				stored, err := ps.store.List(ctx)
				if err != nil {
					fmt.Fprintf(stderr, "aistat: %s: could not list accounts: %s\n", ps.id, err)
					return providerStore{}, accounts.Account{}, false
				}
				matches := address.Match(stored, arg)
				switch len(matches) {
				case 0:
					fmt.Fprintf(stderr, "no stored account matches %q\n", arg)
					return providerStore{}, accounts.Account{}, false
				case 1:
					return ps, matches[0], true
				default:
					writeAccountAmbiguity(stderr, stored, arg, matches)
					return providerStore{}, accounts.Account{}, false
				}
			}
		}
		fmt.Fprintf(stderr, "unknown provider %q\n", providerName)
		return providerStore{}, accounts.Account{}, false
	}

	// Infer provider by searching all stores (D7). List errors are collected and
	// emitted only when no match is found, mirroring runSwitchInferProvider's pattern.
	type candidate struct {
		ps     providerStore
		acct   accounts.Account
		stored []accounts.Account
	}
	var candidates []candidate
	var listErrs []string
	for _, ps := range stores {
		stored, err := ps.store.List(ctx)
		if err != nil {
			listErrs = append(listErrs, fmt.Sprintf("aistat: %s: could not list accounts: %s", ps.id, err))
			continue
		}
		for _, m := range address.Match(stored, arg) {
			candidates = append(candidates, candidate{ps: ps, acct: m, stored: stored})
		}
	}

	switch len(candidates) {
	case 0:
		for _, e := range listErrs {
			fmt.Fprintln(stderr, e)
		}
		fmt.Fprintf(stderr, "no stored account matches %q\n", arg)
		return providerStore{}, accounts.Account{}, false
	case 1:
		return candidates[0].ps, candidates[0].acct, true
	default:
		// Check if all matches are in the same provider.
		providerSet := map[string]bool{}
		for _, c := range candidates {
			providerSet[c.ps.id] = true
		}
		if len(providerSet) > 1 {
			fmt.Fprintf(stderr, "multiple providers have an account matching %q; specify provider: aistat accounts remove %s <provider>\n", arg, arg)
			return providerStore{}, accounts.Account{}, false
		}
		// All in the same provider — use the canonical ambiguity labels.
		matches := make([]accounts.Account, 0, len(candidates))
		for _, candidate := range candidates {
			matches = append(matches, candidate.acct)
		}
		writeAccountAmbiguity(stderr, candidates[0].stored, arg, matches)
		return providerStore{}, accounts.Account{}, false
	}
}

func sortAccountsForList(accountsToSort []accounts.Account) {
	sort.Slice(accountsToSort, func(i, j int) bool {
		if accountsToSort[i].Email != accountsToSort[j].Email {
			return accountsToSort[i].Email < accountsToSort[j].Email
		}
		return accountsToSort[i].Key() < accountsToSort[j].Key()
	})
}

func writeAccountAmbiguity(w io.Writer, stored []accounts.Account, argument string, matches []accounts.Account) {
	labels := make([]string, 0, len(matches))
	for _, account := range matches {
		labels = append(labels, address.AccountLabel(stored, account))
	}
	sort.Strings(labels)
	fmt.Fprintf(w, "multiple stored accounts match %q; use one of: %s\n", argument, strings.Join(labels, ", "))
}

// makeRealActiveKeyResolver returns a resolveActiveKey function backed by the
// live Claude credential. Used by runAccountsSubcommand; tests inject a stub instead.
func makeRealActiveKeyResolver(g globals, stderr io.Writer) func(context.Context, []accounts.Account) (string, error) {
	var debugW io.Writer
	if g.Debug {
		debugW = stderr
	}
	client := claude.New(debugW, claude.DefaultUserAgent(resolvedVersion()))
	return func(ctx context.Context, stored []accounts.Account) (string, error) {
		credCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		c, credErr := cred.ReadClaudeCredential(credCtx)
		if credErr != nil {
			if errors.Is(credErr, cred.ErrClaudeTokenNotFound) {
				return "", nil
			}
			return "", nil
		}
		return claude.ResolveActiveKey(claude.ReconcileInput{
			LiveBlob: &c,
			Stored:   stored,
			LookupProfile: func(token string) (claude.Profile, error) {
				return client.GetProfile(ctx, token)
			},
			Now: time.Now(),
		})
	}
}

// makeRealCodexActiveKeyResolver returns a resolveActiveKey function backed
// by the live Codex credential. Delegates to resolveCodexActiveKey (switch.go).
func makeRealCodexActiveKeyResolver(_ globals, _ io.Writer) func(context.Context, []accounts.Account) (string, error) {
	return func(ctx context.Context, stored []accounts.Account) (string, error) {
		return resolveCodexActiveKey(ctx, stored)
	}
}
