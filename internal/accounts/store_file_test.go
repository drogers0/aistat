//go:build linux || windows

package accounts

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/drogers0/aistat/v2/internal/testenv"
)

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
