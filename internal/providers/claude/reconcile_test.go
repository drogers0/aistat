package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/providers"
	"github.com/drogers0/aistat/v2/internal/testutil"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// rawBlob returns a minimal valid Claude credential JSON blob.
func rawBlob(accessToken, refreshToken string, expiresAt int64) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": refreshToken,
			"expiresAt":    expiresAt,
		},
	})
	return json.RawMessage(b)
}

// makeCred constructs a cred.Credential from inline values, mirroring
// parseClaudeCredFull behaviour.
func makeCred(accessToken, refreshToken string, expiresAt int64) *cred.Credential {
	raw := rawBlob(accessToken, refreshToken, expiresAt)
	return &cred.Credential{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Raw:          []byte(raw),
	}
}

// makeAccount constructs a stored Account with the given identity and token fields.
func makeAccount(uuid, email, accessToken, refreshToken string, expiresAt int64) accounts.Account {
	return accounts.Account{
		UUID:             uuid,
		Email:            email,
		DisplayName:      email,
		OrganizationUUID: accounts.PersonalOrganizationUUID,
		LastSeenAt:       time.Time{},
		RawBlob:          rawBlob(accessToken, refreshToken, expiresAt),
	}
}

// noProfileCall returns a LookupProfile stub that fails the test if called.
func noProfileCall(t *testing.T) func(string) (Profile, error) {
	t.Helper()
	return func(string) (Profile, error) {
		t.Fatal("LookupProfile must not be called on the byte-match path")
		return Profile{}, nil
	}
}

// fixedProfile returns a LookupProfile stub that always returns the given Profile.
func fixedProfile(p Profile) func(string) (Profile, error) {
	return func(string) (Profile, error) { return p, nil }
}

// errProfile returns a LookupProfile stub that always returns the given error.
func errProfile(err error) func(string) (Profile, error) {
	return func(string) (Profile, error) { return Profile{}, err }
}

// ── test clock ──────────────────────────────────────────────────────────────

var testNow = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)

// ── TestReconcile_ByteMatch ──────────────────────────────────────────────────

func TestReconcile_ByteMatch(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"sets active", func(t *testing.T) {
			// D1 branch 1: the live access token byte-matches slot[0]; the slot
			// becomes active and is upserted; no profile call is made.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}
			live := makeCred("tok-a", "ref-a", 1000)

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: noProfileCall(t),
				Now:           testNow,
			})

			if out.ActiveKey != "uuid-0_personal" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-0_personal")
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
		{"stale refresh token", func(t *testing.T) {
			// D1 branch 1 with drift: the live blob carries a newer refreshToken
			// and expiresAt. After upsert the stored slot must reflect the live values.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a-old", 1000),
			}
			// Same access token, but refreshToken and expiresAt differ.
			live := makeCred("tok-a", "ref-a-new", 9999)

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: noProfileCall(t),
				Now:           testNow,
			})

			if out.ActiveKey != "uuid-0_personal" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-0_personal")
			}
			if !out.Upserted {
				t.Error("Upserted = false, want true")
			}
			slot := out.Accounts[0]
			if got := StoredRefreshToken(slot); got != "ref-a-new" {
				t.Errorf("StoredRefreshToken() = %q, want %q", got, "ref-a-new")
			}
			if got := StoredExpiresAt(slot); got != 9999 {
				t.Errorf("StoredExpiresAt() = %d, want 9999", got)
			}
			if slot.LastSeenAt != testNow {
				t.Errorf("slot.LastSeenAt = %v, want %v", slot.LastSeenAt, testNow)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── TestReconcile_Profile ────────────────────────────────────────────────────

func TestReconcile_Profile(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"uuid matches slot1", func(t *testing.T) {
			// D1 branch 2 with UUID match on slot[1] and identity drift: the profile
			// returns a different email for the existing UUID; that email is overwritten (D9).
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeAccount("uuid-1", "user1-old@example.com", "tok-b", "ref-b", 2000),
			}
			live := makeCred("tok-c", "ref-c", 3000) // no byte-match

			prof := Profile{
				AccountUUID:      "uuid-1",
				Email:            "user1-new@example.com", // changed email (identity drift)
				DisplayName:      "New Name",
				RateLimitTier:    "claude_max_5x",
				OrganizationUUID: accounts.PersonalOrganizationUUID,
			}

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: fixedProfile(prof),
				Now:           testNow,
			})

			if out.ActiveKey != "uuid-1_personal" {
				t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-1_personal")
			}
			if !out.Upserted {
				t.Error("Upserted = false, want true")
			}
			if out.Inserted {
				t.Error("Inserted = true, want false")
			}
			// D9: email overwritten to profile's new value.
			if got := out.Accounts[1].Email; got != "user1-new@example.com" {
				t.Errorf("slot[1].Email = %q, want %q", got, "user1-new@example.com")
			}
			if got := out.Accounts[1].RateLimitTier; got != "claude_max_5x" {
				t.Errorf("slot[1].RateLimitTier = %q, want %q", got, "claude_max_5x")
			}
		}},
		{"uuid new slot", func(t *testing.T) {
			// D1 branch 2 with no UUID match: a new slot is inserted.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-new", "ref-new", 5000)

			prof := Profile{
				AccountUUID:   "uuid-brand-new",
				Email:         "brand-new@example.com",
				DisplayName:   "Brand New",
				RateLimitTier: "claude_max_20x",
			}

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: fixedProfile(prof),
				Now:           testNow,
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
		{"401 fallback", func(t *testing.T) {
			// D1 branch 4 triggered by a 401. CaptureWarn must match the generic
			// template; LiveUnstored must be the live blob.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-c", "ref-c", 3000)

			authErr := fmt.Errorf("%w: HTTP 401 from profile", providers.ErrAuthDenied)

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: errProfile(authErr),
				Now:           testNow,
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
			if !strings.Contains(out.CaptureWarn, "could not capture live account profile") {
				t.Errorf("CaptureWarn = %q, want generic fallback template", out.CaptureWarn)
			}
			if !strings.Contains(out.CaptureWarn, "claude /login") {
				t.Errorf("CaptureWarn = %q, want recovery hint containing 'claude /login'", out.CaptureWarn)
			}
			if strings.Contains(out.CaptureWarn, "missing required fields") {
				t.Errorf("CaptureWarn must not contain 'missing required fields' for a 401 error")
			}
			if out.Inserted || out.Upserted {
				t.Error("Inserted/Upserted must be false on fallback")
			}
			// Stored slice must be unchanged.
			if len(out.Accounts) != len(stored) {
				t.Errorf("Accounts len = %d, want %d", len(out.Accounts), len(stored))
			}
		}},
		{"503 fallback", func(t *testing.T) {
			// D1 branch 4 triggered by a transient 503 error.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-c", "ref-c", 3000)

			transientErr := fmt.Errorf("%w: HTTP 503", providers.ErrTransient)

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: errProfile(transientErr),
				Now:           testNow,
			})

			if out.LiveUnstored == nil {
				t.Fatal("LiveUnstored should be non-nil on fallback")
			}
			if !strings.Contains(out.CaptureWarn, "could not capture live account profile") {
				t.Errorf("CaptureWarn = %q, want generic fallback template", out.CaptureWarn)
			}
			if !strings.Contains(out.CaptureWarn, "claude /login") {
				t.Errorf("CaptureWarn = %q, want recovery hint containing 'claude /login'", out.CaptureWarn)
			}
		}},
		{"missing fields stricter diagnostic", func(t *testing.T) {
			// D1 branch 4 triggered by ErrProfileMissingFields (HTTP 200 but no
			// uuid/email). CaptureWarn must use the stricter diagnostic, not the
			// generic template.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-c", "ref-c", 3000)

			missingErr := fmt.Errorf("%w: got uuid=%q email=%q organization.uuid=%q", ErrProfileMissingFields, "", "", "")

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: errProfile(missingErr),
				Now:           testNow,
			})

			if out.LiveUnstored == nil {
				t.Fatal("LiveUnstored should be non-nil on fallback")
			}
			want := "aistat: claude: profile response missing required fields (account.uuid/account.email/organization.uuid); rendering live row without storing; file an issue at https://github.com/drogers0/aistat/issues"
			if out.CaptureWarn != want {
				t.Errorf("CaptureWarn = %q, want %q", out.CaptureWarn, want)
			}
			if strings.Contains(out.CaptureWarn, "claude /login") {
				t.Errorf("CaptureWarn = %q, must not contain 'claude /login' for missing-fields case", out.CaptureWarn)
			}
		}},
		{"new uuid empty store", func(t *testing.T) {
			// Insert branch of D1 step 2 works correctly when the stored set is
			// completely empty.
			live := makeCred("tok-new", "ref-new", 5000)

			prof := Profile{
				AccountUUID:   "uuid-fresh",
				Email:         "fresh@example.com",
				DisplayName:   "Fresh",
				RateLimitTier: "default_claude_max_5x",
			}

			out := Reconcile(ReconcileInput{
				LiveBlob:      live,
				Stored:        nil,
				LookupProfile: fixedProfile(prof),
				Now:           testNow,
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
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── TestReconcile_LiveAbsent ─────────────────────────────────────────────────

func TestReconcile_LiveAbsent(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"stored n", func(t *testing.T) {
			// D1 branch 3: no live credential. No slot becomes active; stored accounts
			// are returned unchanged; no profile call.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}

			out := Reconcile(ReconcileInput{
				LiveBlob:      nil,
				Stored:        stored,
				LookupProfile: noProfileCall(t),
				Now:           testNow,
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
		{"stored empty", func(t *testing.T) {
			// D1 branch 3 with no stored accounts.
			out := Reconcile(ReconcileInput{
				LiveBlob:      nil,
				Stored:        nil,
				LookupProfile: noProfileCall(t),
				Now:           testNow,
			})

			if out.ActiveKey != "" {
				t.Errorf("ActiveKey = %q, want empty", out.ActiveKey)
			}
			if len(out.Accounts) != 0 {
				t.Errorf("Accounts len = %d, want 0", len(out.Accounts))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

// ── TestReconcile_DuplicateAccessToken ──────────────────────────────────────

// TestReconcile_DuplicateAccessToken pins profile-based canonical selection
// when token matches alone identify multiple stored contexts.
func TestReconcile_DuplicateAccessToken(t *testing.T) {
	// Store the profiled account second: selecting the first token match would be
	// observably wrong.
	stored := []accounts.Account{
		makeAccount("uuid-second", "second@example.com", "tok-dup", "ref-b", 2000),
		makeAccount("uuid-first", "first@example.com", "tok-dup", "ref-a", 1000),
	}
	live := makeCred("tok-dup", "ref-a", 1000)
	profileCalls := 0

	out := Reconcile(ReconcileInput{
		LiveBlob: live,
		Stored:   stored,
		LookupProfile: func(string) (Profile, error) {
			profileCalls++
			return Profile{AccountUUID: "uuid-first", Email: "first@example.com", OrganizationUUID: accounts.PersonalOrganizationUUID}, nil
		},
		Now: testNow,
	})

	// Duplicate tokens are resolved by their profiled canonical key.
	if profileCalls != 1 {
		t.Fatalf("profile calls = %d, want 1", profileCalls)
	}
	if out.ActiveKey != "uuid-first_personal" {
		t.Errorf("ActiveKey = %q, want %q", out.ActiveKey, "uuid-first_personal")
	}
	if !out.Upserted {
		t.Error("Upserted = false, want true")
	}
}

// ── TestResolveActiveKey ─────────────────────────────────────────────────────

func TestResolveActiveKey(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"byte match", func(t *testing.T) {
			// Byte-match fast path: returns the slot UUID without calling LookupProfile.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}
			live := makeCred("tok-b", "ref-b", 2000) // matches slot[1]

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: noProfileCall(t),
				Now:           testNow,
			})

			testutil.WantNoErr(t, err)
			if uuid != "uuid-1_personal" {
				t.Errorf("uuid = %q, want %q", uuid, "uuid-1_personal")
			}
		}},
		{"profile lookup success", func(t *testing.T) {
			// Profile-lookup path: returns the account UUID from the profile (which
			// may or may not match a slot).
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-c", "ref-c", 3000) // no byte-match

			prof := Profile{AccountUUID: "uuid-from-profile", Email: "x@example.com"}

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: fixedProfile(prof),
				Now:           testNow,
			})

			testutil.WantNoErr(t, err)
			if uuid != "" {
				t.Errorf("uuid = %q, want empty for unstored profile key", uuid)
			}
		}},
		{"profile 401 returns empty", func(t *testing.T) {
			// A 401 from the profile endpoint returns ("", nil) — not an error —
			// so the caller can proceed without surfacing a spurious failure.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-c", "ref-c", 3000)

			authErr := fmt.Errorf("%w: HTTP 401", providers.ErrAuthDenied)

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: errProfile(authErr),
				Now:           testNow,
			})

			if err != nil {
				t.Errorf("expected nil error for 401, got: %v", err)
			}
			if uuid != "" {
				t.Errorf("uuid = %q, want empty for 401", uuid)
			}
		}},
		{"profile transient returns err", func(t *testing.T) {
			// A transient 503 from the profile endpoint is surfaced as an error so
			// the caller can distinguish it from a definitive "no active account".
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-c", "ref-c", 3000)

			transientErr := fmt.Errorf("%w: HTTP 503", providers.ErrTransient)

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: errProfile(transientErr),
				Now:           testNow,
			})

			testutil.WantErrIs(t, err, providers.ErrTransient)
			if uuid != "" {
				t.Errorf("uuid = %q, want empty on error", uuid)
			}
		}},
		{"no mutation", func(t *testing.T) {
			// D11 no-write guarantee: ResolveActiveKey must not mutate any element
			// in in.Stored, even when a profile call is made and succeeds.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
				makeAccount("uuid-1", "user1@example.com", "tok-b", "ref-b", 2000),
			}

			// Capture pre-call state for comparison.
			snapshot := make([]accounts.Account, len(stored))
			copy(snapshot, stored)

			live := makeCred("tok-c", "ref-c", 3000) // no byte-match forces a profile call

			prof := Profile{AccountUUID: "uuid-0", Email: "changed@example.com"}

			_, _ = ResolveActiveKey(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: fixedProfile(prof),
				Now:           testNow,
			})

			// Assert stored slice is byte-equal to the snapshot across all fields.
			for i, orig := range snapshot {
				got := stored[i]
				if got.UUID != orig.UUID {
					t.Errorf("stored[%d].UUID mutated: %q → %q", i, orig.UUID, got.UUID)
				}
				if got.Email != orig.Email {
					t.Errorf("stored[%d].Email mutated: %q → %q", i, orig.Email, got.Email)
				}
				if got.DisplayName != orig.DisplayName {
					t.Errorf("stored[%d].DisplayName mutated: %q → %q", i, orig.DisplayName, got.DisplayName)
				}
				if got.RateLimitTier != orig.RateLimitTier {
					t.Errorf("stored[%d].RateLimitTier mutated: %q → %q", i, orig.RateLimitTier, got.RateLimitTier)
				}
				if string(got.RawBlob) != string(orig.RawBlob) {
					t.Errorf("stored[%d].RawBlob mutated", i)
				}
				if got.LastSeenAt != orig.LastSeenAt {
					t.Errorf("stored[%d].LastSeenAt mutated: %v → %v", i, orig.LastSeenAt, got.LastSeenAt)
				}
			}
		}},
		{"profile missing fields returns empty", func(t *testing.T) {
			// ErrProfileMissingFields is treated like 401/403: returns ("", nil)
			// rather than surfacing an error.
			stored := []accounts.Account{
				makeAccount("uuid-0", "user0@example.com", "tok-a", "ref-a", 1000),
			}
			live := makeCred("tok-c", "ref-c", 3000)

			missingErr := fmt.Errorf("%w: got uuid=%q email=%q", ErrProfileMissingFields, "", "")

			uuid, err := ResolveActiveKey(ReconcileInput{
				LiveBlob:      live,
				Stored:        stored,
				LookupProfile: errProfile(missingErr),
				Now:           testNow,
			})

			if err != nil {
				t.Errorf("expected nil error for ErrProfileMissingFields, got: %v", err)
			}
			if uuid != "" {
				t.Errorf("uuid = %q, want empty for ErrProfileMissingFields", uuid)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}
