// Package autoswitch holds the pure pieces of the conditional-switch feature:
// threshold resolution (process env > defaults). The cmd layer wires this
// into `switch --if-needed`.
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
// "disabled" — Resolve always sets one field explicitly; consumers must not
// treat Threshold{} as unset.
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

// Resolve determines both thresholds. Precedence per key: non-empty process
// env > built-in default. Errors name the offending key and source so the
// user knows which value to fix.
func Resolve(getenv func(string) string) (Thresholds, error) {
	fh, err := resolveOne(EnvFiveHour, DefaultFiveHour, getenv)
	if err != nil {
		return Thresholds{}, err
	}
	wk, err := resolveOne(EnvWeekly, DefaultWeekly, getenv)
	if err != nil {
		return Thresholds{}, err
	}
	return Thresholds{FiveHour: fh, Weekly: wk}, nil
}

func resolveOne(key string, def float64, getenv func(string) string) (Threshold, error) {
	if v := getenv(key); v != "" {
		t, err := ParseThreshold(v)
		if err != nil {
			return Threshold{}, fmt.Errorf("%s: %w (source: environment)", key, err)
		}
		return t, nil
	}
	return Threshold{Pct: def}, nil
}
