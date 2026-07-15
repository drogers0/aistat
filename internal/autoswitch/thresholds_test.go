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
		wantErr string // "" = no error; otherwise a substring
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
		{"negative rejected", "-1", 0, false, `invalid threshold "-1"`},
		{"whitespace rejected", " 85", 0, false, `invalid threshold " 85"`},
		{"trailing whitespace rejected", "85 ", 0, false, `invalid threshold "85 "`},
		{"overflow rejected", "99999999999999999999999999999", 0, false, `invalid threshold`},
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

func TestResolveOne(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == EnvFiveHour {
				return v
			}
			return ""
		}
	}
	tests := []struct {
		name    string
		envVal  string
		want    Threshold
		wantErr string
	}{
		{"default when env unset", "", Threshold{Pct: DefaultFiveHour}, ""},
		{"env wins over default", "70", Threshold{Pct: 70}, ""},
		{"off via env", "off", Threshold{Off: true}, ""},
		{"invalid env value names source", "banana",
			Threshold{}, `AISTAT_IF_ABOVE_5H: invalid threshold "banana": want an integer 1-100 or "off" (source: environment)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveOne(EnvFiveHour, DefaultFiveHour, env(tt.envVal))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveOne err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveOne unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveOne = %+v, want %+v", got, tt.want)
			}
		})
	}
}
