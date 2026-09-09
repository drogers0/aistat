package accounts

import (
	"bytes"
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

func makeTestAccount(uuid, email string) Account {
	raw := rawBlob("at-"+uuid, "rt-"+uuid, 0)
	a, err := NewAccount(raw, uuid, email, "", "", "", "", "", time.Now())
	if err != nil {
		panic(err)
	}
	return a
}

func promotionFixture() (Account, Account, Account) {
	source := makeTestAccount("550e8400-e29b-41d4-a716-446655440000", "legacy@example.com")
	source.RawBlob = rawBlob("access", "refresh-a", 0)
	source.LastSeenAt = time.Date(2026, 9, 8, 11, 0, 0, 111222333, time.UTC)
	destination := source
	destination.RawBlob = rawBlob("access", "refresh-b", 0)
	destination.Email = "canonical@example.com"
	destination.DisplayName = "Canonical"
	destination.RateLimitTier = "max"
	destination.OrganizationUUID = "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"
	destination.OrganizationName = "Acme"
	destination.OrganizationType = "claude_team"
	destination.LastSeenAt = time.Date(2026, 9, 8, 12, 0, 0, 123456789, time.UTC)
	sibling := makeTestAccount("11111111-1111-4111-8111-111111111111", "sibling@example.com")
	sibling.LastSeenAt = time.Date(2026, 9, 7, 12, 0, 0, 987654321, time.UTC)
	return source, destination, sibling
}

func accountMap(t *testing.T, store Store) map[string]Account {
	t.Helper()
	list, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make(map[string]Account, len(list))
	for _, account := range list {
		got[account.Key()] = account
	}
	return got
}

func assertAccounts(t *testing.T, store Store, want map[string]Account) {
	t.Helper()
	if got := accountMap(t, store); !reflect.DeepEqual(got, want) {
		t.Errorf("stored accounts = %#v, want %#v", got, want)
	}
}

func putAccounts(t *testing.T, store Store, accounts ...Account) {
	t.Helper()
	for _, account := range accounts {
		if err := store.Upsert(context.Background(), account); err != nil {
			t.Fatalf("Upsert(%s): %v", account.Key(), err)
		}
	}
}

func TestMemoryStorePromotionContract(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"absent destination installs desired metadata and preserves sibling", func(t *testing.T) {
			store := NewMemoryStore()
			source, destination, sibling := promotionFixture()
			putAccounts(t, store, source, sibling)
			result, err := store.Promote(context.Background(), Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination})
			if result != PromotionCompleted || err != nil {
				t.Fatalf("Promote = %v, %v", result, err)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
		}},
		{"source and destination preconditions preserve every record", func(t *testing.T) {
			for _, scenario := range []struct {
				name   string
				setup  func(*testing.T, Store, Account, Account, Account)
				input  func(Account, Account) Promotion
				result PromotionResult
			}{
				{"source changed", func(t *testing.T, store Store, source, _ Account, sibling Account) {
					putAccounts(t, store, source, sibling)
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: rawBlob("access", "other", 0), Destination: destination}
				}, PromotionSourceChanged},
				{"destination absent when expected", func(t *testing.T, store Store, source, _ Account, sibling Account) {
					putAccounts(t, store, source, sibling)
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}
				}, PromotionDestinationChanged},
				{"destination differs", func(t *testing.T, store Store, source, destination, sibling Account) {
					changed := destination
					changed.RawBlob = rawBlob("access", "refresh-c", 0)
					putAccounts(t, store, source, changed, sibling)
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}
				}, PromotionDestinationChanged},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					store := NewMemoryStore()
					source, destination, sibling := promotionFixture()
					scenario.setup(t, store, source, destination, sibling)
					before := accountMap(t, store)
					result, err := store.Promote(context.Background(), scenario.input(source, destination))
					if result != scenario.result || err != nil {
						t.Fatalf("Promote = %v, %v, want %v, nil", result, err, scenario.result)
					}
					assertAccounts(t, store, before)
				})
			}
		}},
		{"existing matching destination refreshes all fields byte for byte", func(t *testing.T) {
			store := NewMemoryStore()
			source, destination, sibling := promotionFixture()
			stale := destination
			stale.Email = "stale@example.com"
			stale.DisplayName = "stale"
			stale.LastSeenAt = time.Time{}
			putAccounts(t, store, source, stale, sibling)
			result, err := store.Promote(context.Background(), Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: stale.RawBlob})
			if result != PromotionCompleted || err != nil {
				t.Fatalf("Promote = %v, %v", result, err)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestMemoryStore(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"list empty", func(t *testing.T) {
			s := NewMemoryStore()
			list, err := s.List(context.Background())
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 0 {
				t.Errorf("want empty list, got %v", list)
			}
		}},
		{"upsert and list", func(t *testing.T) {
			s := NewMemoryStore()
			ctx := context.Background()

			a := makeTestAccount("uuid-1", "user1@example.com")
			if err := s.Upsert(ctx, a); err != nil {
				t.Fatalf("Upsert: %v", err)
			}

			list, err := s.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 1 {
				t.Fatalf("want 1 account, got %d", len(list))
			}
			if list[0].UUID != "uuid-1" {
				t.Errorf("UUID: got %q", list[0].UUID)
			}
		}},
		{"upsert updates", func(t *testing.T) {
			s := NewMemoryStore()
			ctx := context.Background()

			a := makeTestAccount("uuid-1", "user1@example.com")
			s.Upsert(ctx, a)

			updated := a
			updated.Email = "updated@example.com"
			s.Upsert(ctx, updated)

			list, _ := s.List(ctx)
			if len(list) != 1 {
				t.Fatalf("want 1 account after update, got %d", len(list))
			}
			if list[0].Email != "updated@example.com" {
				t.Errorf("Email after update: got %q", list[0].Email)
			}
		}},
		{"delete", func(t *testing.T) {
			s := NewMemoryStore()
			ctx := context.Background()

			s.Upsert(ctx, makeTestAccount("uuid-1", "user1@example.com"))
			s.Upsert(ctx, makeTestAccount("uuid-2", "user2@example.com"))

			if err := s.Delete(ctx, "uuid-1"); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			list, _ := s.List(ctx)
			if len(list) != 1 {
				t.Fatalf("want 1 account after delete, got %d", len(list))
			}
			if list[0].UUID != "uuid-2" {
				t.Errorf("remaining UUID: got %q, want uuid-2", list[0].UUID)
			}
		}},
		{"delete non-existent", func(t *testing.T) {
			s := NewMemoryStore()
			// Delete on an absent UUID must not error.
			if err := s.Delete(context.Background(), "no-such-uuid"); err != nil {
				t.Errorf("Delete non-existent: want nil, got %v", err)
			}
		}},
		{"same account UUID contexts coexist under opaque keys", func(t *testing.T) {
			s := NewMemoryStore()
			ctx := context.Background()
			legacy := makeTestAccount("550e8400-e29b-41d4-a716-446655440000", "user@example.com")
			team := legacy
			team.OrganizationUUID = "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"
			team.OrganizationName = "Acme"
			team.OrganizationType = "claude_team"
			if err := s.Upsert(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			if err := s.Upsert(ctx, team); err != nil {
				t.Fatal(err)
			}
			list, err := s.List(ctx)
			if err != nil || len(list) != 2 {
				t.Fatalf("List = %#v, %v; want two contexts", list, err)
			}
			if err := s.Delete(ctx, team.Key()); err != nil {
				t.Fatal(err)
			}
			list, _ = s.List(ctx)
			if len(list) != 1 || list[0].Key() != "550e8400-e29b-41d4-a716-446655440000" {
				t.Errorf("remaining contexts = %#v", list)
			}
		}},
		{"promotion compares both snapshots", func(t *testing.T) {
			s := NewMemoryStore()
			ctx := context.Background()
			source := makeTestAccount("550e8400-e29b-41d4-a716-446655440000", "user@example.com")
			source.RawBlob = rawBlob("access", "refresh-a", 0)
			destination := source
			destination.RawBlob = rawBlob("access", "refresh-b", 0)
			destination.OrganizationUUID = "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"
			destination.OrganizationName = "Acme"
			destination.OrganizationType = "claude_team"
			sibling := makeTestAccount("11111111-1111-4111-8111-111111111111", "sibling@example.com")
			for _, account := range []Account{source, sibling} {
				if err := s.Upsert(ctx, account); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.Promote(ctx, Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination})
			if err != nil || result != PromotionCompleted {
				t.Fatalf("Promote = %v, %v", result, err)
			}
			list, _ := s.List(ctx)
			if len(list) != 2 {
				t.Fatalf("List length = %d, want 2", len(list))
			}
			if result, err = s.Promote(ctx, Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}); err != nil || result != PromotionSourceChanged {
				t.Errorf("missing source Promote = %v, %v", result, err)
			}
			changedDestination := destination
			changedDestination.RawBlob = rawBlob("access", "refresh-c", 0)
			if err := s.Upsert(ctx, changedDestination); err != nil {
				t.Fatal(err)
			}
			if err := s.Upsert(ctx, source); err != nil {
				t.Fatal(err)
			}
			if result, err = s.Promote(ctx, Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}); err != nil || result != PromotionDestinationChanged {
				t.Errorf("changed destination Promote = %v, %v", result, err)
			}
		}},
		{"concurrent upserts", func(t *testing.T) {
			s := NewMemoryStore()
			ctx := context.Background()

			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				uuid := "uuid-" + string(rune('a'+i))
				email := uuid + "@example.com"
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.Upsert(ctx, makeTestAccount(uuid, email))
				}()
			}
			wg.Wait()

			list, err := s.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 20 {
				t.Errorf("want 20 accounts, got %d", len(list))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestMemoryStorePromote_RetriesAfterSourceDeleteBoundary(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	source := makeTestAccount("550e8400-e29b-41d4-a716-446655440000", "person@example.com")
	source.RawBlob = rawBlob("access", "refresh-a", 0)
	destination := source
	destination.OrganizationUUID = "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"
	destination.OrganizationName = "Acme"
	destination.OrganizationType = "claude_team"
	destination.RawBlob = rawBlob("access", "refresh-b", 0)

	// This is the durable state after a destination write succeeded but source
	// deletion was interrupted: U and refreshed K both remain. A retry must use
	// K's B snapshot, preserve its desired metadata/blob, and retire U.
	for _, account := range []Account{source, destination} {
		if err := store.Upsert(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.Promote(ctx, Promotion{
		SourceKey:                  source.Key(),
		ObservedSourceRawBlob:      source.RawBlob,
		Destination:                destination,
		ExpectedDestinationPresent: true,
		ExpectedDestinationRawBlob: destination.RawBlob,
	})
	if err != nil || result != PromotionCompleted {
		t.Fatalf("Promote retry = %v, %v; want completed, nil", result, err)
	}
	stored, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Key() != "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042" || !bytes.Equal(stored[0].RawBlob, destination.RawBlob) {
		t.Fatalf("post-retry store = %#v, want only destination B", stored)
	}
}
