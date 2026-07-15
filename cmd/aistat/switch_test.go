package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// stubSwitchClient implements switchable for tests.
type stubSwitchClient struct {
	fetchResults       []providers.AccountResult
	fetchErr           error
	reconcileCalled    bool
	reconcileFn        func(context.Context) error // optional; runs (in addition to setting reconcileCalled) e.g. to mutate a backing store
	postSwitchVerifyFn func(context.Context, accounts.Account) error
}

func (s *stubSwitchClient) FetchForSwitch(_ context.Context) ([]providers.AccountResult, error) {
	return s.fetchResults, s.fetchErr
}

func (s *stubSwitchClient) ReconcileAndPersist(ctx context.Context) error {
	s.reconcileCalled = true
	if s.reconcileFn != nil {
		return s.reconcileFn(ctx)
	}
	return nil
}

func (s *stubSwitchClient) PostSwitchVerify(ctx context.Context, target accounts.Account) error {
	if s.postSwitchVerifyFn != nil {
		return s.postSwitchVerifyFn(ctx, target)
	}
	return nil
}

// withSwitchClient swaps newSwitchClient for the duration of the test.
func withSwitchClient(t *testing.T, stub *stubSwitchClient) {
	t.Helper()
	old := newSwitchClient
	newSwitchClient = func(_ io.Writer, _ string, _ accounts.Store) switchable {
		return stub
	}
	t.Cleanup(func() { newSwitchClient = old })
}

// withSwitchActiveUUID stubs switchLookupActiveUUID to return a fixed UUID.
func withSwitchActiveUUID(t *testing.T, uuid string) {
	t.Helper()
	old := switchLookupActiveUUID
	switchLookupActiveUUID = func(_ context.Context, _ []accounts.Account, _ io.Writer) (string, error) {
		return uuid, nil
	}
	t.Cleanup(func() { switchLookupActiveUUID = old })
}

// withFetchLiveUsageFn stubs fetchLiveUsage with a function that receives the token.
func withFetchLiveUsageFn(t *testing.T, fn func(token string) (map[string]providers.Limit, error)) {
	t.Helper()
	old := fetchLiveUsage
	fetchLiveUsage = func(_ context.Context, token, _, _ string, _ io.Writer) (map[string]providers.Limit, error) {
		return fn(token)
	}
	t.Cleanup(func() { fetchLiveUsage = old })
}

// withWriteBlob stubs writeClaudeLiveBlob, capturing the written bytes.
// Setting *writeErr before the call makes the stub return that error.
func withWriteBlob(t *testing.T) (written *[]byte, writeErr *error) {
	t.Helper()
	var blob []byte
	var werr error
	old := writeClaudeLiveBlob
	writeClaudeLiveBlob = func(_ context.Context, raw []byte) error {
		if werr != nil {
			return werr
		}
		blob = append([]byte{}, raw...)
		return nil
	}
	t.Cleanup(func() { writeClaudeLiveBlob = old })
	return &blob, &werr
}

// runSwitchTest calls runSwitch directly with an empty globals struct.
func runSwitchTest(args ...string) runResult {
	var stdout, stderr bytes.Buffer
	code := runSwitch(args, &stdout, &stderr, globals{})
	return runResult{stdout.String(), stderr.String(), code}
}

// withWatchSleep swaps the watch loop's inter-tick sleep so `switch --watch`
// tests can bound the loop (returning a non-nil error stops watchLoop).
func withWatchSleep(t *testing.T, fn func(context.Context, time.Duration) error) {
	t.Helper()
	old := watchSleepFn
	watchSleepFn = fn
	t.Cleanup(func() { watchSleepFn = old })
}

// makeLimits builds a Limit map with the given five_hour RemainingPercent.
func makeLimits(fiveHourRemaining float64) map[string]providers.Limit {
	return map[string]providers.Limit{
		"five_hour": {
			RemainingPercent: fiveHourRemaining,
			UsedPercent:      100 - fiveHourRemaining,
		},
	}
}

// makeLimitsFull builds a Limit map from explicit per-window remaining
// percentages. A window absent from the map is omitted, mirroring how providers
// drop untouched/inapplicable windows. Only RemainingPercent (and its UsedPercent
// complement) is set — the auto-pick comparator reads nothing else.
func makeLimitsFull(remaining map[string]float64) map[string]providers.Limit {
	out := map[string]providers.Limit{}
	for k, r := range remaining {
		out[k] = providers.Limit{RemainingPercent: r, UsedPercent: 100 - r}
	}
	return out
}

func TestScoreAccount(t *testing.T) {
	seen := time.Now()
	tests := []struct {
		name      string
		limits    map[string]float64
		exhausted bool
		immediate int
		sustained int
	}{
		{"absent five_hour is full immediate", map[string]float64{"seven_day": 50}, false, 20, 10},
		{"present five_hour buckets", map[string]float64{"five_hour": 60, "seven_day": 90}, false, 12, 18},
		{"empty limits is a fresh full account", map[string]float64{}, false, 20, 20},
		{"exhausted just below boundary", map[string]float64{"seven_day": 0.999}, true, 20, 0},
		{"usable exactly at boundary", map[string]float64{"seven_day": 1.0}, false, 20, 0},
		{"sustained takes min across long windows", map[string]float64{"five_hour": 100, "seven_day": 80, "thirty_day": 5}, false, 20, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := scoreAccount(makeLimitsFull(tt.limits), seen)
			if s.exhausted != tt.exhausted || s.immediate != tt.immediate || s.sustained != tt.sustained {
				t.Errorf("scoreAccount = {exhausted:%v immediate:%d sustained:%d}, want {exhausted:%v immediate:%d sustained:%d}",
					s.exhausted, s.immediate, s.sustained, tt.exhausted, tt.immediate, tt.sustained)
			}
		})
	}
}

func TestScoreBetter(t *testing.T) {
	now := time.Now()
	usable := scoreAccount(makeLimitsFull(map[string]float64{"five_hour": 30, "seven_day": 50}), now)
	spent := scoreAccount(makeLimitsFull(map[string]float64{"five_hour": 100, "seven_day": 0}), now)
	if !usable.better(spent) {
		t.Error("non-exhausted account must beat an exhausted one regardless of 5h headroom")
	}
	if spent.better(usable) {
		t.Error("exhausted account must not beat a usable one")
	}

	// Equal exhaustion + immediate bucket → more weekly runway wins.
	hi := scoreAccount(makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 90}), now)
	lo := scoreAccount(makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 30}), now.Add(time.Hour))
	if !hi.better(lo) {
		t.Error("among same-5h-bucket accounts, more weekly runway must win over a more-recent lastSeen")
	}
}

// getAccountFromStore returns the account for uuid from the store, failing the
// test if it is absent.
func getAccountFromStore(t *testing.T, ms *accounts.MemoryStore, uuid string) accounts.Account {
	t.Helper()
	all, err := ms.List(context.Background())
	testutil.WantNoErr(t, err)
	for _, a := range all {
		if a.UUID == uuid {
			return a
		}
	}
	t.Fatalf("getAccountFromStore: uuid %q not found", uuid)
	return accounts.Account{}
}

// ---- --to targeted switch tests ----

func TestSwitchTo(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"email happy path", func(t *testing.T) {
			ms := withMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))

			withSwitchActiveUUID(t, "uuid-work") // work is currently active
			stub := &stubSwitchClient{}
			withSwitchClient(t, stub)
			written, _ := withWriteBlob(t)

			r := runSwitchTest("--to", "personal") // email-substring match
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com (uuid uuid-personal)")
			wantOut(t, r, "was work@example.com")

			// Written blob must match personal account's RawBlob.
			personal := getAccountFromStore(t, ms, "uuid-personal")
			if !bytes.Equal(*written, []byte(personal.RawBlob)) {
				t.Errorf("written blob mismatch: got %q, want %q", *written, personal.RawBlob)
			}

			// ReconcileAndPersist must be called post-write.
			if !stub.reconcileCalled {
				t.Error("ReconcileAndPersist was not called after successful write")
			}
		}},
		{"uuid prefix happy path", func(t *testing.T) {
			ms := withMemoryStore(t)
			now := time.Now()
			// Use hex UUIDs so the prefix triggers the uuidish branch.
			seedAccount(t, ms, "bbbb6666-7777-8888-9999-000000000000", "work@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))
			seedAccount(t, ms, "aaaa1111-2222-3333-4444-555555555555", "personal@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))

			withSwitchActiveUUID(t, "bbbb6666-7777-8888-9999-000000000000")
			stub := &stubSwitchClient{}
			withSwitchClient(t, stub)
			written, _ := withWriteBlob(t)

			r := runSwitchTest("--to", "aaaa1111") // UUID-prefix (8 hex chars)
			wantExit(t, r, 0)
			wantOut(t, r, "aaaa1111-2222-3333-4444-555555555555")

			personal := getAccountFromStore(t, ms, "aaaa1111-2222-3333-4444-555555555555")
			if !bytes.Equal(*written, []byte(personal.RawBlob)) {
				t.Errorf("written blob mismatch")
			}
			if !stub.reconcileCalled {
				t.Error("ReconcileAndPersist was not called")
			}
		}},
		{"already active no write", func(t *testing.T) {
			ms := withMemoryStore(t)
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", time.Now())
			withSwitchActiveUUID(t, "uuid-personal") // personal is active

			written, _ := withWriteBlob(t)

			r := runSwitchTest("--to", "personal")
			wantExit(t, r, 0)
			wantOut(t, r, "already on personal@example.com")
			if *written != nil {
				t.Error("writeClaudeLiveBlob should not have been called")
			}
		}},
		{"unknown target errors", func(t *testing.T) {
			ms := withMemoryStore(t)
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", time.Now())
			withSwitchActiveUUID(t, "uuid-work")

			r := runSwitchTest("--to", "nobody@example.com")
			wantExit(t, r, 2)
			wantErrOut(t, r, `no stored account matches "nobody@example.com"`)
		}},
		{"multiple matches disambiguate error", func(t *testing.T) {
			ms := withMemoryStore(t)
			now := time.Now()
			// Both emails contain "example" → multiple matches.
			seedAccount(t, ms, "uuid-a", "a@example.com", "default_claude_max_5x", now)
			seedAccount(t, ms, "uuid-b", "b@example.com", "default_claude_max_20x", now)
			withSwitchActiveUUID(t, "uuid-a")

			r := runSwitchTest("--to", "example")
			wantExit(t, r, 2)
			wantErrOut(t, r, `multiple stored accounts match "example", disambiguate by uuid`)
		}},
		{"write error no reconcile", func(t *testing.T) {
			ms := withMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))

			withSwitchActiveUUID(t, "uuid-work")
			stub := &stubSwitchClient{}
			withSwitchClient(t, stub)
			_, writeErrPtr := withWriteBlob(t)
			*writeErrPtr = errors.New("keychain locked")

			r := runSwitchTest("--to", "personal")
			wantExit(t, r, 2)
			wantErrOut(t, r, "aistat: claude: write to live credential failed: keychain locked")
			// ReconcileAndPersist must NOT have been called (store must be unchanged).
			if stub.reconcileCalled {
				t.Error("ReconcileAndPersist must not be called when write fails")
			}
		}},
		{"store open failure errors", func(t *testing.T) {
			old := openAccountStore
			openAccountStore = func(_ io.Writer) (accounts.Store, error) {
				return nil, errors.New("disk unavailable")
			}
			t.Cleanup(func() { openAccountStore = old })

			r := runSwitchTest("--to", "anyone")
			wantExit(t, r, 2)
			wantErrOut(t, r, "aistat: claude: could not open account store: disk unavailable")
		}},
		{"store list failure errors", func(t *testing.T) {
			old := openAccountStore
			openAccountStore = func(_ io.Writer) (accounts.Store, error) {
				return &failListStore{listErr: errors.New("disk I/O error")}, nil
			}
			t.Cleanup(func() { openAccountStore = old })
			withCodexMemoryStore(t) // empty Codex store for determinism

			r := runSwitchTest("--to", "anyone")
			wantExit(t, r, 2)
			wantErrOut(t, r, "aistat: claude: could not list accounts: disk I/O error")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ---- Auto-pick switch tests ----

func TestSwitchAutoPick(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"zero stored both empty", func(t *testing.T) {
			withMemoryStore(t)      // empty Claude store
			withCodexMemoryStore(t) // empty Codex store
			withSwitchActiveUUID(t, "")

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantErrOut(t, r, "no providers have multiple stored accounts")
		}},
		{"higher headroom wins", func(t *testing.T) {
			ms := withMemoryStore(t)
			withCodexMemoryStore(t) // isolate from any real Codex store on dev machines
			now := time.Now()
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))

			withSwitchActiveUUID(t, "uuid-work")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "personal@example.com", UUID: "uuid-personal", Limits: makeLimits(80)},
				},
			}
			withSwitchClient(t, stub)
			// Active (work) has 20% remaining → personal wins.
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(20), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "switched to personal@example.com")
			wantOut(t, r, "was work@example.com")
			personal := getAccountFromStore(t, ms, "uuid-personal")
			if !bytes.Equal(*written, []byte(personal.RawBlob)) {
				t.Errorf("written blob mismatch")
			}
		}},
		{"active already best no write", func(t *testing.T) {
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			seedAccount(t, ms, "uuid-work", "work@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))

			withSwitchActiveUUID(t, "uuid-personal")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					// work is non-active, only 20% remaining
					{Email: "work@example.com", UUID: "uuid-work", Limits: makeLimits(20)},
				},
			}
			withSwitchClient(t, stub)
			// Active (personal) has 80% remaining → already best.
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(80), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "already on best account (personal@example.com)")
			if *written != nil {
				t.Error("writeClaudeLiveBlob should not have been called when active is already best")
			}
		}},
		{"tiebreaker most recent wins", func(t *testing.T) {
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			// accountA: 82% remaining, last seen 2 hours ago (floor(82/5)=16)
			// accountB: 80% remaining, last seen 1 hour ago  (floor(80/5)=16)
			// Same bucket → tiebreak by LastSeenAt → accountB wins (more recent).
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-30*time.Minute))
			seedAccount(t, ms, "uuid-a", "accounta@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))
			seedAccount(t, ms, "uuid-b", "accountb@example.com", "default_claude_max_20x", now.Add(-1*time.Hour))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "accounta@example.com", UUID: "uuid-a", Limits: makeLimits(82)},
					{Email: "accountb@example.com", UUID: "uuid-b", Limits: makeLimits(80)},
				},
			}
			withSwitchClient(t, stub)
			// Active has 50% remaining (bucket 10), below both candidates (bucket 16).
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(50), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			// accountB should win (same bucket as A, but more recent LastSeenAt).
			wantOut(t, r, "accountb@example.com")
			accountB := getAccountFromStore(t, ms, "uuid-b")
			if !bytes.Equal(*written, []byte(accountB.RawBlob)) {
				t.Errorf("written blob does not match accountB's RawBlob")
			}
		}},
		{"no five_hour window beats exhausted five_hour active", func(t *testing.T) {
			// Issue #16: active is exhausted on its 5h window (1% remaining) but
			// the candidate has no five_hour window at all (untouched ⇒ full
			// headroom) plus a healthy seven_day. The candidate must be picked.
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-30*time.Minute))
			seedAccount(t, ms, "uuid-fresh", "fresh@example.com", "default_claude_max_5x", now.Add(-2*time.Hour))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "fresh@example.com", UUID: "uuid-fresh", Limits: makeLimitsFull(map[string]float64{"seven_day": 63})},
				},
			}
			withSwitchClient(t, stub)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 1, "seven_day": 80}), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "fresh@example.com")
			fresh := getAccountFromStore(t, ms, "uuid-fresh")
			if !bytes.Equal(*written, []byte(fresh.RawBlob)) {
				t.Errorf("written blob does not match fresh account's RawBlob")
			}
		}},
		{"exhausted long window not picked over usable active", func(t *testing.T) {
			// Candidate has a fresh 5h window but its seven_day is exhausted (0%);
			// active is usable on both. The exhaustion gate must keep us put.
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			seedAccount(t, ms, "uuid-spent", "spent@example.com", "default_claude_max_5x", now.Add(-30*time.Minute))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "spent@example.com", UUID: "uuid-spent", Limits: makeLimitsFull(map[string]float64{"five_hour": 100, "seven_day": 0})},
				},
			}
			withSwitchClient(t, stub)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 50, "seven_day": 80}), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "already on best account (active@example.com)")
			if *written != nil {
				t.Error("writeClaudeLiveBlob should not have been called for an exhausted candidate")
			}
		}},
		{"sustained tiebreak prefers more weekly runway", func(t *testing.T) {
			// Two candidates share the five_hour bucket; the lower-seven_day one is
			// seeded more-recent so current code's LastSeenAt tiebreak picks it.
			// New code must instead pick the higher-seven_day candidate.
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-3*time.Hour))
			seedAccount(t, ms, "uuid-hi", "hi@example.com", "default_claude_max_5x", now.Add(-2*time.Hour))
			seedAccount(t, ms, "uuid-lo", "lo@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "hi@example.com", UUID: "uuid-hi", Limits: makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 90})},
					{Email: "lo@example.com", UUID: "uuid-lo", Limits: makeLimitsFull(map[string]float64{"five_hour": 60, "seven_day": 30})},
				},
			}
			withSwitchClient(t, stub)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 10, "seven_day": 80}), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "hi@example.com")
			hi := getAccountFromStore(t, ms, "uuid-hi")
			if !bytes.Equal(*written, []byte(hi.RawBlob)) {
				t.Errorf("written blob does not match hi account's RawBlob")
			}
		}},
		{"free account exhausted on thirty_day not picked", func(t *testing.T) {
			// Regression guard (passes on current code): a free account with only
			// thirty_day at 0% remaining must never be auto-picked.
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			seedAccount(t, ms, "uuid-free", "free@example.com", "default_claude_max_5x", now.Add(-30*time.Minute))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "free@example.com", UUID: "uuid-free", Limits: makeLimitsFull(map[string]float64{"thirty_day": 0})},
				},
			}
			withSwitchClient(t, stub)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 40, "seven_day": 70}), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "already on best account (active@example.com)")
			if *written != nil {
				t.Error("writeClaudeLiveBlob should not have been called for an exhausted free account")
			}
		}},
		{"low but not exhausted long window still picked", func(t *testing.T) {
			// Exhaustion-only policy: a candidate with a fresh 5h and a low-but-not
			// -exhausted seven_day (7%) is still picked over a half-used active.
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			seedAccount(t, ms, "uuid-low", "low@example.com", "default_claude_max_5x", now.Add(-30*time.Minute))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "low@example.com", UUID: "uuid-low", Limits: makeLimitsFull(map[string]float64{"five_hour": 100, "seven_day": 7})},
				},
			}
			withSwitchClient(t, stub)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimitsFull(map[string]float64{"five_hour": 50, "seven_day": 80}), nil
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "low@example.com")
			low := getAccountFromStore(t, ms, "uuid-low")
			if !bytes.Equal(*written, []byte(low.RawBlob)) {
				t.Errorf("written blob does not match low account's RawBlob")
			}
		}},
		{"one failing excluded other wins", func(t *testing.T) {
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			seedAccount(t, ms, "uuid-good", "good@example.com", "default_claude_max_20x", now.Add(-30*time.Minute))
			// "bad" is excluded — FetchForSwitch already warned; stub does not return it.

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					// Only good@example.com survived; bad@example.com was excluded.
					{Email: "good@example.com", UUID: "uuid-good", Limits: makeLimits(70)},
				},
			}
			withSwitchClient(t, stub)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(20), nil // active at 20% → good at 70% wins
			})
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantOut(t, r, "good@example.com")
			good := getAccountFromStore(t, ms, "uuid-good")
			if !bytes.Equal(*written, []byte(good.RawBlob)) {
				t.Errorf("written blob mismatch")
			}
		}},
		{"all failing exits 2", func(t *testing.T) {
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			seedAccount(t, ms, "uuid-other", "other@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchResults: nil, // all excluded
			}
			withSwitchClient(t, stub)
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 2)
			wantErrOut(t, r, "auto-pick failed: no accounts produced usable usage data")
			if *written != nil {
				t.Error("writeClaudeLiveBlob should not have been called")
			}
		}},
		{"fetch error exits 2", func(t *testing.T) {
			ms := withMemoryStore(t)
			withCodexMemoryStore(t)
			now := time.Now()
			seedAccount(t, ms, "uuid-active", "active@example.com", "default_claude_max_5x", now.Add(-1*time.Hour))
			seedAccount(t, ms, "uuid-other", "other@example.com", "default_claude_max_20x", now.Add(-2*time.Hour))

			withSwitchActiveUUID(t, "uuid-active")
			stub := &stubSwitchClient{
				fetchErr: errors.New("network timeout"),
			}
			withSwitchClient(t, stub)
			written, _ := withWriteBlob(t)

			r := runSwitchTest()
			wantExit(t, r, 2)
			wantErrOut(t, r, "aistat: claude: auto-pick usage fetch failed: network timeout")
			if *written != nil {
				t.Error("writeClaudeLiveBlob should not have been called")
			}
		}},
		{"only one account no eligible providers", func(t *testing.T) {
			ms := withMemoryStore(t)
			seedAccount(t, ms, "uuid-personal", "personal@example.com", "default_claude_max_5x", time.Now())
			withCodexMemoryStore(t) // empty Codex store
			withSwitchActiveUUID(t, "uuid-personal")

			r := runSwitchTest()
			wantExit(t, r, 0)
			wantErrOut(t, r, "no providers have multiple stored accounts")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// failListStore is a Store whose List always returns an error; all other ops succeed.
type failListStore struct {
	listErr error
}

func (f *failListStore) List(_ context.Context) ([]accounts.Account, error) {
	return nil, f.listErr
}
func (f *failListStore) Upsert(_ context.Context, _ accounts.Account) error { return nil }
func (f *failListStore) Delete(_ context.Context, _ string) error           { return nil }

// ---- PostSwitchVerify tests (all use Codex scaffold per B4#2) ----

// scaffoldCodexSwitch sets up the Codex scaffold for PostSwitchVerify tests:
// two accounts ("alice" active-other, "bob" active), active = bob, --to alice.
// Returns the stub so callers can set postSwitchVerifyFn before calling runSwitchTest.
func scaffoldCodexSwitch(t *testing.T) *stubCodexSwitchClient {
	t.Helper()
	now := time.Now()
	ms := withCodexMemoryStore(t)
	seedCodexAccount(t, ms, "uuid-alice", "alice@example.com", "plan", now.Add(-1*time.Hour))
	seedCodexAccount(t, ms, "uuid-bob", "bob@example.com", "plan", now.Add(-2*time.Hour))
	withCodexActiveUUID(t, "uuid-bob")
	withMemoryStore(t) // empty Claude store — must not be touched
	stub := &stubCodexSwitchClient{}
	withCodexSwitchClient(t, stub)
	withCodexWriteBlob(t)
	return stub
}

func TestSwitchPostSwitchVerify(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"auth denied warns exit 0", func(t *testing.T) {
			stub := scaffoldCodexSwitch(t)
			stub.postSwitchVerifyFn = func(_ context.Context, _ accounts.Account) error {
				return fmt.Errorf("alice@example.com: tokens revoked by upstream...: %w", providers.ErrAuthDenied)
			}

			r := runSwitchTest("codex", "--to", "alice")
			wantExit(t, r, 0)
			wantOut(t, r, "switched to")
			wantErrOut(t, r, "aistat: codex: switched-to account's tokens are not usable: alice@example.com:")
			wantErrOut(t, r, "tokens revoked")
			if strings.Count(r.stderr, "aistat: codex:") != 1 {
				t.Errorf("expected exactly one 'aistat: codex:' in stderr, got %d: %q",
					strings.Count(r.stderr, "aistat: codex:"), r.stderr)
			}
		}},
		{"transient error silenced", func(t *testing.T) {
			stub := scaffoldCodexSwitch(t)
			stub.postSwitchVerifyFn = func(_ context.Context, _ accounts.Account) error {
				return providers.ErrTransient
			}

			r := runSwitchTest("codex", "--to", "alice")
			wantExit(t, r, 0)
			if strings.Contains(r.stderr, "tokens are not usable") {
				t.Errorf("transient error should be silenced; stderr: %q", r.stderr)
			}
		}},
		{"nil error no warning", func(t *testing.T) {
			scaffoldCodexSwitch(t) // postSwitchVerifyFn unset → returns nil

			r := runSwitchTest("codex", "--to", "alice")
			wantExit(t, r, 0)
			if strings.Contains(r.stderr, "tokens are not usable") {
				t.Errorf("nil verify should produce no warning; stderr: %q", r.stderr)
			}
		}},
		{"non-wrapping error silenced", func(t *testing.T) {
			stub := scaffoldCodexSwitch(t)
			stub.postSwitchVerifyFn = func(_ context.Context, _ accounts.Account) error {
				return errors.New("plain error without ErrAuthDenied wrap")
			}

			r := runSwitchTest("codex", "--to", "alice")
			wantExit(t, r, 0)
			if strings.Contains(r.stderr, "tokens are not usable") {
				t.Errorf("non-wrapping error should be silenced; stderr: %q", r.stderr)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
