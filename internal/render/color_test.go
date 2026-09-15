package render

import "testing"

// TestColorPercent pins both halves of the contract: the exact ramp boundaries
// when color is on, and byte-identical pre-color output when it is off. The
// escapes are spelled literally rather than via the package constants so a
// typo in a constant fails here.
func TestColorPercent(t *testing.T) {
	tests := []struct {
		name    string
		p       float64
		wantOff string
		wantOn  string
	}{
		{"zero", 0, "0%", "0%"},
		{"exactly 70 is uncolored", 70, "70%", "70%"},
		{"just above 70 is orange", 70.1, "70.1%", "\x1b[38;5;208m70.1%\x1b[0m"},
		{"exactly 85 is still orange", 85, "85%", "\x1b[38;5;208m85%\x1b[0m"},
		{"just above 85 is red", 85.1, "85.1%", "\x1b[31m85.1%\x1b[0m"},
		{"just below 100 is red", 99.9, "99.9%", "\x1b[31m99.9%\x1b[0m"},
		{"exactly 100 is dark red", 100, "100%", "\x1b[38;5;88m100%\x1b[0m"},
		{"above 100 is dark red", 100.5, "100.5%", "\x1b[38;5;88m100.5%\x1b[0m"},
		{"copilot fraction below the ramp", 67.3, "67.3%", "67.3%"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := colorPercent(tt.p, false); got != tt.wantOff {
				t.Errorf("colorPercent(%v, false) = %q, want %q", tt.p, got, tt.wantOff)
			}
			if got := colorPercent(tt.p, true); got != tt.wantOn {
				t.Errorf("colorPercent(%v, true) = %q, want %q", tt.p, got, tt.wantOn)
			}
		})
	}
}
