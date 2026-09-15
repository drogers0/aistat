package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/drogers0/aistat/v2/internal/testutil"
)

// fakeEnv builds a getenv closure over a literal environment, so precedence is
// exercised without mutating the process environment.
func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// devNull is a character device on every supported OS: a real *os.File that
// satisfies isTerminal without needing a pty. Sound only because isTerminal is
// pure; a console-gated implementation would report false for NUL on Windows.
func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestResolveColor(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		env     map[string]string
		tty     bool
		want    bool
		wantErr string
	}{
		{name: "always beats NO_COLOR and a pipe", mode: colorAlways, env: map[string]string{"NO_COLOR": "1"}, want: true},
		{name: "never beats CLICOLOR_FORCE and a tty", mode: colorNever, env: map[string]string{"CLICOLOR_FORCE": "1"}, tty: true, want: false},
		{name: "auto honors CLICOLOR_FORCE into a pipe", mode: colorAuto, env: map[string]string{"CLICOLOR_FORCE": "1"}, want: true},
		{name: "auto honors NO_COLOR on a tty", mode: colorAuto, env: map[string]string{"NO_COLOR": "1"}, tty: true, want: false},
		{name: "force outranks NO_COLOR", mode: colorAuto, env: map[string]string{"CLICOLOR_FORCE": "1", "NO_COLOR": "1"}, want: true},
		{name: "TERM=dumb disables on a tty", mode: colorAuto, env: map[string]string{"TERM": "dumb"}, tty: true, want: false},
		{name: "TERM is matched exactly not by prefix", mode: colorAuto, env: map[string]string{"TERM": "dumber"}, tty: true, want: true},
		{name: "auto to a pipe is off", mode: colorAuto, want: false},
		{name: "auto to a tty is on", mode: colorAuto, tty: true, want: true},
		{name: "empty behaves as auto", mode: "", tty: true, want: true},
		{name: "explicit --color= behaves as auto", mode: "", want: false},
		{name: "unknown mode errors", mode: "bogus", wantErr: `--color must be one of auto, always, never (got "bogus")`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout interface {
				Write([]byte) (int, error)
			} = &bytes.Buffer{}
			if tt.tty {
				stdout = devNull(t)
			}
			got, err := resolveColor(tt.mode, stdout, fakeEnv(tt.env))
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveColor(%q) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	t.Run("a buffer is not a terminal", func(t *testing.T) {
		if isTerminal(&bytes.Buffer{}) {
			t.Error("bytes.Buffer reported as a terminal")
		}
	})
	t.Run("a regular file is not a terminal", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "aistat")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if isTerminal(f) {
			t.Error("regular file reported as a terminal")
		}
	})
	// Terminal detection means character-device, not true TTY, so
	// `--watch > /dev/null` is admitted and loops rendering nothing. That gap is
	// deliberate (a real TTY test needs arch-specific ioctls) and is pinned here
	// so the next reader finds it documented rather than surprising.
	t.Run("a character device is a terminal", func(t *testing.T) {
		if !isTerminal(devNull(t)) {
			t.Error("os.DevNull not reported as a terminal")
		}
	})
}

// TestColorModeValidationIsUniform is the guard against --color being validated
// only where it is consumed: `usage` renders colored text, the others do not, so
// a check at the point of use would let two of these three accept a bad value.
func TestColorModeValidationIsUniform(t *testing.T) {
	const want = `--color must be one of auto, always, never (got "bogus")`
	tests := []struct {
		name string
		args []string
	}{
		{"usage", []string{"usage", "--color=bogus"}},
		{"switch", []string{"switch", "--color=bogus"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMemoryStore(t)
			var stdout, stderr bytes.Buffer
			if code := run(tt.args, &stdout, &stderr); code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
			}
		})
	}

	// accounts goes through runAccounts with injected memory stores rather than
	// the top-level run: runAccountsSubcommand opens both real account stores
	// before any flag parsing, so a machine with an unreadable keychain would
	// report a store-open error here instead of the color error.
	t.Run("accounts list", func(t *testing.T) {
		r := runAccountsTest(testutil.MemStore(t), noopResolver, globals{}, "list", "--color=bogus")
		if r.code != 2 {
			t.Errorf("exit code = %d, want 2 (stderr: %s)", r.code, r.stderr)
		}
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", r.stderr, want)
		}
	})

	t.Run("help still short-circuits a bad color", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--color=bogus", "--help"}, &stdout, &stderr); code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		if !strings.Contains(stdout.String(), "aistat — report and manage") {
			t.Errorf("stdout = %q, want help text", stdout.String())
		}
	})
}
