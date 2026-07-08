//go:build !darwin && !linux && !windows

package accounts

import (
	"context"
	"errors"
)

// ErrUnsupportedPlatform is returned by Store operations on platforms that
// have no account store implementation.
var ErrUnsupportedPlatform = errors.New("accounts: platform not supported")

type unsupportedStore struct{}

// OpenStore returns the unsupported-platform store stub.
func OpenStore(provider Provider, opts ...Option) (Store, error) {
	if err := provider.validate(); err != nil {
		return nil, err
	}
	return unsupportedStore{}, nil
}

func (unsupportedStore) List(_ context.Context) ([]Account, error) { return nil, nil }

func (unsupportedStore) Upsert(_ context.Context, _ Account) error {
	return ErrUnsupportedPlatform
}

func (unsupportedStore) Delete(_ context.Context, _ string) error {
	return ErrUnsupportedPlatform
}
