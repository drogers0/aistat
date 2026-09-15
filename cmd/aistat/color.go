package main

import (
	"fmt"
	"io"
	"os"
)

// Accepted --color values. Empty is treated as colorAuto: both the flag's unset
// default and an explicit `--color=` land there.
const (
	colorAuto   = "auto"
	colorAlways = "always"
	colorNever  = "never"
)

// validateColorMode reports whether mode is an accepted --color value. It is
// the single source of the error text, shared by handleGlobals (which rejects a
// bad value for every subcommand) and resolveColor.
func validateColorMode(mode string) error {
	switch mode {
	case "", colorAuto, colorAlways, colorNever:
		return nil
	}
	return fmt.Errorf("--color must be one of auto, always, never (got %q)", mode)
}

// resolveColor decides whether text output carries ANSI escapes. Precedence,
// highest first: --color, CLICOLOR_FORCE, NO_COLOR, TERM=dumb, stdout is a
// terminal. CLICOLOR_FORCE outranks NO_COLOR so a wrapper can force color into
// a pipe even where the environment sets NO_COLOR globally.
func resolveColor(mode string, stdout io.Writer, getenv func(string) string) (bool, error) {
	if err := validateColorMode(mode); err != nil {
		return false, err
	}
	switch mode {
	case colorAlways:
		return true, nil
	case colorNever:
		return false, nil
	}
	if getenv("CLICOLOR_FORCE") != "" {
		return true, nil
	}
	if getenv("NO_COLOR") != "" {
		return false, nil
	}
	if getenv("TERM") == "dumb" {
		return false, nil
	}
	return isTerminalFn(stdout), nil
}

// isTerminalFn is the terminal-detection seam. Tests replace it to exercise the
// watch loop without a real character device; production always uses isTerminal.
var isTerminalFn = isTerminal

// isTerminal reports whether w is an os.File backed by a character device.
//
// Deliberately pure: it inspects, it never configures the terminal. It is also
// deliberately not a true TTY test: that needs a TCGETS/TIOCGETA ioctl, which is
// GOOS- and arch-specific raw syscall code, or a third-party dependency. The
// practical gap is `> /dev/null`, which reports true here. A redraw loop against
// /dev/null is a visibly hung command the user cancels, not a wrong answer.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
