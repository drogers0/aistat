package render

// ANSI escapes for the usage-percent ramp. The thresholds are a reading aid and
// are deliberately independent of the autoswitch trigger defaults (85 / 95): a
// user who sets AISTAT_IF_ABOVE_5H must not find the colors changing meaning.
const (
	ansiReset   = "\x1b[0m"
	ansiOrange  = "\x1b[38;5;208m"
	ansiRed     = "\x1b[31m"
	ansiDarkRed = "\x1b[38;5;88m"
)

// colorPercent renders a used-percent with its trailing "%", wrapped in the ramp
// color when color is on. Only live usage percentages go through here; watcher
// thresholds and reset durations use formatPercent directly and stay uncolored.
func colorPercent(p float64, color bool) string {
	s := formatPercent(p) + "%"
	if !color {
		return s
	}
	switch {
	case p >= 100:
		return ansiDarkRed + s + ansiReset
	case p > 85:
		return ansiRed + s + ansiReset
	case p > 70:
		return ansiOrange + s + ansiReset
	}
	return s
}
