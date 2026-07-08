package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
	"github.com/drogers0/aistat/v2/internal/notify"
	"github.com/drogers0/aistat/v2/internal/orchestrate"
)

// newDedupNotifier wraps a notification sender so an unchanged message for the
// same title is sent at most once per cooldown. This is what makes `aistat
// watch` notify a persistent "no better account" state once instead of every
// tick. A changed message (e.g. an actual switch) sends immediately and resets
// the entry. now must be injected for testability.
func newDedupNotifier(inner func(context.Context, string, string) error, cooldown time.Duration, now func() time.Time) func(context.Context, string, string) error {
	type entry struct {
		msg string
		at  time.Time
	}
	last := map[string]entry{}
	return func(ctx context.Context, title, message string) error {
		if e, ok := last[title]; ok && e.msg == message && now().Sub(e.at) < cooldown {
			return nil // unchanged within cooldown → suppress
		}
		if err := inner(ctx, title, message); err != nil {
			return err // don't remember a failed send — the next tick retries
		}
		last[title] = entry{msg: message, at: now()}
		return nil
	}
}

// watchThresholdDisplay renders a threshold for the startup line: "off" when
// disabled, else "≥ N%".
func watchThresholdDisplay(t autoswitch.Threshold) string {
	if t.Off {
		return "off"
	}
	return fmt.Sprintf("≥ %.0f%%", t.Pct)
}

// sleepWithCtx sleeps for d or until ctx is cancelled, returning ctx.Err() on
// cancellation so the caller can stop cleanly.
func sleepWithCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// watchLoop ticks immediately, then every interval, until ctx is cancelled.
// tick runs one conditional-switch round; its errors are intentionally not
// fatal — a transient failure must not stop the daemon (KeepAlive/systemd
// would restart it anyway, losing the in-memory dedup state). sleep is the
// interruptible wait (injected for tests).
func watchLoop(ctx context.Context, interval time.Duration, tick func(), sleep func(context.Context, time.Duration) error) {
	for {
		tick()
		if err := sleep(ctx, interval); err != nil {
			return
		}
	}
}

// runWatch implements the `aistat watch` subcommand: a foreground daemon that
// runs the conditional-switch round (`switch --if-needed --notify`) on a
// timer. It is kept alive by the OS service manager (launchd KeepAlive /
// systemd Restart=always) rather than by any code aistat owns.
func runWatch(args []string, stdout, stderr io.Writer, g globals) int {
	// 1. Flag setup + two-pass parse (mirrors runSwitch).
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var interval int
	fs.IntVar(&interval, "interval", 300, "")
	var if5h, ifWeekly string
	fs.StringVar(&if5h, "if-above-5h", "85", "")
	fs.StringVar(&ifWeekly, "if-above-weekly", "95", "")
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

	// 2. Validate — all before building handles or looping, so validation is
	// unit-testable without opening real stores.
	if providerArg != "" && !knownSwitchProvider(providerArg) {
		fmt.Fprintf(stderr, "unknown provider %q — want claude or codex\n", providerArg)
		return int(orchestrate.StatusUsageError)
	}
	if interval < 60 {
		fmt.Fprintln(stderr, "--interval must be at least 60 seconds")
		return int(orchestrate.StatusUsageError)
	}
	fiveHourTh, err := autoswitch.ParseThreshold(if5h)
	if err != nil {
		fmt.Fprintf(stderr, "--if-above-5h: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}
	weeklyTh, err := autoswitch.ParseThreshold(ifWeekly)
	if err != nil {
		fmt.Fprintf(stderr, "--if-above-weekly: %s\n", err)
		return int(orchestrate.StatusUsageError)
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
	if providerArg != "" {
		handles = []switchHandle{handleByID(handles, providerArg)}
	}

	// Install a dedup notifier for the daemon's lifetime: watch owns the
	// process, so mutating the package-level seam is fine here (production
	// only — tests that call runWatch must save/restore it themselves, but
	// the validation tests below return before this line).
	sendNotification = newDedupNotifier(notify.Send, time.Hour, time.Now)

	opts := switchOpts{
		ifNeeded: true,
		notify:   true,
		th:       autoswitch.Thresholds{FiveHour: fiveHourTh, Weekly: weeklyTh},
	}

	providerDesc := "all providers"
	if providerArg != "" {
		providerDesc = providerArg
	}
	fmt.Fprintf(stdout, "watching %s every %ds (5h %s, weekly %s)\n", providerDesc, interval, watchThresholdDisplay(fiveHourTh), watchThresholdDisplay(weeklyTh))

	watchLoop(ctx, time.Duration(interval)*time.Second, func() {
		_ = runSwitchBulk(ctx, handles, opts, stdout, stderr, debugW)
	}, sleepWithCtx)

	return 0
}
