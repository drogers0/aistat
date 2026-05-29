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
	sb.WriteString("aistat — report and manage Claude / Codex / Copilot usage\n\nUsage:\n  aistat [global flags] [subcommand] [args]\n\nSubcommands:\n  usage [provider]      Report usage for all providers (default), or one: claude, codex, copilot\n  switch [--to <id>]    Switch the live Claude account (auto-picks most headroom by default)\n  accounts list         List stored Claude accounts\n  accounts remove <id>  Remove a stored Claude account (by email or uuid)\n\nGlobal flags:\n  -h, --human    Render human-readable text instead of JSON (affects `usage` only)\n      --debug    Write per-request and per-provider lines to stderr\n      --version  Print version and exit\n      --help     Print this help and exit\n\nusage flags:\n  --refresh      Bypass the per-account usage cache (~90s TTL) and force a fresh read;\n                 writes through so the next invocation still benefits.\n\nExit codes:\n  0  All requested providers succeeded.\n  1  One or more requested providers failed at runtime.\n  2  Usage error (unknown subcommand, malformed flags).\n  3  Stdout write error (broken pipe, disk full).\n")
	return sb.String()
}

// globals holds the global flags shared across all subcommands.
type globals struct {
	Debug   bool
	Human   bool
	Help    bool
	Version bool
}

// scanGlobals walks args left-to-right, consuming known global flags and
// returning the first non-flag token as the subcommand. Unknown flags are
// left in rest and passed to the subcommand's own FlagSet.
//
// Rules:
//   - "--debug" / "--human" / "-h" / "--help" / "--version" → set the matching bool
//   - "--<known-global>=<value>" → return error (reject =value form for globals)
//   - Any other token starting with "-" or "--" → append to rest
//   - First non-flag token → subcommand; stop; remaining tokens go into rest
func scanGlobals(args []string) (g globals, sub string, rest []string, err error) {
	knownGlobals := map[string]bool{
		"debug": true, "human": true, "h": true, "help": true, "version": true,
	}
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			// First non-flag token is the subcommand. Everything after it is rest.
			return g, arg, args[i+1:], nil
		}
		// Strip leading dashes to get the flag name, checking for = form.
		name := strings.TrimLeft(arg, "-")
		if eqIdx := strings.IndexByte(name, '='); eqIdx >= 0 {
			flagName := name[:eqIdx]
			if knownGlobals[flagName] {
				return g, "", nil, fmt.Errorf("--flag=value form not supported for global flags; use `--" + flagName + "`")
			}
			// Unknown flag with = form: leave in rest; subcommand FlagSet handles it.
			rest = append(rest, arg)
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

// registerGlobalFlags registers --debug, --human, -h, --help, and --version on
// fs, mutating g when parsed. This allows global flags placed after the
// subcommand token to be accepted (e.g. `aistat usage claude --debug`).
func registerGlobalFlags(fs *flag.FlagSet, g *globals) {
	fs.BoolVar(&g.Debug, "debug", g.Debug, "")
	fs.BoolVar(&g.Human, "human", g.Human, "")
	fs.BoolVar(&g.Human, "h", g.Human, "")
	fs.BoolVar(&g.Help, "help", g.Help, "")
	fs.BoolVar(&g.Version, "version", g.Version, "")
}

// handleGlobals prints help/version on stdout if either flag is set and returns
// (true, 0). Subcommand entry points call this after FlagSet parsing so
// `aistat <sub> --help` and `aistat <sub> --version` both work uniformly.
func handleGlobals(g globals, stdout io.Writer) (handled bool, code int) {
	if g.Help {
		fmt.Fprint(stdout, helpText)
		return true, 0
	}
	if g.Version {
		fmt.Fprintln(stdout, resolvedVersion())
		return true, 0
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
	if handled, code := handleGlobals(g, stdout); handled {
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

