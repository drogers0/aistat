//go:build darwin

package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// safeWriter wraps an io.Writer with a mutex. Mirrors httpx.ConcurrencySafeWriter.
// Needed because the orphan-warn path in List may be called from goroutines
// exercising the store concurrently (e.g. parallel test cases), allowing
// callers to pass a plain *bytes.Buffer to WithDebug without a data race.
type safeWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *safeWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

const (
	darwinIndexAccount = "index"
)

// darwinPerAccountService returns the per-account keychain service name for
// the given provider and opaque key.
func darwinPerAccountService(p Provider, key string) string {
	return "aistat:accounts:" + string(p) + ":" + key
}

// darwinAccountIndexService returns the keychain service name for the index
// item for the given provider.
func darwinAccountIndexService(p Provider) string {
	return "aistat:accounts:" + string(p) + ":index"
}

type darwinStore struct {
	provider Provider
	lockPath string
	debug    *safeWriter // nil when debug is disabled
}

type darwinSecurityResult struct {
	Output []byte
	Stderr []byte
	Err    error
}

var runDarwinSecurity = runDarwinSecurityExec

func runDarwinSecurityExec(ctx context.Context, combined bool, args ...string) darwinSecurityResult {
	cmd := exec.CommandContext(ctx, "security", args...)
	if combined {
		output, err := cmd.CombinedOutput()
		return darwinSecurityResult{Output: output, Err: err}
	}
	output, err := cmd.Output()
	result := darwinSecurityResult{Output: output, Err: err}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.Stderr = exitErr.Stderr
	}
	return result
}

// OpenStore returns the macOS Keychain-backed account store for the given provider.
// The lock file sentinel is created (lazily, mode 0600) at os.UserCacheDir()/aistat/store.lock;
// its parent directory is created with mode 0700 if absent.
func OpenStore(provider Provider, opts ...Option) (Store, error) {
	if err := provider.validate(); err != nil {
		return nil, err
	}
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("accounts: cannot resolve user cache dir: %w", err)
	}
	lockDir := filepath.Join(cacheDir, "aistat")
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		return nil, fmt.Errorf("accounts: cannot create lock dir %s: %w", lockDir, err)
	}
	s := &darwinStore{
		provider: provider,
		lockPath: filepath.Join(lockDir, "store.lock"),
	}
	if cfg.debug != nil {
		s.debug = &safeWriter{w: cfg.debug}
	}
	return s, nil
}

// withLock acquires an exclusive process-level flock on the sentinel lock file,
// calls fn, then releases it. The lock serializes concurrent aistat processes
// across the read-index → read-items → mutate → write-items → write-index
// critical section, preventing lost-index races. Each call to withLock opens
// the file anew (new open file description), so goroutines in the same process
// also serialize correctly.
func (s *darwinStore) withLock(fn func() error) error {
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("accounts: open lock file %s: %w", s.lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("accounts: acquire store lock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// readIndex reads the list of opaque keys from the index keychain item.
// Returns a nil slice (not an error) if the index item does not exist.
func (s *darwinStore) readIndex(ctx context.Context) ([]string, error) {
	result := runDarwinSecurity(ctx, false, "find-generic-password",
		"-s", darwinAccountIndexService(s.provider), "-a", darwinIndexAccount, "-w")
	if result.Err != nil {
		if darwinIsNotFound(result) {
			return nil, nil
		}
		return nil, fmt.Errorf("accounts: read index: %w", darwinKeychainErr(result))
	}
	data := strings.TrimSpace(string(result.Output))
	if data == "" {
		return nil, nil
	}
	var idx struct {
		Keys  []string `json:"keys"`
		UUIDs []string `json:"uuids"`
	}
	if err := json.Unmarshal([]byte(data), &idx); err != nil {
		return nil, fmt.Errorf("accounts: parse index: %w", err)
	}
	return dedupeKeys(append(idx.Keys, idx.UUIDs...)), nil
}

// writeIndex persists the opaque key list as the index keychain item.
// An empty slice deletes the index item entirely (clean state after final remove).
func (s *darwinStore) writeIndex(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return darwinDeleteItem(ctx, darwinAccountIndexService(s.provider), darwinIndexAccount)
	}
	data, err := json.Marshal(struct {
		Keys []string `json:"keys"`
	}{Keys: dedupeKeys(keys)})
	if err != nil {
		return err
	}
	return darwinWriteItem(ctx, darwinAccountIndexService(s.provider), darwinIndexAccount, string(data))
}

// darwinReadAccountItem reads the Account JSON for the given opaque key. It
// reports absent only for a positively classified Keychain absence. Looks up
// by service only (no account filter) so that identity-drift email changes
// (D9) do not break the read path.
func darwinReadAccountItem(ctx context.Context, p Provider, key string) ([]byte, bool, error) {
	svc := darwinPerAccountService(p, key)
	result := runDarwinSecurity(ctx, false, "find-generic-password", "-s", svc, "-w")
	if result.Err != nil {
		if darwinIsNotFound(result) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("accounts: read account %s: %w", key, darwinKeychainErr(result))
	}
	data := strings.TrimSpace(string(result.Output))
	if data == "" {
		return nil, false, fmt.Errorf("accounts: read account %s: empty keychain value", key)
	}
	return []byte(data), false, nil
}

// upsertAccountItem writes the Account JSON as the per-account keychain item.
// See darwinWriteItem below for the `-U` upsert semantics. The explicit
// pre-delete here is NOT redundant after `-U`: it is required for D9 identity
// drift, where a stored account's email may change. `-U` matches by (service,
// account), so a changed email would miss the existing row and silently
// insert a duplicate. The pre-delete by service only removes the old row first.
//
// Failure mode: if the pre-delete succeeds but the subsequent add fails, the
// per-account item is gone while its opaque key may still be in the index. A
// later List silently compacts that positively classified absence on a
// successful repair. A failed repair leaves the index intact and emits debug
// diagnostics; the next List can retry the repair.
func upsertAccountItem(ctx context.Context, p Provider, a Account) error {
	svc := darwinPerAccountService(p, a.Key())
	// darwinDeleteItem returns nil for "not found", so this is always safe on
	// first-time upsert. Propagate any other error (e.g. permission denied).
	if err := darwinDeleteItem(ctx, svc, ""); err != nil {
		return fmt.Errorf("accounts: pre-upsert delete of %s: %w", a.Key(), err)
	}
	return darwinWriteItem(ctx, svc, a.Email, string(mustMarshal(a)))
}

// darwinWriteItem calls `security add-generic-password -U`, which creates the
// item if it does not exist and updates the value in place if it does. The -U
// flag matches by (service, account); callers that need to handle a change to
// either of those uniqueness fields must delete-then-add explicitly (see
// upsertAccountItem for the D9 identity-drift case where email may change).
//
// Without -U the call would exit 45 ("item already exists") on every write
// after the first to the same (service, account) — the historical bug that
// silently dropped index updates after the first Upsert.
func darwinWriteItem(ctx context.Context, service, account, value string) error {
	result := runDarwinSecurity(ctx, true, "add-generic-password",
		"-U", "-s", service, "-a", account, "-w", value)
	if result.Err != nil {
		return fmt.Errorf("keychain write %s/%s: %s", service, account,
			strings.TrimSpace(string(result.Output)))
	}
	return nil
}

// darwinDeleteItem calls `security delete-generic-password`. Returns nil if
// the item is not found. When account is empty, the lookup is by service only.
func darwinDeleteItem(ctx context.Context, service, account string) error {
	args := []string{"delete-generic-password", "-s", service}
	if account != "" {
		args = append(args, "-a", account)
	}
	result := runDarwinSecurity(ctx, false, args...)
	if result.Err != nil {
		if darwinIsNotFound(result) {
			return nil
		}
		return fmt.Errorf("keychain delete %s: %w", service, darwinKeychainErr(result))
	}
	return nil
}

func (s *darwinStore) List(ctx context.Context) ([]Account, error) {
	var accounts []Account
	err := s.withLock(func() error {
		keys, err := s.readIndex(ctx)
		if err != nil {
			return err
		}
		validKeys := make([]string, 0, len(keys))
		var missingKeys []string
		for _, key := range keys {
			data, absent, err := darwinReadAccountItem(ctx, s.provider, key)
			if err != nil {
				return err
			}
			if absent {
				missingKeys = append(missingKeys, key)
				continue
			}
			var a Account
			if err := json.Unmarshal(data, &a); err != nil {
				return fmt.Errorf("accounts: parse account %s: %w", key, err)
			}
			accounts = append(accounts, a)
			validKeys = append(validKeys, key)
		}
		if len(missingKeys) != 0 {
			if err := s.writeIndex(ctx, validKeys); err != nil {
				if s.debug != nil {
					for _, key := range missingKeys {
						fmt.Fprintf(s.debug, "aistat: orphan account index entry %s\n", key)
					}
					fmt.Fprintf(s.debug, "aistat: could not repair orphan account index entries (%s)\n", err)
				}
			}
		}
		return nil
	})
	return accounts, err
}

func (s *darwinStore) Upsert(ctx context.Context, a Account) error {
	return s.withLock(func() error {
		// Write per-account item FIRST, then update index.
		// If we crash after writing the item but before updating the index, the
		// item is an orphan-without-index — silently ignored by List with a
		// --debug warn. The opposite ordering would produce an in-index item
		// whose fetch returns nil (an orphan-in-index), which is harder to
		// detect and surface.
		if err := upsertAccountItem(ctx, s.provider, a); err != nil {
			return err
		}
		keys, err := s.readIndex(ctx)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if key == a.Key() {
				return nil // already indexed
			}
		}
		keys = append(keys, a.Key())
		return s.writeIndex(ctx, keys)
	})
}

func (s *darwinStore) Delete(ctx context.Context, key string) error {
	return s.withLock(func() error {
		// Update index FIRST, then delete per-account item.
		// If we crash after updating the index but before deleting the item,
		// the item is an orphan-without-index — silently ignored by List.
		// The opposite ordering (delete item first) risks a key remaining in
		// the index with no backing item — an orphan-in-index that is harder
		// to surface cleanly.
		keys, err := s.readIndex(ctx)
		if err != nil {
			return err
		}
		filtered := make([]string, 0, len(keys))
		for _, id := range keys {
			if id != key {
				filtered = append(filtered, id)
			}
		}
		if err := s.writeIndex(ctx, filtered); err != nil {
			return err
		}
		svc := darwinPerAccountService(s.provider, key)
		return darwinDeleteItem(ctx, svc, "")
	})
}

func (s *darwinStore) Promote(ctx context.Context, instruction Promotion) (PromotionResult, error) {
	var result PromotionResult
	err := s.withLock(func() error {
		sourceData, absent, err := darwinReadAccountItem(ctx, s.provider, instruction.SourceKey)
		if err != nil {
			return err
		}
		if absent {
			result = PromotionSourceChanged
			return nil
		}
		var source Account
		if err := json.Unmarshal(sourceData, &source); err != nil {
			return fmt.Errorf("accounts: parse account %s: %w", instruction.SourceKey, err)
		}
		if !bytes.Equal(source.RawBlob, instruction.ObservedSourceRawBlob) {
			result = PromotionSourceChanged
			return nil
		}

		destinationKey := instruction.Destination.Key()
		destinationData, destinationAbsent, err := darwinReadAccountItem(ctx, s.provider, destinationKey)
		if err != nil {
			return err
		}
		keys, err := s.readIndex(ctx)
		if err != nil {
			return err
		}
		// List walks the index, so an unindexed item is debris from a promotion
		// interrupted before its index write, not a stored account. Counting it as
		// present wedges the account: the source blob rotates away from that frozen
		// copy and the CAS can never match again.
		indexed := slices.Contains(keys, destinationKey)
		var existingDestination Account
		present := !destinationAbsent && indexed
		if present {
			if err := json.Unmarshal(destinationData, &existingDestination); err != nil {
				return fmt.Errorf("accounts: parse account %s: %w", destinationKey, err)
			}
		}
		if present != instruction.ExpectedDestinationPresent ||
			(present && !bytes.Equal(existingDestination.RawBlob, instruction.ExpectedDestinationRawBlob)) {
			result = PromotionDestinationChanged
			return nil
		}

		account := instruction.Destination.Email
		if present {
			account = existingDestination.Email
		}
		if !destinationAbsent && !indexed {
			// Unindexed debris: remove it first. darwinWriteItem's -U matches on
			// (service, account), so writing over a stale item whose account
			// attribute differs would add a second item rather than replace it.
			if err := darwinDeleteItem(ctx, darwinPerAccountService(s.provider, destinationKey), ""); err != nil {
				return err
			}
		}
		if err := darwinWriteItem(ctx, darwinPerAccountService(s.provider, destinationKey), account, string(mustMarshal(instruction.Destination))); err != nil {
			return err
		}
		keys = appendKey(keys, destinationKey)
		if err := s.writeIndex(ctx, keys); err != nil {
			return err
		}
		if err := darwinDeleteItem(ctx, darwinPerAccountService(s.provider, instruction.SourceKey), ""); err != nil {
			return err
		}
		keys = removeKey(keys, instruction.SourceKey)
		if err := s.writeIndex(ctx, keys); err != nil {
			return err
		}
		result = PromotionCompleted
		return nil
	})
	return result, err
}

// darwinIsNotFound reports whether a security-command error means "item not found."
func darwinIsNotFound(result darwinSecurityResult) bool {
	var ee *exec.ExitError
	if !errors.As(result.Err, &ee) {
		return false
	}
	// /usr/bin/security exits 44 (errSecItemNotFound) when the item is absent.
	if ee.ExitCode() == 44 {
		return true
	}
	return strings.Contains(strings.TrimSpace(string(result.Stderr)), "could not be found")
}

// darwinKeychainErr extracts a readable message from a security-command failure.

func darwinKeychainErr(result darwinSecurityResult) error {
	if s := strings.TrimSpace(string(result.Stderr)); s != "" {
		return fmt.Errorf("keychain: %s", s)
	}
	return result.Err
}

func dedupeKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	deduped := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, key)
	}
	return deduped
}

func appendKey(keys []string, key string) []string {
	for _, existing := range keys {
		if existing == key {
			return keys
		}
	}
	return append(keys, key)
}

func removeKey(keys []string, key string) []string {
	filtered := make([]string, 0, len(keys))
	for _, existing := range keys {
		if existing != key {
			filtered = append(filtered, existing)
		}
	}
	return filtered
}

// mustMarshal marshals v to JSON, panicking on error.
// Used only where v is a known-serializable type (Account).
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("accounts: mustMarshal: %v", err))
	}
	return b
}
