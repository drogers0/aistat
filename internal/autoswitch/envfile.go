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
// allowed). A missing file is not an error — it returns an empty map so the
// resolution chain falls through to defaults.
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
// first.
func WriteEnvFile(path, fiveHour, weekly string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content := "# aistat conditional-switch thresholds: used percent 1-100, or \"off\".\n" +
		"# Read on every `aistat switch --if-needed` run — edit freely, no reinstall needed.\n" +
		EnvFiveHour + "=" + fiveHour + "\n" +
		EnvWeekly + "=" + weekly + "\n"
	return os.WriteFile(path, []byte(content), 0o644)
}
