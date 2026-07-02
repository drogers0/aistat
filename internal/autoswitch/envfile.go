package autoswitch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultEnvFilePath is where launchd-driven runs read their thresholds from,
// since launchd does not inherit the user's shell environment.
func DefaultEnvFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "aistat", "autoswitch.env"), nil
}

// ReadEnvFile parses path as KEY=VALUE lines ("#" comments and blank lines
// allowed; duplicate keys are last-wins). A missing file is not an error — it
// returns nil so the resolution chain falls through to defaults.
func ReadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	vals := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return nil, fmt.Errorf("%s:%d: malformed line (want KEY=VALUE)", path, i+1)
		}
		vals[strings.TrimSpace(line[:eq])] = strings.TrimSpace(line[eq+1:])
	}
	return vals, nil
}

// WriteEnvFile writes both threshold values, creating parent directories as
// needed. Values are written verbatim — callers validate via ParseThreshold
// first. The write goes through a temporary file in the same directory plus
// os.Rename (the pattern in internal/cred/keychain_linux.go): launchd-driven
// runs read this file every few minutes, so a torn write must never be
// observable.
func WriteEnvFile(path, fiveHour, weekly string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating autoswitch env directory: %w", err)
	}
	content := "# aistat conditional-switch thresholds: used percent 1-100, or \"off\".\n" +
		"# Read on every `aistat switch --if-needed` run — edit freely, no reinstall needed.\n" +
		EnvFiveHour + "=" + fiveHour + "\n" +
		EnvWeekly + "=" + weekly + "\n"

	tmp, err := os.CreateTemp(dir, ".autoswitch-*.env")
	if err != nil {
		return fmt.Errorf("creating temporary autoswitch env file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("setting autoswitch env file mode: %w", err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing autoswitch env file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing autoswitch env file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing autoswitch env file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming autoswitch env file into place: %w", err)
	}
	committed = true
	return nil
}
