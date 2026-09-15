// Package watchstate publishes and reads the state of running
// `aistat switch --watch` daemons.
//
// A watcher's thresholds are resolved once at launch from its own flags and
// environment (a launchd plist or systemd unit), not the user's interactive
// shell. `aistat usage` therefore cannot recompute them; it must read what the
// running process actually holds. Each watcher owns exactly one file,
// $CACHE/aistat/watch/<pid>.json, rewritten atomically every tick, so there is
// no cross-process contention and no lock file (unlike the usage cache, whose
// single shared file needs one).
//
// Heartbeats are advisory. A watcher killed without cleanup leaves its file
// behind; readers detect that from LastTick via Stale rather than by probing
// the process table. Leftovers are reaped two ways: a starting watcher
// supersedes heartbeats with its own scope (a launchd/systemd restart replaces
// its predecessor), and List deletes any heartbeat older than expireAfter.
package watchstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// expireAfter is how long a heartbeat may go without a tick before List
// deletes it: long enough to surface a dead watcher as STALE, short enough
// that an abandoned file does not linger for good.
const expireAfter = 24 * time.Hour

// Thresholds are the trigger levels a watcher resolved at launch. A nil field
// means that window is disabled ("off"), which is distinct from 0 ("trigger at
// 0% used"). Weekly is not named for a window: it applies to whichever long
// window is binding (seven_day or thirty_day).
type Thresholds struct {
	FiveHour *float64 `json:"five_hour"`
	Weekly   *float64 `json:"weekly"`
}

// Heartbeat is one running watcher's published state. Providers is the
// watcher's scope, so a bulk watcher lists every provider it covers.
type Heartbeat struct {
	PID          int        `json:"pid"`
	Providers    []string   `json:"providers"`
	IntervalSecs int        `json:"interval_seconds"`
	Thresholds   Thresholds `json:"thresholds"`
	LastTick     time.Time  `json:"last_tick"`
}

// Covers reports whether this watcher watches providerID.
func (h Heartbeat) Covers(providerID string) bool {
	return slices.Contains(h.Providers, providerID)
}

// Stale reports whether a watcher that last ticked at lastTick on an
// intervalSecs cadence is presumed dead at now. A live watcher publishes at the
// start and end of every tick, so its gaps are one sleep or one tick. 1.2x the
// interval is a heuristic: a tick that runs longer than that (fetches timing
// out across several accounts) briefly reads as stale.
func Stale(lastTick time.Time, intervalSecs int, now time.Time) bool {
	return now.Sub(lastTick) > time.Duration(intervalSecs)*time.Second*6/5
}

// dir resolves $CACHE/aistat/watch without creating it.
func dir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("watch state: cannot resolve cache dir: %w", err)
	}
	return filepath.Join(base, "aistat", "watch"), nil
}

func path(d string, pid int) string {
	return filepath.Join(d, fmt.Sprintf("%d.json", pid))
}

// Publish atomically writes hb to this watcher's own file.
func Publish(hb Heartbeat) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0700); err != nil {
		return fmt.Errorf("watch state: cannot create %s: %w", d, err)
	}
	data, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d, ".watch-*.tmp")
	if err != nil {
		return fmt.Errorf("watch state: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	if closeErr := tmp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(tmpName, path(d, hb.PID))
	}
	if writeErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("watch state: write %s: %w", path(d, hb.PID), writeErr)
	}
	return nil
}

// Clear removes this watcher's file on clean shutdown. A missing file is not
// an error.
func Clear(pid int) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.Remove(path(d, pid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("watch state: %w", err)
	}
	return nil
}

// Supersede removes the heartbeats of other watchers with exactly hb's provider
// scope, so a restarted watcher replaces its predecessor instead of leaving it
// behind as STALE. A live duplicate watcher is not harmed: it republishes on
// its next tick.
func Supersede(hb Heartbeat, now time.Time) error {
	others, err := List(now)
	if err != nil {
		return err
	}
	d, err := dir()
	if err != nil {
		return err
	}
	for _, o := range others {
		if o.PID != hb.PID && slices.Equal(o.Providers, hb.Providers) {
			if err := os.Remove(path(d, o.PID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("watch state: %w", err)
			}
		}
	}
	return nil
}

// List returns every published heartbeat, newest tick first, deleting any whose
// last tick is more than expireAfter before now. A missing state directory
// means no watcher has ever run and yields nothing. Unreadable or malformed
// files are skipped: a corrupt heartbeat must never break `aistat usage`.
func List(now time.Time) ([]Heartbeat, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("watch state: read %s: %w", d, err)
	}
	var out []Heartbeat
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var hb Heartbeat
		if err := json.Unmarshal(data, &hb); err != nil {
			continue
		}
		if now.Sub(hb.LastTick) > expireAfter {
			os.Remove(filepath.Join(d, e.Name())) // best-effort: retried on the next read
			continue
		}
		out = append(out, hb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTick.After(out[j].LastTick) })
	return out, nil
}
