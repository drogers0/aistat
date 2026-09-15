package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/drogers0/aistat/v2/internal/httpx"
	"github.com/drogers0/aistat/v2/internal/orchestrate"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/render"
	"github.com/drogers0/aistat/v2/internal/watchstate"
)

// runUsage runs the `usage` subcommand: fetch and render provider limits.
// args contains everything after the "usage" subcommand token (or all original
// args when invoked with no subcommand). An optional first positional argument
// names a single provider to query; all other arguments are flags.
func runUsage(args []string, stdout, stderr io.Writer, g globals) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	registerGlobalFlags(fs, &g)
	fakeFn := registerFakeMode(fs)
	var refresh, watch bool
	fs.BoolVar(&refresh, "refresh", false, "")
	fs.BoolVar(&watch, "watch", false, "")
	fs.BoolVar(&watch, "w", false, "")
	var interval int
	fs.IntVar(&interval, "interval", defaultWatchIntervalSecs, "")

	// First pass: parse any leading flags before the optional provider positional.
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}

	// After --help/--version check (they may appear after "usage" token).
	if handled, code := handleGlobals(g, stdout, stderr); handled {
		return code
	}

	// Extract the optional provider positional (first non-flag arg from first pass).
	var service string
	tail := fs.Args()
	if len(tail) > 0 {
		service = tail[0]
		tail = tail[1:]
	}

	// Second pass: parse any trailing flags after the optional provider.
	// Always call Parse (even with empty tail) so fs.Args() is reset and
	// fs.NArg() correctly reflects only unconsumed positionals from this pass.
	if err := fs.Parse(tail); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}

	// After --help/--version that may appear after the provider.
	if handled, code := handleGlobals(g, stdout, stderr); handled {
		return code
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected positional argument: %s\n", fs.Arg(0))
		return int(orchestrate.StatusUsageError)
	}

	if service != "" && !slices.Contains(providers.KnownProviderIDs, service) {
		fmt.Fprintf(stderr, "usage %s: provider must be one of %s\n",
			service, strings.Join(providers.KnownProviderIDs, ", "))
		return int(orchestrate.StatusUsageError)
	}

	// Detect an explicit --interval by presence, not a value sentinel, so a
	// nonsensical --interval 0 still hits the floor check below.
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
	if watch {
		// Flag-shape errors first: they are what the user controls. The
		// environmental check comes last.
		if !g.Human {
			fmt.Fprintln(stderr, "aistat: usage --watch requires -h/--human")
			return int(orchestrate.StatusUsageError)
		}
		if refresh {
			fmt.Fprintln(stderr, "aistat: --refresh cannot be combined with --watch")
			return int(orchestrate.StatusUsageError)
		}
		if interval < 1 {
			fmt.Fprintln(stderr, "--interval must be at least 1 second")
			return int(orchestrate.StatusUsageError)
		}
		if !isTerminalFn(stdout) {
			fmt.Fprintln(stderr, "aistat: usage --watch requires a terminal")
			return int(orchestrate.StatusUsageError)
		}
	}

	// handleGlobals has already rejected an invalid mode, so this cannot fail in
	// practice; report it rather than discarding the error.
	color, err := resolveColor(g.Color, stdout, os.Getenv)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}

	requested := selectedProviders(service)

	serialStderr := httpx.NewConcurrencySafeWriter(stderr)
	chosen, orchDebug := buildProviders(serialStderr, g.Debug, refresh, fakeFn)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	run := usageRun{
		requested: requested,
		chosen:    chosen,
		orchDebug: orchDebug,
		human:     g.Human,
		color:     color,
		stderr:    stderr,
		debug:     g.Debug,
	}

	if watch {
		// Per-tick provider failures are visible in the frame and do not decide
		// the exit code; only a frame that cannot reach the terminal does.
		if err := runUsageWatch(ctx, stdout, time.Duration(interval)*time.Second, func(w io.Writer) {
			_, _ = run.render(ctx, w)
		}); err != nil {
			fmt.Fprintln(stderr, err.Error())
			return int(orchestrate.StatusRenderError)
		}
		return int(orchestrate.StatusOK)
	}

	status, renderErr := run.render(ctx, stdout)
	if renderErr != nil {
		fmt.Fprintln(stderr, renderErr.Error())
		return int(orchestrate.StatusRenderError)
	}
	return int(status)
}

// usageRun holds everything one fetch/render round needs. It is built once in
// runUsage and reused by every watch tick, so the loop cannot drift from the
// one-shot path.
type usageRun struct {
	requested []string
	chosen    []providers.Provider
	orchDebug io.Writer // nil unless --debug
	human     bool
	color     bool
	stderr    io.Writer
	debug     bool
}

// render runs one fetch/render round into w, returning the orchestrator status
// and any write/encode error from the renderer. The error is returned rather
// than printed so each caller decides what it means.
func (u usageRun) render(ctx context.Context, w io.Writer) (orchestrate.ExitStatus, error) {
	report, status := orchestrate.Run(ctx, u.requested, u.chosen, orchestrate.Options{Debug: u.orchDebug})
	attachWatchers(&report, u.requested, u.stderr, u.debug)
	if u.human {
		return status, render.Text(w, report, u.requested, u.color)
	}
	return status, render.JSON(w, report)
}

// attachWatchers nests each `switch --watch` heartbeat under every requested
// provider it covers, so a watcher shows up beside the accounts it guards.
// A heartbeat whose watcher has died is kept and rendered STALE. A watcher scoped to a provider the caller did not ask for is not
// reported: scoped output stays scoped.
//
// Advisory and best-effort: a watcher's own published thresholds are the only
// accurate ones (it resolved them from its own environment, not this shell's),
// and an unreadable state directory must never fail a usage report.
func attachWatchers(report *providers.Report, requested []string, stderr io.Writer, debug bool) {
	now := time.Now()
	heartbeats, err := watchstate.List(now)
	if err != nil {
		if debug {
			fmt.Fprintf(stderr, "aistat: %s\n", err)
		}
		return
	}
	for _, id := range requested {
		result, ok := report.Providers[id]
		if !ok {
			continue
		}
		var views []providers.WatcherView
		for _, hb := range heartbeats {
			if !hb.Covers(id) {
				continue
			}
			views = append(views, providers.WatcherView{
				PID:          hb.PID,
				IntervalSecs: hb.IntervalSecs,
				Thresholds:   hb.Thresholds,
				LastTick:     hb.LastTick,
				ReadAt:       now,
			})
		}
		if len(views) == 0 {
			continue
		}
		result.Watchers = views
		report.Providers[id] = result
	}
}

// selectedProviders returns the provider ID list to query. When service is
// empty, all known providers are requested.
func selectedProviders(service string) []string {
	if service == "" {
		return append([]string(nil), providers.KnownProviderIDs...)
	}
	return []string{service}
}
