package codex

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
)

// ReconcileInput is the full input set for Reconcile and ResolveActiveKey.
//
// LookupID is called with the live id_token when no byte-match is found and
// the id_token is non-empty. It must not be nil when LiveBlob is non-nil.
// Identity resolution is pure (D1: JWT decode, no network).
type ReconcileInput struct {
	LiveBlob *cred.Credential // nil if absent; Raw holds exact live bytes
	Stored   []accounts.Account
	LookupID func(idToken string) (sub, email string, err error)
	Now      time.Time
}

// ReconcileOutput is the result of a Reconcile call.
type ReconcileOutput struct {
	Accounts     []accounts.Account
	ActiveKey    string           // "" if none
	CaptureWarn  string           // non-empty when fallback applied (D3)
	Inserted     bool             // true if a new account slot was created
	Upserted     bool             // true if an existing slot was updated
	LiveUnstored *cred.Credential // non-nil when fallback applied; render-only
}

// Reconcile executes the Codex D-tree over the live credential and stored
// account slots. It is a pure function — all I/O (file read, JWT decode) is
// pre-resolved and passed via in. The caller is responsible for persisting
// out.Accounts when out.Inserted or out.Upserted is true.
func Reconcile(in ReconcileInput) ReconcileOutput {
	out := ReconcileOutput{
		Accounts: make([]accounts.Account, len(in.Stored)),
	}
	// Shallow copy: RawBlob is []byte (reference type); every code path that
	// changes RawBlob assigns a fresh slice — never index-writes in place.
	copy(out.Accounts, in.Stored)

	if in.LiveBlob == nil {
		return out
	}

	matchIdx, sub, email, lookupErr := findActive(in)

	switch {
	case matchIdx >= 0:
		// Byte-match: upsert RawBlob and LastSeenAt without a LookupID call.
		slot := out.Accounts[matchIdx]
		slot.RawBlob = json.RawMessage(in.LiveBlob.Raw)
		slot.LastSeenAt = in.Now
		out.Accounts[matchIdx] = slot
		out.ActiveKey = slot.Key()
		out.Upserted = true

	case lookupErr != nil:
		// Identity lookup failed (missing id_token or parse error): render an
		// unstored live row and emit CaptureWarn (D3).
		out.LiveUnstored = in.LiveBlob
		out.CaptureWarn = fmt.Sprintf(
			"aistat: codex: could not identify live account (%s); rendering live row without storing — run `codex login` if this persists across runs",
			lookupErr.Error(),
		)

	default:
		// LookupID succeeded. UUID wins for matching; overwrite email if drifted.
		for i, acct := range out.Accounts {
			if acct.UUID == sub {
				slot := out.Accounts[i]
				slot.Email = email
				slot.RawBlob = json.RawMessage(in.LiveBlob.Raw)
				slot.LastSeenAt = in.Now
				out.Accounts[i] = slot
				out.ActiveKey = slot.Key()
				out.Upserted = true
				return out
			}
		}
		// No UUID match — insert a new slot.
		out.Accounts = append(out.Accounts, accounts.Account{
			UUID:       sub,
			Email:      email,
			LastSeenAt: in.Now,
			RawBlob:    json.RawMessage(in.LiveBlob.Raw),
		})
		out.ActiveKey = out.Accounts[len(out.Accounts)-1].Key()
		out.Inserted = true
	}

	return out
}

// ResolveActiveKey is the read-only variant used by FetchForSwitch to identify
// the active account key without any store mutations. All LookupID failures
// (parse errors, missing id_token) return ("", nil) — not an error — because
// D1 guarantees there are no transient network failures on the identity path
// (pure JWT decode). T4's switch.go must not depend on surfaced errors from
// Codex ResolveActiveKey.
func ResolveActiveKey(in ReconcileInput) (string, error) {
	if in.LiveBlob == nil {
		return "", nil
	}

	matchIdx, sub, _, lookupErr := findActive(in)

	if matchIdx >= 0 {
		return in.Stored[matchIdx].Key(), nil
	}

	if lookupErr != nil {
		// All LookupID failures are parse errors (D1); never transient.
		return "", nil
	}

	if accountIndex(in.Stored, sub) >= 0 {
		return sub, nil
	}
	return "", nil
}

func accountIndex(stored []accounts.Account, key string) int {
	for i, account := range stored {
		if account.Key() == key {
			return i
		}
	}
	return -1
}

// findActive resolves the active account from the live blob without mutating
// in.Stored. It first scans for a byte-match; if none is found it calls
// LookupID via the id_token extracted from the live blob.
//
// Returns:
//   - matchIdx >= 0: index into in.Stored of the byte-matched slot. No LookupID call.
//   - matchIdx == -1, lookupErr == nil: LookupID succeeded; sub and email populated.
//   - matchIdx == -1, lookupErr != nil: byte-match missed and LookupID failed (or
//     id_token was absent/empty; LookupID is then never called).
//
// Precondition: in.LiveBlob is non-nil.
func findActive(in ReconcileInput) (matchIdx int, sub, email string, lookupErr error) {
	// Step 1: byte-match. First match wins (pathological duplicate-AT case).
	for i, acct := range in.Stored {
		if StoredAccessToken(acct) == in.LiveBlob.AccessToken {
			return i, "", "", nil
		}
	}

	// Step 2: extract id_token from live blob; guard before calling LookupID.
	idToken := extractIDToken(in.LiveBlob.Raw)
	if idToken == "" {
		return -1, "", "", fmt.Errorf("no id_token in live credential")
	}

	s, e, err := in.LookupID(idToken)
	if err != nil {
		return -1, "", "", err
	}
	return -1, s, e, nil
}

// extractIDToken parses tokens.id_token from raw Codex auth.json bytes.
// Returns "" on any error (missing field, malformed JSON, etc.).
func extractIDToken(raw []byte) string {
	var r struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return ""
	}
	return r.Tokens.IDToken
}
