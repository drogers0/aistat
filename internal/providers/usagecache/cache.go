//go:build darwin || linux || windows

// Package usagecache is a provider-neutral, per-UUID usage limit cache backed
// by a single JSON file in $CACHE/aistat/usage/. It is used by provider
// packages (e.g. internal/providers/claude) to reduce API round-trips.
//
// Callers supply a provider name; the package computes <provider>-v1.json and
// <provider>.cache.lock under $CACHE/aistat/usage/ so each provider's files
// are isolated without the caller needing to know the naming convention.
package usagecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
)

const ttlDefault = 90 * time.Second

// Cache is a file-backed, per-UUID usage limit cache. All exported methods are
// safe for concurrent use. The zero value is not usable; construct via New.
type Cache struct {
	provider    string
	path        string
	lockPath    string
	ttl         time.Duration
	now         func() time.Time
	warn        func(string)
	once        sync.Once // guards all warn calls — fires at most once per Cache
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

// New constructs a Cache for the given provider. The data file is stored at
// $CACHE/aistat/usage/<provider>-v1.json and locked via
// $CACHE/aistat/usage/<provider>.cache.lock. The parent directory is created
// with mode 0700 if absent.
//
// provider must consist only of lowercase ASCII letters, digits, underscore,
// or dash. On invalid input, returns a disabled cache that warns once on first
// use.
//
// nowFn defaults to time.Now if nil. warnFn defaults to a silent no-op if nil.
// AISTAT_USAGE_CACHE_TTL overrides the default 90s TTL if parseable as a duration.
//
// On any setup failure returns a no-op cache (disabled=true); the warn fires
// at most once, on the first Get or Put call.
func New(provider string, nowFn func() time.Time, warnFn func(string)) *Cache {
	if nowFn == nil {
		nowFn = time.Now
	}
	if warnFn == nil {
		warnFn = func(string) {}
	}

	ttl := ttlDefault
	if s := os.Getenv("AISTAT_USAGE_CACHE_TTL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			ttl = d
		}
	}

	if !isValidProvider(provider) {
		return disabledCache("", nowFn, warnFn, ttl, fmt.Sprintf("invalid provider %q", provider))
	}

	cacheBase, err := os.UserCacheDir()
	if err != nil {
		return disabledCache(provider, nowFn, warnFn, ttl, fmt.Sprintf("cannot resolve cache dir: %v", err))
	}
	dir := filepath.Join(cacheBase, "aistat", "usage")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return disabledCache(provider, nowFn, warnFn, ttl, fmt.Sprintf("cannot create cache dir %s: %v", dir, err))
	}
	return &Cache{
		provider: provider,
		path:     filepath.Join(dir, provider+"-v1.json"),
		lockPath: filepath.Join(dir, provider+".cache.lock"),
		ttl:      ttl,
		now:      nowFn,
		warn:     warnFn,
	}
}

// isValidProvider reports whether s is a valid provider string: non-empty,
// consisting only of lowercase ASCII letters, digits, underscore, or dash.
func isValidProvider(s string) bool {
	if s == "" {
		return false
	}
	for _, b := range []byte(s) {
		if !((b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-') {
			return false
		}
	}
	return true
}

func disabledCache(provider string, nowFn func() time.Time, warnFn func(string), ttl time.Duration, reason string) *Cache {
	var msg string
	if provider != "" {
		msg = "aistat: " + provider + ": usage cache disabled (" + reason + ")"
	} else {
		msg = "aistat: usage cache disabled (" + reason + ")"
	}
	return &Cache{
		provider:    provider,
		disabled:    true,
		disabledMsg: msg,
		ttl:         ttl,
		now:         nowFn,
		warn:        warnFn,
	}
}

// atomicWrite marshals cf to JSON and atomically replaces c.path via a
// temp-file-in-same-dir + rename, preserving mode 0600.
func (c *Cache) atomicWrite(cf cacheFile) error {
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

// GetWithAge returns the limits, the age of the cache entry (now - FetchedAt),
// and hit status. Returns (nil, 0, false) on miss, expired entry, missing
// file, parse error, disabled state, or any other read failure. Parse errors
// fire the warn line once; subsequent reads stay quiet. The cache stores
// absolute ResetsAt in each Limit; ResetAfterSeconds is NOT recomputed here —
// that is the caller's responsibility.
func (c *Cache) GetWithAge(uuid string) (map[string]providers.Limit, time.Duration, bool) {
	if c.disabled {
		c.once.Do(func() { c.warn(c.disabledMsg) })
		return nil, 0, false
	}

	var result map[string]providers.Limit
	var age time.Duration
	var found bool

	err := c.withLock(false, func() error {
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
				c.warn(fmt.Sprintf("aistat: %s: usage cache: corrupt file, ignoring: %v", c.provider, err))
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
			c.warn(fmt.Sprintf("aistat: %s: usage cache: read error: %v", c.provider, err))
		})
		return nil, 0, false
	}
	return result, age, found
}

// Get returns (limits, true) if a non-expired entry exists for uuid.
// Returns (nil, false) on miss, expired, or any error. See GetWithAge for
// the full contract.
func (c *Cache) Get(uuid string) (map[string]providers.Limit, bool) {
	m, _, ok := c.GetWithAge(uuid)
	return m, ok
}

// Put writes limits under uuid, replacing any existing entry. Best effort:
// errors are swallowed (warn fires at most once on the first write failure).
// Writes via tmp + rename under LOCK_EX on the sentinel lock file.
func (c *Cache) Put(uuid string, limits map[string]providers.Limit) {
	if c.disabled {
		c.once.Do(func() { c.warn(c.disabledMsg) })
		return
	}

	err := c.withLock(true, func() error {
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
			c.warn(fmt.Sprintf("aistat: %s: usage cache: write failed: %v", c.provider, err))
		})
	}
}
