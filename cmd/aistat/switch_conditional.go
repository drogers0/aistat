package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
	"github.com/drogers0/aistat/v2/internal/notify"
	"github.com/drogers0/aistat/v2/internal/providers"
)

// switchOpts carries the conditional-switch mode through the dispatch helpers.
// The zero value means unconditional switch with no notifications — exactly
// the behavior before --if-needed existed.
type switchOpts struct {
	ifNeeded bool
	notify   bool
	th       autoswitch.Thresholds
	// notifier delivers desktop notifications. A nil notifier means "use the
	// default seam" (sendNotification), so the common path leaves it unset;
	// watch installs a dedup-wrapping notifier here instead of mutating the
	// package global at runtime.
	notifier func(context.Context, string, string) error
}

// sendNotification posts a desktop notification. Package-level injection
// seam — the default when switchOpts.notifier is nil, and overridden by tests.
var sendNotification = notify.Send

// notifyBestEffort sends a desktop notification, logging failure to stderr
// without affecting the exit code. A nil notifier falls back to the
// sendNotification seam. Bounded: fire-and-forget osascript must not hang a
// launchd run on a stuck permission prompt.
func notifyBestEffort(ctx context.Context, notifier func(context.Context, string, string) error, providerID, message string, stderr io.Writer) {
	if notifier == nil {
		notifier = sendNotification
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := notifier(ctx, providers.Title(providerID), message); err != nil {
		fmt.Fprintf(stderr, "aistat: %s: notification failed: %s\n", providerID, err)
	}
}

// bindingLongWindow returns the present long window (seven_day/thirty_day)
// with the least remaining headroom; ties resolve to the earlier longKeys
// entry, matching longRemaining's historical pick. ok=false when none is
// present. Comparison uses RemainingPercent while callers display UsedPercent —
// the two are complements by provider construction.
func bindingLongWindow(l map[string]providers.Limit) (key string, w providers.Limit, ok bool) {
	remaining := 100.0
	for _, k := range longKeys {
		if lw, present := l[k]; present {
			if !ok || lw.RemainingPercent < remaining {
				remaining = lw.RemainingPercent
				key, w, ok = k, lw, true
			}
		}
	}
	return key, w, ok
}

// triggerReason reports whether the active account's limits breach th and, if
// so, why — e.g. "five_hour at 87%". five_hour is checked before the long
// windows; a window absent from l cannot trigger.
func triggerReason(l map[string]providers.Limit, th autoswitch.Thresholds) (string, bool) {
	if !th.FiveHour.Off {
		if w, ok := l[shortKey]; ok && w.UsedPercent >= th.FiveHour.Pct {
			return fmt.Sprintf("%s at %.0f%%", shortKey, w.UsedPercent), true
		}
	}
	if !th.Weekly.Off {
		if key, w, ok := bindingLongWindow(l); ok && w.UsedPercent >= th.Weekly.Pct {
			return fmt.Sprintf("%s at %.0f%%", key, w.UsedPercent), true
		}
	}
	return "", false
}

// usageSummary describes the most relevant window for the "no switch needed"
// line: five_hour when present, else the binding long window.
func usageSummary(l map[string]providers.Limit) string {
	if w, ok := l[shortKey]; ok {
		return fmt.Sprintf("%s at %.0f%%", shortKey, w.UsedPercent)
	}
	if key, w, ok := bindingLongWindow(l); ok {
		return fmt.Sprintf("%s at %.0f%%", key, w.UsedPercent)
	}
	return "no usage windows"
}
