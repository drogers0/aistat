package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/drogers0/aistat/v2/internal/accounts"
	"github.com/drogers0/aistat/v2/internal/cred"
	"github.com/drogers0/aistat/v2/internal/providers"
)

// ReconcileInput is the full input set for Reconcile and ResolveActiveKey.
// LookupProfile is called only when the candidate set cannot identify one
// canonical slot without profiling.
type ReconcileInput struct {
	LiveBlob      *cred.Credential
	Stored        []accounts.Account
	LookupProfile func(accessToken string) (Profile, error)
	Now           time.Time
}

// ReconcileOutput is the result of a Reconcile call.
type ReconcileOutput struct {
	Accounts     []accounts.Account
	ActiveKey    string
	Promotion    *accounts.Promotion
	CaptureWarn  string
	Inserted     bool
	Upserted     bool
	LiveUnstored *cred.Credential
}

// Reconcile reconciles the live credential with stored Claude contexts. A
// legacy token match is intentionally profiled before canonical candidates so
// a previously interrupted promotion retries independent of Store.List order.
func Reconcile(in ReconcileInput) ReconcileOutput {
	out := ReconcileOutput{Accounts: append([]accounts.Account(nil), in.Stored...)}
	if in.LiveBlob == nil {
		return out
	}

	selection := selectActive(in)
	if selection.profileErr != nil {
		out.LiveUnstored = in.LiveBlob
		out.CaptureWarn = captureWarning(selection.profileErr)
		return out
	}

	if selection.direct >= 0 {
		out.Accounts[selection.direct] = refreshed(out.Accounts[selection.direct], in.LiveBlob, in.Now)
		out.ActiveKey = out.Accounts[selection.direct].Key()
		out.Upserted = true
		return out
	}

	if selection.profile.AccountUUID == "" {
		return out
	}
	destination := profiledAccount(selection.profile, in.LiveBlob, in.Now)
	if selection.legacy >= 0 {
		source := in.Stored[selection.legacy]
		instruction := &accounts.Promotion{
			SourceKey:             source.Key(),
			ObservedSourceRawBlob: source.RawBlob,
			Destination:           destination,
		}
		if destinationIndex := accountIndex(in.Stored, destination.Key()); destinationIndex >= 0 {
			instruction.ExpectedDestinationPresent = true
			instruction.ExpectedDestinationRawBlob = in.Stored[destinationIndex].RawBlob
		}
		out.Promotion = instruction
		out.ActiveKey = source.Key()
		return out
	}

	if target := accountIndex(out.Accounts, destination.Key()); target >= 0 {
		out.Accounts[target] = destination
		out.ActiveKey = destination.Key()
		out.Upserted = true
		return out
	}

	out.Accounts = append(out.Accounts, destination)
	out.ActiveKey = destination.Key()
	out.Inserted = true
	return out
}

// ResolveActiveKey identifies the stored active slot without writing. A
// profile-only identity is useful only when its canonical key is already
// stored; otherwise it must not select an arbitrary token-sharing context.
func ResolveActiveKey(in ReconcileInput) (string, error) {
	if in.LiveBlob == nil {
		return "", nil
	}
	selection := selectActive(in)
	if selection.profileErr != nil {
		if errors.Is(selection.profileErr, providers.ErrAuthDenied) || errors.Is(selection.profileErr, ErrProfileMissingFields) {
			return "", nil
		}
		return "", selection.profileErr
	}
	if selection.direct >= 0 {
		return in.Stored[selection.direct].Key(), nil
	}
	if selection.legacy >= 0 {
		return in.Stored[selection.legacy].Key(), nil
	}
	if selection.profile.AccountUUID == "" {
		return "", nil
	}
	key := profileKey(selection.profile)
	if accountIndex(in.Stored, key) >= 0 {
		return key, nil
	}
	return "", nil
}

type activeSelection struct {
	direct     int
	legacy     int
	profile    Profile
	profileErr error
}

func selectActive(in ReconcileInput) activeSelection {
	var legacy, canonical []int
	for i, account := range in.Stored {
		if StoredAccessToken(account) != in.LiveBlob.AccessToken {
			continue
		}
		if account.OrganizationUUID == "" {
			legacy = append(legacy, i)
		} else {
			canonical = append(canonical, i)
		}
	}
	if len(legacy) == 0 && len(canonical) == 1 {
		return activeSelection{direct: canonical[0], legacy: -1}
	}
	profile, err := in.LookupProfile(in.LiveBlob.AccessToken)
	if err != nil {
		return activeSelection{direct: -1, legacy: -1, profileErr: err}
	}
	for _, index := range legacy {
		if in.Stored[index].UUID == profile.AccountUUID {
			return activeSelection{direct: -1, legacy: index, profile: profile}
		}
	}
	return activeSelection{direct: -1, legacy: -1, profile: profile}
}

func accountIndex(stored []accounts.Account, key string) int {
	for i, account := range stored {
		if account.Key() == key {
			return i
		}
	}
	return -1
}

func profileKey(profile Profile) string {
	return accounts.Account{UUID: profile.AccountUUID, OrganizationUUID: profile.OrganizationUUID}.Key()
}

func profiledAccount(profile Profile, live *cred.Credential, now time.Time) accounts.Account {
	return accounts.Account{
		UUID:             profile.AccountUUID,
		Email:            profile.Email,
		DisplayName:      profile.DisplayName,
		RateLimitTier:    profile.RateLimitTier,
		OrganizationUUID: profile.OrganizationUUID,
		OrganizationName: profile.OrganizationName,
		OrganizationType: profile.OrganizationType,
		LastSeenAt:       now,
		RawBlob:          json.RawMessage(live.Raw),
	}
}

func refreshed(account accounts.Account, live *cred.Credential, now time.Time) accounts.Account {
	account.RawBlob = json.RawMessage(live.Raw)
	account.LastSeenAt = now
	return account
}

func captureWarning(err error) string {
	if errors.Is(err, ErrProfileMissingFields) {
		return "aistat: claude: profile response missing required fields (account.uuid/account.email/organization.uuid); rendering live row without storing; file an issue at https://github.com/drogers0/aistat/issues"
	}
	return fmt.Sprintf("aistat: claude: could not capture live account profile (%s); rendering live row without storing — run `claude /login` if this persists across runs", err)
}
