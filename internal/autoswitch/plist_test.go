package autoswitch

import (
	"os"
	"strings"
	"testing"
)

func TestPlistXML(t *testing.T) {
	t.Run("matches golden file", func(t *testing.T) {
		want, err := os.ReadFile("testdata/autoswitch.plist")
		if err != nil {
			t.Fatal(err)
		}
		got := PlistXML("/usr/local/bin/aistat", 300, "/Users/u/Library/Logs/aistat-autoswitch.log")
		if got != string(want) {
			t.Errorf("PlistXML mismatch with golden file\ngot:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("escapes xml metacharacters in paths", func(t *testing.T) {
		got := PlistXML("/tmp/a&b/aistat", 60, "/tmp/log<x>.log")
		if !strings.Contains(got, "/tmp/a&amp;b/aistat") {
			t.Errorf("binary path not escaped:\n%s", got)
		}
		if !strings.Contains(got, "/tmp/log&lt;x&gt;.log") {
			t.Errorf("log path not escaped:\n%s", got)
		}
	})
}

func TestParseInterval(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   int
		wantOK bool
	}{
		{"roundtrip from PlistXML", PlistXML("/bin/aistat", 240, "/tmp/l.log"), 240, true},
		{"missing marker", "<plist></plist>", 0, false},
		{"malformed integer", "<key>StartInterval</key><integer>abc</integer>", 0, false},
		{"empty input", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseInterval(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ParseInterval = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
