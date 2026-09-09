package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// ── fixtures ─────────────────────────────────────────────────────────────────

// jwtFromClaims builds a signature-free 3-segment JWT (header.payload.testsig)
// for the given claims. The fixed third segment satisfies the 3-non-empty-segment
// check in both cred.ParseCodexIDToken and cred.ParseJWTExp.
func jwtFromClaims(claims map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".testsig"
}

// syntheticIDToken builds a signature-free identity JWT whose payload is
// {"sub":<sub>,"email":<email>,"exp":<expSec>}.
func syntheticIDToken(sub, email string, expSec int64) string {
	return jwtFromClaims(map[string]any{"sub": sub, "email": email, "exp": expSec})
}

// accessJWT builds a synthetic access_token JWT carrying exp — the production
// expiry source read by StoredExpiresAt (cred.ParseJWTExp). sub is derived from
// accessToken so the string is deterministic (same (accessToken, expSec) ⇒
// identical bytes, preserving reconcile byte-match) and distinct per account.
func accessJWT(accessToken string, expSec int64) string {
	return jwtFromClaims(map[string]any{"sub": "sub-" + accessToken, "exp": expSec})
}

// rawCodexBlob builds a minimal valid Codex auth.json blob. When expiresAtSec==0
// the access_token is the bare accessToken string and no id_token is emitted
// (StoredExpiresAt → 0, no proactive refresh). When expiresAtSec!=0 the exp rides
// a synthetic access_token JWT (the production expiry source), and a sub-bearing
// id_token is emitted for reconcile's identity/lookup path.
func rawCodexBlob(accessToken, refreshToken string, expiresAtSec int64) json.RawMessage {
	sub := "sub-" + accessToken
	at := accessToken
	tokens := map[string]any{"refresh_token": refreshToken}
	if expiresAtSec != 0 {
		at = accessJWT(accessToken, expiresAtSec)
		tokens["id_token"] = syntheticIDToken(sub, sub+"@example.com", expiresAtSec)
	}
	tokens["access_token"] = at
	b, _ := json.Marshal(map[string]any{"tokens": tokens})
	return json.RawMessage(b)
}

// makeCodexCred wraps rawCodexBlob in a cred.Credential. AccessToken is read back
// from the blob so it equals StoredAccessToken (bare for exp 0, the JWT for
// exp!=0) — keeping reconcile byte-match consistent. ExpiresAt is always 0 for
// codex (production no longer derives it; see cred.parseCodexCredFull).
func makeCodexCred(accessToken, refreshToken string, expiresAtSec int64) *cred.Credential {
	raw := rawCodexBlob(accessToken, refreshToken, expiresAtSec)
	var r struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	_ = json.Unmarshal(raw, &r)
	return &cred.Credential{
		AccessToken:  r.Tokens.AccessToken,
		RefreshToken: refreshToken,
		Raw:          []byte(raw),
	}
}

// makeCodexAccount builds a stored Account with embedded Codex tokens.
func makeCodexAccount(uuid, email, accessToken, refreshToken string, expiresAtSec int64) accounts.Account {
	raw := rawCodexBlob(accessToken, refreshToken, expiresAtSec)
	return accounts.Account{
		UUID:       uuid,
		Email:      email,
		LastSeenAt: time.Time{},
		RawBlob:    raw,
	}
}

// ── LookupID stubs ────────────────────────────────────────────────────────────

func noLookupCall(t *testing.T) func(string) (string, string, error) {
	t.Helper()
	return func(string) (string, string, error) {
		t.Fatal("LookupID must not be called on the byte-match path")
		return "", "", nil
	}
}

func fixedLookup(sub, email string) func(string) (string, string, error) {
	return func(string) (string, string, error) { return sub, email, nil }
}

func errLookup(err error) func(string) (string, string, error) {
	return func(string) (string, string, error) { return "", "", err }
}

// ── test clock ────────────────────────────────────────────────────────────────

var testNow = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

// ── Reconcile tests ───────────────────────────────────────────────────────────

func TestReconcile(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"byte match sets active", func(t *testing.T) {
			// live AT byte-matches slot[0] → active, Upserted, no LookupID call.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeCodexAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}
			live := makeCodexCred("tok-a", "ref-a", 1000)

			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: noLookupCall(t),
				Now:      testNow,
			})

			if out.ActiveKey != "uuid-0" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-0")
			}
			if !out.Upserted {
				t.Error("Upserted = false, want true")
			}
			if out.Inserted {
				t.Error("Inserted = true, want false")
			}
			if out.LiveUnstored != nil {
				t.Errorf("LiveUnstored should be nil on byte-match, got %v", out.LiveUnstored)
			}
			if out.CaptureWarn != "" {
				t.Errorf("CaptureWarn should be empty on byte-match, got %q", out.CaptureWarn)
			}
			if len(out.Accounts) != 2 {
				t.Errorf("Accounts len = %d, want 2", len(out.Accounts))
			}
		}},
		{"byte match stale refresh token", func(t *testing.T) {
			// Same AT (same accessToken+exp ⇒ byte-identical access_token JWT, so
			// byte-match holds), different RT in live → RawBlob updated, LastSeenAt stamped.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a-old", 1000),
			}
			live := makeCodexCred("tok-a", "ref-a-new", 1000)

			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: noLookupCall(t),
				Now:      testNow,
			})

			if out.ActiveKey != "uuid-0" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-0")
			}
			if !out.Upserted {
				t.Error("Upserted = false, want true")
			}
			slot := out.Accounts[0]
			if got := StoredRefreshToken(slot); got != "ref-a-new" {
				t.Errorf("StoredRefreshToken() = %q, want %q", got, "ref-a-new")
			}
			if slot.LastSeenAt != testNow {
				t.Errorf("slot.LastSeenAt = %v, want %v", slot.LastSeenAt, testNow)
			}
		}},
		{"lookup id matches stored uuid", func(t *testing.T) {
			// no byte-match, LookupID returns sub matching stored UUID → Upserted, email drift applied.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "old@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCodexCred("tok-b", "ref-b", 2000)

			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: fixedLookup("uuid-0", "new@example.com"),
				Now:      testNow,
			})

			if out.ActiveKey != "uuid-0" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-0")
			}
			if !out.Upserted {
				t.Error("Upserted = false, want true")
			}
			if out.Inserted {
				t.Error("Inserted = true, want false")
			}
			if got := out.Accounts[0].Email; got != "new@example.com" {
				t.Errorf("Email = %q, want %q", got, "new@example.com")
			}
		}},
		{"lookup id new slot", func(t *testing.T) {
			// no byte-match, LookupID sub doesn't match any stored UUID → Inserted.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCodexCred("tok-new", "ref-new", 5000)

			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: fixedLookup("uuid-brand-new", "brand-new@example.com"),
				Now:      testNow,
			})

			if out.ActiveKey != "uuid-brand-new" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-brand-new")
			}
			if !out.Inserted {
				t.Error("Inserted = false, want true")
			}
			if out.Upserted {
				t.Error("Upserted = true, want false")
			}
			if len(out.Accounts) != 2 {
				t.Fatalf("Accounts len = %d, want 2", len(out.Accounts))
			}
			newSlot := out.Accounts[1]
			if newSlot.UUID != "uuid-brand-new" {
				t.Errorf("new slot UUID = %q, want %q", newSlot.UUID, "uuid-brand-new")
			}
			if newSlot.Email != "brand-new@example.com" {
				t.Errorf("new slot Email = %q, want %q", newSlot.Email, "brand-new@example.com")
			}
		}},
		{"lookup id empty store", func(t *testing.T) {
			// no stored accounts, live present → Inserted.
			live := makeCodexCred("tok-new", "ref-new", 5000)

			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   nil,
				LookupID: fixedLookup("uuid-fresh", "fresh@example.com"),
				Now:      testNow,
			})

			if out.ActiveKey != "uuid-fresh" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-fresh")
			}
			if !out.Inserted {
				t.Error("Inserted = false, want true")
			}
			if len(out.Accounts) != 1 {
				t.Fatalf("Accounts len = %d, want 1", len(out.Accounts))
			}
			if out.Accounts[0].UUID != "uuid-fresh" {
				t.Errorf("Accounts[0].UUID = %q, want %q", out.Accounts[0].UUID, "uuid-fresh")
			}
		}},
		{"lookup id fails fallback", func(t *testing.T) {
			// LookupID returns error → LiveUnstored set, CaptureWarn matches D3 template.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCodexCred("tok-b", "ref-b", 2000)

			lookupErr := fmt.Errorf("jwt parse failed")
			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: errLookup(lookupErr),
				Now:      testNow,
			})

			if out.ActiveKey != "" {
				t.Errorf("ActiveKey = %q, want empty", out.ActiveKey)
			}
			if out.LiveUnstored == nil {
				t.Fatal("LiveUnstored should be non-nil on fallback")
			}
			if out.LiveUnstored != live {
				t.Error("LiveUnstored should point to the live blob")
			}
			if !strings.Contains(out.CaptureWarn, "could not identify live account") {
				t.Errorf("CaptureWarn = %q, want 'could not identify live account'", out.CaptureWarn)
			}
			if !strings.Contains(out.CaptureWarn, "codex login") {
				t.Errorf("CaptureWarn = %q, want recovery hint containing 'codex login'", out.CaptureWarn)
			}
			if out.Inserted || out.Upserted {
				t.Error("Inserted/Upserted must be false on fallback")
			}
			if len(out.Accounts) != len(stored) {
				t.Errorf("Accounts len = %d, want %d", len(out.Accounts), len(stored))
			}
		}},
		{"missing id_token fallback", func(t *testing.T) {
			// live blob has no tokens.id_token → extractIDToken returns "" → guard triggers
			// before LookupID is called → LiveUnstored with CaptureWarn mentioning "no id_token".
			raw := rawCodexBlob("tok-b", "ref-b", 0)
			live := &cred.Credential{
				AccessToken:  "tok-b",
				RefreshToken: "ref-b",
				Raw:          []byte(raw),
			}
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}

			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: noLookupCall(t), // must NOT be called
				Now:      testNow,
			})

			if out.LiveUnstored == nil {
				t.Fatal("LiveUnstored should be non-nil when id_token is absent")
			}
			if !strings.Contains(out.CaptureWarn, "no id_token") {
				t.Errorf("CaptureWarn = %q, want 'no id_token'", out.CaptureWarn)
			}
		}},
		{"live absent stored n", func(t *testing.T) {
			// no live credential → all accounts returned, none active.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeCodexAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}

			out := Reconcile(ReconcileInput{
				LiveBlob: nil,
				Stored:   stored,
				LookupID: noLookupCall(t),
				Now:      testNow,
			})

			if out.ActiveKey != "" {
				t.Errorf("ActiveKey = %q, want empty", out.ActiveKey)
			}
			if len(out.Accounts) != 2 {
				t.Errorf("Accounts len = %d, want 2", len(out.Accounts))
			}
			for i, got := range out.Accounts {
				if got.UUID != stored[i].UUID {
					t.Errorf("Accounts[%d].UUID = %q, want %q", i, got.UUID, stored[i].UUID)
				}
			}
			if out.Inserted || out.Upserted {
				t.Error("Inserted/Upserted must be false when live credential is absent")
			}
		}},
		{"live absent stored empty", func(t *testing.T) {
			// no live, no stored → empty Accounts.
			out := Reconcile(ReconcileInput{
				LiveBlob: nil,
				Stored:   nil,
				LookupID: noLookupCall(t),
				Now:      testNow,
			})

			if out.ActiveKey != "" {
				t.Errorf("ActiveKey = %q, want empty", out.ActiveKey)
			}
			if len(out.Accounts) != 0 {
				t.Errorf("Accounts len = %d, want 0", len(out.Accounts))
			}
		}},
		{"duplicate access token first match wins", func(t *testing.T) {
			// two stored slots share AT → first-match-wins.
			stored := []accounts.Account{
				makeCodexAccount("uuid-first", "first@example.com", "tok-dup", "ref-a", 1000),
				makeCodexAccount("uuid-second", "second@example.com", "tok-dup", "ref-b", 2000),
			}
			live := makeCodexCred("tok-dup", "ref-a", 1000)

			out := Reconcile(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: noLookupCall(t),
				Now:      testNow,
			})

			if out.ActiveKey != "uuid-first" {
				t.Errorf("ActiveKey = %q, want %q (first-match-wins)", out.ActiveKey, "uuid-first")
			}
			if !out.Upserted {
				t.Error("Upserted = false, want true")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── ResolveActiveKey tests ───────────────────────────────────────────────────

func TestResolveActiveKey(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"byte match", func(t *testing.T) {
			// returns stored UUID, no LookupID call.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeCodexAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}
			live := makeCodexCred("tok-b", "ref-b", 2000) // matches slot[1]

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: noLookupCall(t),
				Now:      testNow,
			})

			testutil.WantNoErr(t, err)
			if uuid != "uuid-1" {
				t.Errorf("uuid = %q, want %q", uuid, "uuid-1")
			}
		}},
		{"lookup success", func(t *testing.T) {
			// returns sub from LookupID.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCodexCred("tok-c", "ref-c", 3000) // no byte-match

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: fixedLookup("uuid-from-lookup", "x@example.com"),
				Now:      testNow,
			})

			testutil.WantNoErr(t, err)
			if uuid != "" {
				t.Errorf("uuid = %q, want empty for unstored lookup key", uuid)
			}
		}},
		{"lookup fails returns empty", func(t *testing.T) {
			// LookupID error → ("", nil).
			// All LookupID failures are pure parse errors (D1); never surfaced as errors
			// from ResolveActiveKey.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCodexCred("tok-c", "ref-c", 3000)

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: errLookup(fmt.Errorf("parse error")),
				Now:      testNow,
			})

			if err != nil {
				t.Errorf("expected nil error (D1: all LookupID failures are parse-only), got: %v", err)
			}
			if uuid != "" {
				t.Errorf("uuid = %q, want empty on lookup failure", uuid)
			}
		}},
		{"no mutation", func(t *testing.T) {
			// asserts in.Stored is not modified.
			stored := []accounts.Account{
				makeCodexAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeCodexAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}
			snapshot := make([]accounts.Account, len(stored))
			copy(snapshot, stored)

			live := makeCodexCred("tok-c", "ref-c", 3000) // no byte-match

			_, _ = ResolveActiveKey(ReconcileInput{
				LiveBlob: live,
				Stored:   stored,
				LookupID: fixedLookup("uuid-0", "changed@example.com"),
				Now:      testNow,
			})

			for i, orig := range snapshot {
				got := stored[i]
				if got.UUID != orig.UUID {
					t.Errorf("stored[%d].UUID mutated: %q → %q", i, orig.UUID, got.UUID)
				}
				if got.Email != orig.Email {
					t.Errorf("stored[%d].Email mutated: %q → %q", i, orig.Email, got.Email)
				}
				if string(got.RawBlob) != string(orig.RawBlob) {
					t.Errorf("stored[%d].RawBlob mutated", i)
				}
				if got.LastSeenAt != orig.LastSeenAt {
					t.Errorf("stored[%d].LastSeenAt mutated: %v → %v", i, orig.LastSeenAt, got.LastSeenAt)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
