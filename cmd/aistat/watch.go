package main

import (
	"context"
	"fmt"
	"time"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
)

// This file holds the loop primitives for `switch --watch`; the subcommand
// entry point lives in switch.go (runSwitch's --watch branch).

// newDedupNotifier wraps a notification sender so an unchanged message for the
// same title is sent at most once per cooldown. This is what makes
// `switch --watch` notify a persistent "no better account" state once instead
// of every tick. A changed message (e.g. an actual switch) sends immediately
// and resets the entry. now must be injected for testability.
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
