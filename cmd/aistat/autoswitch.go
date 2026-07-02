package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/drogers0/aistat/v2/internal/autoswitch"
	"github.com/drogers0/aistat/v2/internal/orchestrate"
)

// Package-level injection seams — overridden by tests.
var (
	autoswitchGOOS       = runtime.GOOS
	autoswitchHomeDir    = os.UserHomeDir
	autoswitchExecutable = os.Executable
	autoswitchUID        = os.Getuid

	// runLaunchctl invokes launchctl, returning combined output for error context.
	runLaunchctl = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	}
)

// autoswitchPaths bundles the three files the subcommand manages.
type autoswitchPaths struct {
	plist string
	log   string
	env   string
}

func resolveAutoswitchPaths() (autoswitchPaths, error) {
	home, err := autoswitchHomeDir()
	if err != nil {
		return autoswitchPaths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	envPath, err := autoswitchEnvPath()
	if err != nil {
		return autoswitchPaths{}, err
	}
	return autoswitchPaths{
		plist: filepath.Join(home, "Library", "LaunchAgents", autoswitch.LaunchdLabel+".plist"),
		log:   filepath.Join(home, "Library", "Logs", "aistat-autoswitch.log"),
		env:   envPath,
	}, nil
}

func launchdDomain() string {
	return fmt.Sprintf("gui/%d", autoswitchUID())
}

// runAutoswitch implements `aistat autoswitch install|uninstall|status`.
func runAutoswitch(args []string, stdout, stderr io.Writer, g globals) int {
	fs := flag.NewFlagSet("autoswitch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var fiveHour, weekly string
	var interval int
	fs.StringVar(&fiveHour, "if-above-5h", "85", "")
	fs.StringVar(&weekly, "if-above-weekly", "95", "")
	fs.IntVar(&interval, "interval", 300, "")
	registerGlobalFlags(fs, &g)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(g, stdout); handled {
		return code
	}
	var verb string
	tail := fs.Args()
	if len(tail) > 0 {
		verb = tail[0]
		tail = tail[1:]
	}
	if err := fs.Parse(tail); err != nil {
		fmt.Fprintln(stderr, err.Error())
		return int(orchestrate.StatusUsageError)
	}
	if handled, code := handleGlobals(g, stdout); handled {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected argument: %s\n", fs.Arg(0))
		return int(orchestrate.StatusUsageError)
	}

	if autoswitchGOOS != "darwin" {
		fmt.Fprintln(stderr, "autoswitch requires launchd (macOS); run `aistat switch --if-needed` from cron or a systemd timer instead")
		return int(orchestrate.StatusUsageError)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch verb {
	case "install":
		return runAutoswitchInstall(ctx, fiveHour, weekly, interval, stdout, stderr)
	case "uninstall":
		return runAutoswitchUninstall(ctx, stdout, stderr)
	case "status":
		return runAutoswitchStatus(ctx, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown autoswitch verb %q — want install, uninstall, or status\n", verb)
		return int(orchestrate.StatusUsageError)
	}
}

func runAutoswitchInstall(ctx context.Context, fiveHour, weekly string, interval int, stdout, stderr io.Writer) int {
	if _, err := autoswitch.ParseThreshold(fiveHour); err != nil {
		fmt.Fprintf(stderr, "--if-above-5h: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}
	if _, err := autoswitch.ParseThreshold(weekly); err != nil {
		fmt.Fprintf(stderr, "--if-above-weekly: %s\n", err)
		return int(orchestrate.StatusUsageError)
	}
	if interval < 60 {
		fmt.Fprintln(stderr, "--interval must be at least 60 seconds")
		return int(orchestrate.StatusUsageError)
	}
	paths, err := resolveAutoswitchPaths()
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	if err := autoswitch.WriteEnvFile(paths.env, fiveHour, weekly); err != nil {
		fmt.Fprintf(stderr, "aistat: write thresholds file: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	bin, err := autoswitchExecutable()
	if err != nil {
		fmt.Fprintf(stderr, "aistat: resolve own binary path: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	for _, dir := range []string{filepath.Dir(paths.plist), filepath.Dir(paths.log)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(stderr, "aistat: %s\n", err)
			return int(orchestrate.StatusAnyFailed)
		}
	}
	if err := os.WriteFile(paths.plist, []byte(autoswitch.PlistXML(bin, interval, paths.log)), 0o644); err != nil {
		fmt.Fprintf(stderr, "aistat: write plist: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	// bootout is expected to fail on first install (job not loaded) — ignored.
	_, _ = runLaunchctl(ctx, "bootout", launchdDomain()+"/"+autoswitch.LaunchdLabel)
	if out, err := runLaunchctl(ctx, "bootstrap", launchdDomain(), paths.plist); err != nil {
		fmt.Fprintf(stderr, "aistat: launchctl bootstrap failed: %s\n%s", err, out)
		return int(orchestrate.StatusAnyFailed)
	}
	fmt.Fprintf(stdout, "installed %s (every %ds)\nthresholds: %s (edit anytime; no reinstall needed)\nlog: %s\n",
		paths.plist, interval, paths.env, paths.log)
	return 0
}

func runAutoswitchUninstall(ctx context.Context, stdout, stderr io.Writer) int {
	paths, err := resolveAutoswitchPaths()
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	// Ignore bootout failure — the job may not be loaded.
	_, _ = runLaunchctl(ctx, "bootout", launchdDomain()+"/"+autoswitch.LaunchdLabel)
	err = os.Remove(paths.plist)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "autoswitch is not installed")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "aistat: remove plist: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	fmt.Fprintf(stdout, "uninstalled %s (thresholds file kept: %s)\n", paths.plist, paths.env)
	return 0
}

func runAutoswitchStatus(ctx context.Context, stdout, stderr io.Writer) int {
	paths, err := resolveAutoswitchPaths()
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	plistData, err := os.ReadFile(paths.plist)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "installed: no")
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "aistat: read plist: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	fmt.Fprintf(stdout, "installed: yes (%s)\n", paths.plist)
	if _, err := runLaunchctl(ctx, "print", launchdDomain()+"/"+autoswitch.LaunchdLabel); err != nil {
		fmt.Fprintln(stdout, "loaded: no")
	} else {
		fmt.Fprintln(stdout, "loaded: yes")
	}
	if n, ok := autoswitch.ParseInterval(string(plistData)); ok {
		fmt.Fprintf(stdout, "interval: %ds\n", n)
	}
	fileVals, err := autoswitch.ReadEnvFile(paths.env)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	th, err := autoswitch.Resolve(os.Getenv, fileVals)
	if err != nil {
		fmt.Fprintf(stderr, "aistat: %s\n", err)
		return int(orchestrate.StatusAnyFailed)
	}
	fmt.Fprintf(stdout, "thresholds: five_hour %s, weekly %s\n", formatThreshold(th.FiveHour), formatThreshold(th.Weekly))
	return 0
}

func formatThreshold(t autoswitch.Threshold) string {
	if t.Off {
		return "off"
	}
	return fmt.Sprintf(">=%.0f%%", t.Pct)
}
