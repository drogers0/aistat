package main

import (
	"bytes"
	"context"
	"io"
	"time"
)

// defaultWatchIntervalSecs is the redraw cadence for `usage --watch`. Lower than
// `switch --watch`'s 300s because this loop only redraws: it reads through the
// same 90s-cached path as a one-shot run, so at 15s roughly one tick in six does
// real HTTP.
const defaultWatchIntervalSecs = 15

// Terminal control for the redraw loop. Clear-and-redraw rather than the
// alternate screen buffer, matching watch(1) and leaving scrollback alone.
const (
	cursorHide = "\x1b[?25l"
	cursorShow = "\x1b[?25h"
	// Home, then erase to end of screen. Erase-to-end rather than a full 2J
	// clear is what keeps the redraw from flickering.
	frameHome = "\x1b[H\x1b[0J"
	// DECSET 2026, synchronized output: Ghostty, kitty, WezTerm and recent
	// iTerm2 hold the frame until the end sequence; others ignore it.
	syncBegin = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
)

// usageWatchSleepFn is the interruptible inter-tick wait, injected in tests.
var usageWatchSleepFn = sleepWithCtx

// runUsageWatch redraws the report every interval until ctx is cancelled or a
// frame fails to reach the terminal. Each frame is built in full and written
// once, so the terminal never shows a half-drawn report. Returns the write error
// that stopped it, or nil on signal-driven shutdown.
func runUsageWatch(ctx context.Context, stdout io.Writer, interval time.Duration, tick func(io.Writer)) error {
	// Armed before the hide is attempted: a partial write (n > 0, err != nil)
	// can hide the cursor and still fail, and leaving a user's cursor hidden
	// after we exit is worse than one redundant restore sequence.
	defer func() { _, _ = io.WriteString(stdout, cursorShow) }()
	if _, err := io.WriteString(stdout, cursorHide); err != nil {
		return err // terminal is already gone; never enter the loop
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeErr error
	watchLoop(ctx, interval, func() {
		var buf bytes.Buffer
		buf.WriteString(syncBegin)
		buf.WriteString(frameHome)
		tick(&buf)
		buf.WriteString(syncEnd)
		if _, err := stdout.Write(buf.Bytes()); err != nil {
			// Looping on a dead descriptor would spin forever. Cancelling the
			// derived context stops watchLoop without giving it an error
			// channel it does not need for `switch --watch`.
			writeErr = err
			cancel()
		}
	}, usageWatchSleepFn)
	return writeErr
}
