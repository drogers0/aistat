// Package accounts provides a provider-neutral persisted account store.
// Each Account holds opaque credential JSON (RawBlob) plus the identity
// fields shared across providers. Token parsing is provider-specific and
// lives in the respective provider package (e.g. internal/providers/claude).
package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PersonalOrganizationUUID identifies the defensive nil-organization Claude
// profile case. It is a storage-key sentinel, not a real organization UUID.
const PersonalOrganizationUUID = "personal"

// Account is a persisted provider identity. RawBlob is the verbatim credential
// JSON from the provider's live store; it is written back byte-for-byte by
// `aistat switch` so unknown fields are never dropped.
type Account struct {
	UUID             string    `json:"uuid"`
	Email            string    `json:"email"`
	DisplayName      string    `json:"display_name"`
	RateLimitTier    string    `json:"rate_limit_tier"`
	OrganizationUUID string    `json:"organization_uuid"`
	OrganizationName string    `json:"organization_name"`
	OrganizationType string    `json:"organization_type"`
	LastSeenAt       time.Time `json:"last_seen_at"`
	// RawBlob is the full credential JSON blob as read from the provider's live
	// store. `aistat switch` writes this blob back verbatim.
	RawBlob json.RawMessage `json:"raw_blob"`
}

// NewAccount constructs an Account from a raw credential JSON blob plus the
// identity fields resolved via the provider's profile endpoint. To avoid a
// cycle with provider packages, the constructor takes discrete identity strings
// rather than a provider-specific profile struct.
//
// Returns an error if raw is empty, not valid JSON, or uuid is empty.
func NewAccount(raw json.RawMessage, uuid, email, displayName, rateLimitTier,
	organizationUUID, organizationName, organizationType string, now time.Time) (Account, error) {
	if len(raw) == 0 {
		return Account{}, errors.New("accounts: raw credential blob is empty")
	}
	if !json.Valid(raw) {
		return Account{}, fmt.Errorf("accounts: raw credential is not valid JSON")
	}
	if uuid == "" {
		return Account{}, errors.New("accounts: uuid is required")
	}
	return Account{
		UUID:             uuid,
		Email:            email,
		DisplayName:      displayName,
		RateLimitTier:    rateLimitTier,
		OrganizationUUID: organizationUUID,
		OrganizationName: organizationName,
		OrganizationType: organizationType,
		LastSeenAt:       now,
		RawBlob:          raw,
	}, nil
}

// Key returns the opaque persisted identity for this account. Callers must not
// parse it: a bare UUID represents Codex or a legacy Claude row.
func (a Account) Key() string {
	if a.OrganizationUUID == "" {
		return a.UUID
	}
	return a.UUID + "_" + a.OrganizationUUID
}
