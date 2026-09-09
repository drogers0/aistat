//go:build !darwin && !linux && !windows

package accounts

import (
	"context"
	"errors"
	"testing"
)

var _ Store = unsupportedStore{}

func TestUnsupportedStorePromote(t *testing.T) {
	result, err := unsupportedStore{}.Promote(context.Background(), Promotion{})
	if result != PromotionSourceChanged || !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Promote = %v, %v", result, err)
	}
}
