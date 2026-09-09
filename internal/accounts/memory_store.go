package accounts

import (
	"bytes"
	"context"
	"sync"
)

// MemoryStore is a thread-safe in-memory implementation of Store for use in
// tests of other packages. It is exported so importing packages can construct
// it directly without going through OpenStore (which is platform-specific).
type MemoryStore struct {
	mu       sync.Mutex
	accounts map[string]Account
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{accounts: make(map[string]Account)}
}

func (m *MemoryStore) List(_ context.Context) ([]Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]Account, 0, len(m.accounts))
	for _, a := range m.accounts {
		list = append(list, a)
	}
	return list, nil
}

func (m *MemoryStore) Upsert(_ context.Context, a Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[a.Key()] = a
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.accounts, key)
	return nil
}

func (m *MemoryStore) Promote(_ context.Context, instruction Promotion) (PromotionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	source, ok := m.accounts[instruction.SourceKey]
	if !ok || !bytes.Equal(source.RawBlob, instruction.ObservedSourceRawBlob) {
		return PromotionSourceChanged, nil
	}
	destination, present := m.accounts[instruction.Destination.Key()]
	if present != instruction.ExpectedDestinationPresent ||
		(present && !bytes.Equal(destination.RawBlob, instruction.ExpectedDestinationRawBlob)) {
		return PromotionDestinationChanged, nil
	}
	m.accounts[instruction.Destination.Key()] = instruction.Destination
	delete(m.accounts, instruction.SourceKey)
	return PromotionCompleted, nil
}
