package main

import (
	"context"
	"fmt"
	"io"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
	"github.com/drogers0/aistat/v2/internal/notify"
	"github.com/drogers0/aistat/v2/internal/providers"
)

// switchOpts carries the conditional-switch mode through the dispatch helpers.
// The zero value means unconditional switch with no notifications — exactly
// the pre---if-needed behavior.
type switchOpts struct {
	ifNeeded bool
	notify   bool
	th       autoswitch.Thresholds
}

// Package-level injection seams — overridden by tests.
var (
	// sendNotification posts a desktop notification.
	sendNotification = notify.Send

	// autoswitchEnvPath resolves the threshold env-file location.
	autoswitchEnvPath = autoswitch.DefaultEnvFilePath
)

// notifyBestEffort sends a desktop notification, logging failure to stderr
// without affecting the exit code.
func notifyBestEffort(ctx context.Context, providerID, message string, stderr io.Writer) {
	if err := sendNotification(ctx, providers.Title(providerID), message); err != nil {
		fmt.Fprintf(stderr, "aistat: %s: notification failed: %s\n", providerID, err)
	}
}

// bindingLongWindow returns the present long window (seven_day/thirty_day)
// with the least remaining headroom. ok=false when none is present.
func bindingLongWindow(l map[string]providers.Limit) (key string, used float64, ok bool) {
	remaining := 100.0
	for _, k := range longKeys {
		if w, present := l[k]; present && w.RemainingPercent <= remaining {
			remaining = w.RemainingPercent
			key, used, ok = k, w.UsedPercent, true
		}
	}
	return key, used, ok
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
		if key, used, ok := bindingLongWindow(l); ok && used >= th.Weekly.Pct {
			return fmt.Sprintf("%s at %.0f%%", key, used), true
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
	if key, used, ok := bindingLongWindow(l); ok {
		return fmt.Sprintf("%s at %.0f%%", key, used)
	}
	return "no usage windows"
}
