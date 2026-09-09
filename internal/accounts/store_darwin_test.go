//go:build darwin

package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// sentinelUUID is a recognisable opaque key used in live keychain tests so
// cleanup can force-delete items even if the test panics.
const sentinelUUID = "aistat-test-00000000-0000-0000-0000-000000000001"

// skipUnlessLive skips the test when AISTAT_LIVE_KEYCHAIN is not set to "1".
func skipUnlessLive(t *testing.T) {
	t.Helper()
	if os.Getenv("AISTAT_LIVE_KEYCHAIN") != "1" {
		t.Skip("set AISTAT_LIVE_KEYCHAIN=1 to run live keychain tests")
	}
}

// forceDeleteSentinel removes the sentinel per-account keychain item and
// its index entry regardless of error. Used in t.Cleanup to avoid leaking
// test items into the user's keychain.
func forceDeleteSentinel(uuid string) {
	ctx := context.Background()
	svc := darwinPerAccountService(ProviderClaude, uuid)
	darwinDeleteItem(ctx, svc, "")
	// Best-effort index clean: open store and delete. Ignore errors.
	if s, err := OpenStore(ProviderClaude); err == nil {
		s.Delete(ctx, uuid)
	}
}

type fakeKeychainItem struct {
	account string
	value   []byte
}

type fakeDarwinSecurity struct {
	items map[string]fakeKeychainItem
	calls []darwinSecurityCall
	fail  func(darwinSecurityCall) *darwinSecurityResult
	after func(darwinSecurityCall) *darwinSecurityResult
}

type darwinSecurityCall struct {
	combined bool
	args     []string
}

func newFakeDarwinSecurity() *fakeDarwinSecurity {
	return &fakeDarwinSecurity{items: make(map[string]fakeKeychainItem)}
}

func (f *fakeDarwinSecurity) run(_ context.Context, combined bool, args ...string) darwinSecurityResult {
	call := darwinSecurityCall{combined: combined, args: append([]string(nil), args...)}
	f.calls = append(f.calls, call)
	if f.fail != nil {
		if result := f.fail(call); result != nil {
			return *result
		}
	}
	service := darwinArg(args, "-s")
	var result darwinSecurityResult
	switch args[0] {
	case "find-generic-password":
		item, ok := f.items[service]
		if !ok {
			result = darwinNotFoundResult(44, "")
			break
		}
		result = darwinSecurityResult{Output: append([]byte(nil), item.value...)}
	case "add-generic-password":
		item := f.items[service]
		if account := darwinArg(args, "-a"); item.account == "" {
			item.account = account
		}
		item.value = []byte(darwinArg(args, "-w"))
		f.items[service] = item
		result = darwinSecurityResult{}
	case "delete-generic-password":
		if _, ok := f.items[service]; !ok {
			result = darwinNotFoundResult(44, "")
			break
		}
		delete(f.items, service)
		result = darwinSecurityResult{}
	default:
		result = darwinSecurityResult{Err: fmt.Errorf("unexpected security command %q", args[0])}
	}
	if f.after != nil {
		if interrupted := f.after(call); interrupted != nil {
			return *interrupted
		}
	}
	return result
}

func darwinArg(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func darwinNotFoundResult(exitCode int, stderr string) darwinSecurityResult {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", exitCode)).Run()
	return darwinSecurityResult{Err: err, Stderr: []byte(stderr)}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func callTrace(calls []darwinSecurityCall) []string {
	trace := make([]string, len(calls))
	for i, call := range calls {
		trace[i] = call.args[0]
	}
	return trace
}

func installDarwinSecurity(t *testing.T, fake *fakeDarwinSecurity) {
	t.Helper()
	previous := runDarwinSecurity
	runDarwinSecurity = fake.run
	t.Cleanup(func() { runDarwinSecurity = previous })
}

func newHermeticDarwinStore(t *testing.T, fake *fakeDarwinSecurity, debug *bytes.Buffer) *darwinStore {
	t.Helper()
	installDarwinSecurity(t, fake)
	store := &darwinStore{provider: ProviderClaude, lockPath: filepath.Join(t.TempDir(), "store.lock")}
	if debug != nil {
		store.debug = &safeWriter{w: debug}
	}
	return store
}

func (f *fakeDarwinSecurity) putAccount(a Account, account string) {
	f.items[darwinPerAccountService(ProviderClaude, a.Key())] = fakeKeychainItem{account: account, value: mustMarshal(a)}
}

func (f *fakeDarwinSecurity) setIndex(keys ...string) {
	data, err := json.Marshal(struct {
		Keys []string `json:"keys"`
	}{Keys: keys})
	if err != nil {
		panic(err)
	}
	f.items[darwinAccountIndexService(ProviderClaude)] = fakeKeychainItem{account: darwinIndexAccount, value: data}
}

func (f *fakeDarwinSecurity) setLegacyIndex(uuids ...string) {
	data, err := json.Marshal(struct {
		UUIDs []string `json:"uuids"`
	}{UUIDs: uuids})
	if err != nil {
		panic(err)
	}
	f.items[darwinAccountIndexService(ProviderClaude)] = fakeKeychainItem{account: darwinIndexAccount, value: data}
}

func (f *fakeDarwinSecurity) indexKeys(t *testing.T) []string {
	t.Helper()
	item, ok := f.items[darwinAccountIndexService(ProviderClaude)]
	if !ok {
		return nil
	}
	var index struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(item.value, &index); err != nil {
		t.Fatalf("parse fake index: %v", err)
	}
	return index.Keys
}

func TestDarwinSecurityProtocol(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"read uses Output and classifies exit 44", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			newHermeticDarwinStore(t, fake, nil)
			_, absent, err := darwinReadAccountItem(context.Background(), ProviderClaude, "missing")
			if err != nil || !absent || len(fake.calls) != 1 || fake.calls[0].combined {
				t.Fatalf("read = absent %t, err %v, calls %#v", absent, err, fake.calls)
			}
		}},
		{"read classifies textual not found from stderr", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			fake.fail = func(call darwinSecurityCall) *darwinSecurityResult {
				if call.args[0] == "find-generic-password" {
					result := darwinNotFoundResult(1, "The specified item could not be found in the keychain.")
					return &result
				}
				return nil
			}
			newHermeticDarwinStore(t, fake, nil)
			_, absent, err := darwinReadAccountItem(context.Background(), ProviderClaude, "missing")
			if err != nil || !absent || len(fake.calls) != 1 || fake.calls[0].combined {
				t.Fatalf("read = absent %t, err %v, calls %#v", absent, err, fake.calls)
			}
		}},
		{"read preserves stderr for a non-not-found failure", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			fake.fail = func(call darwinSecurityCall) *darwinSecurityResult {
				if call.args[0] == "find-generic-password" {
					result := darwinNotFoundResult(1, "read access denied")
					return &result
				}
				return nil
			}
			newHermeticDarwinStore(t, fake, nil)
			_, absent, err := darwinReadAccountItem(context.Background(), ProviderClaude, "denied")
			if got, want := errString(err), "accounts: read account denied: keychain: read access denied"; got != want {
				t.Errorf("read error = %q, want %q", got, want)
			}
			if absent || len(fake.calls) != 1 || fake.calls[0].combined {
				t.Errorf("read absent=%t, calls=%#v", absent, fake.calls)
			}
		}},
		{"write uses CombinedOutput diagnostics", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			fake.fail = func(call darwinSecurityCall) *darwinSecurityResult {
				if call.args[0] == "add-generic-password" {
					result := darwinSecurityResult{Output: []byte("write denied"), Err: errors.New("exit status 1")}
					return &result
				}
				return nil
			}
			newHermeticDarwinStore(t, fake, nil)
			err := darwinWriteItem(context.Background(), "service", "account", "value")
			if got, want := errString(err), "keychain write service/account: write denied"; got != want {
				t.Errorf("write error = %q, want %q", got, want)
			}
			if len(fake.calls) != 1 || !fake.calls[0].combined {
				t.Errorf("write calls %#v", fake.calls)
			}
		}},
		{"legacy uuids index is read and rewritten as keys", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			store := newHermeticDarwinStore(t, fake, nil)
			legacy := makeTestAccount("550e8400-e29b-41d4-a716-446655440000", "legacy@example.com")
			newer := makeTestAccount("11111111-1111-4111-8111-111111111111", "new@example.com")
			fake.putAccount(legacy, "legacy@example.com")
			fake.setLegacyIndex(legacy.Key())
			if got, err := store.List(context.Background()); err != nil || len(got) != 1 || got[0].Key() != "550e8400-e29b-41d4-a716-446655440000" {
				t.Fatalf("List = %#v, %v; want legacy key", got, err)
			}
			if err := store.Upsert(context.Background(), newer); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			var index map[string]json.RawMessage
			if err := json.Unmarshal(fake.items[darwinAccountIndexService(ProviderClaude)].value, &index); err != nil {
				t.Fatalf("parse rewritten index: %v", err)
			}
			if got, want := fake.indexKeys(t), []string{"550e8400-e29b-41d4-a716-446655440000", "11111111-1111-4111-8111-111111111111"}; !reflect.DeepEqual(got, want) {
				t.Errorf("rewritten keys = %v, want %v", got, want)
			}
			if _, ok := index["uuids"]; ok {
				t.Errorf("rewritten index retained legacy uuids: %s", fake.items[darwinAccountIndexService(ProviderClaude)].value)
			}
			if _, ok := index["keys"]; !ok {
				t.Errorf("rewritten index omitted keys: %s", fake.items[darwinAccountIndexService(ProviderClaude)].value)
			}
		}},
		{"successful empty account value does not compact index", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			store := newHermeticDarwinStore(t, fake, nil)
			fake.setIndex("live")
			fake.items[darwinPerAccountService(ProviderClaude, "live")] = fakeKeychainItem{account: "live@example.com", value: []byte(" \n")}
			if _, err := store.List(context.Background()); err == nil || !strings.Contains(err.Error(), "empty keychain value") {
				t.Fatalf("List err = %v", err)
			}
			if got := fake.indexKeys(t); !reflect.DeepEqual(got, []string{"live"}) {
				t.Errorf("index after malformed read = %v, want [live]", got)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestDarwinPromotionProtocol(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"absent destination installs desired destination and keeps sibling", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			store := newHermeticDarwinStore(t, fake, nil)
			source, destination, sibling := promotionFixture()
			fake.putAccount(source, "legacy@example.com")
			fake.putAccount(sibling, "sibling@example.com")
			fake.setIndex(source.Key(), sibling.Key())
			result, err := store.Promote(context.Background(), Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination})
			if result != PromotionCompleted || err != nil {
				t.Fatalf("Promote = %v, %v", result, err)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
		}},
		{"matching destination refreshes metadata and preserves Keychain account", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			store := newHermeticDarwinStore(t, fake, nil)
			source, destination, sibling := promotionFixture()
			stale := destination
			stale.Email = "stale@example.com"
			stale.LastSeenAt = stale.LastSeenAt.Add(-1)
			fake.putAccount(source, "legacy@example.com")
			fake.putAccount(stale, "preserved@example.com")
			fake.putAccount(sibling, "sibling@example.com")
			fake.setIndex(source.Key(), destination.Key(), sibling.Key())
			result, err := store.Promote(context.Background(), Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: stale.RawBlob})
			if result != PromotionCompleted || err != nil {
				t.Fatalf("Promote = %v, %v", result, err)
			}
			if got := fake.items[darwinPerAccountService(ProviderClaude, destination.Key())].account; got != "preserved@example.com" {
				t.Errorf("destination Keychain account = %q", got)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
		}},
		{"changed source and differing destination leave all records intact", func(t *testing.T) {
			for _, scenario := range []struct {
				name   string
				setup  func(fake *fakeDarwinSecurity, source, destination, sibling Account)
				input  func(source, destination Account) Promotion
				result PromotionResult
			}{
				{"source changed", func(fake *fakeDarwinSecurity, source, _ Account, sibling Account) {
					source.RawBlob = rawBlob("access", "changed", 0)
					fake.putAccount(source, "legacy@example.com")
					fake.putAccount(sibling, "sibling@example.com")
					fake.setIndex(source.Key(), sibling.Key())
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination}
				}, PromotionSourceChanged},
				{"destination differs", func(fake *fakeDarwinSecurity, source, destination, sibling Account) {
					changed := destination
					changed.RawBlob = rawBlob("access", "changed", 0)
					fake.putAccount(source, "legacy@example.com")
					fake.putAccount(changed, "canonical@example.com")
					fake.putAccount(sibling, "sibling@example.com")
					fake.setIndex(source.Key(), destination.Key(), sibling.Key())
				}, func(source, destination Account) Promotion {
					return Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}
				}, PromotionDestinationChanged},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					fake := newFakeDarwinSecurity()
					store := newHermeticDarwinStore(t, fake, nil)
					source, destination, sibling := promotionFixture()
					scenario.setup(fake, source, destination, sibling)
					before := accountMap(t, store)
					result, err := store.Promote(context.Background(), scenario.input(source, destination))
					if result != scenario.result || err != nil {
						t.Fatalf("Promote = %v, %v", result, err)
					}
					assertAccounts(t, store, before)
				})
			}
		}},
		{"post-commit interruptions stop at every durable boundary", func(t *testing.T) {
			// One complete Promote issues these commands in order; interrupting at a
			// durable boundary aborts immediately after that command, so every scenario
			// expects this sequence truncated at its own boundary. Stating the order
			// once is the point: four hand-written traces, each a prefix of the next,
			// drift apart the moment one is edited.
			promoteCalls := []string{
				"find-generic-password",   // 1 read source
				"find-generic-password",   // 2 read destination
				"find-generic-password",   // 3 read index: the index, not the item, decides whether the destination is a live account
				"add-generic-password",    // 4 write destination      — boundary "destination write"
				"add-generic-password",    // 5 write index, source and destination both listed — boundary "first index write"
				"delete-generic-password", // 6 delete source          — boundary "source delete"
				"add-generic-password",    // 7 write index, source retired — boundary "final index write"
			}
			for _, scenario := range []struct {
				name       string
				interrupt  func(source, destination Account) func(darwinSecurityCall) bool
				stopsAfter int // promoteCalls length reached before this scenario's boundary aborts
				wantIndex  func(source, destination, sibling Account) []string
				sourceGone bool
			}{
				{"destination write", func(_ Account, destination Account) func(darwinSecurityCall) bool {
					return func(call darwinSecurityCall) bool {
						return call.args[0] == "add-generic-password" && darwinArg(call.args, "-s") == darwinPerAccountService(ProviderClaude, destination.Key())
					}
				}, 4, func(_ Account, _ Account, _ Account) []string {
					return []string{"550e8400-e29b-41d4-a716-446655440000", "11111111-1111-4111-8111-111111111111"}
				}, false},
				{"first index write", func(_ Account, _ Account) func(darwinSecurityCall) bool {
					return func(call darwinSecurityCall) bool {
						return call.args[0] == "add-generic-password" && strings.HasSuffix(darwinArg(call.args, "-s"), ":index")
					}
				}, 5, func(source, destination, sibling Account) []string {
					return []string{"550e8400-e29b-41d4-a716-446655440000", "11111111-1111-4111-8111-111111111111", "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"}
				}, false},
				{"source delete", func(source Account, _ Account) func(darwinSecurityCall) bool {
					return func(call darwinSecurityCall) bool {
						return call.args[0] == "delete-generic-password" && darwinArg(call.args, "-s") == darwinPerAccountService(ProviderClaude, source.Key())
					}
				}, 6, func(source, destination, sibling Account) []string {
					return []string{"550e8400-e29b-41d4-a716-446655440000", "11111111-1111-4111-8111-111111111111", "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"}
				}, true},
				{"final index write", func(_ Account, _ Account) func(darwinSecurityCall) bool {
					indexWrites := 0
					return func(call darwinSecurityCall) bool {
						if call.args[0] == "add-generic-password" && strings.HasSuffix(darwinArg(call.args, "-s"), ":index") {
							indexWrites++
						}
						return indexWrites == 2
					}
				}, 7, func(_ Account, _ Account, _ Account) []string {
					return []string{"11111111-1111-4111-8111-111111111111", "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042"}
				}, true},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					fake := newFakeDarwinSecurity()
					store := newHermeticDarwinStore(t, fake, nil)
					source, destination, sibling := promotionFixture()
					fake.putAccount(source, "legacy@example.com")
					fake.putAccount(sibling, "sibling@example.com")
					fake.setIndex(source.Key(), sibling.Key())
					shouldInterrupt := scenario.interrupt(source, destination)
					fake.after = func(call darwinSecurityCall) *darwinSecurityResult {
						if shouldInterrupt(call) {
							result := darwinSecurityResult{Err: errors.New("interrupted after durable command")}
							return &result
						}
						return nil
					}
					result, err := store.Promote(context.Background(), Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination})
					if result != PromotionResultUnspecified || err == nil {
						t.Fatalf("Promote = %v, %v", result, err)
					}
					if got, want := callTrace(fake.calls), promoteCalls[:scenario.stopsAfter]; !reflect.DeepEqual(got, want) {
						t.Fatalf("security call trace = %v, want %v; later command ran", got, want)
					}
					if _, ok := fake.items[darwinPerAccountService(ProviderClaude, destination.Key())]; !ok {
						t.Error("destination durable state missing")
					}
					if got, want := fake.indexKeys(t), scenario.wantIndex(source, destination, sibling); !reflect.DeepEqual(got, want) {
						t.Errorf("durable index = %v, want %v", got, want)
					}
					_, sourcePresent := fake.items[darwinPerAccountService(ProviderClaude, source.Key())]
					if sourcePresent == scenario.sourceGone {
						t.Errorf("source present = %t, want %t", sourcePresent, !scenario.sourceGone)
					}
				})
			}
		}},
		{"source delete error leaves U indexed and retry retires it", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			store := newHermeticDarwinStore(t, fake, nil)
			source, destination, sibling := promotionFixture()
			fake.putAccount(source, "legacy@example.com")
			fake.putAccount(destination, "canonical@example.com")
			fake.putAccount(sibling, "sibling@example.com")
			fake.setIndex(source.Key(), destination.Key(), sibling.Key())
			fake.fail = func(call darwinSecurityCall) *darwinSecurityResult {
				if call.args[0] == "delete-generic-password" && darwinArg(call.args, "-s") == darwinPerAccountService(ProviderClaude, source.Key()) {
					result := darwinSecurityResult{Stderr: []byte("source delete denied"), Err: errors.New("exit 1")}
					return &result
				}
				return nil
			}
			instruction := Promotion{SourceKey: source.Key(), ObservedSourceRawBlob: source.RawBlob, Destination: destination, ExpectedDestinationPresent: true, ExpectedDestinationRawBlob: destination.RawBlob}
			result, err := store.Promote(context.Background(), instruction)
			if result != PromotionResultUnspecified || errString(err) != "keychain delete aistat:accounts:claude:550e8400-e29b-41d4-a716-446655440000: keychain: source delete denied" {
				t.Fatalf("failed Promote = %v, %v", result, err)
			}
			fake.fail = nil
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000": source, "550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
			result, err = store.Promote(context.Background(), instruction)
			if result != PromotionCompleted || err != nil {
				t.Fatalf("retry Promote = %v, %v", result, err)
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination, "11111111-1111-4111-8111-111111111111": sibling})
		}},
		{"compaction failure warns and later repair is silent", func(t *testing.T) {
			fake := newFakeDarwinSecurity()
			var debug bytes.Buffer
			store := newHermeticDarwinStore(t, fake, &debug)
			_, destination, _ := promotionFixture()
			fake.putAccount(destination, "canonical@example.com")
			fake.setIndex("missing", destination.Key())
			failed := false
			fake.fail = func(call darwinSecurityCall) *darwinSecurityResult {
				if !failed && call.args[0] == "add-generic-password" && strings.HasSuffix(darwinArg(call.args, "-s"), ":index") {
					failed = true
					result := darwinSecurityResult{Output: []byte("repair denied"), Err: errors.New("exit 1")}
					return &result
				}
				return nil
			}
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination})
			wantWarning := "aistat: orphan account index entry missing\naistat: could not repair orphan account index entries (keychain write aistat:accounts:claude:index/index: repair denied)\n"
			if got := debug.String(); got != wantWarning {
				t.Errorf("debug = %q, want %q", got, wantWarning)
			}
			fake.fail = nil
			debug.Reset()
			assertAccounts(t, store, map[string]Account{"550e8400-e29b-41d4-a716-446655440000_7d3c58e9-6a2b-4f81-b771-1c9e5d3a7042": destination})
			if debug.Len() != 0 {
				t.Errorf("successful repair warned: %q", debug.String())
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// TestDarwinServiceNaming pins the per-account and index service name formats
// before Step 4 introduces provider parameterization. These tests are non-live
// and do not require AISTAT_LIVE_KEYCHAIN.
func TestDarwinServiceNaming(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"per-account service name", func(t *testing.T) {
			uuid := "550e8400-e29b-41d4-a716-446655440000"
			want := "aistat:accounts:claude:" + uuid
			if got := darwinPerAccountService(ProviderClaude, uuid); got != want {
				t.Errorf("darwinPerAccountService: got %q, want %q", got, want)
			}
		}},
		{"index service name", func(t *testing.T) {
			want := "aistat:accounts:claude:index"
			if got := darwinAccountIndexService(ProviderClaude); got != want {
				t.Errorf("darwinAccountIndexService: got %q, want %q", got, want)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestDarwinStore(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"add list delete", func(t *testing.T) {
			skipUnlessLive(t)

			t.Cleanup(func() { forceDeleteSentinel(sentinelUUID) })

			s, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ctx := context.Background()

			a := makeTestAccount(sentinelUUID, "aistat-test@example.com")

			if err := s.Upsert(ctx, a); err != nil {
				t.Fatalf("Upsert: %v", err)
			}

			list, err := s.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			found := false
			for _, acct := range list {
				if acct.UUID == sentinelUUID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("sentinel UUID not found in List result")
			}

			if err := s.Delete(ctx, sentinelUUID); err != nil {
				t.Fatalf("Delete: %v", err)
			}

			list, err = s.List(ctx)
			if err != nil {
				t.Fatalf("List after delete: %v", err)
			}
			for _, acct := range list {
				if acct.UUID == sentinelUUID {
					t.Errorf("sentinel UUID still present after delete")
				}
			}
		}},
		{"orphan index handling", func(t *testing.T) {
			skipUnlessLive(t)

			orphanUUID := "aistat-test-orphan-0000-0000-0000-000000000002"
			liveUUID := "aistat-test-live-0000-0000-0000-000000000003"
			t.Cleanup(func() {
				forceDeleteSentinel(orphanUUID)
				forceDeleteSentinel(liveUUID)
			})

			s, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ctx := context.Background()

			// Upsert the live account normally.
			liveAcct := makeTestAccount(liveUUID, "live@example.com")
			if err := s.Upsert(ctx, liveAcct); err != nil {
				t.Fatalf("Upsert live: %v", err)
			}

			// Manually write an orphan opaque key into the index without creating the
			// per-account item, simulating a crash between index-update and item-write
			// on a Delete.
			ds := s.(*darwinStore)
			if err := ds.withLock(func() error {
				uuids, err := ds.readIndex(ctx)
				if err != nil {
					return err
				}
				uuids = append(uuids, orphanUUID)
				return ds.writeIndex(ctx, uuids)
			}); err != nil {
				t.Fatalf("inject orphan: %v", err)
			}

			// List repairs positively-classified missing entries and returns only
			// the live account without an orphan warning.
			var debugBuf bytes.Buffer
			sWithDebug, err := OpenStore(ProviderClaude, WithDebug(&debugBuf))
			if err != nil {
				t.Fatalf("OpenStore with debug: %v", err)
			}
			list, err := sWithDebug.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(list) != 1 || list[0].UUID != liveUUID {
				t.Errorf("want [%s], got %v", liveUUID, list)
			}
			if debugBuf.Len() != 0 {
				t.Errorf("successful orphan repair should not warn; got: %q", debugBuf.String())
			}
			if err := ds.withLock(func() error {
				keys, err := ds.readIndex(ctx)
				if err != nil {
					return err
				}
				if len(keys) != 1 || keys[0] != "aistat-test-live-0000-0000-0000-000000000003" {
					t.Errorf("repaired index = %v, want live account key", keys)
				}
				return nil
			}); err != nil {
				t.Fatalf("read repaired index: %v", err)
			}
		}},
		{"concurrent upserts", func(t *testing.T) {
			skipUnlessLive(t)

			uuid1 := "aistat-test-conc1-0000-0000-0000-000000000004"
			uuid2 := "aistat-test-conc2-0000-0000-0000-000000000005"
			t.Cleanup(func() {
				forceDeleteSentinel(uuid1)
				forceDeleteSentinel(uuid2)
			})

			s, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ctx := context.Background()

			a1 := makeTestAccount(uuid1, "conc1@example.com")
			a2 := makeTestAccount(uuid2, "conc2@example.com")

			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); s.Upsert(ctx, a1) }()
			go func() { defer wg.Done(); s.Upsert(ctx, a2) }()
			wg.Wait()

			list, err := s.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			found := map[string]bool{}
			for _, a := range list {
				found[a.UUID] = true
			}
			if !found[uuid1] || !found[uuid2] {
				t.Errorf("want both UUIDs present after concurrent upserts; got: %v", list)
			}
		}},
		// Pins the regression where the second and later Upserts (and any
		// non-emptying Delete) failed to update the index because
		// darwinWriteItem called `security add-generic-password` without -U.
		// Sequential (not concurrent) so the failure mode is deterministic.
		// Covers both directions: Upsert grows the index, Delete shrinks it.
		{"index grows and shrinks", func(t *testing.T) {
			skipUnlessLive(t)

			uuid1 := "aistat-test-grow1-0000-0000-0000-000000000006"
			uuid2 := "aistat-test-grow2-0000-0000-0000-000000000007"
			t.Cleanup(func() {
				forceDeleteSentinel(uuid1)
				forceDeleteSentinel(uuid2)
			})

			s, err := OpenStore(ProviderClaude)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			ctx := context.Background()

			if err := s.Upsert(ctx, makeTestAccount(uuid1, "grow1@example.com")); err != nil {
				t.Fatalf("Upsert 1: %v", err)
			}
			if err := s.Upsert(ctx, makeTestAccount(uuid2, "grow2@example.com")); err != nil {
				t.Fatalf("Upsert 2: %v", err)
			}

			list, err := s.List(ctx)
			if err != nil {
				t.Fatalf("List after upserts: %v", err)
			}
			found := map[string]bool{}
			for _, a := range list {
				found[a.UUID] = true
			}
			if !found[uuid1] || !found[uuid2] {
				t.Fatalf("want both UUIDs present after sequential upserts; got: %v", list)
			}

			// Delete uuid1 — index post-filter is non-empty ([uuid2]), so the writeIndex
			// inside Delete exercises the same upsert path the bug lived in.
			if err := s.Delete(ctx, uuid1); err != nil {
				t.Fatalf("Delete uuid1: %v", err)
			}

			list, err = s.List(ctx)
			if err != nil {
				t.Fatalf("List after delete: %v", err)
			}
			found = map[string]bool{}
			for _, a := range list {
				found[a.UUID] = true
			}
			if found[uuid1] {
				t.Errorf("uuid1 should be gone after delete; got: %v", list)
			}
			if !found[uuid2] {
				t.Errorf("uuid2 should still be present after deleting uuid1; got: %v", list)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
