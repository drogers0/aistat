package accounts

import (
	"context"
	"encoding/json"
	"io"
)

type Promotion struct {
	SourceKey                  string
	ObservedSourceRawBlob      json.RawMessage
	Destination                Account
	ExpectedDestinationPresent bool
	ExpectedDestinationRawBlob json.RawMessage
}

type PromotionResult uint8

const (
	PromotionResultUnspecified PromotionResult = iota
	PromotionCompleted
	PromotionSourceChanged
	PromotionDestinationChanged
)

// Store persists provider accounts across invocations. All methods are safe for
// concurrent use. The concrete implementation is platform-specific; obtain one
// via OpenStore.
type Store interface {
	// List returns all stored accounts in unspecified order.
	List(ctx context.Context) ([]Account, error)
	// Upsert inserts or replaces the account identified by a.Key().
	Upsert(ctx context.Context, a Account) error
	// Delete removes the account with the given key. No-op if absent.
	Delete(ctx context.Context, key string) error
	Promote(ctx context.Context, instruction Promotion) (PromotionResult, error)
}

// config holds options passed to OpenStore.
type config struct {
	debug io.Writer // nil disables debug output
}

// Option configures OpenStore behaviour.
type Option func(*config)

// WithDebug wires a writer that receives debug/diagnostic lines (e.g. orphan
// index warnings on darwin). Platform implementations that emit on this writer
// from concurrent goroutines wrap it internally for thread safety.
func WithDebug(w io.Writer) Option {
	return func(c *config) { c.debug = w }
}
