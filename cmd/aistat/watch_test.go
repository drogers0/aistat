package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDedupNotifier exercises newDedupNotifier's suppression window.
func TestDedupNotifier(t *testing.T) {
	type call struct {
		title, msg string
	}
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"first message for a title always sends", func(t *testing.T) {
			var calls []call
			nowVal := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time { return nowVal }
			inner := func(_ context.Context, title, msg string) error {
				calls = append(calls, call{title, msg})
				return nil
			}
			d := newDedupNotifier(inner, time.Hour, now)
			if err := d(context.Background(), "Claude", "hit 85%"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("expected 1 call, got %d: %v", len(calls), calls)
			}
		}},
		{"identical message within cooldown is suppressed", func(t *testing.T) {
			var calls []call
			nowVal := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time { return nowVal }
			inner := func(_ context.Context, title, msg string) error {
				calls = append(calls, call{title, msg})
				return nil
			}
			d := newDedupNotifier(inner, time.Hour, now)
			_ = d(context.Background(), "Claude", "hit 85%")
			nowVal = nowVal.Add(10 * time.Minute)
			_ = d(context.Background(), "Claude", "hit 85%")
			if len(calls) != 1 {
				t.Fatalf("expected 1 call (second suppressed), got %d: %v", len(calls), calls)
			}
		}},
		{"changed message for same title sends immediately", func(t *testing.T) {
			var calls []call
			nowVal := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time { return nowVal }
			inner := func(_ context.Context, title, msg string) error {
				calls = append(calls, call{title, msg})
				return nil
			}
			d := newDedupNotifier(inner, time.Hour, now)
			_ = d(context.Background(), "Claude", "hit 85%")
			nowVal = nowVal.Add(time.Minute)
			_ = d(context.Background(), "Claude", "switched to a@b.com")
			if len(calls) != 2 {
				t.Fatalf("expected 2 calls (changed message resets), got %d: %v", len(calls), calls)
			}
			if calls[1].msg != "switched to a@b.com" {
				t.Errorf("second call msg = %q, want %q", calls[1].msg, "switched to a@b.com")
			}
		}},
		{"same message resends after cooldown elapses", func(t *testing.T) {
			var calls []call
			nowVal := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time { return nowVal }
			inner := func(_ context.Context, title, msg string) error {
				calls = append(calls, call{title, msg})
				return nil
			}
			d := newDedupNotifier(inner, time.Hour, now)
			_ = d(context.Background(), "Claude", "hit 85%")
			nowVal = nowVal.Add(2 * time.Hour)
			_ = d(context.Background(), "Claude", "hit 85%")
			if len(calls) != 2 {
				t.Fatalf("expected 2 calls (cooldown elapsed), got %d: %v", len(calls), calls)
			}
		}},
		{"different title is independent", func(t *testing.T) {
			var calls []call
			nowVal := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time { return nowVal }
			inner := func(_ context.Context, title, msg string) error {
				calls = append(calls, call{title, msg})
				return nil
			}
			d := newDedupNotifier(inner, time.Hour, now)
			_ = d(context.Background(), "Claude", "hit 85%")
			_ = d(context.Background(), "Codex", "hit 85%")
			if len(calls) != 2 {
				t.Fatalf("expected 2 calls (independent titles), got %d: %v", len(calls), calls)
			}
		}},
		{"failed send is not remembered so the next tick retries", func(t *testing.T) {
			var calls []call
			nowVal := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			now := func() time.Time { return nowVal }
			failNext := true
			inner := func(_ context.Context, title, msg string) error {
				calls = append(calls, call{title, msg})
				if failNext {
					failNext = false
					return errors.New("osascript hiccup")
				}
				return nil
			}
			d := newDedupNotifier(inner, time.Hour, now)
			if err := d(context.Background(), "Claude", "hit 85%"); err == nil {
				t.Fatal("expected the failed send to surface its error")
			}
			nowVal = nowVal.Add(time.Minute) // still well within cooldown
			if err := d(context.Background(), "Claude", "hit 85%"); err != nil {
				t.Fatalf("retry after a failed send should succeed: %v", err)
			}
			if len(calls) != 2 {
				t.Fatalf("expected 2 calls (failed send must not suppress the retry), got %d: %v", len(calls), calls)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestWatchLoop exercises the tick/sleep loop mechanics.
func TestWatchLoop(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"ticks N times then stops when sleep signals cancellation", func(t *testing.T) {
			const n = 3
			ticks := 0
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sleep := func(_ context.Context, _ time.Duration) error {
				calls++
				if calls >= n {
					cancel()
					return context.Canceled
				}
				return nil
			}
			watchLoop(ctx, time.Millisecond, func() { ticks++ }, sleep)
			if ticks != n {
				t.Errorf("ticks = %d, want %d", ticks, n)
			}
		}},
		{"already-cancelled context still runs tick once", func(t *testing.T) {
			ticks := 0
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // pre-cancelled
			sleep := func(ctx context.Context, _ time.Duration) error {
				return ctx.Err()
			}
			watchLoop(ctx, time.Millisecond, func() { ticks++ }, sleep)
			if ticks != 1 {
				t.Errorf("ticks = %d, want 1", ticks)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// Watch-entry validation now lives at the switch entry point; see
// TestSwitchConditional's watch cases in switch_conditional_test.go.
