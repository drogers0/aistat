package main

import (
	"testing"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
)

func th(fiveHour, weekly float64, fiveOff, weeklyOff bool) autoswitch.Thresholds {
	return autoswitch.Thresholds{
		FiveHour: autoswitch.Threshold{Pct: fiveHour, Off: fiveOff},
		Weekly:   autoswitch.Threshold{Pct: weekly, Off: weeklyOff},
	}
}

func TestTriggerReason(t *testing.T) {
	tests := []struct {
		name       string
		remaining  map[string]float64 // makeLimitsFull input: RemainingPercent per window
		th         autoswitch.Thresholds
		wantReason string
		wantHit    bool
	}{
		{"below both thresholds", map[string]float64{"five_hour": 58, "seven_day": 50},
			th(85, 95, false, false), "", false},
		{"five_hour at threshold triggers", map[string]float64{"five_hour": 15, "seven_day": 50},
			th(85, 95, false, false), "five_hour at 85%", true},
		{"five_hour above threshold triggers", map[string]float64{"five_hour": 13},
			th(85, 95, false, false), "five_hour at 87%", true},
		{"weekly triggers when 5h is fine", map[string]float64{"five_hour": 60, "seven_day": 4},
			th(85, 95, false, false), "seven_day at 96%", true},
		{"thirty_day is the binding long window", map[string]float64{"five_hour": 60, "seven_day": 50, "thirty_day": 3},
			th(85, 95, false, false), "thirty_day at 97%", true},
		{"5h checked before weekly", map[string]float64{"five_hour": 10, "seven_day": 2},
			th(85, 95, false, false), "five_hour at 90%", true},
		{"missing five_hour window cannot trigger 5h", map[string]float64{"seven_day": 50},
			th(85, 95, false, false), "", false},
		{"no windows at all never triggers", map[string]float64{},
			th(85, 95, false, false), "", false},
		{"off disables five_hour", map[string]float64{"five_hour": 1, "seven_day": 50},
			th(0, 95, true, false), "", false},
		{"off disables weekly", map[string]float64{"five_hour": 60, "seven_day": 1},
			th(85, 0, false, true), "", false},
		{"both off never triggers", map[string]float64{"five_hour": 1, "seven_day": 1},
			th(0, 0, true, true), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, hit := triggerReason(makeLimitsFull(tt.remaining), tt.th)
			if reason != tt.wantReason || hit != tt.wantHit {
				t.Errorf("triggerReason = (%q, %v), want (%q, %v)", reason, hit, tt.wantReason, tt.wantHit)
			}
		})
	}
}

func TestUsageSummary(t *testing.T) {
	tests := []struct {
		name      string
		remaining map[string]float64
		want      string
	}{
		{"prefers five_hour", map[string]float64{"five_hour": 58, "seven_day": 10}, "five_hour at 42%"},
		{"falls back to binding long window", map[string]float64{"seven_day": 70, "thirty_day": 20}, "thirty_day at 80%"},
		{"no windows", map[string]float64{}, "no usage windows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usageSummary(makeLimitsFull(tt.remaining)); got != tt.want {
				t.Errorf("usageSummary = %q, want %q", got, tt.want)
			}
		})
	}
}
