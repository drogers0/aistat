// Package notify shows desktop notifications. macOS uses Notification Center
// via osascript; every other platform is a silent no-op. Callers must treat
// errors as non-fatal — a failed notification never changes an exit code.
package notify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// goos is a seam over runtime.GOOS so tests can exercise darwin and
// non-darwin branches without build tags.
var goos = runtime.GOOS

// runOsascript is the command-runner seam; tests replace it. The default
// surfaces osascript's stderr in the error so TCC / notification-permission
// denials under launchd are diagnosable from the log.
var runOsascript = func(ctx context.Context, script string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// Send displays a desktop notification with the given title and message.
func Send(ctx context.Context, title, message string) error {
	if goos != "darwin" {
		return nil
	}
	script := `display notification "` + escapeAppleScript(message) + `" with title "` + escapeAppleScript(title) + `"`
	return runOsascript(ctx, script)
}

// escapeAppleScript escapes backslashes and double quotes for embedding in an
// AppleScript string literal.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
