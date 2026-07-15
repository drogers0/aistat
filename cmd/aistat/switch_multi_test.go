package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// withCodexMemoryStore swaps openCodexAccountStore for a MemoryStore during the test.
func withCodexMemoryStore(t *testing.T) *accounts.MemoryStore {
	t.Helper()
	ms := accounts.NewMemoryStore()
	old := openCodexAccountStore
	openCodexAccountStore = func(_ io.Writer) (accounts.Store, error) { return ms, nil }
	t.Cleanup(func() { openCodexAccountStore = old })
	return ms
}

// stubCodexSwitchClient implements switchable for Codex switch tests.
type stubCodexSwitchClient struct {
	fetchResults       []providers.AccountResult
	fetchErr           error
	reconcileCalled    bool
	postSwitchVerifyFn func(context.Context, accounts.Account) error
}

func (s *stubCodexSwitchClient) FetchForSwitch(_ context.Context) ([]providers.AccountResult, error) {
	return s.fetchResults, s.fetchErr
}

func (s *stubCodexSwitchClient) ReconcileAndPersist(_ context.Context) error {
	s.reconcileCalled = true
	return nil
}

func (s *stubCodexSwitchClient) PostSwitchVerify(ctx context.Context, target accounts.Account) error {
	if s.postSwitchVerifyFn != nil {
		return s.postSwitchVerifyFn(ctx, target)
	}
	return nil
}

// withCodexSwitchClient swaps newCodexSwitchClient for the duration of the test.
func withCodexSwitchClient(t *testing.T, stub *stubCodexSwitchClient) {
	t.Helper()
	old := newCodexSwitchClient
	newCodexSwitchClient = func(_ io.Writer, _ string, _ accounts.Store) switchable { return stub }
	t.Cleanup(func() { newCodexSwitchClient = old })
}

// withCodexWriteBlob stubs writeCodexLiveBlob, capturing the written bytes.
func withCodexWriteBlob(t *testing.T) (written *[]byte, writeErr *error) {
	t.Helper()
	var blob []byte
	var werr error
	old := writeCodexLiveBlob
	writeCodexLiveBlob = func(_ context.Context, raw []byte) error {
		if werr != nil {
			return werr
		}
		blob = append([]byte{}, raw...)
		return nil
	}
	t.Cleanup(func() { writeCodexLiveBlob = old })
	return &blob, &werr
}

// withCodexActiveUUID stubs switchLookupCodexActiveUUID to return a fixed UUID.
func withCodexActiveUUID(t *testing.T, uuid string) {
	t.Helper()
	old := switchLookupCodexActiveUUID
	switchLookupCodexActiveUUID = func(_ context.Context, _ []accounts.Account, _ io.Writer) (string, error) {
		return uuid, nil
	}
	t.Cleanup(func() { switchLookupCodexActiveUUID = old })
}

// withCodexFetchLiveUsageFn stubs the codex fetch seam (distinct from claude's
// fetchLiveUsage), giving the callback the token the gate would send.
func withCodexFetchLiveUsageFn(t *testing.T, fn func(token string) (map[string]providers.Limit, error)) {
	t.Helper()
	old := fetchCodexLiveUsage
	fetchCodexLiveUsage = func(_ context.Context, token, _, _ string, _ io.Writer) (map[string]providers.Limit, error) {
		return fn(token)
	}
	t.Cleanup(func() { fetchCodexLiveUsage = old })
}

// seedCodexAccount inserts a Codex-shaped account into ms.
func seedCodexAccount(t *testing.T, ms *accounts.MemoryStore, uuid, email, plan string, lastSeen time.Time) {
	t.Helper()
	rawBlob := []byte(`{"tokens":{"access_token":"ctok-` + uuid + `","refresh_token":"crt-` + uuid + `"}}`)
	a, err := accounts.NewAccount(rawBlob, uuid, email, email, plan, lastSeen)
	testutil.WantNoErr(t, err)
	if err := ms.Upsert(context.Background(), a); err != nil {
		t.Fatalf("seedCodexAccount Upsert: %v", err)
	}
}

// runSwitchMultiTest calls runSwitch with empty globals and captures output.
func runSwitchMultiTest(args ...string) runResult {
	var stdout, stderr bytes.Buffer
	code := runSwitch(args, &stdout, &stderr, globals{})
	return runResult{stdout.String(), stderr.String(), code}
}

// ---- Bulk switch tests ----

func TestSwitchBulk(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"both providers switched", func(t *testing.T) {
			now := time.Now()

			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-cwork", "cwork@example.com", "plan", now.Add(-2*time.Hour))
			seedAccount(t, claudeMS, "uuid-cpersonal", "cpersonal@example.com", "plan", now.Add(-1*time.Hour))
			withSwitchActiveUUID(t, "uuid-cwork")

			claudeStub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "cpersonal@example.com", UUID: "uuid-cpersonal", Limits: makeLimits(80)},
				},
			}
			withSwitchClient(t, claudeStub)
			claudeWritten, _ := withWriteBlob(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(20), nil
			})

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-dwork", "dwork@chatgpt.com", "plan", now.Add(-2*time.Hour))
			seedCodexAccount(t, codexMS, "uuid-dpersonal", "dpersonal@chatgpt.com", "plan", now.Add(-1*time.Hour))
			withCodexActiveUUID(t, "uuid-dwork")

			codexStub := &stubCodexSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "dpersonal@chatgpt.com", UUID: "uuid-dpersonal", Limits: makeLimits(80)},
				},
			}
			withCodexSwitchClient(t, codexStub)
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest()
			wantExit(t, r, 0)
			wantOut(t, r, "[claude]")
			wantOut(t, r, "[codex]")
			if *claudeWritten == nil {
				t.Error("Claude blob not written")
			}
			if *codexWritten == nil {
				t.Error("Codex blob not written")
			}
		}},
		{"only claude has multiple", func(t *testing.T) {
			now := time.Now()

			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-cw", "cw@example.com", "plan", now.Add(-2*time.Hour))
			seedAccount(t, claudeMS, "uuid-cp", "cp@example.com", "plan", now.Add(-1*time.Hour))
			withSwitchActiveUUID(t, "uuid-cw")

			claudeStub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "cp@example.com", UUID: "uuid-cp", Limits: makeLimits(80)},
				},
			}
			withSwitchClient(t, claudeStub)
			claudeWritten, _ := withWriteBlob(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(20), nil
			})

			// Codex has only 1 account → not eligible.
			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-donly", "only@chatgpt.com", "plan", now)
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest()
			wantExit(t, r, 0)
			wantOut(t, r, "[claude]")
			if strings.Contains(r.stdout, "[codex]") {
				t.Errorf("[codex] header should not appear; stdout: %q", r.stdout)
			}
			if *claudeWritten == nil {
				t.Error("Claude blob should have been written")
			}
			if *codexWritten != nil {
				t.Error("Codex blob should NOT have been written")
			}
		}},
		{"only codex has multiple", func(t *testing.T) {
			now := time.Now()

			// Claude has only 1 account → not eligible.
			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-conly", "conly@example.com", "plan", now)
			claudeWritten, _ := withWriteBlob(t)

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-dwork", "dwork@chatgpt.com", "plan", now.Add(-2*time.Hour))
			seedCodexAccount(t, codexMS, "uuid-dpersonal", "dpersonal@chatgpt.com", "plan", now.Add(-1*time.Hour))
			withCodexActiveUUID(t, "uuid-dwork")

			codexStub := &stubCodexSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "dpersonal@chatgpt.com", UUID: "uuid-dpersonal", Limits: makeLimits(80)},
				},
			}
			withCodexSwitchClient(t, codexStub)
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest()
			wantExit(t, r, 0)
			if strings.Contains(r.stdout, "[claude]") {
				t.Errorf("[claude] header should not appear; stdout: %q", r.stdout)
			}
			wantOut(t, r, "[codex]")
			if *claudeWritten != nil {
				t.Error("Claude blob should NOT have been written")
			}
			if *codexWritten == nil {
				t.Error("Codex blob should have been written")
			}
		}},
		{"no provider has multiple", func(t *testing.T) {
			now := time.Now()

			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-conly", "conly@example.com", "plan", now)

			withCodexMemoryStore(t) // empty Codex

			r := runSwitchMultiTest()
			wantExit(t, r, 0)
			wantErrOut(t, r, "no providers have multiple stored accounts")
		}},
		{"skips empty store", func(t *testing.T) {
			now := time.Now()

			// Claude empty.
			withMemoryStore(t)
			claudeWritten, _ := withWriteBlob(t)

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-dw", "dw@chatgpt.com", "plan", now.Add(-2*time.Hour))
			seedCodexAccount(t, codexMS, "uuid-dp", "dp@chatgpt.com", "plan", now.Add(-1*time.Hour))
			withCodexActiveUUID(t, "uuid-dw")

			codexStub := &stubCodexSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "dp@chatgpt.com", UUID: "uuid-dp", Limits: makeLimits(80)},
				},
			}
			withCodexSwitchClient(t, codexStub)
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest()
			wantExit(t, r, 0)
			wantOut(t, r, "[codex]")
			if *claudeWritten != nil {
				t.Error("Claude blob should NOT have been written (empty store, skipped)")
			}
			if *codexWritten == nil {
				t.Error("Codex blob should have been written")
			}
		}},
		{"partial failure one provider succeeds", func(t *testing.T) {
			now := time.Now()

			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-cf1", "cf1@example.com", "plan", now.Add(-2*time.Hour))
			seedAccount(t, claudeMS, "uuid-cf2", "cf2@example.com", "plan", now.Add(-1*time.Hour))
			withSwitchActiveUUID(t, "uuid-cf1")
			withSwitchClient(t, &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "cf2@example.com", UUID: "uuid-cf2", Limits: makeLimits(80)},
				},
			})
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(20), nil
			})
			_, claudeWriteErr := withWriteBlob(t)
			*claudeWriteErr = errors.New("keychain locked")

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-ds1", "ds1@chatgpt.com", "plan", now.Add(-2*time.Hour))
			seedCodexAccount(t, codexMS, "uuid-ds2", "ds2@chatgpt.com", "plan", now.Add(-1*time.Hour))
			withCodexActiveUUID(t, "uuid-ds1")
			withCodexSwitchClient(t, &stubCodexSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "ds2@chatgpt.com", UUID: "uuid-ds2", Limits: makeLimits(80)},
				},
			})
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest()
			wantExit(t, r, 2)
			// D5: per-provider headers must appear in stdout even when a provider fails.
			wantOut(t, r, "[claude]")
			wantOut(t, r, "[codex]")
			wantErrOut(t, r, "write to live credential failed")
			if *codexWritten == nil {
				t.Error("Codex blob should have been written despite Claude failure")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ---- Provider-arg tests ----

func TestSwitchProviderArg(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"claude switches only claude", func(t *testing.T) {
			now := time.Now()

			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-cw2", "cw2@example.com", "plan", now.Add(-2*time.Hour))
			seedAccount(t, claudeMS, "uuid-cp2", "cp2@example.com", "plan", now.Add(-1*time.Hour))
			withSwitchActiveUUID(t, "uuid-cw2")

			claudeStub := &stubSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "cp2@example.com", UUID: "uuid-cp2", Limits: makeLimits(80)},
				},
			}
			withSwitchClient(t, claudeStub)
			claudeWritten, _ := withWriteBlob(t)
			withFetchLiveUsageFn(t, func(_ string) (map[string]providers.Limit, error) {
				return makeLimits(20), nil
			})

			withCodexMemoryStore(t) // empty Codex — must NOT be touched
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest("claude")
			wantExit(t, r, 0)
			if *claudeWritten == nil {
				t.Error("Claude blob should have been written")
			}
			if *codexWritten != nil {
				t.Error("Codex should NOT have been touched")
			}
		}},
		{"codex switches only codex", func(t *testing.T) {
			now := time.Now()

			withMemoryStore(t) // empty Claude — must NOT be touched
			claudeWritten, _ := withWriteBlob(t)

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-d1", "d1@chatgpt.com", "plan", now.Add(-2*time.Hour))
			seedCodexAccount(t, codexMS, "uuid-d2", "d2@chatgpt.com", "plan", now.Add(-1*time.Hour))
			withCodexActiveUUID(t, "uuid-d1")

			codexStub := &stubCodexSwitchClient{
				fetchResults: []providers.AccountResult{
					{Email: "d2@chatgpt.com", UUID: "uuid-d2", Limits: makeLimits(80)},
				},
			}
			withCodexSwitchClient(t, codexStub)
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest("codex")
			wantExit(t, r, 0)
			if *claudeWritten != nil {
				t.Error("Claude should NOT have been touched")
			}
			if *codexWritten == nil {
				t.Error("Codex blob should have been written")
			}
		}},
		{"unknown provider errors", func(t *testing.T) {
			withMemoryStore(t)
			withCodexMemoryStore(t)

			r := runSwitchMultiTest("bogus")
			wantExit(t, r, 2)
			wantErrOut(t, r, "unknown provider")
		}},
		{"claude one account login hint", func(t *testing.T) {
			ms := withMemoryStore(t)
			seedAccount(t, ms, "uuid-only", "only@example.com", "plan", time.Now())
			withSwitchActiveUUID(t, "uuid-only")
			withCodexMemoryStore(t)

			r := runSwitchMultiTest("claude")
			wantExit(t, r, 2)
			wantErrOut(t, r, "only one account stored; nothing to switch to (run `claude /login` to add another)")
		}},
		{"codex zero accounts errors", func(t *testing.T) {
			withMemoryStore(t)
			withCodexMemoryStore(t) // empty Codex store

			r := runSwitchMultiTest("codex")
			wantExit(t, r, 2)
			wantErrOut(t, r, "no accounts stored")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ---- Infer-provider tests (--to without provider) ----

func TestSwitchToInfer(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"claude unique match switches claude", func(t *testing.T) {
			ms := withMemoryStore(t)
			seedAccount(t, ms, "uuid-cu", "unique@claude.com", "plan", time.Now())
			seedAccount(t, ms, "uuid-cu2", "other@claude.com", "plan", time.Now())
			withSwitchActiveUUID(t, "uuid-cu2")
			withSwitchClient(t, &stubSwitchClient{})
			claudeWritten, _ := withWriteBlob(t)

			withCodexMemoryStore(t) // empty → no Codex match
			withCodexWriteBlob(t)

			r := runSwitchMultiTest("--to", "unique")
			wantExit(t, r, 0)
			if *claudeWritten == nil {
				t.Error("Claude blob should have been written")
			}
		}},
		{"codex unique match switches codex", func(t *testing.T) {
			withMemoryStore(t) // empty Claude → no Claude match
			claudeWritten, _ := withWriteBlob(t)

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-du", "user@codex.com", "plan", time.Now())
			seedCodexAccount(t, codexMS, "uuid-da", "active@codex.com", "plan", time.Now())
			withCodexActiveUUID(t, "uuid-da")
			withCodexSwitchClient(t, &stubCodexSwitchClient{})
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest("--to", "user@codex")
			wantExit(t, r, 0)
			if *claudeWritten != nil {
				t.Error("Claude should NOT have been touched")
			}
			if *codexWritten == nil {
				t.Error("Codex blob should have been written")
			}
		}},
		{"ambiguous across providers errors", func(t *testing.T) {
			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-cs", "shared@example.com", "plan", time.Now())

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-ds", "shared@chatgpt.com", "plan", time.Now())

			r := runSwitchMultiTest("--to", "shared")
			wantExit(t, r, 2)
			wantErrOut(t, r, "multiple providers match")
		}},
		{"same provider multi match shows single-provider message", func(t *testing.T) {
			claudeMS := withMemoryStore(t)
			seedAccount(t, claudeMS, "uuid-ca", "shared@work.com", "plan", time.Now())
			seedAccount(t, claudeMS, "uuid-cb", "shared@personal.com", "plan", time.Now())

			withCodexMemoryStore(t) // empty → no Codex match

			r := runSwitchMultiTest("--to", "shared")
			wantExit(t, r, 2)
			wantErrOut(t, r, "multiple stored accounts match")
			if strings.Contains(r.stderr, "multiple providers match") {
				t.Errorf("should NOT show cross-provider ambiguity message; stderr: %q", r.stderr)
			}
		}},
		{"codex with explicit provider arg", func(t *testing.T) {
			withMemoryStore(t) // empty Claude
			claudeWritten, _ := withWriteBlob(t)

			codexMS := withCodexMemoryStore(t)
			seedCodexAccount(t, codexMS, "uuid-dtarget", "target@codex.com", "plan", time.Now())
			seedCodexAccount(t, codexMS, "uuid-dactive", "active@codex.com", "plan", time.Now())
			withCodexActiveUUID(t, "uuid-dactive")
			withCodexSwitchClient(t, &stubCodexSwitchClient{})
			codexWritten, _ := withCodexWriteBlob(t)

			r := runSwitchMultiTest("codex", "--to", "target@codex.com")
			wantExit(t, r, 0)
			if *claudeWritten != nil {
				t.Error("Claude should NOT have been touched")
			}
			if *codexWritten == nil {
				t.Error("Codex blob should have been written")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
