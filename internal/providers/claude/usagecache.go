//go:build darwin || linux

package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
)

const usageCacheTTLDefault = 90 * time.Second

// usageCacheFilename is versioned by suffix; bumping the format means writing
// a new filename and ignoring older ones. No in-file schema field, no
// version-mismatch branches.
const usageCacheFilename = "claude-v1.json"

type usageCache struct {
	path        string
	lockPath    string
	ttl         time.Duration
	now         func() time.Time // wired from Client.now in newUsageCache
	warn        func(string)
	once        sync.Once // guards all warn calls — fires at most once per usageCache instance
	disabled    bool
	disabledMsg string // pre-composed warn message; empty when not disabled
}

type cacheFile struct {
	Entries map[string]cacheEntry `json:"entries"`
}

type cacheEntry struct {
	FetchedAt time.Time                  `json:"fetched_at"`
	Limits    map[string]providers.Limit `json:"limits"`
}

// newUsageCache resolves $CACHE/aistat/usage/claude-v1.json, creates the
// parent directory (mode 0700) if absent. nowFn defaults to time.Now if nil;
// warnFn defaults to a silent no-op if nil. On any setup failure returns a
// no-op cache (disabled=true); the warn fires at most once, on the first
// Get or Put call.
func newUsageCache(nowFn func() time.Time, warnFn func(string)) *usageCache {
	if nowFn == nil {
		nowFn = time.Now
	}
	if warnFn == nil {
		warnFn = func(string) {}
	}

	ttl := usageCacheTTLDefault
	if s := os.Getenv("AISTAT_USAGE_CACHE_TTL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			ttl = d
		}
	}

	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return disabledCache(nowFn, warnFn, ttl, fmt.Sprintf("cannot resolve cache dir: %v", err))
	}
	dir := filepath.Join(cacheBase, "aistat", "usage")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return disabledCache(nowFn, warnFn, ttl, fmt.Sprintf("cannot create cache dir %s: %v", dir, err))
	}
	return &usageCache{
		path:     filepath.Join(dir, usageCacheFilename),
		lockPath: filepath.Join(dir, "claude.cache.lock"),
		ttl:      ttl,
		now:      nowFn,
		warn:     warnFn,
	}
}

func disabledCache(nowFn func() time.Time, warnFn func(string), ttl time.Duration, reason string) *usageCache {
	return &usageCache{
		disabled:    true,
		disabledMsg: "aistat: claude: usage cache disabled (" + reason + ")",
		ttl:         ttl,
		now:         nowFn,
		warn:        warnFn,
	}
}

// withLock opens the sentinel lock file (creating it mode 0600 if absent),
// acquires the requested flock mode, calls fn, then releases the lock. The
// lock sits on a separate sentinel file rather than the data file because
// atomicWrite replaces the data file via rename — a flock on the data file's
// open fd would travel with the orphaned inode after rename, letting a second
// writer race ahead with stale state. The sentinel never gets renamed so the
// lock anchors a stable serialization point.
func (c *usageCache) withLock(mode int, fn func() error) error {
	f, err := os.OpenFile(c.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("usage cache: open lock file: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		return fmt.Errorf("usage cache: acquire lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// atomicWrite marshals cf to JSON and atomically replaces c.path via a
// temp-file-in-same-dir + rename, preserving mode 0600.
func (c *usageCache) atomicWrite(cf cacheFile) error {
	data, err := json.Marshal(cf)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return fmt.Errorf("usage cache: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	var writeErr error
	if _, writeErr = tmp.Write(data); writeErr == nil {
		writeErr = tmp.Sync()
	}
	tmp.Close()
	if writeErr != nil {
		os.Remove(tmpName)
		return fmt.Errorf("usage cache: write temp file: %w", writeErr)
	}
	// os.CreateTemp creates with mode 0600 — no chmod needed.
	if err := os.Rename(tmpName, c.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("usage cache: rename temp file: %w", err)
	}
	return nil
}

// getWithAge returns the limits, the age of the cache entry (now - FetchedAt),
// and hit status. Returns (nil, 0, false) on miss, expired entry, missing
// file, parse error, disabled state, or any other read failure. Parse errors
// fire the warn line once; subsequent reads stay quiet. The cache stores
// absolute ResetsAt in each Limit; ResetAfterSeconds is NOT recomputed here —
// that is the caller's responsibility.
func (c *usageCache) getWithAge(uuid string) (map[string]providers.Limit, time.Duration, bool) {
	if c.disabled {
		c.once.Do(func() { c.warn(c.disabledMsg) })
		return nil, 0, false
	}

	var result map[string]providers.Limit
	var age time.Duration
	var found bool

	err := c.withLock(syscall.LOCK_SH, func() error {
		data, err := os.ReadFile(c.path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // miss
			}
			return err
		}
		var cf cacheFile
		if err := json.Unmarshal(data, &cf); err != nil {
			c.once.Do(func() {
				c.warn(fmt.Sprintf("aistat: claude: usage cache: corrupt file, ignoring: %v", err))
			})
			return nil // treat as miss
		}
		entry, ok := cf.Entries[uuid]
		if !ok {
			return nil // miss
		}
		now := c.now()
		entryAge := now.Sub(entry.FetchedAt)
		if entryAge > c.ttl {
			return nil // expired
		}
		result = entry.Limits
		age = entryAge
		found = true
		return nil
	})

	if err != nil {
		c.once.Do(func() {
			c.warn(fmt.Sprintf("aistat: claude: usage cache: read error: %v", err))
		})
		return nil, 0, false
	}
	return result, age, found
}

// Get returns (limits, true) if a non-expired entry exists for uuid.
// Returns (nil, false) on miss, expired, or any error. See getWithAge for
// the full contract.
func (c *usageCache) Get(uuid string) (map[string]providers.Limit, bool) {
	m, _, ok := c.getWithAge(uuid)
	return m, ok
}

// Put writes limits under uuid, replacing any existing entry. Best effort:
// errors are swallowed (warn fires at most once on the first write failure).
// Writes via tmp + rename under LOCK_EX on the sentinel lock file.
func (c *usageCache) Put(uuid string, limits map[string]providers.Limit) {
	if c.disabled {
		c.once.Do(func() { c.warn(c.disabledMsg) })
		return
	}

	err := c.withLock(syscall.LOCK_EX, func() error {
		var cf cacheFile
		data, err := os.ReadFile(c.path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if len(data) > 0 {
			// Ignore parse errors on read-for-update. A corrupt file is treated
			// as empty: cf stays zero-valued and we overwrite it with a fresh
			// single-entry file below. Other UUIDs' entries are lost, but the
			// cache is fail-open — they will be repopulated on their next fetch.
			json.Unmarshal(data, &cf) //nolint:errcheck
		}
		if cf.Entries == nil {
			cf.Entries = make(map[string]cacheEntry)
		}
		cf.Entries[uuid] = cacheEntry{
			FetchedAt: c.now(),
			Limits:    limits,
		}
		return c.atomicWrite(cf)
	})

	if err != nil {
		c.once.Do(func() {
			c.warn(fmt.Sprintf("aistat: claude: usage cache: write failed: %v", err))
		})
	}
}
