//go:build linux || windows

package accounts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/testenv"
)

func TestFileStorePromotionContract(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"CAS outcomes preserve siblings and distinguish absent destination", func(t *testing.T) {
			for _, scenario := range []struct {
				name   string
				setup  func(t *testing.T, store Store, source, destination, sibling Account)
				input  func(source, destination Account) Promotion
				result PromotionResult
				want   func(source, destination, sibling Account) map[string]Account
			}{
				{"completed absent destination", func(t *testing.T, store Store, source, _ Account, sibling Account) {
					putAccounts(t, store, source, sibling)
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination}
				}, PromotionCompleted, func(_ Account, destination, sibling Account) map[string]Account {
					return map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling}
				}},
				{"source changed", func(t *testing.T, store Store, source, _ Account, sibling Account) {
					putAccounts(t, store, source, sibling)
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: rawBlob("access", "other", 0), Destination: destination}
				}, PromotionSourceChanged, nil},
				{"destination absent when expected", func(t *testing.T, store Store, source, _ Account, sibling Account) {
					putAccounts(t, store, source, sibling)
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}
				}, PromotionDestinationChanged, nil},
				{"destination differs", func(t *testing.T, store Store, source, destination, sibling Account) {
					changed := destination
					changed.RawBlob = rawBlob("access", "refresh-c", 0)
					putAccounts(t, store, source, changed, sibling)
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}
				}, PromotionDestinationChanged, nil},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					testenv.RedirectHome(t, t.TempDir())
					store, err := OpenStore(ProviderClaude)
					if err != nil {
						t.Fatal(err)
					}
					source, destination, sibling := promotionFixture()
					scenario.setup(t, store, source, destination, sibling)
					before := accountMap(t, store)
					result, err := store.Promote(context.Background(), scenario.input(source, destination))
					if result != scenario.result || err != nil {
						t.Fatalf("Promote = %v, %v, want %v, nil", result, err, scenario.result)
					}
					if scenario.want != nil {
						assertAccounts(t, store, scenario.want(source, destination, sibling))
					} else {
						assertAccounts(t, store, before)
					}
				})
			}
		}},
		{"existing destination receives desired fields byte for byte", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir())
			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			source, destination, sibling := promotionFixture()
			stale := destination
			stale.Email = "stale@example.com"
			stale.LastSeenAt = stale.LastSeenAt.Add(-time.Hour)
			putAccounts(t, store, source, stale, sibling)
			result, err := store.Promote(context.Background(), Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: stale.RawBlob})
			if result != PromotionCompleted || err != nil {
				t.Fatalf("Promote = %v, %v", result, err)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
		}},
		{"atomic map write failure leaves U and K until retry", func(t *testing.T) {
			testenv.RedirectHome(t, t.TempDir())
			opened, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			store := opened.(*fileStore)
			source, destination, sibling := promotionFixture()
			putAccounts(t, store, source, destination, sibling)
			store.writeAccount = func(map[string]Account) error { return fmt.Errorf("injected atomic map write failure") }
			instruction := Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}
			result, err := store.Promote(context.Background(), instruction)
			if result != PromotionResultUnspecified || err == nil {
				t.Fatalf("failed Promote = %v, %v", result, err)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000": source, "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
			store.writeAccount = nil
			result, err = store.Promote(context.Background(), instruction)
			if result != PromotionCompleted || err != nil {
				t.Fatalf("retry Promote = %v, %v", result, err)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestFileStore(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"round trip", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ctx := context.Background()

			a1 := makeTestAccount("uuid-1", "user1@example.com")
			if err := store.Upsert(ctx, a1); err != nil {
				t.Fatalf("Upsert: %v", err)
			}

			list, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 1 || list[0].UUID != "uuid-1" {
				t.Errorf("want [uuid-1], got %v", list)
			}

			if err := store.Delete(ctx, "uuid-1"); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			list, err = store.List(ctx)
			if err != nil {
				t.Fatalf("List after delete: %v", err)
			}
			if len(list) != 0 {
				t.Errorf("want empty list after delete, got %v", list)
			}
		}},
		{"same UUID contexts coexist", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)
			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			legacy := makeTestAccount("550e8400-e29b-41d4-a716-446655440000", "user@example.com")
			team := legacy
			team.OrganizationUUID = "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"
			team.OrganizationName = "Acme"
			team.OrganizationType = "claude_team"
			for _, account := range []Account{legacy, team} {
				if err := store.Upsert(ctx, account); err != nil {
					t.Fatal(err)
				}
			}
			list, err := store.List(ctx)
			if err != nil || len(list) != 2 {
				t.Fatalf("List = %#v, %v; want two contexts", list, err)
			}
			if err := store.Delete(ctx, team.Key()); err != nil {
				t.Fatal(err)
			}
			list, _ = store.List(ctx)
			if len(list) != 1 || list[0].Key() != "550e8400-e29b-41d4-a716-446655440000" {
				t.Errorf("remaining contexts = %#v", list)
			}
		}},
		{"promotion writes one key transition", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)
			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			source := makeTestAccount("550e8400-e29b-41d4-a716-446655440000", "user@example.com")
			source.RawBlob = rawBlob("access", "refresh-a", 0)
			destination := source
			destination.RawBlob = rawBlob("access", "refresh-b", 0)
			destination.OrganizationUUID = "7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"
			if err := store.Upsert(ctx, source); err != nil {
				t.Fatal(err)
			}
			result, err := store.Promote(ctx, Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination})
			if err != nil || result != PromotionCompleted {
				t.Fatalf("Promote = %v, %v", result, err)
			}
			list, _ := store.List(ctx)
			if len(list) != 1 || list[0].Key() != "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042" || string(list[0].RawBlob) != string(destination.RawBlob) {
				t.Errorf("promoted accounts = %#v", list)
			}
		}},
		{"file mode", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX file modes are advisory on Windows (NTFS ACLs)")
			}
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			store.Upsert(context.Background(), makeTestAccount("uuid-1", "user1@example.com"))

			path := filepath.Join(home, ".config", "aistat", "accounts", "claude.json")
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if fi.Mode().Perm() != 0600 {
				t.Errorf("file mode: got %04o, want 0600", fi.Mode().Perm())
			}
		}},
		{"parent dir mode", func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("POSIX dir modes are advisory on Windows (NTFS ACLs)")
			}
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			if _, err := OpenStore(ProviderClaude); err != nil {
				t.Fatalf("OpenStore: %v", err)
			}

			dir := filepath.Join(home, ".config", "aistat", "accounts")
			fi, err := os.Stat(dir)
			if err != nil {
				t.Fatalf("stat dir: %v", err)
			}
			if fi.Mode().Perm() != 0700 {
				t.Errorf("dir mode: got %04o, want 0700", fi.Mode().Perm())
			}
		}},
		{"concurrent upserts", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ctx := context.Background()

			a1 := makeTestAccount("uuid-1", "user1@example.com")
			a2 := makeTestAccount("uuid-2", "user2@example.com")

			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); store.Upsert(ctx, a1) }()
			go func() { defer wg.Done(); store.Upsert(ctx, a2) }()
			wg.Wait()

			list, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 2 {
				t.Errorf("want 2 accounts after concurrent upserts, got %d: %v", len(list), list)
			}
		}},
		{"empty after final delete removes file", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ctx := context.Background()

			store.Upsert(ctx, makeTestAccount("uuid-1", "user1@example.com"))
			store.Delete(ctx, "uuid-1")

			path := filepath.Join(home, ".config", "aistat", "accounts", "claude.json")
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("expected file to be removed after deleting last account; stat err: %v", err)
			}
		}},
		// Missing file → empty list, no error.
		// Exercises the path where no file has ever been written.
		{"list missing file", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}

			list, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List on missing file: %v", err)
			}
			if len(list) != 0 {
				t.Errorf("want empty list on missing file, got %v", list)
			}
		}},
		// Pins the exact on-disk location of the Claude account store before
		// migration. Ensures parameterization refactor preserves the existing path.
		{"claude file path", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ls := store.(*fileStore)

			wantPath := filepath.Join(home, ".config", "aistat", "accounts", "claude.json")
			if ls.path != wantPath {
				t.Errorf("store path: got %q, want %q", ls.path, wantPath)
			}
		}},
		{"claude lock path", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ls := store.(*fileStore)

			wantLock := filepath.Join(home, ".config", "aistat", "accounts", ".claude.lock")
			if ls.lockPath != wantLock {
				t.Errorf("lock path: got %q, want %q", ls.lockPath, wantLock)
			}
		}},
		{"corrupt json error", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			dir := filepath.Join(home, ".config", "aistat", "accounts")
			os.MkdirAll(dir, 0700)
			path := filepath.Join(dir, "claude.json")
			os.WriteFile(path, []byte("{not valid json"), 0600)

			store, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}

			_, err = store.List(context.Background())
			if err == nil {
				t.Fatal("expected error on corrupt JSON, got nil")
			}
		}},
		// Opens both ProviderClaude and ProviderCodex stores under the same temp
		// home, upserts one account into each, and asserts that Codex uses its
		// own files and the two stores do not share data.
		{"codex path isolation", func(t *testing.T) {
			home := t.TempDir()
			testenv.RedirectHome(t, home)

			claudeStore, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore(claude): %v", err)
			}
			codexStore, err := OpenStore(ProviderCodex)
			if err != nil {
				t.Fatalf("OpenStore(codex): %v", err)
			}

			ctx := context.Background()
			claudeAcct := makeTestAccount("uuid-claude", "claude@example.com")
			codexAcct := makeTestAccount("uuid-codex", "codex@example.com")

			if err := claudeStore.Upsert(ctx, claudeAcct); err != nil {
				t.Fatalf("claude Upsert: %v", err)
			}
			if err := codexStore.Upsert(ctx, codexAcct); err != nil {
				t.Fatalf("codex Upsert: %v", err)
			}

			dir := filepath.Join(home, ".config", "aistat", "accounts")

			// Assert Codex data and lock paths.
			cs := codexStore.(*fileStore)
			wantCodexPath := filepath.Join(dir, "codex.json")
			wantCodexLock := filepath.Join(dir, ".codex.lock")
			if cs.path != wantCodexPath {
				t.Errorf("codex store path: got %q, want %q", cs.path, wantCodexPath)
			}
			if cs.lockPath != wantCodexLock {
				t.Errorf("codex lock path: got %q, want %q", cs.lockPath, wantCodexLock)
			}

			// Assert data file exists for Codex.
			if _, err := os.Stat(wantCodexPath); err != nil {
				t.Errorf("codex data file not found: %v", err)
			}

			// Claude store sees only the Claude account.
			claudeList, err := claudeStore.List(ctx)
			if err != nil {
				t.Fatalf("claude List: %v", err)
			}
			if len(claudeList) != 1 || claudeList[0].UUID != "uuid-claude" {
				t.Errorf("claude List: want [uuid-claude], got %v", claudeList)
			}

			// Codex store sees only the Codex account.
			codexList, err := codexStore.List(ctx)
			if err != nil {
				t.Fatalf("codex List: %v", err)
			}
			if len(codexList) != 1 || codexList[0].UUID != "uuid-codex" {
				t.Errorf("codex List: want [uuid-codex], got %v", codexList)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
