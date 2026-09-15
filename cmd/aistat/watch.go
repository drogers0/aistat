package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
	"github.com/drogers0/aistat/v2/internal/watchstate"
)

// This file holds the loop primitives shared by both watch modes: `switch
// --watch` (entry point in switch.go, runSwitch's --watch branch) and `usage
// --watch` (entry point in usage_watch.go). watchLoop and sleepWithCtx are
// generic cadence primitives; newDedupNotifier and the heartbeat helpers below
// are switch-only.

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

// newHeartbeat builds this watcher's published state. A bulk watcher (empty
// providerArg) covers every switchable provider.
func newHeartbeat(handles []switchHandle, providerArg string, interval int, th autoswitch.Thresholds) watchstate.Heartbeat {
	scope := []string{providerArg}
	if providerArg == "" {
		scope = scope[:0]
		for _, h := range handles {
			scope = append(scope, h.id)
		}
	}
	return watchstate.Heartbeat{
		PID:          os.Getpid(),
		Providers:    scope,
		IntervalSecs: interval,
		Thresholds:   watchstate.Thresholds{FiveHour: thresholdPct(th.FiveHour), Weekly: thresholdPct(th.Weekly)},
	}
}

// thresholdPct converts a resolved threshold to the heartbeat's nil-means-off
// representation.
func thresholdPct(t autoswitch.Threshold) *float64 {
	if t.Off {
		return nil
	}
	return &t.Pct
}
