// Package autoswitch holds the pure pieces of the conditional-switch feature:
// per-window threshold resolution (non-empty process env > built-in default).
// The cmd layer picks an explicit `--if-above-*` flag over this fallback, and
// wires the result into `switch`'s conditional / `--watch` modes.
package autoswitch

import (
	"fmt"
	"strconv"
)

// Environment-variable names and built-in defaults for the two trigger
// windows. Values are used-percent levels: switch when used >= value.
const (
	EnvFiveHour = "AISTAT_IF_ABOVE_5H"
	EnvWeekly   = "AISTAT_IF_ABOVE_WEEKLY"

	DefaultFiveHour = 85
	DefaultWeekly   = 95
)

// Threshold is one window's trigger level. Off (value "off") disables the
// window as a trigger entirely.
// The zero value (Pct 0, Off false) means "trigger at 0% used", not
// "disabled" — ResolveOne / ParseThreshold always set one field explicitly;
// consumers must not treat Threshold{} as unset.
type Threshold struct {
	Pct float64
	Off bool
}

// Thresholds carries both resolved trigger levels.
type Thresholds struct {
	FiveHour Threshold
	Weekly   Threshold
}

// ParseThreshold parses "off" or an integer 1-100.
func ParseThreshold(s string) (Threshold, error) {
	if s == "off" {
		return Threshold{Off: true}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 100 {
		return Threshold{}, fmt.Errorf("invalid threshold %q: want an integer 1-100 or \"off\"", s)
	}
	return Threshold{Pct: float64(n)}, nil
}

// ResolveOne resolves one window's threshold: a non-empty process env value >
// the built-in default. An invalid env value errors, naming the key and source
// so the user knows which value to fix. The cmd layer calls this only for
// windows the user did not pin with an explicit `--if-above-*` flag.
func ResolveOne(key string, def float64, getenv func(string) string) (Threshold, error) {
	if v := getenv(key); v != "" {
		t, err := ParseThreshold(v)
		if err != nil {
			return Threshold{}, fmt.Errorf("%s: %w (source: environment)", key, err)
		}
		return t, nil
	}
	return Threshold{Pct: def}, nil
}
