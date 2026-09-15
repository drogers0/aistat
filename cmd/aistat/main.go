package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/drogers0/aistat/v2/internal/orchestrate"
)

// version is the goreleaser-injected build tag (via `-ldflags "-X main.version=..."`);
// empty for go-install or working-tree builds, in which case resolvedVersion()
// falls back to debug.ReadBuildInfo — real ("vX") for `go install …@vX`, "(devel)" → "dev"
// for working-tree builds.
var version = ""

func resolvedVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

var helpText = buildHelpText()

func buildHelpText() string {
	var sb strings.Builder
	sb.WriteString("aistat — report and manage Claude / Codex / Copilot usage\n\nUsage:\n  aistat [global flags] [subcommand] [args]\n\nSubcommands:\n  usage [<provider>]                             Report usage for all providers (default), or one: claude, codex, copilot\n  switch [<provider>] [flags]                    Switch the live account; bulk if no provider, conditional with a threshold flag or --watch\n  accounts list [<provider>]                     List stored accounts (all providers if omitted)\n  accounts remove <id> [<provider>]              Remove a stored account; provider inferred from id if unambiguous\n\nGlobal flags:\n  -h, --human    Render human-readable text instead of JSON\n      --color <when>  Colorize -h output: auto (default), always, never.\n                      Also honors CLICOLOR_FORCE, NO_COLOR and TERM=dumb.\n      --debug    Write per-request and per-provider lines to stderr\n      --version  Print version and exit\n      --help     Print this help and exit\n\nusage flags:\n  --refresh      Bypass the per-account usage cache (~90s TTL) and force a fresh read\n  --watch, -w    Redraw the report on a timer in the foreground (requires -h and a terminal)\n  --interval N   Seconds between --watch redraws (default 15, minimum 1)\n\nswitch flags:\n  --to <id>          Switch to a specific stored account (address, organization slug, email substring, or UUID prefix)\n  --if-above-5h N    Used-percent threshold for the 5-hour window; presence makes the switch\n                     conditional (only switch when the active account exceeds it). \"off\" disables.\n  --if-above-weekly N  Used-percent threshold for the weekly window (as above)\n                     Threshold precedence per window: flag > env AISTAT_IF_ABOVE_5H /\n                     AISTAT_IF_ABOVE_WEEKLY > built-in default (85 / 95).\n  --watch, -w        Run the conditional switch on a timer in the foreground (implies conditional;\n                     always notifies, dedup'd across ticks; daemonize via launchd/systemd)\n  --interval N       Seconds between --watch checks (default 300, minimum 60)\n  --notify           Desktop notification on switch, or when a threshold is hit with no better account (macOS)\n\nswitch notes:\n  Without a provider, switches every provider that has ≥2 stored accounts.\n  --to <id> matches a full address, a unique organization slug, an email substring, or a UUID prefix. Without a provider arg,\n  the provider is inferred when <id> matches exactly one provider's store.\n  --to cannot be combined with threshold flags or --watch.\n\naccounts notes:\n  Default output is JSON; use -h/--human for text.\n  remove infers the provider when <id> is unambiguous across stores.\n\nExit codes:\n  0  All requested operations succeeded (or nothing to do).\n  1  One or more providers failed at runtime.\n  2  Usage error (unknown subcommand, malformed flags, ambiguous id).\n  3  Stdout write error (broken pipe, disk full).\n")
	return sb.String()
}

// globals holds the global flags shared across all subcommands.
type globals struct {
	Debug   bool
	Human   bool
	Help    bool
	Version bool
	Color   string // auto (default/empty) | always | never
}

// scanGlobals walks args left-to-right, consuming known global flags and
// returning the first non-flag token as the subcommand. Unknown flags are
// left in rest and passed to the subcommand's own FlagSet.
//
// Rules:
//   - "--debug" / "--human" / "-h" / "--help" / "--version" → set the matching bool
//   - "--<boolean-global>=<value>" → return error (reject =value form for bools)
//   - "--color=<value>" or "--color <value>" → set Color, consuming the value
//   - Any other value-taking flag → passed to rest along with its value
//   - Any other token starting with "-" or "--" → append to rest
//   - First non-flag token → subcommand; stop. rest is every non-global flag
//     seen before it, plus every remaining token after it.
//
// The loop is indexed rather than ranged because a value-taking flag consumes
// the following token.
func scanGlobals(args []string) (g globals, sub string, rest []string, err error) {
	boolGlobals := map[string]bool{
		"debug": true, "human": true, "h": true, "help": true, "version": true,
	}
	// Every flag that takes a value must be recognized here, whether or not it is
	// a global, because the scanner cannot find the subcommand token without
	// knowing which tokens are flag *values*. Otherwise `aistat --color never
	// usage` parses "never" as the subcommand, and so does `aistat -h --watch
	// --interval 15` with "15". A nil target means "not a global": pass the flag
	// and its value through to rest for the subcommand's own FlagSet.
	valueFlags := map[string]*string{
		"color":           &g.Color,
		"interval":        nil,
		"to":              nil,
		"if-above-5h":     nil,
		"if-above-weekly": nil,
		// Registered even though it only exists under -tags=fake: this file is
		// not build-tagged, and an unknown --fake-fail still fails loudly at the
		// subcommand FlagSet in a production build.
		"fake-fail": nil,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			// First non-flag token is the subcommand. Flags seen before it are
			// kept: returning only args[i+1:] would silently drop them, so
			// `aistat --refresh usage` ignored --refresh entirely.
			return g, arg, append(rest, args[i+1:]...), nil
		}
		// Strip leading dashes to get the flag name, checking for = form.
		name := strings.TrimLeft(arg, "-")
		if eqIdx := strings.IndexByte(name, '='); eqIdx >= 0 {
			flagName := name[:eqIdx]
			if boolGlobals[flagName] {
				return g, "", nil, fmt.Errorf("--flag=value form not supported for global flags; use `--%s`", flagName)
			}
			if target, ok := valueFlags[flagName]; ok {
				// The =value form carries its own value, so nothing to consume.
				if target != nil {
					*target = name[eqIdx+1:]
					continue
				}
			}
			// Unknown flag with = form: leave in rest; subcommand FlagSet handles it.
			rest = append(rest, arg)
			continue
		}
		if target, ok := valueFlags[name]; ok {
			if i+1 >= len(args) {
				return g, "", nil, fmt.Errorf("flag needs an argument: --%s", name)
			}
			i++
			if target != nil {
				*target = args[i]
			} else {
				rest = append(rest, arg, args[i])
			}
			continue
		}
		switch name {
		case "debug":
			g.Debug = true
		case "human", "h":
			g.Human = true
		case "help":
			g.Help = true
		case "version":
			g.Version = true
		default:
			rest = append(rest, arg)
		}
	}
	// No non-flag token found — no subcommand.
	return g, "", rest, nil
}

// registerGlobalFlags registers --color, --debug, --human, -h, --help, and
// --version on fs, mutating g when parsed. This allows global flags placed after the
// subcommand token to be accepted (e.g. `aistat usage claude --debug`).
func registerGlobalFlags(fs *flag.FlagSet, g *globals) {
	fs.StringVar(&g.Color, "color", g.Color, "")
	fs.BoolVar(&g.Debug, "debug", g.Debug, "")
	fs.BoolVar(&g.Human, "human", g.Human, "")
	fs.BoolVar(&g.Human, "h", g.Human, "")
	fs.BoolVar(&g.Help, "help", g.Help, "")
	fs.BoolVar(&g.Version, "version", g.Version, "")
}

// handleGlobals validates global flag values, then prints help/version on
// stdout if either flag is set, returning (true, code) when it handled the run.
// Subcommand entry points call this after FlagSet parsing so `aistat <sub>
// --help` and `aistat <sub> --version` both work uniformly.
//
// Validating --color here rather than where it is consumed is what makes a bad
// value behave identically for every subcommand: only `usage` renders colored
// text, so a check at the point of use would let `accounts list --color=bogus`
// and `switch --color=bogus` accept it silently. Re-running on each parse pass
// is harmless, being a pure check on the same value, and it catches a --color
// that appears after the subcommand token.
func handleGlobals(g globals, stdout, stderr io.Writer) (handled bool, code int) {
	// Help and version short-circuit first, so `--color=bogus --help` still helps.
	if g.Help {
		fmt.Fprint(stdout, helpText)
		return true, 0
	}
	if g.Version {
		fmt.Fprintln(stdout, resolvedVersion())
		return true, 0
	}
	if err := validateColorMode(g.Color); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return true, int(orchestrate.StatusUsageError)
	}
	return false, 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	g, sub, rest, scanErr := scanGlobals(args)
	if scanErr != nil {
		fmt.Fprintln(stderr, scanErr.Error())
		return int(orchestrate.StatusUsageError)
	}

	// --help and --version short-circuit before subcommand dispatch.
	if handled, code := handleGlobals(g, stdout, stderr); handled {
		return code
	}

	switch sub {
	case "", "usage":
		return runUsage(rest, stdout, stderr, g)
	case "switch":
		return runSwitch(rest, stdout, stderr, g)
	case "accounts":
		return runAccountsSubcommand(rest, stdout, stderr, g)
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", sub)
		return int(orchestrate.StatusUsageError)
	}
}
