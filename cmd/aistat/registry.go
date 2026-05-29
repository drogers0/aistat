package main

import (
	"fmt"
	"io"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/httpx"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/providers/claude"
	"github.com/drogers0/aistat/v2/internal/providers/codex"
	"github.com/drogers0/aistat/v2/internal/providers/copilot"
)

// realProviders constructs the live providers. serialStderr is the
// mutex-wrapped writer that backs three concurrent stderr consumers
// (per-Doer debug, orchestrator per-provider summary, Copilot warn), so
// all three serialize through one mutex. The mutex is always in the path
// because copilot.warn is unconditional. includeDebug toggles per-request
// debug logging — when false, Doers receive a nil Debug writer.
//
// The platform account store is opened here and passed to the Claude client
// via WithStore. On open failure the warn is emitted unconditionally (not
// gated on --debug, so the user sees keychain breakage) and a MemoryStore
// is used as a safe no-op fallback: List returns empty, Upsert/Delete are
// no-ops for this run, but the live credential still drives a usable result.
func realProviders(serialStderr *httpx.ConcurrencySafeWriter, includeDebug bool, cacheBypass bool) []providers.Provider {
	var debugSink io.Writer
	if includeDebug {
		debugSink = serialStderr
	}

	var storeDebug io.Writer
	if includeDebug {
		storeDebug = serialStderr
	}
	store, err := accounts.OpenStore(accounts.WithDebug(storeDebug))
	if err != nil {
		fmt.Fprintln(serialStderr, "aistat: claude: could not open account store ("+err.Error()+"); proceeding with live credential only")
		store = accounts.NewMemoryStore()
	}

	v := resolvedVersion()
	return []providers.Provider{
		claude.New(debugSink, claude.DefaultUserAgent(v), claude.WithStore(store), claude.WithCacheBypass(cacheBypass)),
		codex.New(debugSink, codex.DefaultUserAgent(v)),
		copilot.New(debugSink, copilot.DefaultUserAgent(v), copilot.WithWarn(wrapWarn(serialStderr))),
	}
}

// wrapWarn returns the warn callback wired into copilot.New. Every line the
// provider emits is prefixed with "aistat: " so the source is obvious
// when --debug is OFF and only the SKU-drift line surfaces. Extracted from
// realProviders so the prefix contract is unit-testable without HTTP.
func wrapWarn(out io.Writer) func(string) {
	return func(s string) { fmt.Fprintln(out, "aistat: "+s) }
}

// buildProviders resolves the provider set (fake-mode-first) and picks the
// orchestrator debug writer. Extracted from runUsage() to keep that function
// scannable and to provide a non-CLI seam for tests that exercise
// warn-wiring against the real provider construction. The mutex inside
// serialStderr is always in the path because copilot.warn is unconditional.
func buildProviders(
	serialStderr *httpx.ConcurrencySafeWriter,
	includeDebug bool,
	cacheBypass bool,
	fakeFn func() []providers.Provider,
) (chosen []providers.Provider, orchDebug io.Writer) {
	if fakeFn != nil {
		chosen = fakeFn()
	}
	if chosen == nil {
		chosen = realProviders(serialStderr, includeDebug, cacheBypass)
	}
	if includeDebug {
		orchDebug = serialStderr
	}
	return chosen, orchDebug
}
