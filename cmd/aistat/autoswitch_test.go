package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
)

// autoswitchSeams fakes GOOS/home/executable/uid and records launchctl calls.
func autoswitchSeams(t *testing.T, fakeGOOS string, launchctlErr error) (home string, calls *[][]string) {
	t.Helper()
	home = t.TempDir()
	var got [][]string
	oldGOOS, oldHome, oldExe, oldUID, oldRun, oldEnv := autoswitchGOOS, autoswitchHomeDir, autoswitchExecutable, autoswitchUID, runLaunchctl, autoswitchEnvPath
	autoswitchGOOS = fakeGOOS
	autoswitchHomeDir = func() (string, error) { return home, nil }
	autoswitchExecutable = func() (string, error) { return "/usr/local/bin/aistat", nil }
	autoswitchUID = func() int { return 501 }
	autoswitchEnvPath = func() (string, error) { return filepath.Join(home, ".config", "aistat", "autoswitch.env"), nil }
	runLaunchctl = func(_ context.Context, args ...string) ([]byte, error) {
		got = append(got, args)
		return nil, launchctlErr
	}
	t.Cleanup(func() {
		autoswitchGOOS, autoswitchHomeDir, autoswitchExecutable, autoswitchUID, runLaunchctl, autoswitchEnvPath =
			oldGOOS, oldHome, oldExe, oldUID, oldRun, oldEnv
	})
	return home, &got
}

func runAutoswitchTest(args ...string) runResult {
	var stdout, stderr bytes.Buffer
	code := runAutoswitch(args, &stdout, &stderr, globals{})
	return runResult{stdout.String(), stderr.String(), code}
}

func plistPathIn(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", autoswitch.LaunchdLabel+".plist")
}

func TestAutoswitch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"non-darwin is rejected with a hint", func(t *testing.T) {
			autoswitchSeams(t, "linux", nil)
			r := runAutoswitchTest("install")
			wantExit(t, r, 2)
			wantErrOut(t, r, "autoswitch requires launchd (macOS); run `aistat switch --if-needed` from cron or a systemd timer instead")
		}},
		{"unknown verb is a usage error", func(t *testing.T) {
			autoswitchSeams(t, "darwin", nil)
			r := runAutoswitchTest("frobnicate")
			wantExit(t, r, 2)
			wantErrOut(t, r, `unknown autoswitch verb "frobnicate"`)
		}},
		{"install writes env file, plist, and bootstraps", func(t *testing.T) {
			t.Setenv(autoswitch.EnvFiveHour, "")
			t.Setenv(autoswitch.EnvWeekly, "")
			home, calls := autoswitchSeams(t, "darwin", nil)
			r := runAutoswitchTest("install", "--if-above-5h", "80", "--if-above-weekly", "90", "--interval", "240")
			wantExit(t, r, 0)
			vals, err := autoswitch.ReadEnvFile(filepath.Join(home, ".config", "aistat", "autoswitch.env"))
			if err != nil || vals[autoswitch.EnvFiveHour] != "80" || vals[autoswitch.EnvWeekly] != "90" {
				t.Fatalf("env file = %v, %v; want 5h=80 weekly=90", vals, err)
			}
			data, err := os.ReadFile(plistPathIn(home))
			if err != nil {
				t.Fatal(err)
			}
			if n, ok := autoswitch.ParseInterval(string(data)); !ok || n != 240 {
				t.Errorf("plist interval = %d/%v, want 240", n, ok)
			}
			if strings.Contains(string(data), "--if-above") {
				t.Error("plist must not bake in threshold values")
			}
			if len(*calls) != 2 || (*calls)[0][0] != "bootout" || (*calls)[1][0] != "bootstrap" {
				t.Errorf("launchctl calls = %v, want bootout then bootstrap", *calls)
			}
			if (*calls)[1][1] != "gui/501" {
				t.Errorf("bootstrap domain = %q, want gui/501", (*calls)[1][1])
			}
		}},
		{"install validates thresholds", func(t *testing.T) {
			autoswitchSeams(t, "darwin", nil)
			r := runAutoswitchTest("install", "--if-above-5h", "banana")
			wantExit(t, r, 2)
			wantErrOut(t, r, `--if-above-5h: invalid threshold "banana"`)
		}},
		{"install validates interval", func(t *testing.T) {
			autoswitchSeams(t, "darwin", nil)
			r := runAutoswitchTest("install", "--interval", "30")
			wantExit(t, r, 2)
			wantErrOut(t, r, "--interval must be at least 60 seconds")
		}},
		{"install surfaces bootstrap failure", func(t *testing.T) {
			autoswitchSeams(t, "darwin", errors.New("Boot-out failed: 5: Input/output error"))
			r := runAutoswitchTest("install")
			wantExit(t, r, 1)
			wantErrOut(t, r, "launchctl bootstrap failed")
		}},
		{"uninstall removes plist and keeps env file", func(t *testing.T) {
			home, calls := autoswitchSeams(t, "darwin", nil)
			if r := runAutoswitchTest("install"); r.code != 0 {
				t.Fatalf("install failed: %+v", r)
			}
			r := runAutoswitchTest("uninstall")
			wantExit(t, r, 0)
			wantOut(t, r, "uninstalled")
			if _, err := os.Stat(plistPathIn(home)); !errors.Is(err, os.ErrNotExist) {
				t.Error("plist still present after uninstall")
			}
			if _, err := os.Stat(filepath.Join(home, ".config", "aistat", "autoswitch.env")); err != nil {
				t.Error("env file must survive uninstall")
			}
			if len(*calls) != 3 || (*calls)[2][0] != "bootout" {
				t.Errorf("launchctl calls = %v", *calls)
			}
		}},
		{"uninstall when not installed is a friendly no-op", func(t *testing.T) {
			autoswitchSeams(t, "darwin", nil)
			r := runAutoswitchTest("uninstall")
			wantExit(t, r, 0)
			wantOut(t, r, "autoswitch is not installed")
		}},
		{"status reports not installed", func(t *testing.T) {
			autoswitchSeams(t, "darwin", nil)
			r := runAutoswitchTest("status")
			wantExit(t, r, 0)
			wantOut(t, r, "installed: no")
		}},
		{"status reports interval, loaded state, and effective thresholds", func(t *testing.T) {
			t.Setenv(autoswitch.EnvFiveHour, "")
			t.Setenv(autoswitch.EnvWeekly, "")
			_, _ = autoswitchSeams(t, "darwin", nil)
			if r := runAutoswitchTest("install", "--if-above-5h", "80"); r.code != 0 {
				t.Fatalf("install failed: %+v", r)
			}
			r := runAutoswitchTest("status")
			wantExit(t, r, 0)
			wantOut(t, r, "installed: yes")
			wantOut(t, r, "loaded: yes")
			wantOut(t, r, "interval: 300s")
			wantOut(t, r, "thresholds: five_hour >=80%, weekly >=95%")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
