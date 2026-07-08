//go:build !darwin && !linux && !windows

// Package usagecache provides a no-op usage cache stub on unsupported platforms.
package usagecache

import (
	"sync"
	"time"

	"github.com/drogers0/aistat/v2/internal/providers"
)

// Cache is a no-op stub on unsupported platforms. All operations return
// miss/no-op; the disabled warn fires at most once.
type Cache struct {
	provider string
	once     sync.Once
	warn     func(string)
}

// New always returns a disabled cache on non-unix platforms.
func New(provider string, nowFn func() time.Time, warnFn func(string)) *Cache {
	if warnFn == nil {
		warnFn = func(string) {}
	}
	return &Cache{provider: provider, warn: warnFn}
}

func (c *Cache) GetWithAge(uuid string) (map[string]providers.Limit, time.Duration, bool) {
	c.once.Do(func() {
		c.warn("aistat: " + c.provider + ": usage cache disabled (platform not supported)")
	})
	return nil, 0, false
}

func (c *Cache) Get(uuid string) (map[string]providers.Limit, bool) {
	m, _, ok := c.GetWithAge(uuid)
	return m, ok
}

func (c *Cache) Put(uuid string, limits map[string]providers.Limit) {
	c.once.Do(func() {
		c.warn("aistat: " + c.provider + ": usage cache disabled (platform not supported)")
	})
}
