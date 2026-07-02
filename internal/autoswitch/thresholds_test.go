package autoswitch

import (
	"strings"
	"testing"
)

func TestParseThreshold(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantPct float64
		wantOff bool
		wantErr string // "" = kein Fehler; sonst Substring
	}{
		{"plain integer", "85", 85, false, ""},
		{"lower bound", "1", 1, false, ""},
		{"upper bound", "100", 100, false, ""},
		{"off disables", "off", 0, true, ""},
		{"zero rejected", "0", 0, false, `invalid threshold "0": want an integer 1-100 or "off"`},
		{"above 100 rejected", "101", 0, false, `invalid threshold "101"`},
		{"garbage rejected", "banana", 0, false, `invalid threshold "banana"`},
		{"float rejected", "85.5", 0, false, `invalid threshold "85.5"`},
		{"empty rejected", "", 0, false, `invalid threshold ""`},
		{"uppercase OFF rejected", "OFF", 0, false, `invalid threshold "OFF"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseThreshold(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseThreshold(%q) err = %v, want substring %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseThreshold(%q) unexpected error: %v", tt.in, err)
			}
			if got.Pct != tt.wantPct || got.Off != tt.wantOff {
				t.Errorf("ParseThreshold(%q) = {Pct:%v Off:%v}, want {Pct:%v Off:%v}",
					tt.in, got.Pct, got.Off, tt.wantPct, tt.wantOff)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name    string
		env     map[string]string
		file    map[string]string
		want5h  Threshold
		wantWk  Threshold
		wantErr string
	}{
		{"defaults when nothing set", nil, nil,
			Threshold{Pct: 85}, Threshold{Pct: 95}, ""},
		{"env wins over file", map[string]string{EnvFiveHour: "70"}, map[string]string{EnvFiveHour: "50"},
			Threshold{Pct: 70}, Threshold{Pct: 95}, ""},
		{"file wins over default", nil, map[string]string{EnvWeekly: "80"},
			Threshold{Pct: 85}, Threshold{Pct: 80}, ""},
		{"off via env", map[string]string{EnvWeekly: "off"}, nil,
			Threshold{Pct: 85}, Threshold{Off: true}, ""},
		{"invalid env value names source", map[string]string{EnvFiveHour: "banana"}, nil,
			Threshold{}, Threshold{}, `AISTAT_IF_ABOVE_5H: invalid threshold "banana": want an integer 1-100 or "off" (source: environment)`},
		{"invalid file value names source", nil, map[string]string{EnvWeekly: "999"},
			Threshold{}, Threshold{}, `AISTAT_IF_ABOVE_WEEKLY: invalid threshold "999": want an integer 1-100 or "off" (source: autoswitch.env)`},
		{"empty env falls through to file", map[string]string{EnvFiveHour: ""}, map[string]string{EnvFiveHour: "40"},
			Threshold{Pct: 40}, Threshold{Pct: 95}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(env(tt.env), tt.file)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Resolve err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve unexpected error: %v", err)
			}
			if got.FiveHour != tt.want5h || got.Weekly != tt.wantWk {
				t.Errorf("Resolve = {FiveHour:%+v Weekly:%+v}, want {FiveHour:%+v Weekly:%+v}",
					got.FiveHour, got.Weekly, tt.want5h, tt.wantWk)
			}
		})
	}
}
