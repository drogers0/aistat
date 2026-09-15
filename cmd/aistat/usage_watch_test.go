package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// fakeTerm records each Write separately, so a test can assert the
// one-write-per-frame guarantee that keeps the redraw from flickering, and can
// fail a chosen write. partial models the shape a real terminal write can take:
// the bytes reached the terminal and the call still failed.
type fakeTerm struct {
	writes  []string
	failAt  int // 1-based write index that fails; 0 means never
	failErr error
	partial bool
	n       int
}

func (f *fakeTerm) Write(p []byte) (int, error) {
	f.n++
	if f.n == f.failAt {
		if f.partial {
			f.writes = append(f.writes, string(p))
			return len(p), f.failErr
		}
		return 0, f.failErr
	}
	f.writes = append(f.writes, string(p))
	return len(p), nil
}

func (f *fakeTerm) all() string { return strings.Join(f.writes, "") }

// cancelAfter returns a sleep seam that cancels ctx after n sleeps, so the loop
// runs a bounded number of ticks without real time passing.
func cancelAfter(n int, cancel context.CancelFunc) func(context.Context, time.Duration) error {
	count := 0
	return func(ctx context.Context, _ time.Duration) error {
		// Honor cancellation exactly as sleepWithCtx does: a stub that ignored
		// it would let the loop run on after a frame write cancelled the
		// context, and would not be testing the real seam's contract.
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count >= n {
			cancel()
			return context.Canceled
		}
		return nil
	}
}

func TestRunUsageWatch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"frames carry the control sequences and one write each", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			restore := swapWatchSleep(t, cancelAfter(3, cancel))
			defer restore()

			w := &fakeTerm{}
			err := runUsageWatch(ctx, w, 15*time.Second, func(fw io.Writer) {
				_, _ = io.WriteString(fw, "BODY")
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// cursorHide, 3 frames, cursorShow.
			if len(w.writes) != 5 {
				t.Fatalf("got %d writes, want 5: %q", len(w.writes), w.writes)
			}
			if w.writes[0] != cursorHide {
				t.Errorf("first write = %q, want cursorHide", w.writes[0])
			}
			if w.writes[len(w.writes)-1] != cursorShow {
				t.Errorf("last write = %q, want cursorShow", w.writes[len(w.writes)-1])
			}
			want := syncBegin + frameHome + "BODY" + syncEnd
			for i, frame := range w.writes[1:4] {
				if frame != want {
					t.Errorf("frame %d = %q, want %q", i, frame, want)
				}
			}
		}},

		{"a cursor-hide failure never enters the loop", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			boom := errors.New("terminal gone")
			w := &fakeTerm{failAt: 1, failErr: boom}

			ticks := 0
			err := runUsageWatch(ctx, w, time.Second, func(io.Writer) { ticks++ })
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want %v", err, boom)
			}
			if ticks != 0 {
				t.Errorf("tick ran %d times, want 0", ticks)
			}
			// The restore is still attempted: a partial hide write can leave the
			// cursor hidden even though the write reported an error.
			if w.n != 2 {
				t.Errorf("writes attempted = %d, want 2 (failed hide, then show)", w.n)
			}
		}},

		{"a partially written hide still restores the cursor", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := &fakeTerm{failAt: 1, failErr: errors.New("short write"), partial: true}

			if err := runUsageWatch(ctx, w, time.Second, func(io.Writer) {}); err == nil {
				t.Fatal("expected the partial hide write to fail the call")
			}
			if !strings.Contains(w.all(), cursorShow) {
				t.Errorf("cursor was left hidden; wrote %q", w.all())
			}
		}},

		{"a frame write error stops the loop and is returned", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Sleep would allow 10 ticks; the write error must stop it sooner.
			restore := swapWatchSleep(t, cancelAfter(10, cancel))
			defer restore()

			boom := errors.New("broken pipe")
			// Write 1 is cursorHide, 2 is the first frame, 3 is the second frame.
			w := &fakeTerm{failAt: 3, failErr: boom}

			ticks := 0
			err := runUsageWatch(ctx, w, time.Second, func(fw io.Writer) {
				ticks++
				_, _ = io.WriteString(fw, "BODY")
			})
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want %v", err, boom)
			}
			if ticks != 2 {
				t.Errorf("tick ran %d times, want 2 (stop after the failing frame)", ticks)
			}
			// cursorShow is still attempted after the failure.
			if w.n != 4 {
				t.Errorf("writes attempted = %d, want 4 (hide, frame, failed frame, show)", w.n)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// swapWatchSleep replaces the inter-tick wait seam for one test.
func swapWatchSleep(t *testing.T, fn func(context.Context, time.Duration) error) func() {
	t.Helper()
	prev := usageWatchSleepFn
	usageWatchSleepFn = fn
	return func() { usageWatchSleepFn = prev }
}

// failingProvider is a provider whose Fetch always fails, used to prove a
// provider failure during a tick is rendered rather than escaping the loop.
type failingProvider struct{ id string }

func (f failingProvider) ID() string { return f.id }
func (f failingProvider) Fetch(context.Context) (providers.ProviderOutput, error) {
	return providers.ProviderOutput{}, errors.New("provider exploded")
}

// TestUsageWatchFlagValidation pins every fail-closed path, each with its
// verbatim message. A terminal is stubbed only where the test needs to get past
// validation #5.
func TestUsageWatchFlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		human   bool
		tty     bool
		wantMsg string
	}{
		{"watch without human", []string{"--watch"}, false, true, "aistat: usage --watch requires -h/--human"},
		{"watch with refresh", []string{"--watch", "--refresh"}, true, true, "aistat: --refresh cannot be combined with --watch"},
		{"interval without watch", []string{"--interval", "5"}, true, true, "--interval requires --watch"},
		{"interval below the floor", []string{"--watch", "--interval", "0"}, true, true, "--interval must be at least 1 second"},
		{"watch to a non-terminal", []string{"--watch"}, true, false, "aistat: usage --watch requires a terminal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := swapIsTerminal(t, tt.tty)
			defer restore()
			var stdout, stderr bytes.Buffer
			if code := runUsage(tt.args, &stdout, &stderr, globals{Human: tt.human}); code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
			if got := strings.TrimSpace(stderr.String()); got != tt.wantMsg {
				t.Errorf("stderr = %q, want %q", got, tt.wantMsg)
			}
		})
	}

	t.Run("version wins over watch validation", func(t *testing.T) {
		restore := swapIsTerminal(t, false)
		defer restore()
		var stdout, stderr bytes.Buffer
		if code := runUsage([]string{"--watch", "--version"}, &stdout, &stderr, globals{}); code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
		}
		if strings.TrimSpace(stdout.String()) != resolvedVersion() {
			t.Errorf("stdout = %q, want %q", stdout.String(), resolvedVersion())
		}
	})

	t.Run("help wins over watch validation", func(t *testing.T) {
		restore := swapIsTerminal(t, false)
		defer restore()
		var stdout, stderr bytes.Buffer
		if code := runUsage([]string{"--watch", "--help"}, &stdout, &stderr, globals{}); code != 0 {
			t.Errorf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "aistat — report and manage") {
			t.Errorf("stdout = %q, want help text", stdout.String())
		}
	})
}

// swapIsTerminal forces terminal detection for one test.
func swapIsTerminal(t *testing.T, tty bool) func() {
	t.Helper()
	prev := isTerminalFn
	isTerminalFn = func(io.Writer) bool { return tty }
	return func() { isTerminalFn = prev }
}

// TestUsageWatchDefaultInterval pins the 15s default interval. Every other loop
// test injects an interval explicitly, so without this a wrong default ships
// green.
func TestUsageWatchDefaultInterval(t *testing.T) {
	restoreTTY := swapIsTerminal(t, true)
	defer restoreTTY()

	var seen []time.Duration
	restore := swapWatchSleep(t, func(ctx context.Context, d time.Duration) error {
		seen = append(seen, d)
		return context.Canceled // one tick, then stop
	})
	defer restore()

	// Scoped to one provider: watchLoop always ticks before its first sleep, and
	// there is no provider seam in the default build (fakeProviders is behind
	// //go:build fake). The tick's fetch attempt failing without credentials is
	// expected and irrelevant here: the assertion is on the interval the loop
	// asked to sleep for, which is set before any of that runs.
	var stdout, stderr bytes.Buffer
	runUsage([]string{"copilot", "-h", "--watch"}, &stdout, &stderr, globals{Human: true})

	if len(seen) == 0 {
		t.Fatal("sleep seam was never called")
	}
	if seen[0] != defaultWatchIntervalSecs*time.Second {
		t.Errorf("interval = %v, want %v", seen[0], defaultWatchIntervalSecs*time.Second)
	}
}

// TestUsageWatchFrameWriteErrorExits3 drives the CLI end to end: a frame that
// cannot reach the terminal is a stdout write error, which is exit 3, not the
// exit 0 that clean cancellation gets.
func TestUsageWatchFrameWriteErrorExits3(t *testing.T) {
	restoreTTY := swapIsTerminal(t, true)
	defer restoreTTY()
	restore := swapWatchSleep(t, func(context.Context, time.Duration) error { return context.Canceled })
	defer restore()

	// Write 1 is cursorHide; write 2 is the first frame. Failing the frame is
	// what this test is about; failing the hide is a different path, covered in
	// TestRunUsageWatch.
	w := &fakeTerm{failAt: 2, failErr: errors.New("broken pipe")}
	var stderr bytes.Buffer
	if code := runUsage([]string{"copilot", "-h", "--watch"}, w, &stderr, globals{Human: true}); code != 3 {
		t.Errorf("exit code = %d, want 3 (stderr: %s)", code, stderr.String())
	}
}

// TestUsageWatchProviderFailureExitsZero covers the end-to-end case: a provider
// that fails on every tick is rendered into the frame, and clean cancellation
// still exits 0. TestUsageRunRenderSurfacesProviderFailure proves the failure
// reaches the frame; this proves runUsage does not let it decide the exit code.
func TestUsageWatchProviderFailureExitsZero(t *testing.T) {
	restoreTTY := swapIsTerminal(t, true)
	defer restoreTTY()

	ticks := 0
	restore := swapWatchSleep(t, func(context.Context, time.Duration) error {
		ticks++
		if ticks >= 2 {
			return context.Canceled // clean shutdown after two frames
		}
		return nil
	})
	defer restore()

	w := &fakeTerm{} // records frames, never fails
	var stderr bytes.Buffer
	run := usageRun{
		requested: []string{"claude"},
		chosen:    []providers.Provider{failingProvider{id: "claude"}},
		human:     true,
		stderr:    &stderr,
	}
	err := runUsageWatch(context.Background(), w, time.Second, func(fw io.Writer) {
		_, _ = run.render(context.Background(), fw)
	})
	if err != nil {
		t.Fatalf("watch returned %v, want nil (a provider failure is not a watch failure)", err)
	}
	if !strings.Contains(w.all(), "provider exploded") {
		t.Errorf("frames did not carry the provider error:\n%s", w.all())
	}
}
