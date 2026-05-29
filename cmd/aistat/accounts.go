package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/orchestrate"
	"github.com/drogers0/aistat/v2/internal/providers/claude"
)

// staleThreshold is the duration after which a stored account is considered stale
// (last_seen_at < now - staleThreshold). Exactly 30 days is NOT stale.
const staleThreshold = 30 * 24 * time.Hour

// uuidishRe matches argument strings that look like UUID prefixes: 8+ hex digits and dashes.
var uuidishRe = regexp.MustCompile(`(?i)^[0-9a-f\-]{8,}$`)

// openAccountStore is the function used by runAccountsSubcommand to open the
// platform account store. Replaced in tests to inject open failures.
var openAccountStore = func(debug io.Writer) (accounts.Store, error) {
	return accounts.OpenStore(accounts.WithDebug(debug))
}

// runAccountsSubcommand is the entry point called from main's dispatch table.
// It opens the platform store (fail-closed on error) and delegates to runAccounts.
func runAccountsSubcommand(args []string, stdout, stderr io.Writer, g globals) int {
	var debugW io.Writer
	if g.Debug {
		debugW = stderr
	}
	store, err := openAccountStore(debugW)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not open account store: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}
	resolver := makeRealActiveUUIDResolver(g, stderr)
	return runAccounts(args, stdout, stderr, g, store, resolver)
}

// runAccounts is the testable inner implementation of the accounts subcommand.
// Tests inject a MemoryStore and a stub resolver directly.
func runAccounts(
	args []string, stdout, stderr io.Writer, g globals,
	store accounts.Store,
	resolveActiveUUID func(ctx context.Context, stored []accounts.Account) (string, error),
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
		return runAccountsList(subArgs, stdout, stderr, &g, store)
	case "remove":
		return runAccountsRemove(subArgs, stdout, stderr, &g, store, resolveActiveUUID)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q \u2014 want \"list\" or \"remove\"\n", sub)
		return int(orchestrate.StatusUsageError)
	}
}

// runAccountsList lists stored Claude accounts sorted by email, with a (stale)
// suffix for accounts whose last_seen_at is more than 30 days ago.
func runAccountsList(
	subArgs []string, stdout, stderr io.Writer, g *globals,
	store accounts.Store,
) int {
	// Parse any flags that appear after "list" (e.g. --help, --debug).
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	accts, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not open account store: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	sort.Slice(accts, func(i, j int) bool {
		return accts[i].Email < accts[j].Email
	})

	now := time.Now()
	for _, a := range accts {
		line := fmt.Sprintf("%s  %s  %s", a.Email, a.UUID, a.RateLimitTier)
		if a.LastSeenAt.Before(now.Add(-staleThreshold)) {
			line += "  (stale)"
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// runAccountsRemove removes a stored account identified by email substring or
// UUID prefix, with active-account protection (D11).
func runAccountsRemove(
	subArgs []string, stdout, stderr io.Writer, g *globals,
	store accounts.Store,
	resolveActiveUUID func(ctx context.Context, stored []accounts.Account) (string, error),
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
		fmt.Fprintln(stderr, "accounts remove requires an email or uuid argument")
		return int(orchestrate.StatusUsageError)
	}
	arg := ids[0]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stored, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not open account store: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	matches := matchAccounts(arg, stored)
	switch len(matches) {
	case 0:
		fmt.Fprintf(stderr, "no stored account matches %q\n", arg)
		return int(orchestrate.StatusUsageError)
	case 1:
		// fall through to active-protection check
	default:
		fmt.Fprintf(stderr, "multiple stored accounts match %q, disambiguate by uuid\n", arg)
		return int(orchestrate.StatusUsageError)
	}

	target := matches[0]

	// D11: active-protection — resolve the active UUID read-only, no store mutation.
	// Fail closed on resolver error: ResolveActiveUUID already normalizes benign
	// "active is unknowable" cases (no live blob, 401/403, missing fields) to
	// ("", nil), so a non-nil error is a transient/actionable failure where
	// proceeding would risk deleting the active account.
	activeUUID, resolveErr := resolveActiveUUID(ctx, stored)
	if resolveErr != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not verify active account: %s (retry, or use `claude /logout` first if you want to remove the active account)\n", resolveErr)
		return int(orchestrate.StatusUsageError)
	}
	if activeUUID != "" && activeUUID == target.UUID {
		fmt.Fprintln(stderr, "cannot remove currently active account \u2014 use 'claude /logout' first")
		return int(orchestrate.StatusUsageError)
	}

	if err := store.Delete(ctx, target.UUID); err != nil {
		fmt.Fprintf(stderr, "aistat: claude: could not remove account: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}

	fmt.Fprintf(stdout, "removed %s (uuid %s)\n", target.Email, target.UUID)
	return 0
}

// matchAccounts resolves arg against stored accounts using the D13 matching rule:
//   - 8+ hex/dash chars → UUID prefix match (case-insensitive)
//   - otherwise → email substring match (case-insensitive)
func matchAccounts(arg string, stored []accounts.Account) []accounts.Account {
	larg := strings.ToLower(arg)
	var matches []accounts.Account
	if uuidishRe.MatchString(arg) {
		for _, a := range stored {
			if strings.HasPrefix(strings.ToLower(a.UUID), larg) {
				matches = append(matches, a)
			}
		}
	} else {
		for _, a := range stored {
			if strings.Contains(strings.ToLower(a.Email), larg) {
				matches = append(matches, a)
			}
		}
	}
	return matches
}

// makeRealActiveUUIDResolver returns a resolveActiveUUID function backed by the
// live Claude credential and a profile HTTP call for cases where no byte-match
// is found. Used by runAccountsSubcommand; tests inject a stub instead.
func makeRealActiveUUIDResolver(g globals, stderr io.Writer) func(context.Context, []accounts.Account) (string, error) {
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
			// Can't determine active account; allow the delete to proceed.
			return "", nil
		}
		return claude.ResolveActiveUUID(claude.ReconcileInput{
			LiveBlob: &c,
			Stored:   stored,
			LookupProfile: func(token string) (claude.Profile, error) {
				return client.GetProfile(ctx, token)
			},
			Now: time.Now(),
		})
	}
}

