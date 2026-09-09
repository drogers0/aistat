package claude

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/httpx"
)

const (
	profileEndpoint = "https://api.anthropic.com/api/oauth/profile"
	profileTimeout  = 3 * time.Second
)

// ErrProfileMissingFields is returned when the profile endpoint responds with
// HTTP 200 but the required account.uuid/account.email/organization.uuid
// identity is missing or invalid.
// The caller (reconcile path, D1 step 4) maps this to the distinct diagnostic:
// "aistat: claude: profile response missing required fields (account.uuid/account.email/organization.uuid);
// rendering live row without storing; file an issue at https://github.com/drogers0/aistat/issues".
var ErrProfileMissingFields = errors.New("profile response missing required fields (account.uuid/account.email/organization.uuid)")

var organizationUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// Profile holds the identity fields extracted from GET /api/oauth/profile.
type Profile struct {
	AccountUUID string
	Email       string
	DisplayName string
	// RateLimitTier is supplied by the organization block. A nil organization
	// is a defensive wire case only; observed personal accounts have one.
	RateLimitTier    string
	OrganizationUUID string
	OrganizationName string
	OrganizationType string
}

// profileWire is the JSON shape returned by GET /api/oauth/profile.
// Only the fields aistat consumes are declared; extras are silently ignored.
type profileWire struct {
	Account struct {
		UUID        string `json:"uuid"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	} `json:"account"`
	// Organization is absent only for the defensive personal sentinel case.
	Organization *struct {
		UUID          string `json:"uuid"`
		Name          string `json:"name"`
		Type          string `json:"organization_type"`
		RateLimitTier string `json:"rate_limit_tier"`
	} `json:"organization"`
}

type profileClient struct {
	doer     *httpx.Doer
	endpoint string
	timeout  time.Duration
}

func newProfileClient(doer *httpx.Doer) *profileClient {
	return &profileClient{
		doer:     doer,
		endpoint: profileEndpoint,
		timeout:  profileTimeout,
	}
}

// Get fetches the profile for the given access token.
// Error classification:
//   - 401/403 → wraps providers.ErrAuthDenied (via httpx.DefaultClassify)
//   - 408/429/5xx → wraps providers.ErrTransient (via httpx.DefaultClassify)
//   - Other 4xx → bare error
//   - HTTP 200 with missing identity fields → ErrProfileMissingFields
func (p *profileClient) Get(ctx context.Context, accessToken string) (Profile, error) {
	var wire profileWire
	if err := p.doer.GetJSON(ctx, p.endpoint, accessToken, p.timeout, &wire, httpx.DefaultClassify); err != nil {
		return Profile{}, err
	}
	wireOrganizationUUID := ""
	if wire.Organization != nil {
		wireOrganizationUUID = wire.Organization.UUID
	}
	if wire.Account.UUID == "" || wire.Account.Email == "" ||
		(wire.Organization != nil && (wireOrganizationUUID == "" || wireOrganizationUUID == accounts.PersonalOrganizationUUID || !organizationUUIDPattern.MatchString(wireOrganizationUUID))) {
		return Profile{}, fmt.Errorf("%w: got uuid=%q email=%q organization.uuid=%q", ErrProfileMissingFields, wire.Account.UUID, wire.Account.Email, wireOrganizationUUID)
	}
	prof := Profile{
		AccountUUID: wire.Account.UUID,
		Email:       wire.Account.Email,
		DisplayName: wire.Account.DisplayName,
	}
	if wire.Organization != nil {
		prof.RateLimitTier = wire.Organization.RateLimitTier
		prof.OrganizationUUID = wire.Organization.UUID
		prof.OrganizationName = wire.Organization.Name
		prof.OrganizationType = wire.Organization.Type
	} else {
		prof.OrganizationUUID = accounts.PersonalOrganizationUUID
	}
	return prof, nil
}
