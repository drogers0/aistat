package autoswitch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadEnvFile(t *testing.T) {
	tests := []struct {
		name    string
		content *string // nil = file does not exist
		want    map[string]string
		wantErr string
	}{
		{"missing file is empty, no error", nil, nil, ""},
		{"parses KEY=VALUE lines", strPtr("AISTAT_IF_ABOVE_5H=85\nAISTAT_IF_ABOVE_WEEKLY=95\n"),
			map[string]string{"AISTAT_IF_ABOVE_5H": "85", "AISTAT_IF_ABOVE_WEEKLY": "95"}, ""},
		{"skips comments and blank lines", strPtr("# comment\n\nAISTAT_IF_ABOVE_5H=70\n"),
			map[string]string{"AISTAT_IF_ABOVE_5H": "70"}, ""},
		{"trims whitespace around key and value", strPtr("  AISTAT_IF_ABOVE_5H = 60 \n"),
			map[string]string{"AISTAT_IF_ABOVE_5H": "60"}, ""},
		{"empty value is a present entry", strPtr("AISTAT_IF_ABOVE_5H=\n"),
			map[string]string{"AISTAT_IF_ABOVE_5H": ""}, ""},
		{"malformed line names file and line", strPtr("AISTAT_IF_ABOVE_5H=85\nnonsense\n"),
			nil, ":2: malformed line (want KEY=VALUE)"},
		{"line starting with = is malformed", strPtr("=85\n"), nil, ":1: malformed line (want KEY=VALUE)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "autoswitch.env")
			if tt.content != nil {
				if err := os.WriteFile(path, []byte(*tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ReadEnvFile(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ReadEnvFile err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadEnvFile unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ReadEnvFile = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("ReadEnvFile[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func TestWriteEnvFile(t *testing.T) {
	t.Run("writes both keys and creates parent dirs", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sub", "dir", "autoswitch.env")
		if err := WriteEnvFile(path, "85", "off"); err != nil {
			t.Fatalf("WriteEnvFile: %v", err)
		}
		vals, err := ReadEnvFile(path)
		if err != nil {
			t.Fatalf("ReadEnvFile roundtrip: %v", err)
		}
		if vals[EnvFiveHour] != "85" || vals[EnvWeekly] != "off" {
			t.Errorf("roundtrip = %v, want 5h=85 weekly=off", vals)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("file mode: got %04o, want 0644", got)
		}
		dirInfo, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Errorf("parent dir mode: got %04o, want 0700", got)
		}
	})
	t.Run("overwrites existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "autoswitch.env")
		if err := WriteEnvFile(path, "85", "95"); err != nil {
			t.Fatal(err)
		}
		if err := WriteEnvFile(path, "70", "90"); err != nil {
			t.Fatal(err)
		}
		vals, err := ReadEnvFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if vals[EnvFiveHour] != "70" || vals[EnvWeekly] != "90" {
			t.Errorf("after overwrite = %v, want 5h=70 weekly=90", vals)
		}
	})
}
